package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The runner's own correctness cannot be established by the specs it runs, so
// the parser is tested against fixture files under testdata.

func init() {
	RegisterScenario("noop", func(*Ctx) error { return nil })
}

func parseFixture(t *testing.T, dir, file string) (*Spec, error) {
	t.Helper()
	path := filepath.Join("testdata", dir, file)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return ParseSpec(path, data)
}

func TestParseValidSpec(t *testing.T) {
	spec, err := parseFixture(t, "specs", "stats-counters.yaml")
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}

	if spec.Version != 1 {
		t.Errorf("version = %d, want 1", spec.Version)
	}
	if spec.Name != "stats-counters" {
		t.Errorf("name = %q", spec.Name)
	}
	if spec.Tier != TierFull {
		t.Errorf("tier = %q", spec.Tier)
	}
	if spec.Feature != "FEAT-0011" || spec.Issue != "ISSUE-0007" {
		t.Errorf("feature/issue = %q/%q", spec.Feature, spec.Issue)
	}
	if !strings.Contains(spec.Description, "comparators") {
		t.Errorf("description = %q", spec.Description)
	}

	auth, ok := spec.Config["auth"].(map[string]any)
	if !ok {
		t.Fatalf("config.auth = %#v, want a map", spec.Config["auth"])
	}
	if auth["enabled"] != false {
		t.Errorf("config.auth.enabled = %#v, want false", auth["enabled"])
	}

	if len(spec.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(spec.Steps))
	}
	step := spec.Steps[0]
	if step.Kind() != StepCommand || step.Cmd != "STATS" {
		t.Errorf("step = %#v", step)
	}
	if got := step.ExpectField["expirations"].Text; got != ">= 1" {
		t.Errorf("expect_field.expirations = %q", got)
	}
	if step.Line != 12 {
		t.Errorf("step line = %d, want 12", step.Line)
	}
}

func TestParseRecordsEveryStepForm(t *testing.T) {
	spec, err := parseFixture(t, "specs", "lifecycle.yaml")
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}

	want := []StepKind{StepCommand, StepSleep, StepScenario, StepRestart, StepCommand, StepKill}
	if len(spec.Steps) != len(want) {
		t.Fatalf("steps = %d, want %d", len(spec.Steps), len(want))
	}
	for i, kind := range want {
		if got := spec.Steps[i].Kind(); got != kind {
			t.Errorf("step %d kind = %q, want %q", i+1, got, kind)
		}
	}
	if got := spec.Steps[1].Sleep.D; got != 5*time.Millisecond {
		t.Errorf("sleep = %s, want 5ms", got)
	}
	if got := spec.Steps[2].Scenario; got != "noop" {
		t.Errorf("scenario = %q", got)
	}

	wantLines := []int{8, 10, 11, 12, 13, 15}
	for i, line := range wantLines {
		if spec.Steps[i].Line != line {
			t.Errorf("step %d line = %d, want %d", i+1, spec.Steps[i].Line, line)
		}
	}
}

func TestParseDefaultsMissingVersion(t *testing.T) {
	spec, err := parseFixture(t, "specs", "soak-idle.yaml")
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if spec.Version != SpecVersion {
		t.Errorf("version = %d, want %d", spec.Version, SpecVersion)
	}
	if spec.Tier != TierSoak {
		t.Errorf("tier = %q", spec.Tier)
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		file string
		want []string
	}{
		// The misspelling is the fixture: the spec file gets `description` wrong on purpose.
		{"unknown-field.yaml", []string{"unknown-field.yaml:5", `unknown field "descriptoin" in spec`}}, //nolint:misspell
		{"unknown-step-field.yaml", []string{"unknown-step-field.yaml:7", `unknown field "expct" in step`}},
		{"bad-tier.yaml", []string{"bad-tier.yaml:3", `tier "smoek" is not one of smoke, full, soak`}},
		{"missing-name.yaml", []string{"name is required"}},
		{"name-mismatch.yaml", []string{
			"name-mismatch.yaml:2", `name "some-other-name" must match the file name "name-mismatch"`,
		}},
		{"unknown-scenario.yaml", []string{
			`scenario "crash_and_recovr" is not registered`, "known scenarios:", "noop", ":8: step 2",
		}},
		{"two-forms.yaml", []string{"step forms cmd and sleep cannot be combined"}},
		{"no-assertion.yaml", []string{"cmd needs one of expect, expect_value, expect_error or expect_field"}},
		{"expect-and-expect-value.yaml", []string{"cmd takes one assertion, got expect and expect_value"}},
		{"two-assertions.yaml", []string{"cmd takes one assertion, got expect and expect_error"}},
		{"bad-sleep.yaml", []string{`"1200" is not a duration`, "bad-sleep.yaml:6"}},
		{"empty-steps.yaml", []string{"steps is required and must not be empty"}},
		{"bad-feature.yaml", []string{`feature "FEAT-10" must look like FEAT-0001`}},
		{"bad-issue.yaml", []string{`issue "BUG-7" must look like ISSUE-0001`}},
		{"future-version.yaml", []string{"version 2 is not supported"}},
		{"restart-false.yaml", []string{"restart must be true"}},
		{"bad-comparator.yaml", []string{"expect_field expirations: >= needs a value to compare against"}},
		{"assertion-without-cmd.yaml", []string{"expect needs a cmd to assert against"}},
		{"expect-list.yaml", []string{"expected a single value, got a list"}},
		{"two-documents.yaml", []string{"exactly one YAML document"}},
		{"comment-only.yaml", []string{"has no document"}},
		{"empty.yaml", []string{"spec file is empty"}},
	}

	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			spec, err := parseFixture(t, "invalid", tc.file)
			if err == nil {
				t.Fatalf("ParseSpec accepted %s: %#v", tc.file, spec)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err.Error(), want)
				}
			}
		})
	}
}

func TestParseReportsEveryProblemAtOnce(t *testing.T) {
	data := []byte(`version: 1
name: many-problems
tier: nope
feature: nonsense
steps:
  - cmd: PING
  - scenario: missing
`)
	_, err := ParseSpec("many-problems.yaml", data)
	if err == nil {
		t.Fatal("ParseSpec accepted a spec with four problems")
	}
	for _, want := range []string{
		`tier "nope"`, `feature "nonsense"`, "cmd needs one of", `scenario "missing"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestStepString(t *testing.T) {
	yes := true
	tests := []struct {
		step Step
		want string
	}{
		{Step{Cmd: "GET k1"}, "cmd: GET k1"},
		{Step{Sleep: Duration{D: 1200 * time.Millisecond, Set: true}}, "sleep: 1.2s"},
		{Step{Scenario: "noop"}, "scenario: noop"},
		{Step{Restart: &yes}, "restart"},
		{Step{Kill: &yes}, "kill"},
		{Step{}, "<empty step>"},
	}
	for _, tc := range tests {
		if got := tc.step.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

func TestTierValid(t *testing.T) {
	for _, tier := range Tiers {
		if !tier.Valid() {
			t.Errorf("%q should be valid", tier)
		}
	}
	if Tier("quick").Valid() {
		t.Error(`"quick" should not be valid`)
	}
}

func TestRegisterScenarioRejectsDuplicates(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registering a duplicate scenario should panic")
		}
	}()
	RegisterScenario("noop", func(*Ctx) error { return nil })
}
