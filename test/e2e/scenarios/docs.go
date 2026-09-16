package scenarios

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/b3vet/atlascache/test/e2e/runner"
)

func init() {
	runner.RegisterScenario("docs_quickstart_executes", docsQuickstartExecutes)
}

// The guide this scenario keeps honest. Its commands are the first thing a new
// user runs, so a command that no longer works is a first impression rather
// than an inconvenience.
const quickstartDoc = "docs/getting-started.md"

// The port the guide tells the reader to use. The harness picks a free port per
// spec, so every extracted command has this rewritten to the real one.
const quickstartDocPort = "6379"

// docsQuickstartExecutes runs the documented quick start against a live server.
//
// It reads the commands out of the markdown rather than restating them here.
// A spec that re-typed them would drift from the document it exists to protect:
// the guide would change, the copy would not, and the gate would pass over a
// broken quick start. Parsing the source of truth is the only version of this
// test that cannot silently stop testing the thing it names.
//
// Two levels of assertion, because they catch different failures:
//
//   - Every extracted command must execute without an error reply. This is what
//     catches a command that was renamed, removed, or written with the wrong
//     arity — the drift that actually happens as a project moves.
//   - Where the guide shows the output, it must match. This catches a reply
//     whose shape or value changed while the command kept working, which is the
//     quieter and more misleading kind of wrong.
//
// Output is compared only where the comparison is sound: a block with the same
// number of output lines as commands, and no shell pipeline in it. That
// deliberately skips the `INFO server | head -2` example, whose documented
// output carries a version string that will change at release — asserting it
// would make the gate fail on a correct release.
func docsQuickstartExecutes(c *runner.Ctx) error {
	root, err := repoRoot()
	if err != nil {
		return err
	}

	path := filepath.Join(root, quickstartDoc)
	source, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", quickstartDoc, err)
	}

	blocks := parseQuickstartBlocks(string(source))
	if len(blocks) == 0 {
		return fmt.Errorf(
			"%s contains no redis-cli commands, so this scenario is asserting nothing; "+
				"either the guide was restructured or the parser no longer recognizes it",
			quickstartDoc)
	}

	port, err := clientPort(c.Info().ClientAddr)
	if err != nil {
		return err
	}

	executed := 0
	compared := 0

	for _, block := range blocks {
		// Output is paired with commands positionally, so it is only trustworthy
		// when the counts agree and nothing in the block is a shell pipeline
		// whose output the server never produced.
		comparable := len(block.output) == len(block.commands) && !block.piped

		for i, command := range block.commands {
			cmd := strings.ReplaceAll(command, " -p "+quickstartDocPort+" ", " -p "+port+" ")
			args := strings.TrimSpace(strings.TrimPrefix(cmd, "redis-cli -p "+port))

			reply, err := c.Send(args)
			if err != nil {
				return fmt.Errorf("%s line %d: %q: %w", quickstartDoc, block.line, args, err)
			}
			executed++

			if reply.Kind == runner.KindError {
				return fmt.Errorf(
					"%s line %d documents %q, which the server refuses: %s",
					quickstartDoc, block.line, args, reply.Text)
			}

			if !comparable {
				continue
			}
			want := block.output[i]
			got := renderLikeRedisCLI(reply)
			if !quickstartOutputMatches(args, want, got) {
				return fmt.Errorf(
					"%s line %d documents %q as printing %q, but it prints %q",
					quickstartDoc, block.line, args, want, got)
			}
			compared++
		}
	}

	c.Logf("%s: %d commands executed, %d outputs compared", quickstartDoc, executed, compared)

	if compared == 0 {
		return fmt.Errorf(
			"no documented output in %s was comparable, so only execution was checked; "+
				"the guide's example output is no longer paired with its commands",
			quickstartDoc)
	}
	return nil
}

// quickstartBlock is one fenced command block and the output block that follows
// it, if any.
type quickstartBlock struct {
	line     int
	commands []string
	output   []string
	piped    bool
}

