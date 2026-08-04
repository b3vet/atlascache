package main

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func mustParseFlags(t *testing.T, args ...string) options {
	t.Helper()
	opts, err := parseFlags(args)
	if err != nil {
		t.Fatalf("parseFlags(%v): %v", args, err)
	}
	return opts
}

func TestParseFlagsRejectsBadCombinations(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"tier and spec together", []string{"--tier", "smoke", "--spec", "ping-basic"}, "pass one or the other"},
		{"unknown tier", []string{"--tier", "quick"}, `unknown tier "quick"`},
		{"parallel below one", []string{"--parallel", "0"}, "--parallel must be at least 1"},
		{"unknown harness", []string{"--harness", "docker"}, `unknown harness "docker"`},
		{"stray argument", []string{"smoke"}, "every option is a flag"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseFlags(tc.args)
			if err == nil {
				t.Fatalf("parseFlags(%v) was accepted", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestListEnumeratesSpecs(t *testing.T) {
	opts := mustParseFlags(t, "--list", "--specs", filepath.Join("testdata", "specs"))
	var out bytes.Buffer
	if err := run(opts, &out, io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}

	text := out.String()
	for _, want := range []string{"NAME", "TIER", "FEATURE", "ping-basic", "smoke", "every-step-form", "full", "ISSUE-0007"} {
		if !strings.Contains(text, want) {
			t.Errorf("listing is missing %q:\n%s", want, text)
		}
	}
}

func TestListFiltersByTier(t *testing.T) {
	opts := mustParseFlags(t, "--list", "--tier", "smoke", "--specs", filepath.Join("testdata", "specs"))
	var out bytes.Buffer
	if err := run(opts, &out, io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(out.String(), "every-step-form") {
		t.Errorf("--tier smoke listed a full-tier spec:\n%s", out.String())
	}
}

func TestRunPassesAndFails(t *testing.T) {
	passing := mustParseFlags(t, "--harness", "fake", "--specs", filepath.Join("testdata", "specs"))
	var out bytes.Buffer
	if err := run(passing, &out, io.Discard); err != nil {
		t.Fatalf("a passing run returned %v:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "2 specs: 2 passed, 0 failed") {
		t.Errorf("report = %s", out.String())
	}

	failing := mustParseFlags(t, "--harness", "fake", "--specs", filepath.Join("testdata", "failing"))
	out.Reset()
	err := run(failing, &out, io.Discard)
	if err == nil {
		t.Fatalf("a failing run reported success:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "2 of 2 specs failed") {
		t.Errorf("error = %v", err)
	}
}

func TestRunOneSpecByName(t *testing.T) {
	opts := mustParseFlags(t, "--harness", "fake", "--spec", "ping-basic", "--specs", filepath.Join("testdata", "specs"))
	var out bytes.Buffer
	if err := run(opts, &out, io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "1 spec: 1 passed, 0 failed") {
		t.Errorf("report = %s", out.String())
	}
}

func TestRunRejectsAnUnknownSpecName(t *testing.T) {
	opts := mustParseFlags(t, "--harness", "fake", "--spec", "nope", "--specs", filepath.Join("testdata", "specs"))
	if err := run(opts, io.Discard, io.Discard); err == nil {
		t.Fatal("run accepted a spec name that does not exist")
	}
}

func TestJSONRunParses(t *testing.T) {
	opts := mustParseFlags(t, "--harness", "fake", "--json", "--parallel", "1",
		"--specs", filepath.Join("testdata", "failing"))
	var out bytes.Buffer
	if err := run(opts, &out, io.Discard); err == nil {
		t.Fatal("a failing run reported success")
	}

	var report struct {
		Harness string `json:"harness"`
		Summary struct {
			Total, Passed, Failed int
		} `json:"summary"`
		Specs []struct {
			Name    string `json:"name"`
			Outcome string `json:"outcome"`
			Failure *struct {
				StepIndex int    `json:"step_index"`
				Line      int    `json:"line"`
				Expected  string `json:"expected"`
				Actual    string `json:"actual"`
				LogTail   string `json:"log_tail"`
			} `json:"failure"`
		} `json:"specs"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("the JSON report does not parse: %v\n%s", err, out.String())
	}
	if report.Harness != "fake" {
		t.Errorf("harness = %q, a fake run must say so in the report", report.Harness)
	}
	if report.Summary.Failed != 2 {
		t.Errorf("summary = %#v", report.Summary)
	}
	for _, spec := range report.Specs {
		if spec.Failure == nil {
			t.Fatalf("%s has no failure detail", spec.Name)
		}
		if spec.Failure.Line == 0 || spec.Failure.LogTail == "" {
			t.Errorf("%s failure = %#v", spec.Name, spec.Failure)
		}
	}
}

// TestProcessHarnessIsTheDefault checks the wiring rather than the harness: a
// run with no --harness must reach the process harness, which is proved here by
// its complaint about the binary it was told to launch. The harness itself is
// tested against a real server in the harness package.
func TestProcessHarnessIsTheDefault(t *testing.T) {
	opts := mustParseFlags(t, "--specs", filepath.Join("testdata", "specs"),
		"--binary", filepath.Join(t.TempDir(), "atlascache-that-was-never-built"))

	var out bytes.Buffer
	err := run(opts, &out, io.Discard)
	if err == nil {
		t.Fatalf("a run against a binary that does not exist reported success:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "make build") {
		t.Errorf("the failure should say how to fix it:\n%s", out.String())
	}
	if strings.Contains(out.String(), "not implemented") {
		t.Errorf("the process harness is still stubbed out:\n%s", out.String())
	}
}

func TestEmptySpecDirectoryIsAnError(t *testing.T) {
	opts := mustParseFlags(t, "--harness", "fake", "--specs", t.TempDir())
	if err := run(opts, io.Discard, io.Discard); err == nil {
		t.Fatal("a run over no specs must not report success")
	}
}

func TestAnEmptyTierIsNotAnError(t *testing.T) {
	// Tiers are cumulative, so an empty selection can only come from below:
	// the fixture holds one soak spec and we ask for smoke.
	opts := mustParseFlags(t, "--harness", "fake", "--tier", "smoke", "--specs", filepath.Join("testdata", "soakonly"))
	var out bytes.Buffer
	if err := run(opts, &out, io.Discard); err != nil {
		t.Fatalf("an empty tier is legitimate, got %v", err)
	}
	if !strings.Contains(out.String(), "0 specs matched tier smoke") {
		t.Errorf("output = %q", out.String())
	}
}

func TestFullTierIncludesSmokeSpecs(t *testing.T) {
	opts := mustParseFlags(t, "--harness", "fake", "--tier", "full", "--specs", filepath.Join("testdata", "specs"))
	var out bytes.Buffer
	if err := run(opts, &out, io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}
	// ping-basic is a smoke spec; the PR gate runs --tier full and must not skip it.
	if !strings.Contains(out.String(), "ping-basic") {
		t.Errorf("full tier must include smoke specs, output = %q", out.String())
	}
}
