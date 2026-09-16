//go:build !unix

package harness

import (
	"errors"
	"os/exec"
	"syscall"
)

// sysProcAttr has no process-group equivalent to set here. The harness still
// runs, but cleanup can only reach the server itself.
func sysProcAttr() *syscall.SysProcAttr { return nil }

// cancelGroup returns nil, which leaves os/exec's own cancellation in place: a
// kill of the process itself. There is no wider group to reach on this
// platform, and replacing the default with something that could only report
// that would leave a cancelled run with nothing killed at all.
func cancelGroup(*exec.Cmd) func() error { return nil }

// signalGroup cannot address a process group off unix. Rather than quietly
// signaling only the leader and leaving children behind, it says so.
func signalGroup(int, syscall.Signal) error {
	return errors.New("signaling a process group is not supported on this platform")
}
