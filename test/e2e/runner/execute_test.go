package runner_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/b3vet/atlascache/test/e2e/runner"
	"github.com/b3vet/atlascache/test/e2e/runner/fakeharness"
)

var defaultReplies = map[string]runner.Reply{
	"PING": runner.StatusReply("PONG"),
	"QUIT": runner.StatusReply("OK"),
	"STATS": runner.MapReply(map[string]runner.Reply{
		"expirations": runner.IntegerReply(3),
		"keys":        runner.IntegerReply(2),
		"hits":        runner.IntegerReply(9),
		"misses":      runner.IntegerReply(1),
	}),
}

// Scenarios register once for the whole test binary; registering inside a test
// would panic the second time that test ran.
func init() {
	runner.RegisterScenario("failing_scenario", func(c *runner.Ctx) error {
		c.Logf("about to give up")
		return errors.New("the server never came back")
	})
	runner.RegisterScenario("panicking_scenario", func(*runner.Ctx) error {
		panic("scenario author forgot a nil check")
	})
	runner.RegisterScenario("hanging_scenario", func(c *runner.Ctx) error {
		<-c.Context().Done()
		return c.Context().Err()
	})
	runner.RegisterScenario("restart_then_ping", func(c *runner.Ctx) error {
		if err := c.Restart(); err != nil {
			return err
		}
		if err := c.Sleep(time.Millisecond); err != nil {
			return err
		}
		if info := c.Info(); info.ClientAddr == "" {
			return errors.New("no client address")
		}
		reply, err := c.Send("PING")
		if err != nil {
			return err
		}
		if reply.String() != "PONG" {
			return errors.New("wrong reply: " + reply.String())
		}
		return nil
	})
}

func loadSpec(t *testing.T, dir, file string) *runner.Spec {
	t.Helper()
	path := filepath.Join("testdata", dir, file)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	spec, err := runner.ParseSpec(path, data)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return spec
}

// executorWith builds an executor over a single fake harness, and returns both
// so a test can inspect what the run did to the server.
func executorWith(h *fakeharness.Harness) *runner.Executor {
	return &runner.Executor{
		Factory: func(runner.HarnessOptions) (runner.Harness, error) { return h, nil },
		Binary:  "fake",
	}
}

func newFake() *fakeharness.Harness {
	h := fakeharness.New(defaultReplies)
	h.Fallback = runner.ErrorReply("ERR unknown command")
	return h
}

// executorPerSpec builds a fresh fake for every spec, which is required
// whenever RunAll is given a parallelism above 1: a single shared fake records
// commands and lifecycle state, so concurrent specs would race on it.
func executorPerSpec() *runner.Executor {
	return &runner.Executor{
		Factory: func(runner.HarnessOptions) (runner.Harness, error) { return newFake(), nil },
		Binary:  "fake",
	}
}

func TestRunPasses(t *testing.T) {
	spec := loadSpec(t, "specs", "ping-basic.yaml")
	result := executorWith(newFake()).Run(context.Background(), spec)

	if result.Outcome != runner.OutcomePass {
		t.Fatalf("outcome = %s, failure = %#v", result.Outcome, result.Failure)
	}
	if result.Name != "ping-basic" || result.Tier != runner.TierSmoke || result.Feature != "FEAT-0010" {
		t.Errorf("result carries the wrong identity: %#v", result)
	}
}

func TestRunFailsOnWrongReply(t *testing.T) {
	spec := loadSpec(t, "failing", "wrong-reply.yaml")
	result := executorWith(newFake()).Run(context.Background(), spec)

	if result.Outcome != runner.OutcomeFail {
		t.Fatal("expected a failure")
	}
	failure := result.Failure
	if failure.Phase != runner.PhaseStep {
		t.Errorf("phase = %s", failure.Phase)
	}
	if failure.StepIndex != 2 {
		t.Errorf("step index = %d, want 2", failure.StepIndex)
	}
	if failure.Line != 10 {
		t.Errorf("line = %d, want 10", failure.Line)
	}
	if failure.Step != "cmd: GET missing" {
		t.Errorf("step = %q", failure.Step)
	}
	if failure.Expected != "NIL" {
		t.Errorf("expected = %q", failure.Expected)
	}
	if !strings.Contains(failure.Actual, "ERR unknown command") {
		t.Errorf("actual = %q", failure.Actual)
	}
	if !strings.Contains(failure.LogTail, "> GET missing") {
		t.Errorf("log tail should carry the server log, got %q", failure.LogTail)
	}
}

