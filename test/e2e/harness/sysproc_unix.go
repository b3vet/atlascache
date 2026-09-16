//go:build unix

package harness

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// sysProcAttr puts the server in its own process group.
//
// The group is what cleanup signals. A server that spawned children — or that
// grows them in a later phase — would otherwise leave them holding the ports
// after the run, and on CI one leaked port turns a single failure into a
// cascade of them.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// cancelGroup is what exec.CommandContext calls when a run's context expires:
// a SIGTERM to the whole group led by the process it started.
//
// os/exec's own cancellation reaches that one process. A binary that forked
// would survive it, and a forked child holding the output pipe open is exactly
// how a run that should have failed turns into a run that hangs. Signaling the
// group is what the harness already does to the server under test.
func cancelGroup(cmd *exec.Cmd) func() error {
	return func() error { return signalGroup(cmd.Process.Pid, syscall.SIGTERM) }
}

// signalGroup sends sig to the whole process group led by pid. A group that has
// already exited is not an error: Stop and Kill on a dead server are no-ops.
func signalGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return fmt.Errorf("refusing to signal pid %d", pid)
	}
	if err := syscall.Kill(-pid, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	return nil
}
