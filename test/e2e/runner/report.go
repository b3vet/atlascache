package runner

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Summary counts the outcomes of one run.
type Summary struct {
	Total  int `json:"total"`
	Passed int `json:"passed"`
	Failed int `json:"failed"`
}

// Report is the whole run, as the --json mode emits it.
type Report struct {
	Harness    string       `json:"harness"`
	Tier       string       `json:"tier,omitempty"`
	Parallel   int          `json:"parallel"`
	DurationMS int64        `json:"duration_ms"`
	Summary    Summary      `json:"summary"`
	Specs      []SpecResult `json:"specs"`
}

// Summarize counts outcomes.
func Summarize(results []SpecResult) Summary {
	summary := Summary{Total: len(results)}
	for _, result := range results {
		if result.Outcome == OutcomeFail {
			summary.Failed++
			continue
		}
		summary.Passed++
	}
	return summary
}

// Reporter receives results as they finish and writes the run's output.
type Reporter interface {
	// SpecFinished is called from the execution goroutines, once per spec.
	SpecFinished(SpecResult)
	// Finish writes everything the reporter owes once the run is over.
	Finish(Report) error
}

// TextReporter writes a line per spec as it finishes, then a detail block for
// each failure and a summary. Passing runs stay quiet: no server log is shown.
type TextReporter struct {
	Out io.Writer

	mu     sync.Mutex
	widths columns
}

type columns struct {
	name    int
	tier    int
	feature int
}

// NewTextReporter builds a reporter whose columns are sized for specs.
func NewTextReporter(out io.Writer, specs []*Spec) *TextReporter {
	widths := columns{name: 4, tier: 4, feature: 7}
	for _, spec := range specs {
		widths.name = max(widths.name, len(spec.Name))
		widths.tier = max(widths.tier, len(spec.Tier))
		widths.feature = max(widths.feature, len(spec.Feature))
	}
	return &TextReporter{Out: out, widths: widths}
}

// SpecFinished prints one result line. Specs finish out of order under
// parallelism, so the line is written whole under a lock.
func (r *TextReporter) SpecFinished(result SpecResult) {
	label := "PASS"
	if result.Outcome == OutcomeFail {
		label = "FAIL"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.Out, "%s  %-*s  %-*s  %-*s  %s\n",
		label,
		r.widths.name, result.Name,
		r.widths.tier, result.Tier,
		r.widths.feature, result.Feature,
		formatDuration(result.Duration()))
}

// Finish prints the failure detail blocks and the summary.
func (r *TextReporter) Finish(report Report) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, result := range report.Specs {
		if result.Failure == nil {
			continue
		}
		writeFailure(r.Out, result)
	}

	fmt.Fprintf(r.Out, "\n%s in %s\n", describeSummary(report.Summary), formatDuration(
		time.Duration(report.DurationMS)*time.Millisecond))
	return nil
}

func describeSummary(summary Summary) string {
	specs := "specs"
	if summary.Total == 1 {
		specs = "spec"
	}
	return fmt.Sprintf("%d %s: %d passed, %d failed", summary.Total, specs, summary.Passed, summary.Failed)
}

func writeFailure(out io.Writer, result SpecResult) {
	failure := result.Failure
	fmt.Fprintf(out, "\n--- FAIL: %s (%s)\n", result.Name, result.Path)

	switch {
	case failure.StepIndex > 0 && failure.Line > 0:
		fmt.Fprintf(out, "    step %d, %s:%d: %s\n", failure.StepIndex, result.Path, failure.Line, failure.Step)
	case failure.StepIndex > 0:
		fmt.Fprintf(out, "    step %d: %s\n", failure.StepIndex, failure.Step)
	default:
		fmt.Fprintf(out, "    %s phase\n", failure.Phase)
	}

	fmt.Fprintf(out, "    %s\n", failure.Message)
	if failure.Expected != "" || failure.Actual != "" {
		fmt.Fprintf(out, "    expected: %s\n", quoteForOutput(failure.Expected))
		fmt.Fprintf(out, "    actual:   %s\n", quoteForOutput(failure.Actual))
	}
	if len(failure.Notes) > 0 {
		fmt.Fprintf(out, "    scenario log:\n")
		for _, note := range failure.Notes {
			fmt.Fprintf(out, "      | %s\n", note)
		}
	}
	if failure.LogTail != "" {
		fmt.Fprintf(out, "    server log (last %d lines):\n", len(strings.Split(failure.LogTail, "\n")))
		for _, line := range strings.Split(failure.LogTail, "\n") {
			fmt.Fprintf(out, "      | %s\n", line)
		}
	}
}

func quoteForOutput(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, "\n\r\t") || strings.TrimSpace(s) != s {
		return fmt.Sprintf("%q", s)
	}
	return s
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}

// JSONReporter emits the whole run as one JSON object, so an automated run can
// parse results rather than scrape text.
type JSONReporter struct {
	Out io.Writer
}

// SpecFinished is a no-op: JSON is written once, at the end.
func (r *JSONReporter) SpecFinished(SpecResult) {}

// Finish writes the report.
func (r *JSONReporter) Finish(report Report) error {
	if report.Specs == nil {
		report.Specs = []SpecResult{}
	}
	enc := json.NewEncoder(r.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// SpecListing is one row of --list output.
type SpecListing struct {
	Name    string `json:"name"`
	Tier    Tier   `json:"tier"`
	Feature string `json:"feature"`
	Issue   string `json:"issue,omitempty"`
	Steps   int    `json:"steps"`
	Path    string `json:"path"`
}

// Listing turns specs into their --list rows.
func Listing(specs []*Spec) []SpecListing {
	rows := make([]SpecListing, 0, len(specs))
	for _, spec := range specs {
		rows = append(rows, SpecListing{
			Name:    spec.Name,
			Tier:    spec.Tier,
			Feature: spec.Feature,
			Issue:   spec.Issue,
			Steps:   len(spec.Steps),
			Path:    spec.Path,
		})
	}
	return rows
}

// WriteListing prints the specs as an aligned table.
func WriteListing(out io.Writer, rows []SpecListing) error {
	widths := struct{ name, tier, feature, issue int }{4, 4, 7, 5}
	for _, row := range rows {
		widths.name = max(widths.name, len(row.Name))
		widths.tier = max(widths.tier, len(row.Tier))
		widths.feature = max(widths.feature, len(row.Feature))
		widths.issue = max(widths.issue, len(row.Issue))
	}

	format := fmt.Sprintf("%%-%ds  %%-%ds  %%-%ds  %%-%ds  %%5s  %%s\n",
		widths.name, widths.tier, widths.feature, widths.issue)
	fmt.Fprintf(out, format, "NAME", "TIER", "FEATURE", "ISSUE", "STEPS", "PATH")
	for _, row := range rows {
		issue := row.Issue
		if issue == "" {
			issue = "-"
		}
		fmt.Fprintf(out, format, row.Name, row.Tier, row.Feature, issue, fmt.Sprint(row.Steps), row.Path)
	}
	return nil
}

// WriteListingJSON prints the specs as JSON.
func WriteListingJSON(out io.Writer, rows []SpecListing) error {
	if rows == nil {
		rows = []SpecListing{}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"specs": rows})
}
