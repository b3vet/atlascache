package fakeharness_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/b3vet/atlascache/test/e2e/runner"
	"github.com/b3vet/atlascache/test/e2e/runner/fakeharness"
)

// The runner's own tests conclude things from this harness: that a restart step
// really restarted, that a command after a kill fails, that a failure carries
// the server log. Every one of those conclusions is only as good as the double,
// so a double that quietly did the wrong thing would not fail those tests — it
// would make them pass while asserting nothing. That is what these tests guard.

func newFake(t *testing.T) *fakeharness.Harness {
	t.Helper()
	h := fakeharness.New(map[string]runner.Reply{
		"PING":  runner.StatusReply("PONG"),
		"GET k": runner.BulkReply("v"),
	})
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return h
}

// TestCommandsAreRefusedUntilTheServerIsStarted. A runner test that kills the
// server and then sends a command concludes the send failed *because the server
// was down*. A fake that answered from its script regardless of state would make
// that test pass no matter what the runner did with the kill step.
func TestCommandsAreRefusedUntilTheServerIsStarted(t *testing.T) {
	t.Parallel()

	h := newFake(t)
	if h.Running() {
		t.Error("a fresh harness reports the server as running before Start")
	}
	if _, err := h.Send(context.Background(), "PING"); err == nil {
		t.Fatal("a command before Start was answered; the fake cannot tell a running server from a stopped one")
	}

	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	reply, err := h.Send(context.Background(), "PING")
	if err != nil {
		t.Fatalf("Send after Start: %v", err)
	}
	if reply.String() != "PONG" {
		t.Errorf("PING = %q, want the scripted PONG", reply.String())
	}

	if err := h.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if h.Running() {
		t.Error("the server is still reported as running after Kill")
	}
	if _, err := h.Send(context.Background(), "PING"); err == nil {
		t.Error("a command after Kill was answered")
	}
}

// TestRestartIsAStopFollowedByAStart. TestRunExecutesEveryStepForm concludes
// that a restart step restarted the server by reading Starts(). A Restart that
// did not go through Start would leave that count at one and the runner test
// would then be asserting nothing about restart at all.
func TestRestartIsAStopFollowedByAStart(t *testing.T) {
	t.Parallel()

	h := newFake(t)
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	if got := h.Starts(); got != 2 {
		t.Errorf("Starts() = %d after one Start and one Restart, want 2", got)
	}
	if !h.Running() {
		t.Error("the server is down after a Restart")
	}
	logs := h.Logs()
	if !strings.Contains(logs, "server stopped") || !strings.Contains(logs, "start #2") {
		t.Errorf("the transcript does not show a stop and a second start:\n%s", logs)
	}
}

// TestStoppingAStoppedServerIsANoOp. The Harness contract says so, and the
// runner relies on it: a spec may end with the server already killed, and the
// stop the runner does afterwards must not turn a passing spec into a failure.
func TestStoppingAStoppedServerIsANoOp(t *testing.T) {
	t.Parallel()

	h := newFake(t)
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Errorf("stopping a killed server = %v, want a no-op", err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Errorf("stopping twice = %v, want a no-op", err)
	}
	if count := strings.Count(h.Logs(), "server stopped"); count != 0 {
		t.Errorf("a no-op stop was recorded %d times as a real one:\n%s", count, h.Logs())
	}
}

// TestRepliesComeFromTheMostSpecificSourceConfigured. Respond, Replies and
// Fallback are how a runner test scripts what the server says. If the precedence
// were wrong, a test that scripted a wrong reply to prove an assertion fails
// would silently get the right one instead, and would pass for the wrong reason.
func TestRepliesComeFromTheMostSpecificSourceConfigured(t *testing.T) {
	t.Parallel()

	h := newFake(t)
	h.Fallback = runner.ErrorReply("ERR unknown command")
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	scripted, err := h.Send(context.Background(), "GET k")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if scripted.Kind != runner.KindBulk || scripted.Text != "v" {
		t.Errorf("GET k = %s %q, want the scripted bulk reply", scripted.Kind, scripted.Text)
	}

	unscripted, err := h.Send(context.Background(), "FROB")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if unscripted.Kind != runner.KindError {
		t.Errorf("an unscripted command answered %s, want the fallback", unscripted.Kind)
	}

	// Respond takes precedence over both, which is how a test scripts a reply
	// that depends on what came before it.
	h.Respond = func(cmd string) (runner.Reply, error) {
		if cmd == "GET k" {
			return runner.NilReply(), nil
		}
		return runner.Reply{}, errors.New("the connection dropped")
	}
	overridden, err := h.Send(context.Background(), "GET k")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if overridden.Kind != runner.KindNil {
		t.Errorf("GET k = %s, want Respond to win over the scripted reply", overridden.Kind)
	}
	if _, err := h.Send(context.Background(), "FROB"); err == nil {
		t.Error("a transport error from Respond was swallowed")
	}
	if !strings.Contains(h.Logs(), "transport error") {
		t.Errorf("the transcript does not record the transport error:\n%s", h.Logs())
	}
}

// TestTheZeroFallbackIsAnInvalidReply. A harness built without a Fallback must
// answer unscripted commands with something that fails an assertion. Answering
// with an empty *status* instead would make `expect: ""` pass and, worse, would
// let a spec that asked for a command nobody scripted look like it was checked.
func TestTheZeroFallbackIsAnInvalidReply(t *testing.T) {
	t.Parallel()

	h := newFake(t)
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	reply, err := h.Send(context.Background(), "NOT SCRIPTED")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if reply.Kind != runner.KindInvalid {
		t.Errorf("an unscripted command with no fallback answered %s, want an invalid reply", reply.Kind)
	}
	if got := reply.String(); got != "<invalid reply>" {
		t.Errorf("the unscripted reply renders as %q, which could match an assertion", got)
	}
}

