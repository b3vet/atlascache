//go:build unix

package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// TestSignalGroupRefusesPidsThatWouldSignalTooMuch. The harness signals the
// negated pid, so pid 0 means "my own process group" — the test run itself —
// and a negative pid is already a group id. A zero pid is exactly what a
// half-built proc carries, so passing it through would have the harness kill the
// runner instead of the server.
func TestSignalGroupRefusesPidsThatWouldSignalTooMuch(t *testing.T) {
	t.Parallel()

	for _, pid := range []int{0, -1, -4242} {
		err := signalGroup(pid, syscall.SIGTERM)
		if err == nil {
			t.Errorf("signalGroup(%d) reported success; it must refuse rather than signal a group it does not own", pid)
			continue
		}
		if !strings.Contains(err.Error(), "refusing to signal") {
			t.Errorf("signalGroup(%d) = %v, want it to say it refused", pid, err)
		}
	}
}

// TestSignalGroupTreatsAnAlreadyDeadGroupAsDone. Stop and Kill are documented
// no-ops against a server that already exited, and a spec may legitimately end
// with the server killed. Reporting the kernel's "no such process" would fail
// those specs during cleanup, after they had already passed.
func TestSignalGroupTreatsAnAlreadyDeadGroupAsDone(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(t.Context(), standIn(t, "exit 0\n"))
	cmd.SysProcAttr = sysProcAttr()
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the stand-in: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the stand-in did not exit cleanly: %v", err)
	}

	if err := signalGroup(pid, syscall.SIGTERM); err != nil {
		t.Errorf("signalGroup against an exited group = %v, want nil", err)
	}
	if err := signalGroup(pid, syscall.SIGKILL); err != nil {
		t.Errorf("signalGroup(SIGKILL) against an exited group = %v, want nil", err)
	}
}

// TestSignalGroupReachesTheChildrenTheServerLeftBehind. Cleanup signals the
// group rather than the process precisely so a server that spawned a child
// cannot leave it holding a port after the run — on CI one leaked port turns a
// single failure into a cascade of them. Signaling the pid alone would leave
// the child alive and this test is what notices.
func TestSignalGroupReachesTheChildrenTheServerLeftBehind(t *testing.T) {
	t.Parallel()

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	// The shell backgrounds a child, reports its pid, and then waits, so the
	// group has two members when the signal arrives.
	cmd := exec.CommandContext(t.Context(), standIn(t, "sleep 60 &\necho $! > "+pidFile+"\nwait\n"))
	cmd.SysProcAttr = sysProcAttr()
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the stand-in: %v", err)
	}
	leader := cmd.Process.Pid
	t.Cleanup(func() {
		if err := signalGroup(leader, syscall.SIGKILL); err != nil {
			t.Errorf("cleaning up the stand-in group: %v", err)
		}
	})

	child := waitForPID(t, pidFile)
	if child == leader {
		t.Fatalf("the stand-in reported the leader's own pid %d; the test proves nothing", child)
	}

	if err := signalGroup(leader, syscall.SIGKILL); err != nil {
		t.Fatalf("signalGroup: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Error("the stand-in reported a clean exit after its group was killed")
	}

	if !waitUntil(t, func() bool { return !alive(child) }) {
		t.Errorf("child pid %d survived a signal to its process group %d", child, leader)
	}
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
