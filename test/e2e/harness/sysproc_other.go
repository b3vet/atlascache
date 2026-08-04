//go:build !unix

package harness

import (
	"errors"
	"syscall"
)

// sysProcAttr has no process-group equivalent to set here. The harness still
// runs, but cleanup can only reach the server itself.
func sysProcAttr() *syscall.SysProcAttr { return nil }

// signalGroup cannot address a process group off unix. Rather than quietly
// signaling only the leader and leaving children behind, it says so.
func signalGroup(int, syscall.Signal) error {
	return errors.New("signaling a process group is not supported on this platform")
}
