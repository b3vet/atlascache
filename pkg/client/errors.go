package client

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
)

// The seven error categories every failure belongs to (P3 §4.2, plus
// [ErrConfig]).
//
// They are sentinels rather than types so that `errors.Is` answers the question
// a caller actually has — "may I retry this?" — without matching on strings or
// on a type that would have to be exported and then kept stable forever.
//
// Every error the SDK returns wraps exactly one of them, except cancellation:
// a call canceled by its own caller belongs to no category and is reported by
// wrapping context.Canceled, because the caller already knows what happened.
var (
	// ErrNetwork is a dial, read or write failure. Retryable.
	ErrNetwork = errors.New("network failure")

	// ErrTimeout is a deadline that expired — the caller's context, or one of
	// the configured dial, read and write timeouts. Retryable.
	ErrTimeout = errors.New("deadline exceeded")

	// ErrProtocol is a reply the SDK could not decode, or one whose shape did
	// not match the conversion asked of it. Not retryable: a connection that
	// has seen one is at an unknown stream position and is discarded.
	ErrProtocol = errors.New("protocol error")

	// ErrServer is an error reply from the server — `-ERR`, `-OOM`, and every
	// other kind but the authentication ones. Not retryable: the command
	// reached the server and the server refused it.
	ErrServer = errors.New("server error")

	// ErrAuth is `-NOAUTH` or `-WRONGPASS`, wherever it surfaces: the handshake
	// on a new connection, or a command on a connection that never
	// authenticated. Not retryable.
	ErrAuth = errors.New("authentication failed")

	// ErrClosed is a call on a client whose Close has already run. Not
	// retryable, and never transient: it is a bug in the caller's lifecycle.
	ErrClosed = errors.New("client is closed")

	// ErrConfig is a malformed option, or an argument a command cannot be built
	// from — anything the SDK refused before it reached the wire. Not
	// retryable: nothing about the server, the network or the passage of time
	// changes the answer.
	//
	// It is the category of every error [New] returns, which is what lets a
	// caller tell "I was configured wrong" from "the server is unhappy"
	// without reaching into [Error] (ISSUE-0023).
	ErrConfig = errors.New("configuration error")
)

// ErrAuthDisabled is the detail under an [ErrAuth] raised by a server that has
// authentication turned off: it answers AUTH with an error because there is no
// password to check, not because the token was wrong.
//
// It is not a seventh category — an error carrying it is an [ErrAuth], and
// `errors.Is` matches both — because what a caller does about it is what they
// do about any authentication failure. It exists so that the one case where the
// caller's credential is *not* the problem can be recognized in code rather
// than by reading the message, and so a program can point its operator at the
// server's configuration instead of at their token.
var ErrAuthDisabled = errors.New("the server has authentication disabled")

// Error is the single concrete error type the SDK returns.
//
// One type rather than seven means `errors.As` always yields the detail — the
// operation, the address, the server's error kind — while `errors.Is` against
// the sentinels above answers the category question. Seven types would have
// made the category check a type switch, which is exactly the API ADR-0022
// rejected.
//
// Reach for it when the category is not enough — to log which server failed, or
// to read the kind the server put on a rejection:
//
//	var atlasErr *client.Error
//	if errors.As(err, &atlasErr) && atlasErr.Kind == "OOM" {
//		// the server is out of memory, not merely unhappy with the command
//	}
type Error struct {
	// Category is one of the seven sentinels above, or nil for a canceled call.
	Category error

	// Op is what was being attempted: a command name ("GET"), or a stage of
	// the connection's life ("dial", "handshake", "pool").
	Op string

	// Addr is the server the call was made against, when it is known.
	Addr string

	// Kind is the error kind a server reply carried — "ERR", "WRONGPASS",
	// "OOM", "NOPROTO". Empty for everything that did not come off the wire.
	Kind string

	// Message is the human-readable detail. For a server reply it is the
	// server's own text, kind included, verbatim — except where the SDK can
	// say something the server's wording does not, in which case the server's
	// text is quoted inside it and Kind still carries the kind it sent. The
	// passwordless-server case under [ErrAuthDisabled] is the only one that
	// does this today.
	Message string

	// Err is the underlying cause, when there is one: a net.Error, an
	// os.ErrDeadlineExceeded, a context error. It is part of the Is/As chain.
	Err error
}

// Error renders the failure as "atlascache: <op> <addr>: <detail>", for
// example:
//
//	atlascache: GET cache.internal:6379: ERR value is not an integer
//	atlascache: dial 127.0.0.1:6379: connect: connection refused
//
// The text is for a human and for a log line. It is not an interface: classify
// with errors.Is against a category sentinel, or read the fields with
// errors.As, and never by matching on this string.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("atlascache")
	if e.Op != "" {
		b.WriteString(": ")
		b.WriteString(e.Op)
	}
	if e.Addr != "" {
		b.WriteString(" ")
		b.WriteString(e.Addr)
	}
	b.WriteString(": ")
	switch {
	case e.Message != "":
		b.WriteString(e.Message)
	case e.Err != nil:
		b.WriteString(e.Err.Error())
	case e.Category != nil:
		b.WriteString(e.Category.Error())
	default:
		b.WriteString("unknown failure")
	}
	return b.String()
}

// Unwrap exposes both the category and the cause, so that
// `errors.Is(err, ErrNetwork)` and `errors.Is(err, context.Canceled)` are both
// answerable from the same error.
func (e *Error) Unwrap() []error {
	switch {
	case e.Category != nil && e.Err != nil:
		return []error{e.Category, e.Err}
	case e.Category != nil:
		return []error{e.Category}
	case e.Err != nil:
		return []error{e.Err}
	default:
		return nil
	}
}

