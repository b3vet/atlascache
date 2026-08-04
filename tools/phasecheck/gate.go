package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
)

// outcome is the verdict of a single check.
type outcome int

const (
	pass outcome = iota
	fail
	skip
)

func (o outcome) mark() string {
	switch o {
	case pass:
		return "[x]"
	case fail:
		return "[!]"
	case skip:
		return "[-]"
	}
	return "[?]"
}

// describe renders a detail for the table. A skipped check states the missing
// dependency, because "skipped" on its own reads as "passed" to a tired eye.
func (o outcome) describe(detail string) string {
	if o == skip {
		return "skipped (" + detail + ")"
	}
	return detail
}

// result is one row of the report.
type result struct {
	Name    string
	Detail  string
	Outcome outcome
}

func count(results []result, o outcome) int {
	n := 0
	for _, r := range results {
		if r.Outcome == o {
			n++
		}
	}
	return n
}

// check is one row's worth of work. Name is known before Run is called so the
// table can be aligned while rows are still streaming out.
type check struct {
	Name string
	Run  func() (outcome, string)
}

// transcript is the output of a subprocess that failed, kept for the tail
// printed under the table.
type transcript struct {
	Name   string
	Output string
}

// coverageThresholds keys the statement-coverage floor by phase. P0 is
// scaffolding, where a high bar buys tests written for the metric rather than
// for insight; from P1 the code being measured is the cache itself.
var coverageThresholds = map[string]float64{"P0": 60}

const defaultCoverageThreshold = 80

func thresholdFor(phase string) float64 {
	if t, ok := coverageThresholds[phase]; ok {
		return t
	}
	return defaultCoverageThreshold
}

// gate holds everything the checks share: where to look, how to run commands,
// and the results of work that more than one check needs.
type gate struct {
	Phase     string
	DevDir    string
	RepoDir   string
	Threshold float64

	// Seams. Tests replace these; nothing else does.
	run      commandFunc
	lookPath func(string) (string, error)

	devErr   error // why the dev tree is unusable, if it is
	features []feature

	modules    []moduleRun // memoized: one `go test` run feeds two checks
	modulesRun bool

	transcripts []transcript
}

func newGate(phase, devDir, repoDir string) *gate {
	g := &gate{
		Phase:     phase,
		DevDir:    devDir,
		RepoDir:   repoDir,
		Threshold: thresholdFor(phase),
		run:       execCommand,
		lookPath:  findGolangciLint,
	}
	g.features, g.devErr = loadFeatures(devDir)
	return g
}

// checks lists every check in report order.
func (g *gate) checks() []check {
	return []check{
		{"e2e --tier full", g.checkE2E},
		{"go test -race", g.checkRace},
		{fmt.Sprintf("coverage >= %s", thresholdLabel(g.Threshold)), g.checkCoverage},
		{"golangci-lint", g.checkLint},
		{fmt.Sprintf("FEATs in %s done", g.Phase), g.checkFeatures},
		{"verification present", g.checkVerification},
		{changelogFile, g.checkChangelog},
		{"worklog entry", g.checkWorklog},
		{"dev/INDEX.md current", g.checkIndex},
	}
}

// devMissing reports whether the dev tree is simply absent, which is the normal
// state of a public clone and not a failure.
func (g *gate) devMissing() bool {
	return errors.Is(g.devErr, fs.ErrNotExist)
}

// devUnavailable returns the skip or fail a dev-reading check owes its caller
// when the tree cannot be read, and false when the tree is fine. An absent tree
// skips; a malformed one fails, because silently skipping a tree that exists
// would hide exactly the drift this gate is for.
func (g *gate) devUnavailable() (outcome, string, bool) {
	switch {
	case g.devMissing():
		return skip, g.DevDir + "/ not present", true
	case g.devErr != nil:
		return fail, g.devErr.Error(), true
	}
	return pass, "", false
}

// exists reports whether path is present, treating any stat error other than
// "not there" as absent — an unreadable file is not something a check can act on
// beyond saying so.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// percent renders a measured coverage figure; thresholdLabel renders a
// configured one, where a trailing ".0" is noise ("coverage >= 80%").
func percent(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64) + "%"
}

func thresholdLabel(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64) + "%"
}
