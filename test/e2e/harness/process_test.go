package harness

import (
	"context"
	"errors"
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

// cleanupGrace is how long a cleanup waits to observe the stand-in exiting on
// its own before resorting to signaling the group. Comfortably longer than the
// gap between Wait returning and `exited` closing, and short enough not to
// matter on the few tests that end with the process still alive.
const cleanupGrace = 250 * time.Millisecond

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
	// Most tests here end with the stand-in already dead, because killing it is
	// what they are testing. Signaling anyway is not merely redundant: once the
	// process is reaped its pgid is free, and a busy machine hands it to
	// something else. Signaling then either fails with EPERM — the flake this
	// cleanup used to produce under parallel load — or, worse, succeeds against
	// a process group that has nothing to do with this test.
	//
	// So the group is signaled only while a member is provably alive, the same
	// rule drain follows and the reason the pgid cannot have been recycled
	// underneath it. Waiting rather than polling matters because the pid becomes
	// reusable the moment Wait reaps it and `exited` closes on the statement
	// after, so a non-blocking check can still see a process that died
	// microseconds ago. One that is genuinely running never closes the channel.
	t.Cleanup(func() {
		select {
		case <-running.exited:
			return
		case <-time.After(cleanupGrace):
		}
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

// waitForPID reads the pid a stand-in wrote, waiting for the file to appear.
func waitForPID(t *testing.T, path string) int {
	t.Helper()

	var pid int
	if !waitUntil(t, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		parsed, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
		if convErr != nil {
			return false
		}
		pid = parsed
		return true
	}) {
		t.Fatal("the stand-in never reported its child's pid")
	}
	return pid
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
		// parent. The forking case, which used to hang here rather than fail,
		// has a test of its own below.
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

// TestRunBinaryReapsABinaryThatForks. This is the one that used to hang.
//
// RunBinary sets Setpgid, but exec.CommandContext cancels the process it
// started and nothing else. A binary that forks a child which inherits stdout
// and outlives it therefore kept the output pipe open with no one left to
// close it, and the call never returned: the run stalled until `go test` or CI
// gave up, reporting a timeout with nothing in it. A gate that hangs is worse
// than one that fails, so the forked child is killed by group and the run comes
// back with an error naming what happened.
//
// The stand-in below is a binary that forks: a child that inherits stdout and
// sleeps well past the run, a parent that reports its pid and exits at once.
func TestRunBinaryReapsABinaryThatForks(t *testing.T) {
	t.Parallel()

	pidFile := filepath.Join(t.TempDir(), "forked.pid")
	h := standInHarness(t, "echo started\nsleep 120 &\necho $! > "+pidFile+"\nexit 0\n")

	// Registered before the run, not after it: when this regresses the run does
	// not return at all, and a test about not leaving processes behind must not
	// leave one behind on its way to saying so.
	t.Cleanup(func() { killLeftovers(pidFile) })

	started := time.Now()
	code, output, err := runBinaryWithin(t, h, "--version")
	elapsed := time.Since(started)

	child := waitForPID(t, pidFile)
	if !waitUntil(t, func() bool { return !alive(child) }) {
		t.Errorf("the forked child (pid %d) is still running %s after the run returned", child, elapsed)
	}
	if err == nil {
		t.Fatalf("RunBinary reported success for a binary that left a process behind (exit %d)", code)
	}
	if !errors.Is(err, errForked) {
		t.Errorf("error = %v, want it to say the binary left a process running", err)
	}
	if code != -1 {
		t.Errorf("exit code = %d, want -1; a run that leaked a process is not a result to assert on", code)
	}
	if !strings.Contains(output, "started") {
		t.Errorf("output = %q, want what the binary actually wrote before it forked", output)
	}
	if !strings.Contains(h.Logs(), "killing process group") {
		t.Errorf("logs = %q, want the harness to record the group it killed", h.Logs())
	}
}

// TestKillGroupRefusesAPidThatIsNotAGroup. kill(-0, sig) means "every process
// in my own process group", which during a test run is the runner itself and
// every spec it has in flight. Cleanup runs on the paths where something has
// already gone wrong and a pid may well be zero, so it must refuse rather than
// signal. If this ever regresses, the test binary dies where it stands — which
// is the only way a bug like this announces itself honestly.
func TestKillGroupRefusesAPidThatIsNotAGroup(t *testing.T) {
	t.Parallel()

	p := &Process{logs: &logBuffer{}}
	for _, pid := range []int{0, -1} {
		p.killGroup(pid, "a run that never started")
	}

	if !strings.Contains(p.Logs(), "not a group to signal") {
		t.Errorf("logs = %q, want the refusal recorded", p.Logs())
	}
	if strings.Contains(p.Logs(), "killing process group") {
		t.Errorf("logs = %q; a pid that is not a group must not be signaled at all", p.Logs())
	}
}

// killLeftovers kills the pid a forking stand-in recorded, if it is still
// there. Nothing to do on the passing path — the harness already killed it.
func killLeftovers(pidFile string) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return
	}
	process, findErr := os.FindProcess(pid)
	if findErr != nil {
		return
	}
	if killErr := process.Kill(); killErr != nil {
		return // already gone, which is what the passing path looks like
	}
}

// runBinaryWithin calls RunBinary and fails the test rather than blocking when
// it does not come back.
//
// The bug this guards against is a hang, so a test for it that could itself
// hang would reproduce the very failure mode it exists to prevent: the package
// would stall until `go test` killed it, minutes later, naming nothing. The
// budget is kept inside the test binary's own deadline so the failure is this
// test's, with this test's name on it.
func runBinaryWithin(t *testing.T, h *Process, args ...string) (int, string, error) {
	t.Helper()

	budget := 10 * time.Second
	if deadline, ok := t.Deadline(); ok {
		if room := time.Until(deadline) - time.Second; room < budget {
			budget = room
		}
	}

	type outcome struct {
		code   int
		output string
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		code, output, err := h.RunBinary(t.Context(), args...)
		done <- outcome{code: code, output: output, err: err}
	}()

	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case got := <-done:
		return got.code, got.output, got.err
	case <-timer.C:
		t.Fatalf("RunBinary did not return within %s; it is waiting on output no one is left to close", budget)
		return 0, "", nil
	}
}

