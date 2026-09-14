package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/b3vet/atlascache/pkg/client"
)

// The exit codes, which are the part of this program a shell script reads
// (FEAT-0029). They are a contract: a script branches on them without parsing
// any text, and changing one silently changes what every such script does.
const (
	exitOK         = 0
	exitFailure    = 1
	exitUsage      = 2
	exitConnection = 3
)

// The stable "kind" tokens the --json error object carries. A message is prose
// and may be reworded; a kind is an identifier and is not.
const (
	kindUsage      = "usage"
	kindConnection = "connection"
	kindTimeout    = "timeout"
	kindAuth       = "auth"
	kindServer     = "server"
	kindProtocol   = "protocol"
	kindNotFound   = "not_found"
	kindClosed     = "closed"
	kindCanceled   = "canceled"
	kindError      = "error"
)

// cliError is a failure the CLI itself decided on — a bad argument, a key that
// is not there — as opposed to one the SDK reported.
type cliError struct {
	code int
	kind string
	err  error
}

func (e *cliError) Error() string { return e.err.Error() }
func (e *cliError) Unwrap() error { return e.err }

// usageErrorf reports a command line that cannot be run as given. Exit 2, so a
// script can tell "I typed this wrong" from "the server said no": retrying the
// first is pointless and retrying the second may not be.
func usageErrorf(format string, args ...any) error {
	return &cliError{code: exitUsage, kind: kindUsage, err: fmt.Errorf(format, args...)}
}

// notFoundError reports a key that is not in the cache.
//
// It is exit 1 rather than 0 because GET's reply cannot carry the distinction:
// a key holding an empty value and a key that does not exist both print
// nothing, and the server is careful to tell them apart, so the CLI has to be
// too. The exit code is the only channel left.
func notFoundError(key string) error {
	return &cliError{code: exitFailure, kind: kindNotFound, err: fmt.Errorf("key %q not found", key)}
}

// classify maps an error onto the exit code and the JSON kind that describe it.
//
// The SDK's six categories (P3 §4.2) are the input, and the four exit codes are
// the output, so the interesting part is where the mapping is not one to one:
//
//   - A timeout is a connection failure, not a command failure. The command may
//     have run, and the caller does not know; that is the same position an
//     unreachable server leaves them in, and it deserves the same retry.
//   - An authentication failure is a command failure, not a connection one. The
//     socket worked. Retrying the same token against the same server will fail
//     the same way, and every attempt is another line in the server's log.
//   - A protocol error is a command failure. The bytes arrived; they were not
//     something this client could read. A retry against whatever is listening
//     on that port will produce the same thing.
func classify(err error) (code int, kind string) {
	var cliErr *cliError
	if errors.As(err, &cliErr) {
		return cliErr.code, cliErr.kind
	}

	switch {
	case errors.Is(err, client.ErrNetwork):
		return exitConnection, kindConnection
	case errors.Is(err, client.ErrTimeout):
		return exitConnection, kindTimeout
	case errors.Is(err, client.ErrAuth):
		return exitFailure, kindAuth
	case errors.Is(err, client.ErrProtocol):
		return exitFailure, kindProtocol
	case errors.Is(err, client.ErrServer):
		return exitFailure, kindServer
	case errors.Is(err, client.ErrClosed):
		return exitFailure, kindClosed
	case errors.Is(err, context.DeadlineExceeded):
		return exitConnection, kindTimeout
	case errors.Is(err, context.Canceled):
		return exitFailure, kindCanceled
	default:
		return exitFailure, kindError
	}
}
