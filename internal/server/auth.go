package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"sync/atomic"
)

// DefaultUser is the only username `AUTH <username> <token>` accepts.
//
// There is one shared token and no user model behind it (ADR-0020). The name is
// accepted because modern client libraries send the two-argument form
// unconditionally with "default" in front; any other username is refused
// explicitly rather than ignored, so a deployment that thinks it has users
// finds out immediately.
const DefaultUser = "default"

// Failures the authenticator reports. They are distinct because the replies
// are: a server with no password set must say so rather than answer WRONGPASS,
// or a client misconfigured with a password gets a diagnostic that sends it
// hunting for the wrong problem.
var (
	// ErrAuthDisabled means AUTH was sent to a server with no token configured.
	ErrAuthDisabled = errors.New("no password is set")

	// ErrWrongPass means the username or the token did not match.
	ErrWrongPass = errors.New("invalid username-password pair")
)

// Authenticator holds the shared token, and answers whether a supplied one
// matches it.
//
// The token is replaceable while the server runs (`auth.token` is hot
// reloadable): a rotation changes what subsequent AUTH calls are checked
// against and leaves already-authenticated connections alone, which is what
// Redis does and what keeps a rotation from disconnecting every client at once.
//
// The zero value is a usable authenticator with auth disabled.
type Authenticator struct {
	state atomic.Pointer[authState]
}

// authState is the enabled flag and the token as one value, so a reload swaps
// both at once and no connection can observe a half-applied change.
//
// The token is stored as its SHA-256 digest rather than as itself. Comparing
// digests means the comparison is over two fixed-length values, so it reveals
// nothing about the length of the configured token — which a direct
// constant-time comparison of the raw bytes still would, because that
// comparison has to return early when the lengths differ. It also means the
// secret is not sitting in the process image in a form a careless log line or a
// core dump could print.
type authState struct {
	enabled bool
	digest  [sha256.Size]byte
}

// NewAuthenticator returns an authenticator for a token, enabled or not.
func NewAuthenticator(enabled bool, token string) *Authenticator {
	a := &Authenticator{}
	a.Set(enabled, token)
	return a
}

// Set replaces the token and the enabled flag, for config hot-reload.
//
// Connections that have already authenticated stay authenticated: their state
// is per-connection and this does not reach into it. Turning auth on affects
// every connection that has not authenticated yet, which is the safe direction
// for a change made while the server is running.
func (a *Authenticator) Set(enabled bool, token string) {
	a.state.Store(&authState{enabled: enabled, digest: sha256.Sum256([]byte(token))})
}

// Required reports whether a connection must authenticate before it may run
// commands beyond the pre-auth allowlist.
func (a *Authenticator) Required() bool {
	if a == nil {
		return false
	}
	state := a.state.Load()
	return state != nil && state.enabled
}

// Verify checks a username and token, returning nil when they authenticate.
//
// An empty username is the one-argument `AUTH <token>` form. Any username other
// than "default" fails without the token being examined at all, which is both
// correct and one less thing for a timing attack to work with.
func (a *Authenticator) Verify(username, token []byte) error {
	if !a.Required() {
		return ErrAuthDisabled
	}
	if len(username) > 0 && string(username) != DefaultUser {
		return ErrWrongPass
	}

	state := a.state.Load()
	supplied := sha256.Sum256(token)

	// subtle.ConstantTimeCompare, not ==: a byte-by-byte comparison that stops
	// at the first difference leaks the shared prefix through its timing, and
	// over enough attempts a prefix is a token. This is the one place in the
	// codebase where that is worth the care.
	if subtle.ConstantTimeCompare(supplied[:], state.digest[:]) != 1 {
		return ErrWrongPass
	}
	return nil
}
