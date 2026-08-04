package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// commandResult is what a check learns from running another tool: whether it
// succeeded, and everything it said while failing.
type commandResult struct {
	Output string
	Err    error
}

func (c commandResult) ok() bool { return c.Err == nil }

// commandFunc runs name with args in dir. It is a field on gate so tests can
// exercise the report without a five-minute test suite behind it.
type commandFunc func(dir, name string, args ...string) commandResult

// execCommand runs a command and captures stdout and stderr together: tools
// split their diagnosis across both, and a check that read only one would
// report "exit status 1" with no reason attached.
//
// The context carries no deadline. Each tool the gate drives owns its own
// budget — the E2E tiers are defined by one — and a timeout here would report a
// killed command as a failed check.
func execCommand(dir, name string, args ...string) commandResult {
	cmd := exec.CommandContext(context.Background(), name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return commandResult{Output: string(out), Err: err}
}

// findGolangciLint locates the linter. `make tools` installs it into
// $(go env GOPATH)/bin, which is frequently not on PATH, so look there before
// concluding it is missing — a lint check that silently skips on a developer
// machine is worse than no lint check.
func findGolangciLint(name string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	out, err := exec.CommandContext(context.Background(), "go", "env", "GOPATH").Output()
	if err != nil {
		return "", errors.New(name + " not found on PATH")
	}
	gopath := strings.TrimSpace(string(out))
	if gopath == "" {
		return "", errors.New(name + " not found on PATH")
	}
	binDir := filepath.Join(gopath, "bin")
	candidate := filepath.Join(binDir, name)
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
		return candidate, nil
	}
	return "", errors.New(name + " not found on PATH or in " + binDir)
}

// summarizeFailure picks the single most informative line out of a failed
// command's output. Go tools name the failing package or test on a line
// starting with FAIL; everything else is likelier to put its verdict last.
func summarizeFailure(res commandResult) string {
	lines := meaningfulLines(res.Output)
	for _, l := range lines {
		if strings.HasPrefix(l, "--- FAIL") {
			return truncate(l)
		}
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "FAIL") {
			return truncate(l)
		}
	}
	if len(lines) > 0 {
		return truncate(lines[len(lines)-1])
	}
	if res.Err != nil {
		return res.Err.Error()
	}
	return "failed with no output"
}

// makeNoisePattern matches GNU make's own failure lines. They say that a recipe
// failed, never why, and they are always the last thing printed — which is
// exactly where a summary looks when nothing better presents itself.
var makeNoisePattern = regexp.MustCompile(`^make(\[\d+\])?: `)

// meaningfulLines drops blank lines, warnings, and make's bookkeeping. A warning
// is advice, not the reason a command exited non-zero.
func meaningfulLines(output string) []string {
	var out []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if strings.TrimSpace(line) == "" || strings.Contains(line, "warning:") || makeNoisePattern.MatchString(line) {
			continue
		}
		out = append(out, line)
	}
	return out
}

// lintIssuePattern matches golangci-lint's issue format, path:line:col: text.
var lintIssuePattern = regexp.MustCompile(`^\S+:\d+:\d+: \S`)

// summarizeLint counts the issues golangci-lint reported and quotes the first,
// which is more use than the per-linter tally it prints at the end.
func summarizeLint(res commandResult) string {
	var first string
	issues := 0
	for _, line := range meaningfulLines(res.Output) {
		if !lintIssuePattern.MatchString(line) {
			continue
		}
		issues++
		if first == "" {
			first = line
		}
	}
	if issues == 0 {
		return summarizeFailure(res)
	}
	if issues == 1 {
		return truncate(first)
	}
	return truncate(fmt.Sprintf("%d issues, first: %s", issues, first))
}

// maxDetail keeps one row on one line on a normal terminal. The full output is
// reprinted under the table, so nothing is lost by clipping here.
const maxDetail = 100

func truncate(s string) string {
	if len(s) <= maxDetail {
		return s
	}
	return s[:maxDetail-1] + "…"
}
