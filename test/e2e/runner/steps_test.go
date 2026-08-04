package runner_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/b3vet/atlascache/test/e2e/runner"
	"github.com/b3vet/atlascache/test/e2e/runner/fakeharness"
)

// Scenarios register once for the whole test binary; registering inside a test
// would panic the second time that test ran.
func init() {
	runner.RegisterScenario("lifecycle_probe", func(c *runner.Ctx) error {
		if err := c.Stop(); err != nil {
			return err
		}
		if err := c.Start(); err != nil {
			return err
		}
		if err := c.Kill(); err != nil {
			return err
		}
		// A zero sleep is a no-op rather than a scheduling round trip, so a
		// scenario can compute a wait and pass it straight through.
		return c.Sleep(0)
	})
	runner.RegisterScenario("sleeps_forever", func(c *runner.Ctx) error {
		return c.Sleep(time.Hour)
	})
}

// specWith wraps steps in the smallest spec that parses, so a test can say what
// it is about in the steps rather than in boilerplate.
func specWith(t *testing.T, name, steps string) *runner.Spec {
	t.Helper()
	return parseInline(t, name+".yaml",
		"version: 1\nname: "+name+"\ntier: full\nfeature: FEAT-0008\nsteps:\n"+steps)
}

// TestAScenarioCanDriveTheWholeServerLifecycle. Ctx is the whole surface a
// scenario has, and each method forwards to one harness method. A forward wired
// to the wrong one — Stop that killed, Kill that stopped — would leave every
// crash-recovery scenario quietly testing something other than a crash.
func TestAScenarioCanDriveTheWholeServerLifecycle(t *testing.T) {
	t.Parallel()

	h := newFake()
	result := executorWith(h).Run(t.Context(), specWith(t, "lifecycle-probe", "  - scenario: lifecycle_probe\n"))
	if result.Outcome != runner.OutcomePass {
		t.Fatalf("outcome = %s, failure = %#v", result.Outcome, result.Failure)
	}

	// One start from the runner, one from the scenario.
	if got := h.Starts(); got != 2 {
		t.Errorf("the server was started %d times, want 2; the scenario's Start did not reach the harness", got)
	}
	if h.Running() {
		t.Error("the scenario's Kill did not reach the harness")
	}
	logs := h.Logs()
	for _, want := range []string{"server stopped", "server started (start #2)", "server killed"} {
		if !strings.Contains(logs, want) {
			t.Errorf("the transcript is missing %q, so a lifecycle call went somewhere else:\n%s", want, logs)
		}
	}
}

// TestASleepIsCutShortWhenTheRunIsCanceled. A scenario that slept through a
// canceled run would hold its spec, its ports and its temp directory past the
// timeout that was supposed to reclaim them, and a hung server would take the
// whole run down with it instead of one spec.
func TestASleepIsCutShortWhenTheRunIsCanceled(t *testing.T) {
	t.Parallel()

	executor := executorWith(newFake())
	executor.SpecTimeout = 50 * time.Millisecond

	started := time.Now()
	result := executor.Run(context.Background(), specWith(t, "sleeps-forever", "  - scenario: sleeps_forever\n"))
	if result.Outcome != runner.OutcomeFail {
		t.Fatal("a scenario that outslept its spec timeout must fail")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the sleep was not cut short: the spec took %s against a 50ms timeout", elapsed)
	}
	if !strings.Contains(result.Failure.Message, "deadline exceeded") {
		t.Errorf("message = %q, want it to say the run ran out of time", result.Failure.Message)
	}
}

// TestStepFailuresNameWhatTheHarnessRefused. Every lifecycle step has its own
// failure message. A step that reported another step's message would send the
// reader to the wrong half of the spec, which is the whole reason the runner
// carries a step index and a source line at all.
func TestStepFailuresNameWhatTheHarnessRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		steps string
		setup func(*fakeharness.Harness)
		want  string
	}{
		{
			name:  "a command sent to a server that is down",
			steps: "  - kill: true\n  - cmd: PING\n    expect: PONG\n",
			want:  "sending the command failed",
		},
		{
			name:  "a restart the harness could not do",
			steps: "  - restart: true\n",
			setup: func(h *fakeharness.Harness) { h.RestartErr = errors.New("the port was still held") },
			want:  "the server did not restart: the port was still held",
		},
		{
			name:  "a kill the harness could not do",
			steps: "  - kill: true\n",
			setup: func(h *fakeharness.Harness) { h.KillErr = errors.New("no such process") },
			want:  "the server could not be killed: no such process",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newFake()
			if tc.setup != nil {
				tc.setup(h)
			}
			result := executorWith(h).Run(t.Context(), specWith(t, "step-failure", tc.steps))

			if result.Outcome != runner.OutcomeFail {
				t.Fatalf("outcome = %s, want a failure", result.Outcome)
			}
			if result.Failure.Phase != runner.PhaseStep {
				t.Errorf("phase = %s, want step", result.Failure.Phase)
			}
			if !strings.Contains(result.Failure.Message, tc.want) {
				t.Errorf("message = %q, want it to contain %q", result.Failure.Message, tc.want)
			}
		})
	}
}

// TestAStepWithNoFormFailsRatherThanBeingSkipped. Parsing rejects a step with no
// form, so this can only be reached by a Spec built in Go — which the runner's
// own callers do. A step the executor did not recognize must fail the spec:
// skipping it silently would let a spec report a pass for work it never did.
func TestAStepWithNoFormFailsRatherThanBeingSkipped(t *testing.T) {
	t.Parallel()

	spec := &runner.Spec{
		Version: 1, Name: "empty-step", Tier: runner.TierFull, Feature: "FEAT-0008",
		Steps: []runner.Step{{}},
	}
	result := executorWith(newFake()).Run(t.Context(), spec)

	if result.Outcome != runner.OutcomeFail {
		t.Fatal("a step the executor does not understand was treated as a pass")
	}
	if !strings.Contains(result.Failure.Message, "unknown step form") {
		t.Errorf("message = %q, want it to say the form was not understood", result.Failure.Message)
	}
}

// TestLogTailIsConfigurable. Zero has to keep meaning "the default" rather than
// "none", or every failure would arrive without the server output that explains
// it; a negative value is how a caller that does not want the log says so.
func TestLogTailIsConfigurable(t *testing.T) {
	t.Parallel()

	spec := loadSpec(t, "failing", "wrong-reply.yaml")

	quiet := executorWith(newFake())
	quiet.LogTail = -1
	if tail := quiet.Run(t.Context(), spec).Failure.LogTail; tail != "" {
		t.Errorf("a negative LogTail still carried %q", tail)
	}

	oneLine := executorWith(newFake())
	oneLine.LogTail = 1
	tail := oneLine.Run(t.Context(), spec).Failure.LogTail
	if tail == "" {
		t.Fatal("LogTail = 1 carried no log at all")
	}
	if strings.Contains(tail, "\n") {
		t.Errorf("LogTail = 1 produced %q, want exactly one line", tail)
	}

	full := executorWith(newFake()).Run(t.Context(), spec).Failure.LogTail
	if !strings.Contains(full, "\n") {
		t.Errorf("the default log tail is one line (%q); zero must mean the default, not one", full)
	}
}
