//go:build unix

package harness

import (
	"errors"
	"fmt"
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
