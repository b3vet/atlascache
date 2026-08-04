package runner_test

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/b3vet/atlascache/test/e2e/runner"
)

func TestTextReportOnFailureIsSelfContained(t *testing.T) {
	spec := loadSpec(t, "failing", "wrong-reply.yaml")
	result := executorWith(newFake()).Run(t.Context(), spec)

	var out bytes.Buffer
	reporter := runner.NewTextReporter(&out, []*runner.Spec{spec})
	reporter.SpecFinished(result)
	if err := reporter.Finish(runner.Report{
		Summary: runner.Summarize([]runner.SpecResult{result}),
		Specs:   []runner.SpecResult{result},
	}); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	text := out.String()
	// Everything needed to diagnose the failure without running it again.
	for _, want := range []string{
		"FAIL  wrong-reply",
		"--- FAIL: wrong-reply",
		"step 2",
		"wrong-reply.yaml:10",
		"cmd: GET missing",
		"expected: NIL",
		"actual:   error: ERR unknown command",
		"server log",
		"> GET missing",
		"1 spec: 0 passed, 1 failed",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report is missing %q:\n%s", want, text)
		}
	}
}

func TestTextReportStaysQuietOnAPass(t *testing.T) {
	spec := loadSpec(t, "specs", "ping-basic.yaml")
	result := executorWith(newFake()).Run(t.Context(), spec)

	var out bytes.Buffer
	reporter := runner.NewTextReporter(&out, []*runner.Spec{spec})
	reporter.SpecFinished(result)
	if err := reporter.Finish(runner.Report{
		Summary: runner.Summarize([]runner.SpecResult{result}),
		Specs:   []runner.SpecResult{result},
	}); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	text := out.String()
	if !strings.Contains(text, "PASS  ping-basic") {
		t.Errorf("report = %q", text)
	}
	if strings.Contains(text, "server log") || strings.Contains(text, "> PING") {
		t.Errorf("a passing run must not print the server log:\n%s", text)
	}
}

func TestJSONReportParses(t *testing.T) {
	specs, err := runner.LoadDir(filepath.Join("testdata", "specs"))
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	failing := loadSpec(t, "failing", "wrong-reply.yaml")
	specs = append(specs, failing)

	results := executorWith(newFake()).RunAll(t.Context(), specs, 2, nil)

	var out bytes.Buffer
	reporter := &runner.JSONReporter{Out: &out}
	reporter.SpecFinished(results[0])
	summary := runner.Summarize(results)
	if err := reporter.Finish(runner.Report{
		Harness: "fake", Tier: "", Parallel: 2, DurationMS: 12,
		Summary: summary, Specs: results,
	}); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	var report runner.Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("the JSON report does not parse: %v\n%s", err, out.String())
	}
	if report.Summary.Total != len(specs) || report.Summary.Failed != 1 {
		t.Errorf("summary = %#v", report.Summary)
	}
	if report.Harness != "fake" || report.Parallel != 2 {
		t.Errorf("report = %#v", report)
	}

	var failure *runner.Failure
	for _, result := range report.Specs {
		if result.Name == "wrong-reply" {
			failure = result.Failure
		}
	}
	if failure == nil {
		t.Fatal("the failing spec has no failure in the JSON report")
	}
	if failure.StepIndex != 2 || failure.Line != 10 || failure.Expected != "NIL" {
		t.Errorf("failure = %#v", failure)
	}
	if failure.LogTail == "" {
		t.Error("the JSON failure should carry the server log tail")
	}
}

func TestListing(t *testing.T) {
	specs, err := runner.LoadDir(filepath.Join("testdata", "specs"))
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}

	var out bytes.Buffer
	if err := runner.WriteListing(&out, runner.Listing(specs)); err != nil {
		t.Fatalf("WriteListing: %v", err)
	}
	text := out.String()
	for _, want := range []string{"NAME", "TIER", "FEATURE", "ping-basic", "smoke", "FEAT-0010", "ISSUE-0007"} {
		if !strings.Contains(text, want) {
			t.Errorf("listing is missing %q:\n%s", want, text)
		}
	}

	out.Reset()
	if err := runner.WriteListingJSON(&out, runner.Listing(specs)); err != nil {
		t.Fatalf("WriteListingJSON: %v", err)
	}
	var parsed struct {
		Specs []runner.SpecListing `json:"specs"`
	}
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("the JSON listing does not parse: %v", err)
	}
	if len(parsed.Specs) != len(specs) {
		t.Fatalf("listed %d specs, want %d", len(parsed.Specs), len(specs))
	}
	if parsed.Specs[1].Name != "ping-basic" || parsed.Specs[1].Steps != 2 {
		t.Errorf("row = %#v", parsed.Specs[1])
	}
}

func TestSummarize(t *testing.T) {
	summary := runner.Summarize([]runner.SpecResult{
		{Outcome: runner.OutcomePass},
		{Outcome: runner.OutcomeFail},
		{Outcome: runner.OutcomePass},
	})
	if summary.Total != 3 || summary.Passed != 2 || summary.Failed != 1 {
		t.Errorf("summary = %#v", summary)
	}
}