func TestRunExecutesEveryStepForm(t *testing.T) {
	h := newFake()
	spec := loadSpec(t, "specs", "lifecycle.yaml")
	result := executorWith(h).Run(context.Background(), spec)

	if result.Outcome != runner.OutcomePass {
		t.Fatalf("outcome = %s, failure = %#v", result.Outcome, result.Failure)
	}
	if got := h.Starts(); got != 2 {
		t.Errorf("server started %d times, want 2 (the initial start and the restart)", got)
	}
	if h.Running() {
		t.Error("the kill step should have left the server down")
	}
	if !strings.Contains(h.Logs(), "server killed") {
		t.Errorf("log = %q", h.Logs())
	}
}

func TestRunFailsWhenTheServerDoesNotStart(t *testing.T) {
	h := newFake()
	h.StartErr = errors.New("health probe timed out after 10s")

	result := executorWith(h).Run(context.Background(), loadSpec(t, "specs", "ping-basic.yaml"))
	if result.Outcome != runner.OutcomeFail {
		t.Fatal("expected a failure")
	}
	if result.Failure.Phase != runner.PhaseStart {
		t.Errorf("phase = %s, want start", result.Failure.Phase)
	}
	if !strings.Contains(result.Failure.LogTail, "health probe timed out") {
		t.Errorf("log tail = %q", result.Failure.LogTail)
	}
}

func TestRunFailsWhenTheServerDoesNotStop(t *testing.T) {
	h := newFake()
	h.StopErr = errors.New("still alive after the graceful window")

	result := executorWith(h).Run(context.Background(), loadSpec(t, "specs", "ping-basic.yaml"))
	if result.Outcome != runner.OutcomeFail || result.Failure.Phase != runner.PhaseStop {
		t.Fatalf("expected a stop failure, got %#v", result.Failure)
	}
}

func TestRunFailsWhenCleanupFails(t *testing.T) {
	h := newFake()
	h.CloseErr = errors.New("temp dir not removed")

	result := executorWith(h).Run(context.Background(), loadSpec(t, "specs", "ping-basic.yaml"))
	if result.Outcome != runner.OutcomeFail || result.Failure.Phase != runner.PhaseCleanup {
		t.Fatalf("expected a cleanup failure, got %#v", result.Failure)
	}
}

func TestRunFailsWhenTheHarnessCannotBeBuilt(t *testing.T) {
	executor := &runner.Executor{
		Factory: func(runner.HarnessOptions) (runner.Harness, error) {
			return nil, errors.New("no free port")
		},
	}
	result := executor.Run(context.Background(), loadSpec(t, "specs", "ping-basic.yaml"))
	if result.Outcome != runner.OutcomeFail || result.Failure.Phase != runner.PhaseHarness {
		t.Fatalf("expected a harness failure, got %#v", result.Failure)
	}
}

func TestRunWithoutAFactoryFails(t *testing.T) {
	result := (&runner.Executor{}).Run(context.Background(), loadSpec(t, "specs", "ping-basic.yaml"))
	if result.Outcome != runner.OutcomeFail {
		t.Fatal("a run with no harness must not report success")
	}
}

func TestFactoryReceivesTheSpecConfig(t *testing.T) {
	var got runner.HarnessOptions
	executor := &runner.Executor{
		Binary: "bin/atlascache",
		Factory: func(opts runner.HarnessOptions) (runner.Harness, error) {
			got = opts
			return newFake(), nil
		},
	}
	executor.Run(context.Background(), loadSpec(t, "specs", "stats-counters.yaml"))

	if got.SpecName != "stats-counters" || got.Binary != "bin/atlascache" {
		t.Errorf("options = %#v", got)
	}
	auth, ok := got.Config["auth"].(map[string]any)
	if !ok || auth["enabled"] != false {
		t.Errorf("config was not passed through: %#v", got.Config)
	}
}

