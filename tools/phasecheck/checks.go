package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// goModules are every module in the repository. `go test ./...` in the root
// module reaches neither pkg/client nor test/e2e — both are separate modules —
// so each is run in its own directory. A check that silently covered one of
// them would be worse than no check at all.
//
// Adding a module to the workspace means adding it here. Nothing enforces that,
// which is the weakness of this list: the gate would keep reporting green while
// covering less. `make tidy-check` is the closest thing to a tripwire, since it
// fails when a module's go.mod drifts.
var goModules = []struct {
	Label string
	Dir   string
}{
	{"root", "."},
	{"pkg/client", "pkg/client"},
	{"test/e2e", "test/e2e"},
}

// goTest is the go subcommand the module checks drive.
const goTest = "test"

// moduleRun is one module's test run: the verdict and, when the tests passed,
// its statement coverage.
type moduleRun struct {
	Label    string
	Result   commandResult
	Coverage float64
	Measured bool
}

// checkE2E runs the full tier. It shells out to `make e2e-full` so the gate
// keeps using whatever build and flags the Makefile settles on, rather than
// growing its own copy of them.
func (g *gate) checkE2E() (outcome, string) {
	res := g.run(g.RepoDir, "make", "e2e-full")
	if !res.ok() {
		g.record("make e2e-full", res)
		return fail, summarizeFailure(res)
	}
	return pass, "green"
}

// checkRace reports the test run itself; checkCoverage reports what that run
// measured. Both read the same execution — running the suite twice to answer
// two questions about it would double the slowest part of the gate after the
// E2E tier.
func (g *gate) checkRace() (outcome, string) {
	runs := g.moduleTests()
	labels := make([]string, 0, len(runs))
	var failures []string
	for _, m := range runs {
		labels = append(labels, m.Label)
		if !m.Result.ok() {
			failures = append(failures, fmt.Sprintf("%s: %s", m.Label, summarizeFailure(m.Result)))
		}
	}
	if len(failures) > 0 {
		return fail, strings.Join(failures, "; ")
	}
	return pass, joinLabels(labels) + " pass"
}

func (g *gate) checkCoverage() (outcome, string) {
	runs := g.moduleTests()

	var measured, below []string
	for _, m := range runs {
		if !m.Measured {
			return fail, fmt.Sprintf("not measured: the %s module's tests did not complete", m.Label)
		}
		measured = append(measured, fmt.Sprintf("%s %s", m.Label, percent(m.Coverage)))
		if m.Coverage < g.Threshold {
			below = append(below, fmt.Sprintf("%s %s < %s", m.Label, percent(m.Coverage), thresholdLabel(g.Threshold)))
		}
	}
	switch {
	case len(below) == len(runs):
		// Every module is short; naming the shortfalls has already reported
		// every figure there is.
		return fail, strings.Join(below, ", ")
	case len(below) > 0:
		return fail, fmt.Sprintf("%s (measured: %s)", strings.Join(below, ", "), strings.Join(measured, ", "))
	}
	return pass, strings.Join(measured, ", ")
}

// moduleTests runs `go test -race -cover` in every module, once, and memoizes
// the result for the two checks that read it.
func (g *gate) moduleTests() []moduleRun {
	if g.modulesRun {
		return g.modules
	}
	g.modulesRun = true

	profiles, err := os.MkdirTemp("", "phasecheck-cover")
	if err != nil {
		// Fall back to no coverage profile: the race verdict is still worth
		// having, and the coverage check will say why it has no number.
		profiles = ""
	} else {
		defer func() { _ = os.RemoveAll(profiles) }()
	}

	for _, m := range goModules {
		dir := filepath.Join(g.RepoDir, m.Dir)
		run := moduleRun{Label: m.Label}

		args := []string{goTest, "-race", "-cover"}
		profile := ""
		if profiles != "" {
			profile = filepath.Join(profiles, strings.ReplaceAll(m.Label, "/", "-")+".cov")
			args = append(args, "-coverprofile="+profile)
		}
		run.Result = g.run(dir, "go", append(args, "./...")...)

		if !run.Result.ok() {
			g.record(fmt.Sprintf("go test -race (%s module)", m.Label), run.Result)
		} else if profile != "" {
			run.Coverage, run.Measured = g.profileTotal(dir, profile)
		}
		g.modules = append(g.modules, run)
	}
	return g.modules
}

// profileTotal asks the toolchain for the statement total rather than averaging
// the per-package percentages `go test` prints, which are unweighted and would
// let one tiny well-tested package carry the number.
func (g *gate) profileTotal(dir, profile string) (float64, bool) {
	res := g.run(dir, "go", "tool", "cover", "-func="+profile)
	if !res.ok() {
		return 0, false
	}
	return parseCoverageTotal(res.Output)
}

// coverageTotalPattern matches the last line of `go tool cover -func`:
//
//	total:	(statements)	70.7%
var coverageTotalPattern = regexp.MustCompile(`(?m)^total:\s+\(statements\)\s+([0-9.]+)%`)

func parseCoverageTotal(output string) (float64, bool) {
	m := coverageTotalPattern.FindStringSubmatch(output)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// checkLint runs golangci-lint in both modules. A missing linter is reported as
// skipped rather than failed — it is not installed by default — but the summary
// says so, because a silent skip here is how lint rot starts.
func (g *gate) checkLint() (outcome, string) {
	linter, err := g.lookPath("golangci-lint")
	if err != nil {
		return skip, err.Error() + "; run `make tools`"
	}

	var failures []string
	for _, m := range goModules {
		res := g.run(filepath.Join(g.RepoDir, m.Dir), linter, "run", "./...")
		if !res.ok() {
			g.record(fmt.Sprintf("golangci-lint (%s module)", m.Label), res)
			failures = append(failures, fmt.Sprintf("%s: %s", m.Label, summarizeLint(res)))
		}
	}
	if len(failures) > 0 {
		return fail, strings.Join(failures, "; ")
	}
	return pass, "clean"
}

// checkIndex delegates to devindex, which already knows how to tell a stale
// index from a current one and says which line disagrees.
func (g *gate) checkIndex() (outcome, string) {
	if g.devMissing() {
		return skip, g.DevDir + "/ not present"
	}
	res := g.run(g.RepoDir, "go", "run", "./tools/devindex", "--check", "--dir", g.DevDir)
	if !res.ok() {
		g.record("devindex --check", res)
		return fail, summarizeDevindex(res)
	}
	return pass, "up to date"
}

// summarizeDevindex returns devindex's own diagnosis. Its errors are multi-line
// and preceded by any warnings it found, so neither the first nor the last line
// of the output is the one worth showing.
func summarizeDevindex(res commandResult) string {
	for _, line := range meaningfulLines(res.Output) {
		if strings.HasPrefix(line, "devindex: ") {
			return truncate(strings.TrimPrefix(line, "devindex: "))
		}
	}
	return summarizeFailure(res)
}

// record keeps a failed command's output for the tail printed under the table.
func (g *gate) record(name string, res commandResult) {
	g.transcripts = append(g.transcripts, transcript{Name: name, Output: res.Output})
}

// joinLabels renders a list of module names for a status row: "root and
// test/e2e", "root, pkg/client and test/e2e". Reading a gate row is the whole
// point of the row, and "a and b and c" reads like a bug.
func joinLabels(labels []string) string {
	switch len(labels) {
	case 0:
		return ""
	case 1:
		return labels[0]
	}
	return strings.Join(labels[:len(labels)-1], ", ") + " and " + labels[len(labels)-1]
}