// Retryable reports whether an error is one a retry could plausibly fix.
//
// It is the table in P3 §4.2 in one function:
//
//	[ErrNetwork]   retryable    the dial, the write or the read failed
//	[ErrTimeout]   retryable    a deadline expired, the caller's or a configured one
//	[ErrProtocol]  no           the reply could not be decoded; the connection is discarded
//	[ErrServer]    no           the command reached the server and was refused
//	[ErrAuth]      no           the token is wrong, or the server wants one
//	[ErrClosed]    no           Close has already run; a caller lifecycle bug
//	[ErrConfig]    no           an option or an argument the SDK refused
//	cancellation   no           the caller canceled it, so the caller decides
//
// It exists so that a caller never has to rebuild that table by matching on
// error strings, which is the failure mode a single opaque error type produces.
//
// It says nothing about whether the *command* is safe to retry — that is the
// caller's judgment, and the reason retries are opt-in per call rather than
// automatic (FEAT-0028). [Client.WithRetries] applies both tests: this one, and
// whether the command's reply survives being asked for twice.
func Retryable(err error) bool {
	return errors.Is(err, ErrNetwork) || errors.Is(err, ErrTimeout)
}

// newError is the one place an *Error is built, so no path can invent a
// category the table above does not list.
func newError(category error, op, addr, message string, cause error) *Error {
	return &Error{Category: category, Op: op, Addr: addr, Message: message, Err: cause}
}

// wireError classifies a failed socket operation.
//
// A deadline that expired is a timeout wherever it came from — the caller's
// context, or the configured read and write timeouts — because the caller's
// question is "was this slow or was it broken?", not which clock fired.
func wireError(op, addr string, err error) *Error {
	switch {
	case err == nil:
		return nil

	case errors.Is(err, context.Canceled):
		return newError(nil, op, addr, "canceled", err)

	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return newError(ErrTimeout, op, addr, "", err)

	default:
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return newError(ErrTimeout, op, addr, "", err)
		}
		return newError(ErrNetwork, op, addr, "", err)
	}
}

// contextError renders a context that expired while the SDK was blocked on
// something other than a socket — the pool, or a backoff delay.
func contextError(op, addr string, err error) *Error {
	if errors.Is(err, context.Canceled) {
		return newError(nil, op, addr, "canceled", err)
	}
	return newError(ErrTimeout, op, addr, "", err)
}

// protocolError is a reply the SDK could not make sense of. Every one of these
// also costs the connection it arrived on: see conn.exchange.
func protocolError(op, addr, message string) *Error {
	return newError(ErrProtocol, op, addr, message, nil)
}

func closedError(op string) *Error {
	return newError(ErrClosed, op, "", "", nil)
}

// configError is something the SDK refused before it reached the wire: a
// malformed option caught by New, or a command that cannot be built from the
// arguments it was given.
//
// op is what the caller was doing — "New" for an option, the command name for
// an argument — because "atlascache: New: DEL needs at least one key" names the
// wrong call, and the name of the call is the first thing a reader looks for.
//
// There is no address: nothing was dialed, and naming a server would suggest
// the server had an opinion about it.
func configError(op, message string) *Error {
	return newError(ErrConfig, op, "", message, nil)
}

// authDisabledPrefix is how a server that has authentication turned off begins
// its refusal of AUTH. It is matched as a prefix rather than in full because
// the rest of the line is not load-bearing and does move: Redis's own wording
// continues ". Did you mean AUTH <username> <password>?", and AtlasCache is
// converging on it (ISSUE-0020). A prefix survives that; an equality check
// would turn a message change into a silent loss of this diagnosis.
const authDisabledPrefix = "Client sent AUTH, but no password is set"

// authFailure classifies a server's refusal of AUTH.
//
// Every refusal is an [ErrAuth], whatever kind the server put on it — a caller
// who pointed a token at the wrong server needs to hear "authentication", not
// "server error". The one worth telling apart is a server with authentication
// disabled: it refuses every AUTH because it has no password to check, so the
// message has to point at the server's configuration rather than at a token
// that is not the problem (ISSUE-0023).
func authFailure(addr string, reply Reply) *Error {
	err := &Error{
		Category: ErrAuth,
		Op:       opAuth,
		Addr:     addr,
		Kind:     reply.Kind,
		Message:  string(reply.Str),
	}

	detail := strings.TrimSpace(strings.TrimPrefix(string(reply.Str), reply.Kind))
	if strings.HasPrefix(detail, authDisabledPrefix) {
		err.Err = ErrAuthDisabled
		err.Message = "this server has authentication disabled and refuses every AUTH, so the token is not the problem: " +
			"drop it, or enable auth on the server (the server said " + strconv.Quote(string(reply.Str)) + ")"
	}
	return err
}

// The server error kinds that mean "authentication", as opposed to "that
// command was wrong".
const (
	kindNoAuth    = "NOAUTH"
	kindWrongPass = "WRONGPASS"
	kindNoPerm    = "NOPERM"
)

var authErrorKinds = map[string]bool{
	kindNoAuth:    true,
	kindWrongPass: true,
	kindNoPerm:    true,
}

// replyError converts a server error reply into the category a caller can act
// on. The distinction that matters is authentication versus everything else:
// one is fixed by configuration and the other is not.
func replyError(op, addr string, reply Reply) *Error {
	category := ErrServer
	if authErrorKinds[reply.Kind] {
		category = ErrAuth
	}
	return &Error{
		Category: category,
		Op:       op,
		Addr:     addr,
		Kind:     reply.Kind,
		Message:  string(reply.Str),
	}
}