// parseQuickstartBlocks pulls the redis-cli commands, and the output shown for
// them, out of the guide's fenced code blocks.
//
// The shape it understands is the one the guide uses: a ```bash block of
// commands, optionally followed by an unlabeled ``` block showing what they
// print. Blocks with no redis-cli line are skipped, which covers the build
// steps, the Go snippet, and the atlasctl examples — those are verified
// elsewhere, by the build itself and by the cli specs.
func parseQuickstartBlocks(source string) []quickstartBlock {
	lines := strings.Split(source, "\n")

	var blocks []quickstartBlock
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "```bash" {
			continue
		}
		start := i + 1
		end := closingFence(lines, start)
		if end < 0 {
			break
		}

		block := quickstartBlock{line: start + 1}
		for _, line := range lines[start:end] {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "redis-cli ") {
				continue
			}
			if before, _, piped := strings.Cut(line, "|"); piped {
				block.piped = true
				line = strings.TrimSpace(before)
			}
			block.commands = append(block.commands, line)
		}
		i = end

		if len(block.commands) == 0 {
			continue
		}

		// An unlabeled fence immediately after the commands, separated by at
		// most a blank line, is that block's output.
		if next := nextFenceStart(lines, end+1); next >= 0 && strings.TrimSpace(lines[next]) == "```" {
			outEnd := closingFence(lines, next+1)
			if outEnd >= 0 {
				for _, line := range lines[next+1 : outEnd] {
					block.output = append(block.output, strings.TrimSpace(line))
				}
				i = outEnd
			}
		}

		blocks = append(blocks, block)
	}
	return blocks
}

// closingFence returns the index of the fence closing a block that starts at
// from, or -1 if the block is never closed.
func closingFence(lines []string, from int) int {
	for i := from; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "```" {
			return i
		}
	}
	return -1
}

// nextFenceStart returns the index of the next fence at or after from, provided
// only blank lines separate it. Anything else — prose, a heading — means the
// block that follows is not this block's output.
func nextFenceStart(lines []string, from int) int {
	for i := from; i < len(lines); i++ {
		switch {
		case strings.TrimSpace(lines[i]) == "":
			continue
		case strings.HasPrefix(strings.TrimSpace(lines[i]), "```"):
			return i
		default:
			return -1
		}
	}
	return -1
}

// renderLikeRedisCLI prints a reply the way redis-cli prints it, since that is
// what the guide shows the reader.
func renderLikeRedisCLI(reply runner.Reply) string {
	switch reply.Kind {
	case runner.KindStatus:
		return reply.Text
	case runner.KindBulk:
		return strconv.Quote(reply.Text)
	case runner.KindInteger:
		return "(integer) " + strconv.FormatInt(reply.Integer, 10)
	case runner.KindNil:
		return "(nil)"
	default:
		return reply.String()
	}
}

// quickstartOutputMatches compares documented output against what the server
// actually printed, with one deliberate tolerance.
//
// A documented TTL is stale the moment it is written: the guide shows
// `(integer) 60` immediately after `SET ... EX 60`, and on a loaded machine the
// second elapses between the two commands. Asserting equality there would make
// the gate fail for a reason that has nothing to do with correctness, which is
// how a suite earns a reputation for flakiness and stops being believed. A
// small window keeps the assertion meaningful — a TTL that came back as 5, or
// as -2, still fails.
func quickstartOutputMatches(command, want, got string) bool {
	if want == got {
		return true
	}
	if !strings.HasPrefix(strings.ToUpper(command), "TTL ") {
		return false
	}
	wantTTL, ok := parseIntegerReply(want)
	if !ok {
		return false
	}
	gotTTL, ok := parseIntegerReply(got)
	if !ok {
		return false
	}
	// Only counts down, and only by the time the two commands took.
	return gotTTL <= wantTTL && wantTTL-gotTTL <= 2
}

func parseIntegerReply(text string) (int64, bool) {
	const prefix = "(integer) "
	if !strings.HasPrefix(text, prefix) {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(text, prefix), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// clientPort pulls the port out of the harness's client address.
func clientPort(addr string) (string, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 || i == len(addr)-1 {
		return "", fmt.Errorf("the harness reports a client address of %q, which carries no port", addr)
	}
	return addr[i+1:], nil
}

// repoRoot walks up from the working directory to the directory holding
// go.work, which is the repository root whatever the runner was invoked from.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("locating the repository root: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.work above %q, so the repository root cannot be located", dir)
		}
		dir = parent
	}
}
