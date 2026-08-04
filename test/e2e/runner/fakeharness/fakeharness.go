// Package fakeharness provides an in-memory runner.Harness for testing the
// runner itself.
//
// The runner's own correctness cannot be established by the specs it runs, so
// it is exercised against a scripted harness with no server behind it. Nothing
// here talks to a process; a run against this harness proves something about
// the runner and nothing about AtlasCache.
package fakeharness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/b3vet/atlascache/test/e2e/runner"
)

// Harness is a scripted, in-memory harness.
type Harness struct {
	// Replies maps a command to the reply it gets. A command with no entry is
	// answered with Fallback.
	Replies map[string]runner.Reply
	// Fallback answers commands missing from Replies. The zero value is an
	// invalid reply, which fails any assertion made against it.
	Fallback runner.Reply
	// Respond, when set, takes precedence over Replies and Fallback.
	Respond func(cmd string) (runner.Reply, error)

	// Failures injected into the lifecycle, to exercise the runner's own paths.
	StartErr, StopErr, RestartErr, KillErr, CloseErr error

	mu      sync.Mutex
	running bool
	closed  bool
	starts  int
	log     []string
}

// New builds a harness that answers the given commands.
func New(replies map[string]runner.Reply) *Harness {
	return &Harness{Replies: replies}
}

// Factory returns a runner.HarnessFactory handing out harnesses built by build.
// The spec name is passed through so a test can script per-spec behavior.
func Factory(build func(runner.HarnessOptions) *Harness) runner.HarnessFactory {
	return func(opts runner.HarnessOptions) (runner.Harness, error) {
		return build(opts), nil
	}
}

// Start marks the server running and records it in the log.
func (h *Harness) Start(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.StartErr != nil {
		h.logf("start failed: %v", h.StartErr)
		return h.StartErr
	}
	h.running = true
	h.starts++
	h.logf("server started (start #%d)", h.starts)
	return nil
}

// Stop marks the server stopped. Stopping a stopped server is a no-op.
func (h *Harness) Stop(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.StopErr != nil {
		h.logf("stop failed: %v", h.StopErr)
		return h.StopErr
	}
	if h.running {
		h.running = false
		h.logf("server stopped")
	}
	return nil
}

// Restart stops and starts the server.
func (h *Harness) Restart(ctx context.Context) error {
	if h.RestartErr != nil {
		h.mu.Lock()
		h.logf("restart failed: %v", h.RestartErr)
		h.mu.Unlock()
		return h.RestartErr
	}
	if err := h.Stop(ctx); err != nil {
		return err
	}
	return h.Start(ctx)
}

// Kill marks the server dead without a graceful exit.
func (h *Harness) Kill() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.KillErr != nil {
		h.logf("kill failed: %v", h.KillErr)
		return h.KillErr
	}
	h.running = false
	h.logf("server killed")
	return nil
}

// Send answers a command from the script. It reconnects implicitly, the way a
// real harness does, so a command after a kill fails on the server being down
// rather than on a stale connection.
func (h *Harness) Send(_ context.Context, cmd string) (runner.Reply, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if !h.running {
		h.logf("refused %q: the server is not running", cmd)
		return runner.Reply{}, errors.New("connection refused: the server is not running")
	}
	h.logf("> %s", cmd)

	if h.Respond != nil {
		reply, err := h.Respond(cmd)
		h.logReply(reply, err)
		return reply, err
	}
	if reply, ok := h.Replies[cmd]; ok {
		h.logReply(reply, nil)
		return reply, nil
	}
	h.logReply(h.Fallback, nil)
	return h.Fallback, nil
}

// fakeAddr stands in for an address, a data directory, anything a real
// harness would hand out. Nothing is listening on it.
const fakeAddr = "fake"

// Info describes the fake server. The addresses are not listening on anything.
func (h *Harness) Info() runner.ServerInfo {
	return runner.ServerInfo{ClientAddr: fakeAddr, AdminAddr: fakeAddr, DataDir: fakeAddr, PID: 0}
}

// Logs returns the recorded transcript, standing in for captured stdout.
func (h *Harness) Logs() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.log, "\n")
}

// Close releases the harness. It is idempotent.
func (h *Harness) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	h.running = false
	h.logf("harness closed")
	return h.CloseErr
}

// Starts reports how many times the server was started, which is how a test
// checks that a restart step really restarted.
func (h *Harness) Starts() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.starts
}

// Running reports whether the fake server is up.
func (h *Harness) Running() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.running
}

// logf appends to the transcript. The caller holds the lock.
func (h *Harness) logf(format string, args ...any) {
	h.log = append(h.log, fmt.Sprintf(format, args...))
}

func (h *Harness) logReply(reply runner.Reply, err error) {
	if err != nil {
		h.logf("< transport error: %v", err)
		return
	}
	h.logf("< %s", reply.String())
}
