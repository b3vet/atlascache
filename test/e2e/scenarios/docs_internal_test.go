package scenarios

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/b3vet/atlascache/test/e2e/runner"
)

// The parser is the part of this scenario that can fail silently: if it stops
// recognizing the guide's shape it finds nothing, and a test that asserts
// nothing passes. The scenario guards against that at runtime by failing on an
// empty parse; these cases pin the shapes it must keep understanding.

func TestDocsScenarioIsRegistered(t *testing.T) {
	if _, ok := runner.LookupScenario("docs_quickstart_executes"); !ok {
		t.Fatal("docs-quickstart names this scenario, and an unregistered scenario fails the spec at run time")
	}
}

func TestParseQuickstartBlocksPairsOutput(t *testing.T) {
	blocks := parseQuickstartBlocks("```bash\n" +
		"redis-cli -p 6379 PING\n" +
		"redis-cli -p 6379 GET hello\n" +
		"```\n\n" +
		"```\nPONG\n\"world\"\n```\n")

	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	wantCommands := []string{"redis-cli -p 6379 PING", "redis-cli -p 6379 GET hello"}
	if !slices.Equal(blocks[0].commands, wantCommands) {
		t.Errorf("commands = %q, want %q", blocks[0].commands, wantCommands)
	}
	wantOutput := []string{"PONG", `"world"`}
	if !slices.Equal(blocks[0].output, wantOutput) {
		t.Errorf("output = %q, want %q", blocks[0].output, wantOutput)
	}
	if blocks[0].piped {
		t.Error("no pipeline in this block, so its output is what the server sent")
	}
}

func TestParseQuickstartBlocksSkipsNonRedisBlocks(t *testing.T) {
	blocks := parseQuickstartBlocks("```bash\nmake build\ncd atlascache\n```\n")
	if len(blocks) != 0 {
		t.Errorf("got %d blocks, want none: build instructions are verified by the build", len(blocks))
	}
}

func TestParseQuickstartBlocksMarksPipelines(t *testing.T) {
	blocks := parseQuickstartBlocks("```bash\nredis-cli -p 6379 INFO server | head -2\n```\n")

	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	if !blocks[0].piped {
		t.Error("output of a pipeline is not what the server sent, so it must not be compared")
	}
	want := []string{"redis-cli -p 6379 INFO server"}
	if !slices.Equal(blocks[0].commands, want) {
		t.Errorf("commands = %q, want %q: the pipeline is stripped before sending", blocks[0].commands, want)
	}
}

func TestParseQuickstartBlocksIgnoresDistantFence(t *testing.T) {
	blocks := parseQuickstartBlocks("```bash\nredis-cli -p 6379 PING\n```\n\n" +
		"Some prose in between.\n\n" +
		"```\nnot the output\n```\n")

	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	if len(blocks[0].output) != 0 {
		t.Errorf("output = %q, want none: prose separates the two, so the second block is unrelated",
			blocks[0].output)
	}
}

func TestParseQuickstartBlocksSurvivesUnclosedFence(t *testing.T) {
	// A half-written guide must not take the suite down with a panic.
	blocks := parseQuickstartBlocks("```bash\nredis-cli -p 6379 PING\n")
	if len(blocks) != 0 {
		t.Errorf("got %d blocks, want none from an unterminated fence", len(blocks))
	}
}

func TestQuickstartOutputMatchesExactly(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    string
		got     string
		match   bool
	}{
		{"identical status", "SET a b", "OK", "OK", true},
		{"different status", "SET a b", "OK", "QUEUED", false},
		{"different bulk", "GET a", `"world"`, `"worlds"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quickstartOutputMatches(tc.command, tc.want, tc.got); got != tc.match {
				t.Errorf("quickstartOutputMatches(%q, %q, %q) = %v, want %v",
					tc.command, tc.want, tc.got, got, tc.match)
			}
		})
	}
}

// A documented TTL is stale the moment it is written, so the comparison allows
// the clock to have moved — but only by the time two commands take, and only
// downward. The bounds are the whole point: too loose and the assertion stops
// meaning anything.
func TestQuickstartOutputMatchesTTLWindow(t *testing.T) {
	cases := []struct {
		name  string
		want  string
		got   string
		match bool
	}{
		{"unchanged", "(integer) 60", "(integer) 60", true},
		{"counted down within the window", "(integer) 60", "(integer) 58", true},
		{"counted down past the window", "(integer) 60", "(integer) 57", false},
		{"missing key must not pass as a live one", "(integer) 60", "(integer) -2", false},
		{"no expiry must not pass as a live one", "(integer) 60", "(integer) -1", false},
		{"a TTL never counts up", "(integer) 60", "(integer) 61", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quickstartOutputMatches("TTL session", tc.want, tc.got); got != tc.match {
				t.Errorf("quickstartOutputMatches(TTL, %q, %q) = %v, want %v",
					tc.want, tc.got, got, tc.match)
			}
		})
	}
}

// The guide is the input this scenario parses. A restructure that leaves it
// unparseable must fail here, rather than pass as a run that asserted nothing.
func TestQuickstartDocIsParseable(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locating the repository root: %v", err)
	}

	source, err := os.ReadFile(filepath.Join(root, quickstartDoc))
	if err != nil {
		t.Fatalf("reading %s: %v", quickstartDoc, err)
	}

	blocks := parseQuickstartBlocks(string(source))
	if len(blocks) == 0 {
		t.Fatalf("%s has no redis-cli commands this scenario can run", quickstartDoc)
	}

	var commands, comparable int
	for _, block := range blocks {
		commands += len(block.commands)
		if len(block.output) == len(block.commands) && !block.piped {
			comparable += len(block.commands)
		}
	}
	if commands == 0 {
		t.Errorf("%s yielded blocks but no commands", quickstartDoc)
	}
	if comparable == 0 {
		t.Errorf("no documented output in %s is comparable, so only execution would be checked", quickstartDoc)
	}
}

func TestClientPort(t *testing.T) {
	port, err := clientPort("127.0.0.1:6379")
	if err != nil {
		t.Fatalf("clientPort: %v", err)
	}
	if port != "6379" {
		t.Errorf("port = %q, want 6379", port)
	}

	if _, err := clientPort("127.0.0.1"); err == nil {
		t.Error("an address with no port cannot be rewritten into the documented commands, so it must error")
	}
}