// TestDrainGivesUpOnAPipeItCannotClose. The reaping above rests on the kill
// closing the pipe. Something the harness cannot reach — a descriptor handed
// outside the process group, or a run with no group left to signal at all —
// must not be able to hold a run open anyway: the last resort is to close the
// harness's own end and return. The point of the whole change is that RunBinary
// always comes back.
//
// The pid here is zero, which is both the worst case and the safest one to
// test with: drain must refuse to signal it (kill(-0, …) is this test run's own
// process group) and must still finish. A drain that signaled it would take
// the test binary with it, which is the only honest way for that bug to report
// itself.
func TestDrainGivesUpOnAPipeItCannotClose(t *testing.T) {
	t.Parallel()

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	// Held by this test, so nothing the harness does will close it.
	defer func() { _ = write.Close() }()

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		buffer := make([]byte, 64)
		for {
			if _, readErr := read.Read(buffer); readErr != nil {
				return
			}
		}
	}()

	p := &Process{logs: &logBuffer{}}
	done := make(chan bool, 1)
	go func() { done <- p.drain(0, read, drained, "a binary holding its own output") }()

	select {
	case forked := <-done:
		if !forked {
			t.Error("drain reported nothing left behind while the pipe was still open")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("drain never returned; a pipe nobody will close must not hold a run open")
	}
	if !strings.Contains(p.Logs(), "outlived the kill") {
		t.Errorf("logs = %q, want the harness to say it closed the pipe itself", p.Logs())
	}
	if !strings.Contains(p.Logs(), "not a group to signal") {
		t.Errorf("logs = %q, want drain to have refused the pid rather than signaled it", p.Logs())
	}
	<-drained
}
