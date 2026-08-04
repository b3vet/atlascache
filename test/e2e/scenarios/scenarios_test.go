package scenarios_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/b3vet/atlascache/test/e2e/runner"
	"github.com/b3vet/atlascache/test/e2e/runner/fakeharness"
	_ "github.com/b3vet/atlascache/test/e2e/scenarios"
)

const crashSpec = `version: 1
name: crash
tier: full
feature: FEAT-0008
steps:
  - scenario: crash_and_recover
`

// runCrashSpec drives crash_and_recover through the executor, which is how a
// spec reaches it, and returns whatever failure came back.
func runCrashSpec(t *testing.T, h runner.Harness) *runner.Failure {
	t.Helper()
	spec, err := runner.ParseSpec("crash.yaml", []byte(crashSpec))
	if err != nil {
		t.Fatalf("parsing the spec: %v", err)
	}
	executor := &runner.Executor{
		Factory: func(runner.HarnessOptions) (runner.Harness, error) { return h, nil },
	}
	return executor.Run(context.Background(), spec).Failure
}

func TestScenariosAreRegistered(t *testing.T) {
	if _, ok := runner.LookupScenario("crash_and_recover"); !ok {
		t.Fatal("crash_and_recover did not register itself")
	}
	if len(runner.ScenarioNames()) == 0 {
		t.Fatal("no scenarios are registered")
	}
}

func TestCrashAndRecover(t *testing.T) {
	h := fakeharness.New(map[string]runner.Reply{"PING": runner.StatusReply("PONG")})
	if failure := runCrashSpec(t, h); failure != nil {
		t.Fatalf("crash_and_recover failed: %#v", failure)
	}
	if h.Starts() != 2 {
		t.Errorf("the server started %d times; the scenario must bring it back after the kill", h.Starts())
	}
	if !strings.Contains(h.Logs(), "server killed") {
		t.Errorf("the scenario did not kill the server:\n%s", h.Logs())
	}
}

func TestCrashAndRecoverFailsWhenTheServerComesBackWrong(t *testing.T) {
	h := fakeharness.New(map[string]runner.Reply{"PING": runner.ErrorReply("ERR loading")})
	failure := runCrashSpec(t, h)
	if failure == nil {
		t.Fatal("a server answering PING with an error must fail the scenario")
	}
	if !strings.Contains(failure.Message, "ERR loading") {
		t.Errorf("message = %q", failure.Message)
	}
	if len(failure.Notes) == 0 || !strings.Contains(strings.Join(failure.Notes, "\n"), "SIGKILL") {
		t.Errorf("the scenario log should show how far it got, got %#v", failure.Notes)
	}
}

func TestCrashAndRecoverFailsWhenTheServerDoesNotComeBack(t *testing.T) {
	h := &staysDead{Harness: fakeharness.New(map[string]runner.Reply{"PING": runner.StatusReply("PONG")})}
	failure := runCrashSpec(t, h)
	if failure == nil {
		t.Fatal("a server that will not restart must fail the scenario")
	}
	if !strings.Contains(failure.Message, "port still held") {
		t.Errorf("message = %q", failure.Message)
	}
}

// staysDead starts once and refuses every start after that, standing in for a
// server that cannot come back from a crash.
type staysDead struct {
	*fakeharness.Harness
	started bool
}

func (h *staysDead) Start(ctx context.Context) error {
	if h.started {
		return errors.New("port still held")
	}
	h.started = true
	return h.Harness.Start(ctx)
}
