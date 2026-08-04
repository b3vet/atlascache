package runner_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/b3vet/atlascache/test/e2e/runner"
)

// TestReplyKindNamesItself. The kind is what a mismatch message shows — "answered
// error ..., want a bulk string". A switch arm returning the wrong name would
// send every future reader of that message hunting the wrong bug, and the zero
// value has to read as invalid so a half-built reply is legible rather than
// looking like a status.
func TestReplyKindNamesItself(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind runner.ReplyKind
		want string
	}{
		{kind: runner.KindInvalid, want: "invalid"},
		{kind: runner.KindStatus, want: "status"},
		{kind: runner.KindBulk, want: "bulk"},
		{kind: runner.KindInteger, want: "integer"},
		{kind: runner.KindNil, want: "nil"},
		{kind: runner.KindError, want: "error"},
		{kind: runner.KindArray, want: "array"},
		{kind: runner.KindMap, want: "map"},
		// A kind from a newer client this runner does not know about must not
		// render as one it does.
		{kind: runner.ReplyKind(99), want: "invalid"},
	}

	for _, tc := range tests {
		if got := tc.kind.String(); got != tc.want {
			t.Errorf("ReplyKind(%d).String() = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

// TestFieldsIgnoresLinesThatAreNotPairs. STATS may arrive as text lines, and an
// expect_field assertion is looked up by key. A line with no separator, or one
// that begins with the separator, carries no field — turning it into an entry
// under the empty key would let `expect_field: {"": ...}` match nothing in
// particular, and would put junk in the "reply has fields: ..." diagnostic.
func TestFieldsIgnoresLinesThatAreNotPairs(t *testing.T) {
	t.Parallel()

	reply := runner.BulkReply("# Stats\n\nkeys:2\nnot a pair\n:no key at all\nhits = 9\n")
	fields := reply.Fields()

	if fields["keys"] != "2" || fields["hits"] != "9" {
		t.Errorf("fields = %v, want keys=2 and hits=9", fields)
	}
	if _, ok := fields[""]; ok {
		t.Errorf("a line beginning with the separator produced an empty key: %v", fields)
	}
	if len(fields) != 2 {
		t.Errorf("fields = %v, want only the two real pairs", fields)
	}
}

// TestFailureReportRendersEveryFailureShape. The failure block is all a reader
// gets — the point of it is that nobody has to rerun the suite to find out what
// happened. Each shape below loses something specific if the wrong branch runs:
// the phase for a failure that never reached a step, the step number for a spec
// with no line information, the scenario's own log, and the quoting that makes a
// trailing space visible instead of invisible.
func TestFailureReportRendersEveryFailureShape(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		failure runner.Failure
		want    []string
		absent  []string
	}{
		{
			name:    "a failure before any step names the phase",
			failure: runner.Failure{Phase: runner.PhaseStart, Message: "the server did not start"},
			want:    []string{"start phase", "the server did not start"},
			absent:  []string{"step 0", "expected:"},
		},
		{
			name: "a step with no source line still names the step",
			failure: runner.Failure{
				Phase: runner.PhaseStep, StepIndex: 3, Step: "cmd: PING", Message: "sending the command failed",
			},
			want:   []string{"step 3: cmd: PING"},
			absent: []string{"step 3, "},
		},
		{
			name: "a difference that is only whitespace is quoted so it can be seen",
			failure: runner.Failure{
				Phase: runner.PhaseStep, StepIndex: 1, Line: 7, Step: "cmd: GET k",
				Message: "reply mismatch", Expected: "OK", Actual: "OK ",
			},
			want: []string{"step 1, ", ":7: cmd: GET k", "expected: OK", `actual:   "OK "`},
		},
		{
			name: "an empty side of a comparison is quoted rather than left blank",
			failure: runner.Failure{
				Phase: runner.PhaseStep, StepIndex: 1, Line: 7, Step: "cmd: GET k",
				Message: "reply mismatch", Expected: "", Actual: "ERR nope",
			},
			want: []string{`expected: ""`, "actual:   ERR nope"},
		},
		{
			name: "a scenario's own log is carried into the block",
			failure: runner.Failure{
				Phase: runner.PhaseStep, StepIndex: 2, Step: "scenario: graceful_shutdown",
				Message: "scenario failed",
				Notes:   []string{"held an idle connection", "the server exited 12ms after SIGTERM"},
				LogTail: "server line one\nserver line two",
			},
			want: []string{
				"scenario log:", "| held an idle connection", "| the server exited 12ms after SIGTERM",
				"server log (last 2 lines):", "| server line one", "| server line two",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			failure := tc.failure
			result := runner.SpecResult{
				Name: "some-spec", Tier: runner.TierFull, Feature: "FEAT-0008",
				Path: "specs/some-spec.yaml", Outcome: runner.OutcomeFail, Failure: &failure,
			}

			var out bytes.Buffer
			reporter := runner.NewTextReporter(&out, nil)
			if err := reporter.Finish(runner.Report{
				Summary: runner.Summarize([]runner.SpecResult{result}),
				Specs:   []runner.SpecResult{result},
			}); err != nil {
				t.Fatalf("Finish: %v", err)
			}

			text := out.String()
			if !strings.Contains(text, "--- FAIL: some-spec (specs/some-spec.yaml)") {
				t.Errorf("the block does not identify the spec and its file:\n%s", text)
			}
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Errorf("the block is missing %q:\n%s", want, text)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(text, absent) {
					t.Errorf("the block contains %q, which does not apply to this failure:\n%s", absent, text)
				}
			}
		})
	}
}
