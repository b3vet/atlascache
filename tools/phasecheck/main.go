// Command phasecheck decides whether a phase is ready to close.
//
//	phasecheck --phase P0     run every check for phase P0
//	phasecheck --dir path     read a dev tree somewhere other than ./dev
//	phasecheck --repo path    run the module checks against another checkout
//
// It aggregates tools that already exist — the E2E runner, `go test`,
// `golangci-lint`, `devindex` — and reads `dev/` front-matter for the checks
// that are about the planning record. Every failing check says why it failed,
// and the command exits non-zero when any check fails.
//
// The dev tree is private and gitignored, so a public clone will not have it.
// The checks that read it report `skipped` rather than failing; the checks that
// read the repository itself still run.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// phasePattern matches a phase id, P0 through P99 — the same form devindex
// requires of a phase plan file.
var phasePattern = regexp.MustCompile(`^P\d{1,2}$`)

const usage = "usage: make phase-check PHASE=P0\n" +
	"       phasecheck --phase P0 [--dir dev] [--repo .]\n"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "phasecheck: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("phasecheck", flag.ContinueOnError)
	flags.SetOutput(stderr)
	// `make phase-check` with no PHASE= expands to a bare --phase, and the
	// default usage dump buries the one thing the reader needs to do next.
	flags.Usage = func() { fmt.Fprint(stderr, usage) }
	phase := flags.String("phase", "", "phase to check, such as P0")
	dir := flags.String("dir", "dev", "path to the dev tree")
	repo := flags.String("repo", ".", "path to the repository root")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *phase == "" {
		return errors.New("missing required flag: --phase (invoke it as `make phase-check PHASE=P0`)")
	}
	if !phasePattern.MatchString(*phase) {
		return fmt.Errorf("--phase %q is not a phase id such as P0", *phase)
	}

	g := newGate(*phase, *dir, *repo)
	results := report(stdout, g)

	if n := count(results, fail); n > 0 {
		return fmt.Errorf("phase %s is not ready to close: %s failed", g.Phase, plural(n, "check", "checks"))
	}
	return nil
}

// report runs every check and prints the table as it goes. Rows are written the
// moment a check finishes rather than at the end: the full tier alone takes
// minutes, and a command that prints nothing for minutes looks hung.
func report(w io.Writer, g *gate) []result {
	checks := g.checks()

	width := 0
	for _, c := range checks {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}

	fmt.Fprintf(w, "phase-check %s\n\n", g.Phase)
	results := make([]result, 0, len(checks))
	for _, c := range checks {
		out, detail := c.Run()
		results = append(results, result{Name: c.Name, Outcome: out, Detail: detail})
		fmt.Fprintf(w, "%s %-*s  %s\n", out.mark(), width, c.Name, out.describe(detail))
	}

	fmt.Fprintf(w, "\n%s\n", verdict(g.Phase, results))
	renderFailures(w, results)
	renderTranscripts(w, g.transcripts)
	return results
}

// verdict is the one line a reader looks for.
func verdict(phase string, results []result) string {
	failed, skipped := count(results, fail), count(results, skip)
	state := "READY TO CLOSE"
	if failed > 0 {
		state = "NOT READY TO CLOSE"
	}
	line := fmt.Sprintf("PHASE %s: %s", phase, state)
	var notes []string
	if failed > 0 {
		notes = append(notes, fmt.Sprintf("%d failed", failed))
	}
	if skipped > 0 {
		notes = append(notes, fmt.Sprintf("%d skipped", skipped))
	}
	if len(notes) == 0 {
		return line
	}
	return fmt.Sprintf("%s (%s)", line, strings.Join(notes, ", "))
}

// renderFailures repeats the failing rows under the verdict, so the reason a
// phase cannot close survives being scrolled past a long table.
func renderFailures(w io.Writer, results []result) {
	first := true
	for _, r := range results {
		if r.Outcome != fail {
			continue
		}
		if first {
			fmt.Fprintf(w, "\nFailed:\n")
			first = false
		}
		fmt.Fprintf(w, "  %s: %s\n", r.Name, r.Detail)
	}
}

// renderTranscripts prints the tail of every command that failed. The row says
// which check failed; this says what the tool itself reported.
func renderTranscripts(w io.Writer, transcripts []transcript) {
	for _, t := range transcripts {
		lines := tail(t.Output, transcriptLines)
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n--- %s: last %s of output ---\n", t.Name, plural(len(lines), "line", "lines"))
		for _, line := range lines {
			fmt.Fprintf(w, "%s\n", line)
		}
	}
}

// transcriptLines bounds the per-failure output tail: enough for a Go build
// error or a failed spec, short of pasting an entire test run.
const transcriptLines = 20

func tail(output string, n int) []string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
