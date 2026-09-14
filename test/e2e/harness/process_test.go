package harness

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/b3vet/atlascache/test/e2e/client"
	"github.com/b3vet/atlascache/test/e2e/runner"
)

// standInHarness builds a harness around a stand-in script rather than the real
// server. It is for the paths that need a binary behaving in a way a working
// server cannot be asked to behave, and that never reach Start.
func standInHarness(t *testing.T, script string) *Process {
	t.Helper()

	h, err := New(runner.HarnessOptions{SpecName: t.Name(), Binary: standIn(t, script)}, Settings{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// launched hands back a Process holding a running stand-in, wired up exactly as
// startOnce would have wired a real server. Start insists on a health endpoint,
// which a shell script cannot serve, so the shutdown paths are driven against a
// process assembled here instead — and a shell script is the only way to get a
// server that ignores SIGTERM on demand.
//
// The script must create the file named by $READY once its signal handlers are
// installed. Without that handshake the test would race the shell's own startup
// and sometimes signal a process that had not reached its trap yet, which is
// how a test about a hung server turns into a test about a slow fork.
func launched(t *testing.T, settings Settings, script string) (*Process, *proc) {
	t.Helper()

	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.CommandContext(t.Context(), standIn(t, script))
	cmd.Env = append(os.Environ(), "READY="+ready)
	cmd.SysProcAttr = sysProcAttr()
	// Same ETXTBSY window the real spawn guards against: this test writes an
	// executable and launches it while sibling tests are forking.
	if err := startWithRetry(cmd); err != nil {
		t.Fatalf("starting the stand-in: %v", err)
	}

	running := &proc{cmd: cmd, pid: cmd.Process.Pid, exited: make(chan struct{})}
	go func() {
		running.waitErr = cmd.Wait()
		close(running.exited)
	}()
	t.Cleanup(func() {
		if err := signalGroup(running.pid, syscall.SIGKILL); err != nil {
			t.Errorf("cleaning up the stand-in: %v", err)
		}
	})

	if !waitUntil(t, func() bool { _, err := os.Stat(ready); return err == nil }) {
		t.Fatal("the stand-in never reported that its signal handlers were installed")
	}
	return &Process{settings: settings, logs: &logBuffer{}, root: t.TempDir(), current: running}, running
}

// waitUntil polls done for a few seconds and reports whether it became true.
func waitUntil(t *testing.T, done func() bool) bool {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestStopKillsAServerThatIgnoresSIGTERM. The graceful window is a bound, not a
// hope. A server that ignores SIGTERM has to be killed and the spec has to be
// told, because a Stop that waited forever would hang the run and a Stop that
// returned success would leave the process holding its port and its data
// directory for every spec that followed.
func TestStopKillsAServerThatIgnoresSIGTERM(t *testing.T) {
	t.Parallel()

	// `trap '' TERM` makes the shell ignore SIGTERM outright; the loop keeps it
	// alive without spinning a core.
	window := 200 * time.Millisecond
	p, running := launched(t, Settings{GracefulWindow: window},
		"trap '' TERM\ntouch \"$READY\"\nwhile :; do sleep 0.05; done\n")

	started := time.Now()
	err := p.Stop(t.Context())
	if err == nil {
		t.Fatal("Stop reported success against a server that ignored SIGTERM")
	}
	if !strings.Contains(err.Error(), "did not exit within 200ms of SIGTERM") {
		t.Errorf("error = %v, want it to name the window that ran out", err)
	}
	if !strings.Contains(err.Error(), "killed") {
		t.Errorf("error = %v, want it to say the server was killed", err)
	}
	if elapsed := time.Since(started); elapsed < window {
		t.Errorf("Stop gave up after %s, before the %s graceful window was spent", elapsed, window)
	}

	if alive(running.pid) {
		t.Errorf("pid %d survived a Stop that reported killing it", running.pid)
	}
	if pid := p.Info().PID; pid != 0 {
		t.Errorf("Info still reports pid %d; a killed process must not stay registered as running", pid)
	}
	if err := p.Stop(t.Context()); err != nil {
		t.Errorf("stopping again after the kill = %v, want a no-op", err)
	}
}

// TestStopReportsAServerThatDiedBadlyAfterSIGTERM. A server that exits non-zero
// on SIGTERM did shut down, but not cleanly. Treating that as success is how a
// graceful-shutdown regression gets through the gate, so the exit status has to
// reach the spec.
func TestStopReportsAServerThatDiedBadlyAfterSIGTERM(t *testing.T) {
	t.Parallel()

	p, running := launched(t, Settings{GracefulWindow: 10 * time.Second},
		"trap 'exit 3' TERM\ntouch \"$READY\"\nwhile :; do sleep 0.05; done\n")

	err := p.Stop(t.Context())
	if err == nil {
		t.Fatal("Stop reported success for a server that exited 3 on SIGTERM")
	}
	if !strings.Contains(err.Error(), "exited badly after SIGTERM") {
		t.Errorf("error = %v, want it to say the exit was not clean", err)
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("error = %v, want it to carry the exit status", err)
	}
	if alive(running.pid) {
		t.Errorf("pid %d is still running after it reported exiting", running.pid)
	}
}

// TestTheHarnessRefusesWorkAfterItIsClosed. Close is called from a defer once
// the spec is over and it removes the directory the server's config and data
// live in. Anything that started a server after that would be operating on a
// tree that no longer exists, so both entry points have to refuse rather than
// half-work.
func TestTheHarnessRefusesWorkAfterItIsClosed(t *testing.T) {
	t.Parallel()

	h := standInHarness(t, "exit 0\n")
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := h.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("Start after Close = %v, want a closed-harness error", err)
	}
	if err := h.Restart(t.Context()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("Restart after Close = %v, want a closed-harness error", err)
	}
	if pid := h.Info().PID; pid != 0 {
		t.Errorf("a closed harness reports pid %d as running", pid)
	}
}

// TestStartingARunningServerIsANoOp. crash_and_recover calls Start on a harness
// it has already killed, and a scenario cannot always know whether the server is
// up. A second Start that launched a second process would leave the first one
// orphaned on its port.
func TestStartingARunningServerIsANoOp(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	mustStart(t, h)

	first := h.Info().PID
	if err := h.Start(testContext(t)); err != nil {
		t.Fatalf("Start on a running server = %v, want a no-op", err)
	}
	if second := h.Info().PID; second != first {
		t.Errorf("a second Start replaced pid %d with %d; the first server was orphaned", first, second)
	}
	if _, err := h.Send(testContext(t), "PING"); err != nil {
		t.Errorf("the server stopped answering after a redundant Start: %v\n%s", err, h.Logs())
	}
}

// TestSendWithoutARunningServerSaysSo. Send dials on demand, so a spec that
// sends after a kill without restarting would otherwise get a connection-refused
// error naming a port rather than the fact that nothing is running.
func TestSendWithoutARunningServerSaysSo(t *testing.T) {
	t.Parallel()

	h := standInHarness(t, "exit 0\n")
	_, err := h.Send(t.Context(), "PING")
	if err == nil {
		t.Fatal("Send reported success with no server running")
	}
	if !strings.Contains(err.Error(), "not running") {
		t.Errorf("error = %v, want it to say the server is not running", err)
	}
}

// TestSendReportsALostPortAsATransportError. The harness reconnects on a
// transport error and fails the spec on anything else, so a dial that cannot
// reach the server has to be classified as transport — misclassifying it would
// turn a server that died into a protocol failure and send the reader hunting
// the wrong bug.
func TestSendReportsALostPortAsATransportError(t *testing.T) {
	t.Parallel()

	ports, err := freePorts(1)
	if err != nil {
		t.Fatalf("freePorts: %v", err)
	}
	release(ports...) // nothing is listening there now

	p := &Process{
		logs:       &logBuffer{},
		root:       t.TempDir(),
		current:    &proc{pid: os.Getpid(), exited: make(chan struct{})},
		clientAddr: net.JoinHostPort(loopback, strconv.Itoa(ports[0])),
	}

	if _, err := p.Send(t.Context(), "PING"); err == nil {
		t.Fatal("Send against a port nothing is listening on reported success")
	} else if !client.IsConnError(err) {
		t.Errorf("error = %v, want a transport error", err)
	}
}

// TestRunBinaryTellsALaunchFailureFromANonZeroExit. The scenarios that read the
// binary directly — an invalid config that must fail fast, --version — treat a
// non-zero exit as the thing under test and the error return as "the run never
// happened". Conflating them would make a missing binary look like a passing
// assertion about a rejected config.
func TestRunBinaryTellsALaunchFailureFromANonZeroExit(t *testing.T) {
	t.Parallel()

	t.Run("a non-zero exit is a result, not an error", func(t *testing.T) {
		t.Parallel()
		h := standInHarness(t, "echo 'atlascache: invalid config' >&2\nexit 7\n")

		code, output, err := h.RunBinary(t.Context(), "--config", "broken.yaml")
		if err != nil {
			t.Fatalf("RunBinary: %v", err)
		}
		if code != 7 {
			t.Errorf("exit code = %d, want 7", code)
		}
		if !strings.Contains(output, "invalid config") {
			t.Errorf("output = %q, want the binary's stderr to be captured too", output)
		}
	})

	t.Run("a binary that cannot be launched is an error", func(t *testing.T) {
		t.Parallel()
		binary := standIn(t, "exit 0\n")
		h, err := New(runner.HarnessOptions{SpecName: t.Name(), Binary: binary}, Settings{})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { _ = h.Close() })
		if removeErr := os.Remove(binary); removeErr != nil {
			t.Fatalf("removing the binary: %v", removeErr)
		}

		code, _, err := h.RunBinary(t.Context(), "--version")
		if err == nil {
			t.Fatal("RunBinary reported success for a binary that is gone")
		}
		if code != -1 {
			t.Errorf("exit code = %d, want -1 for a run that never happened", code)
		}
	})

	t.Run("a binary that never exits is cut off by the context", func(t *testing.T) {
		t.Parallel()
		// `exec` so the stand-in *is* the long-running process rather than its
		// parent: the context cancels the process it launched, and a forked
		// child would keep the output pipe open long after that.
		h := standInHarness(t, "exec sleep 120\n")

		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()

		started := time.Now()
		code, _, err := h.RunBinary(ctx, "--version")
		if err == nil {
			t.Fatal("RunBinary reported success for a binary that never exits")
		}
		if code != -1 {
			t.Errorf("exit code = %d, want -1; a run the context cut short has no exit status", code)
		}
		if elapsed := time.Since(started); elapsed > 10*time.Second {
			t.Errorf("RunBinary took %s to honor a 200ms context", elapsed)
		}
	})
}
