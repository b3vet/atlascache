package runner

import (
	"strings"
	"testing"
)

// A parse error is read by a contributor writing a spec, usually for the first
// time. These tests are about the message rather than the rejection: a spec that
// is refused without saying where or what to write instead costs more time than
// the mistake did.

// TestAStepWithNoFormListsTheFormsItCouldHaveUsed. "no step form given" alone
// leaves the author guessing at a vocabulary they have not learned yet, so the
// message has to enumerate it.
func TestAStepWithNoFormListsTheFormsItCouldHaveUsed(t *testing.T) {
	t.Parallel()

	_, err := ParseSpec("no-form.yaml", []byte(
		"version: 1\nname: no-form\ntier: full\nfeature: FEAT-0008\nsteps:\n  - {}\n"))
	if err == nil {
		t.Fatal("a step with no form at all was accepted")
	}
	if !strings.Contains(err.Error(), "no step form given") {
		t.Errorf("error = %v, want it to say no form was given", err)
	}
	for _, form := range []StepKind{StepCommand, StepSleep, StepScenario, StepRestart, StepKill} {
		if !strings.Contains(err.Error(), string(form)) {
			t.Errorf("error %q does not offer the %q form", err, form)
		}
	}
}

// TestScalarFieldsRejectTheWrongShapeByName. `expect:` written as a block and
// `sleep:` written as a list are both easy mistakes in YAML, and both would
// otherwise surface as an unhelpful type error from the decoder. The message has
// to name the shape that was found, at the line it was found on.
func TestScalarFieldsRejectTheWrongShapeByName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		steps string
		want  []string
	}{
		{
			name:  "expect written as a block",
			steps: "  - cmd: STATS\n    expect:\n      keys: 2\n",
			want:  []string{"expected a single value, got a map", "shape.yaml:9"},
		},
		{
			name:  "sleep written as a list",
			steps: "  - sleep:\n      - 1s\n",
			want:  []string{"expected a duration such as 1200ms, got a list", "shape.yaml:8"},
		},
		{
			name:  "sleep written without a unit",
			steps: "  - sleep: 1200\n",
			want:  []string{`"1200" is not a duration`, "write a unit"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseSpec("shape.yaml", []byte(
				"version: 1\nname: shape\ntier: full\nfeature: FEAT-0008\ndescription: x\nsteps:\n"+tc.steps))
			if err == nil {
				t.Fatal("the spec was accepted")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// TestAYAMLErrorWithoutALineStillNamesTheFile. Most decoder messages carry a
// line the runner rewrites into file:line form, but not all of them do. One that
// did not would otherwise be reported with no file at all, and a run over a tree
// of specs would not say which one failed to parse.
func TestAYAMLErrorWithoutALineStillNamesTheFile(t *testing.T) {
	t.Parallel()

	// A control character is refused by the YAML scanner before it has a line
	// to blame it on.
	_, err := ParseSpec("bad-bytes.yaml", []byte(
		"version: 1\nname: bad-bytes\ntier: full\nfeature: FEAT-0008\ndescription: \"a\x01b\"\nsteps:\n  - cmd: PING\n    expect: PONG\n"))
	if err == nil {
		t.Fatal("a spec containing a control character was accepted")
	}
	if !strings.HasPrefix(err.Error(), "bad-bytes.yaml: ") {
		t.Errorf("error = %q, want it to begin with the file it came from", err)
	}
	if !strings.Contains(err.Error(), "control characters") {
		t.Errorf("error = %q, want the decoder's own explanation", err)
	}
}
