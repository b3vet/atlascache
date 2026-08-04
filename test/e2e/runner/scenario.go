package runner

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ScenarioFunc is the Go escape hatch a spec reaches for when a case needs real
// control flow — timing, concurrency, crash recovery. It returns nil on success
// and an error describing the failure otherwise.
type ScenarioFunc func(*Ctx) error

var (
	scenarioMu sync.RWMutex
	scenarios  = map[string]ScenarioFunc{}
)

// RegisterScenario adds a scenario under name. It is meant to be called from
// init in the scenarios package. A duplicate or empty name panics: both are
// programming errors that would otherwise surface as a silently wrong test.
func RegisterScenario(name string, fn ScenarioFunc) {
	if name == "" {
		panic("runner: scenario name is empty")
	}
	if fn == nil {
		panic("runner: scenario " + name + " is nil")
	}
	scenarioMu.Lock()
	defer scenarioMu.Unlock()
	if _, exists := scenarios[name]; exists {
		panic("runner: scenario " + name + " is already registered")
	}
	scenarios[name] = fn
}

// LookupScenario returns the scenario registered under name.
func LookupScenario(name string) (ScenarioFunc, bool) {
	scenarioMu.RLock()
	defer scenarioMu.RUnlock()
	fn, ok := scenarios[name]
	return fn, ok
}

// ScenarioNames lists every registered scenario, sorted.
func ScenarioNames() []string {
	scenarioMu.RLock()
	defer scenarioMu.RUnlock()
	names := make([]string, 0, len(scenarios))
	for name := range scenarios {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func describeScenarios() string {
	names := ScenarioNames()
	if len(names) == 0 {
		return "none are registered"
	}
	return strings.Join(names, ", ")
}

// Ctx is what a scenario is handed: the spec it belongs to, the harness driving
// the server, and a log that is printed with the failure if the scenario fails.
type Ctx struct {
	Spec    *Spec
	Harness Harness

	ctx context.Context

	mu  sync.Mutex
	log []string
}

// Context returns the run context. It is canceled when the spec times out or
// the run is interrupted; a scenario that blocks must select on its Done.
func (c *Ctx) Context() context.Context { return c.ctx }

// Send issues a command and returns the reply. An error reply comes back as a
// Reply of kind KindError, not as an error.
func (c *Ctx) Send(cmd string) (Reply, error) { return c.Harness.Send(c.ctx, cmd) }

// Start starts the server.
func (c *Ctx) Start() error { return c.Harness.Start(c.ctx) }

// Stop stops the server gracefully.
func (c *Ctx) Stop() error { return c.Harness.Stop(c.ctx) }

// Restart stops and restarts the server against the same data directory.
func (c *Ctx) Restart() error { return c.Harness.Restart(c.ctx) }

// Kill sends SIGKILL to the server.
func (c *Ctx) Kill() error { return c.Harness.Kill() }

// Info describes the running server, for scenarios that need to reach it
// outside the command protocol.
func (c *Ctx) Info() ServerInfo { return c.Harness.Info() }

// Sleep waits for d, or returns early if the run is canceled.
func (c *Ctx) Sleep(d time.Duration) error { return sleep(c.ctx, d) }

// Logf records a line shown alongside the failure if the scenario fails.
func (c *Ctx) Logf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log = append(c.log, fmt.Sprintf(format, args...))
}

// Log returns the lines recorded with Logf.
func (c *Ctx) Log() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.log...)
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