func TestScenarioFailureCarriesItsLog(t *testing.T) {
	spec := parseInline(t, "scenario-failure.yaml", `version: 1
name: scenario-failure
tier: full
feature: FEAT-0008
steps:
  - scenario: failing_scenario
`)
	result := executorWith(newFake()).Run(context.Background(), spec)
	if result.Outcome != runner.OutcomeFail {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(result.Failure.Message, "the server never came back") {
		t.Errorf("message = %q", result.Failure.Message)
	}
	if len(result.Failure.Notes) == 0 || result.Failure.Notes[0] != "about to give up" {
		t.Errorf("notes = %#v", result.Failure.Notes)
	}
}

func TestPanickingScenarioFailsTheSpecAndNotTheRun(t *testing.T) {
	spec := parseInline(t, "scenario-panic.yaml", `version: 1
name: scenario-panic
tier: full
feature: FEAT-0008
steps:
  - scenario: panicking_scenario
`)
	result := executorWith(newFake()).Run(context.Background(), spec)
	if result.Outcome != runner.OutcomeFail {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(result.Failure.Message, "forgot a nil check") {
		t.Errorf("message = %q", result.Failure.Message)
	}
}

func TestScenarioDrivesTheHarness(t *testing.T) {
	h := newFake()
	spec := parseInline(t, "scenario-drives.yaml", `version: 1
name: scenario-drives
tier: full
feature: FEAT-0008
steps:
  - scenario: restart_then_ping
`)
	result := executorWith(h).Run(context.Background(), spec)
	if result.Outcome != runner.OutcomePass {
		t.Fatalf("outcome = %s, failure = %#v", result.Outcome, result.Failure)
	}
	if h.Starts() != 2 {
		t.Errorf("starts = %d, want 2", h.Starts())
	}
}

func TestSpecTimeoutEndsAHangingStep(t *testing.T) {
	spec := parseInline(t, "scenario-hangs.yaml", `version: 1
name: scenario-hangs
tier: full
feature: FEAT-0008
steps:
  - scenario: hanging_scenario
`)
	executor := executorWith(newFake())
	executor.SpecTimeout = 50 * time.Millisecond

	started := time.Now()
	result := executor.Run(context.Background(), spec)
	if result.Outcome != runner.OutcomeFail {
		t.Fatal("a spec that outran its timeout must fail")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the timeout did not fire: took %s", elapsed)
	}
}

func TestRunAllIsBoundedAndOrdered(t *testing.T) {
	specs, err := runner.LoadDir(filepath.Join("testdata", "specs"))
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}

	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)
	executor := &runner.Executor{
		Factory: func(runner.HarnessOptions) (runner.Harness, error) {
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()
			h := newFake()
			h.Respond = func(cmd string) (runner.Reply, error) {
				time.Sleep(2 * time.Millisecond)
				if reply, ok := defaultReplies[cmd]; ok {
					return reply, nil
				}
				return runner.ErrorReply("ERR unknown command"), nil
			}
			return &countingHarness{Harness: h, done: func() {
				mu.Lock()
				inFlight--
				mu.Unlock()
			}}, nil
		},
	}

	var finished int
	results := executor.RunAll(context.Background(), specs, 2, func(runner.SpecResult) {
		mu.Lock()
		finished++
		mu.Unlock()
	})

	if len(results) != len(specs) {
		t.Fatalf("results = %d, want %d", len(results), len(specs))
	}
	if finished != len(specs) {
		t.Errorf("progress was called %d times, want %d", finished, len(specs))
	}
	if peak > 2 {
		t.Errorf("%d specs ran at once, want at most 2", peak)
	}
	for i := 1; i < len(results); i++ {
		if results[i-1].Name > results[i].Name {
			t.Fatalf("results are not sorted by name: %v", names(results))
		}
	}
	for _, result := range results {
		if result.Outcome != runner.OutcomePass {
			t.Errorf("%s failed: %#v", result.Name, result.Failure)
		}
	}
}

// countingHarness notes when a harness is released, so the parallelism test can
// see how many were live at once.
type countingHarness struct {
	*fakeharness.Harness
	done func()
	once sync.Once
}

func (h *countingHarness) Close() error {
	h.once.Do(h.done)
	return h.Harness.Close()
}

func names(results []runner.SpecResult) []string {
	out := make([]string, len(results))
	for i, result := range results {
		out[i] = result.Name
	}
	return out
}

func parseInline(t *testing.T, name, body string) *runner.Spec {
	t.Helper()
	spec, err := runner.ParseSpec(name, []byte(body))
	if err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	return spec
}