// TestInjectedFailuresAreReportedAndDoNotChangeState. The runner's failure-path
// tests inject these. A harness that returned the error but performed the
// operation anyway — or performed it and then returned the error — would leave
// those tests asserting a message while the state underneath was the opposite
// of what the failure implied.
func TestInjectedFailuresAreReportedAndDoNotChangeState(t *testing.T) {
	t.Parallel()

	t.Run("a start that fails leaves the server down", func(t *testing.T) {
		t.Parallel()
		h := newFake(t)
		h.StartErr = errors.New("health probe timed out")

		if err := h.Start(context.Background()); !errors.Is(err, h.StartErr) {
			t.Fatalf("Start = %v, want the injected error", err)
		}
		if h.Running() {
			t.Error("a failed Start left the server marked as running")
		}
		if h.Starts() != 0 {
			t.Errorf("Starts() = %d after a failed start, want 0", h.Starts())
		}
		if !strings.Contains(h.Logs(), "health probe timed out") {
			t.Errorf("the failure is not in the transcript:\n%s", h.Logs())
		}
	})

	t.Run("a restart that fails does not restart", func(t *testing.T) {
		t.Parallel()
		h := newFake(t)
		h.RestartErr = errors.New("the port was still held")
		if err := h.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}

		if err := h.Restart(context.Background()); !errors.Is(err, h.RestartErr) {
			t.Fatalf("Restart = %v, want the injected error", err)
		}
		if h.Starts() != 1 {
			t.Errorf("Starts() = %d after a failed restart, want 1", h.Starts())
		}
	})

	t.Run("a stop and a kill that fail are reported", func(t *testing.T) {
		t.Parallel()
		h := newFake(t)
		h.StopErr = errors.New("still alive after the graceful window")
		h.KillErr = errors.New("no such process")
		if err := h.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}

		if err := h.Stop(context.Background()); !errors.Is(err, h.StopErr) {
			t.Errorf("Stop = %v, want the injected error", err)
		}
		if err := h.Kill(); !errors.Is(err, h.KillErr) {
			t.Errorf("Kill = %v, want the injected error", err)
		}
		if !h.Running() {
			t.Error("a stop that failed still marked the server as down")
		}
	})
}

// TestCloseIsIdempotentAndCarriesItsError. The runner closes in a defer and
// reports a cleanup failure only when nothing worse happened, so Close has to be
// safe to call twice and has to surface the injected error the first time.
func TestCloseIsIdempotentAndCarriesItsError(t *testing.T) {
	t.Parallel()

	h := fakeharness.New(nil)
	h.CloseErr = errors.New("temp dir not removed")
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := h.Close(); !errors.Is(err, h.CloseErr) {
		t.Errorf("Close = %v, want the injected error", err)
	}
	if h.Running() {
		t.Error("Close left the server marked as running")
	}
	if err := h.Close(); err != nil {
		t.Errorf("closing twice = %v; Close must be idempotent", err)
	}
}

// TestInfoDescribesAServerThatIsNotThere. Info must hand back addresses that
// look like addresses so a scenario reading them does not crash, and a zero pid
// so nothing mistakes the fake for a real process to signal.
func TestInfoDescribesAServerThatIsNotThere(t *testing.T) {
	t.Parallel()

	info := newFake(t).Info()
	if info.ClientAddr == "" || info.AdminAddr == "" || info.DataDir == "" {
		t.Errorf("Info = %+v, want every field populated", info)
	}
	if info.PID != 0 {
		t.Errorf("Info reports pid %d; there is no process, and a non-zero pid could get something else signaled", info.PID)
	}
}

// TestFactoryBuildsOnePerSpec. The runner calls a factory once per spec and runs
// specs in parallel; a factory handing out one shared harness would let two
// specs race on its transcript and its lifecycle counters.
func TestFactoryBuildsOnePerSpec(t *testing.T) {
	t.Parallel()

	var names []string
	factory := fakeharness.Factory(func(opts runner.HarnessOptions) *fakeharness.Harness {
		names = append(names, opts.SpecName)
		return fakeharness.New(map[string]runner.Reply{"PING": runner.StatusReply("PONG")})
	})

	first, err := factory(runner.HarnessOptions{SpecName: "spec-one"})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	second, err := factory(runner.HarnessOptions{SpecName: "spec-two"})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}

	if first == second {
		t.Error("the factory handed out the same harness twice")
	}
	if len(names) != 2 || names[0] != "spec-one" || names[1] != "spec-two" {
		t.Errorf("the factory saw %v, want each spec's own name passed through", names)
	}
}

// TestConcurrentUseIsSafe. Specs run in parallel and a scenario may drive the
// harness from several goroutines, so a data race here would surface as a
// flake in whatever runner test happened to be running — the hardest kind of
// failure to attribute. The race detector is the assertion.
func TestConcurrentUseIsSafe(t *testing.T) {
	t.Parallel()

	h := newFake(t)
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := h.Send(context.Background(), "PING"); err != nil {
					t.Errorf("Send under concurrent use: %v", err)
					return
				}
				_ = h.Logs()
				_ = h.Starts()
				_ = h.Running()
			}
		}()
	}
	wg.Wait()

	if !h.Running() {
		t.Error("the server went down under concurrent reads")
	}
}
