package runner

import (
	"context"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultLogTail is how many lines of captured server log a failure carries.
const DefaultLogTail = 20

// DefaultSpecTimeout bounds one spec, so a hung server fails the run instead of
// hanging CI.
const DefaultSpecTimeout = 60 * time.Second

// Outcome is how a spec ended.
type Outcome string

// The outcomes a spec can reach.
const (
	OutcomePass Outcome = "pass"
	OutcomeFail Outcome = "fail"
)

// Phase is where in a spec's life a failure happened, which narrows a
// diagnosis before the message is even read.
type Phase string

// The phases a spec passes through.
const (
	PhaseHarness Phase = "harness"
	PhaseStart   Phase = "start"
	PhaseStep    Phase = "step"
	PhaseStop    Phase = "stop"
	PhaseCleanup Phase = "cleanup"
)

// Failure is everything needed to diagnose a failing spec without rerunning it.
type Failure struct {
	Phase     Phase    `json:"phase"`
	StepIndex int      `json:"step_index,omitempty"`
	Line      int      `json:"line,omitempty"`
	Step      string   `json:"step,omitempty"`
	Message   string   `json:"message"`
	Expected  string   `json:"expected,omitempty"`
	Actual    string   `json:"actual,omitempty"`
	Notes     []string `json:"notes,omitempty"`
	LogTail   string   `json:"log_tail,omitempty"`
}

// SpecResult is one spec's run.
type SpecResult struct {
	Name       string   `json:"name"`
	Tier       Tier     `json:"tier"`
	Feature    string   `json:"feature"`
	Issue      string   `json:"issue,omitempty"`
	Path       string   `json:"path"`
	Outcome    Outcome  `json:"outcome"`
	DurationMS int64    `json:"duration_ms"`
	Failure    *Failure `json:"failure,omitempty"`
}

// Duration is the spec's wall-clock run time.
func (r SpecResult) Duration() time.Duration { return time.Duration(r.DurationMS) * time.Millisecond }

// Executor runs specs against harnesses built by Factory.
type Executor struct {
	// Factory builds one harness per spec. Required.
	Factory HarnessFactory
	// Binary is the server binary passed through to the factory.
	Binary string
	// SpecTimeout bounds one spec. Zero means DefaultSpecTimeout.
	SpecTimeout time.Duration
	// LogTail is how many server log lines a failure carries. Zero means
	// DefaultLogTail; a negative value drops the log entirely.
	LogTail int
}

func (e *Executor) specTimeout() time.Duration {
	if e.SpecTimeout <= 0 {
		return DefaultSpecTimeout
	}
	return e.SpecTimeout
}

func (e *Executor) logTail() int {
	if e.LogTail == 0 {
		return DefaultLogTail
	}
	return e.LogTail
}

// RunAll executes specs with at most parallel of them in flight. Results come
// back sorted by spec name, so two runs of the same suite produce the same
// report. progress, when non-nil, is called as each spec finishes and may be
// called from several goroutines at once.
func (e *Executor) RunAll(ctx context.Context, specs []*Spec, parallel int, progress func(SpecResult)) []SpecResult {
	if parallel < 1 {
		parallel = 1
	}

	results := make([]SpecResult, len(specs))
	slots := make(chan struct{}, parallel)
	var wg sync.WaitGroup

	for i, spec := range specs {
		wg.Add(1)
		go func(i int, spec *Spec) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()

			result := e.Run(ctx, spec)
			results[i] = result
			if progress != nil {
				progress(result)
			}
		}(i, spec)
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	return results
}

// Run executes one spec end to end: build a harness, start the server, walk the
// steps, stop the server, clean up.
func (e *Executor) Run(ctx context.Context, spec *Spec) SpecResult {
	result := SpecResult{
		Name:    spec.Name,
		Tier:    spec.Tier,
		Feature: spec.Feature,
		Issue:   spec.Issue,
		Path:    spec.Path,
		Outcome: OutcomePass,
	}

	started := time.Now()
	failure := e.run(ctx, spec)
	result.DurationMS = time.Since(started).Milliseconds()

	if failure != nil {
		result.Outcome = OutcomeFail
		result.Failure = failure
	}
	return result
}

func (e *Executor) run(ctx context.Context, spec *Spec) (failure *Failure) {
	if e.Factory == nil {
		return &Failure{Phase: PhaseHarness, Message: "no harness factory is configured"}
	}

	ctx, cancel := context.WithTimeout(ctx, e.specTimeout())
	defer cancel()

	harness, err := e.Factory(HarnessOptions{
		SpecName: spec.Name,
		Binary:   e.Binary,
		Config:   spec.Config,
	})
	if err != nil {
		return &Failure{Phase: PhaseHarness, Message: "building the harness failed: " + err.Error()}
	}

	// A harness that cannot clean up has leaked a process or a temp directory,
	// which is a real failure — but only worth reporting when nothing worse
	// already happened.
	defer func() {
		if err := harness.Close(); err != nil && failure == nil {
			failure = &Failure{Phase: PhaseCleanup, Message: "cleaning up after the spec failed: " + err.Error()}
		}
	}()

	if err := harness.Start(ctx); err != nil {
		return e.withLogs(harness, &Failure{Phase: PhaseStart, Message: "the server did not start: " + err.Error()})
	}

	for i, step := range spec.Steps {
		if err := ctx.Err(); err != nil {
			return e.withLogs(harness, &Failure{
				Phase: PhaseStep, StepIndex: i + 1, Line: step.Line, Step: step.String(),
				Message: "the run was cut short: " + err.Error(),
			})
		}
		if failure := e.runStep(ctx, harness, spec, i, step); failure != nil {
			return e.withLogs(harness, failure)
		}
	}

	if err := harness.Stop(ctx); err != nil {
		return e.withLogs(harness, &Failure{Phase: PhaseStop, Message: "the server did not stop cleanly: " + err.Error()})
	}
	return nil
}

func (e *Executor) runStep(ctx context.Context, harness Harness, spec *Spec, index int, step Step) *Failure {
	fail := func(message string, expected, actual string) *Failure {
		return &Failure{
			Phase: PhaseStep, StepIndex: index + 1, Line: step.Line, Step: step.String(),
			Message: message, Expected: expected, Actual: actual,
		}
	}

	switch step.Kind() {
	case StepCommand:
		reply, err := harness.Send(ctx, step.Cmd)
		if err != nil {
			return fail("sending the command failed: "+err.Error(), "", "")
		}
		if result := assertReply(step, reply); !result.ok {
			return fail(result.message, result.expected, result.actual)
		}

	case StepSleep:
		if err := sleep(ctx, step.Sleep.D); err != nil {
			return fail("the sleep was cut short: "+err.Error(), "", "")
		}

	case StepScenario:
		return e.runScenario(ctx, harness, spec, step, fail)

	case StepRestart:
		if err := harness.Restart(ctx); err != nil {
			return fail("the server did not restart: "+err.Error(), "", "")
		}

	case StepKill:
		if err := harness.Kill(); err != nil {
			return fail("the server could not be killed: "+err.Error(), "", "")
		}

	default:
		return fail("unknown step form", "", "")
	}
	return nil
}

func (e *Executor) runScenario(
	ctx context.Context, harness Harness, spec *Spec,
	step Step, fail func(string, string, string) *Failure,
) *Failure {
	fn, ok := LookupScenario(step.Scenario)
	if !ok {
		return fail(fmt.Sprintf("scenario %q is not registered", step.Scenario), "", "")
	}

	scenarioCtx := &Ctx{Spec: spec, Harness: harness, ctx: ctx}
	err := callScenario(fn, scenarioCtx)
	if err == nil {
		return nil
	}

	failure := fail("scenario "+step.Scenario+" failed: "+err.Error(), "", "")
	failure.Notes = scenarioCtx.Log()
	return failure
}

// callScenario turns a panicking scenario into a failed step, so one bad
// scenario cannot take down a whole parallel run.
func callScenario(fn ScenarioFunc, c *Ctx) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			c.Logf("panic: %v", recovered)
			for _, line := range strings.Split(strings.TrimSpace(string(debug.Stack())), "\n") {
				c.Logf("%s", line)
			}
			err = fmt.Errorf("panic: %v", recovered)
		}
	}()
	return fn(c)
}

func (e *Executor) withLogs(harness Harness, failure *Failure) *Failure {
	failure.LogTail = tailLines(harness.Logs(), e.logTail())
	return failure
}
