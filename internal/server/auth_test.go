package server

import (
	"bytes"
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testToken is the shared secret the authenticated servers in this file use. It
// is deliberately long and distinctive so the "never logged" test can search
// for it without matching something else by accident.
const testToken = "s3cr3t-token-3f9a2b7c-do-not-log-me"

const (
	replyNoAuth    = "-NOAUTH Authentication required\r\n"
	replyWrongPass = "-WRONGPASS invalid username-password pair\r\n"
	replyOK        = "+OK\r\n"
)

// newAuthServer starts a server that requires authentication, and returns it
// alongside the buffer its log is written to.
func newAuthServer(t *testing.T) (*Server, *syncBuffer) {
	t.Helper()

	logs := &syncBuffer{}
	srv, err := New(context.Background(), "127.0.0.1:0", zerolog.New(logs), newFakeStore(),
		WithAuth(NewAuthenticator(true, testToken)))
	require.NoError(t, err)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()
	t.Cleanup(func() { shutdownServer(t, srv, serveErr) })

	return srv, logs
}

// syncBuffer is a zerolog sink a test can read while the server writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestAuthenticator covers the comparison itself, away from the wire.
func TestAuthenticator(t *testing.T) {
	t.Run("a disabled authenticator requires nothing and says so", func(t *testing.T) {
		auth := NewAuthenticator(false, "")
		assert.False(t, auth.Required())
		assert.ErrorIs(t, auth.Verify(nil, []byte("anything")), ErrAuthDisabled,
			"a passwordless server must report that, not WRONGPASS")
	})

	t.Run("a disabled authenticator with a token set still requires nothing", func(t *testing.T) {
		// auth.enabled is the switch; leaving a token in the file with the
		// feature off must not half-enable it.
		auth := NewAuthenticator(false, testToken)
		assert.False(t, auth.Required())
		assert.ErrorIs(t, auth.Verify(nil, []byte(testToken)), ErrAuthDisabled)
	})

	auth := NewAuthenticator(true, testToken)
	require.True(t, auth.Required())

	cases := []struct {
		name     string
		username string
		token    string
		want     error
	}{
		{"the one-argument form", "", testToken, nil},
		{"the default username", DefaultUser, testToken, nil},
		{"another username, right token", "admin", testToken, ErrWrongPass},
		{"another username, wrong token", "admin", "nope", ErrWrongPass},
		{"the default username, wrong token", DefaultUser, "nope", ErrWrongPass},
		{"an empty token", "", "", ErrWrongPass},
		{"a prefix of the token", "", testToken[:len(testToken)-1], ErrWrongPass},
		{"the token with a suffix", "", testToken + "x", ErrWrongPass},
		{"the username in the wrong case", "DEFAULT", testToken, ErrWrongPass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := auth.Verify([]byte(tc.username), []byte(tc.token))
			if tc.want == nil {
				assert.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, tc.want)
		})
	}

	t.Run("a rotation changes what later calls are checked against", func(t *testing.T) {
		rotating := NewAuthenticator(true, testToken)
		rotating.Set(true, "the-new-token")

		assert.ErrorIs(t, rotating.Verify(nil, []byte(testToken)), ErrWrongPass, "the old token stops working")
		assert.NoError(t, rotating.Verify(nil, []byte("the-new-token")))
	})

	t.Run("the zero value is a disabled authenticator", func(t *testing.T) {
		var zero Authenticator
		assert.False(t, zero.Required())
	})

	t.Run("a nil authenticator requires nothing", func(t *testing.T) {
		var nilAuth *Authenticator
		assert.False(t, nilAuth.Required())
	})
}

// TestAuthRequiredForEveryCommandOutsideTheAllowlist is the test FEAT-0022 is
// really about.
//
// It walks the dispatch table itself rather than a list written out by hand, so
// a command added later is covered the day it is added. The realistic failure
// this catches is not the gate being written wrongly today — it is a new
// command, six months from now, that quietly bypasses it. A hand-written list
// would not have that command in it, and would pass.
func TestAuthRequiredForEveryCommandOutsideTheAllowlist(t *testing.T) {
	srv, _ := newAuthServer(t)

	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	require.NotEmpty(t, names)

	gated := 0
	for _, name := range names {
		spec := commands[name]
		t.Run(name, func(t *testing.T) {
			// Enough arguments to clear the arity check, so a refusal is the
			// gate talking and not the argument count.
			args := append([]string{name}, filler(spec.minArgs)...)
			reply := exchange(t, srv, args...)

			if noAuthCommands[name] {
				assert.NotEqual(t, replyNoAuth, reply, "%s is on the pre-auth allowlist", name)
				return
			}
			gated++
			assert.Equal(t, replyNoAuth, reply,
				"%s ran on an unauthenticated connection; the dispatch gate does not cover it", name)
		})
	}

	assert.Equal(t, len(commands)-len(noAuthCommands), gated, "every command outside the allowlist was checked")

	t.Run("an unknown command is refused too, so the table is not a fingerprint", func(t *testing.T) {
		assert.Equal(t, replyNoAuth, exchange(t, srv, "NOSUCHCOMMAND"))
	})

	t.Run("the allowlist is exactly the four commands FEAT-0022 names", func(t *testing.T) {
		allowed := make([]string, 0, len(noAuthCommands))
		for name := range noAuthCommands {
			allowed = append(allowed, name)
		}
		sort.Strings(allowed)
		assert.Equal(t, []string{cmdAuth, cmdHello, cmdPing, cmdQuit}, allowed)
	})
}

// filler returns n placeholder arguments, enough to satisfy any command's
// minimum. They are valid where a command parses them: "1" is a legal count, a
// legal TTL and a legal key alike.
func filler(n int) []string {
	args := make([]string, n)
	for i := range args {
		args[i] = "1"
	}
	return args
}

// TestAuthAcceptsBothForms covers ADR-0020's table row by row.
func TestAuthAcceptsBothForms(t *testing.T) {
	srv, _ := newAuthServer(t)

	t.Run("AUTH <token>", func(t *testing.T) {
		assert.Equal(t, replyOK, exchange(t, srv, "AUTH", testToken))
	})

	t.Run("AUTH default <token>", func(t *testing.T) {
		assert.Equal(t, replyOK, exchange(t, srv, "AUTH", DefaultUser, testToken))
	})

	t.Run("a wrong token", func(t *testing.T) {
		assert.Equal(t, replyWrongPass, exchange(t, srv, "AUTH", "wrong"))
	})

	t.Run("a username that is not default, with the right token", func(t *testing.T) {
		// Accepting it would imply a user model that does not exist. Refusing
		// it explicitly is what tells the operator there is none.
		assert.Equal(t, replyWrongPass, exchange(t, srv, "AUTH", "admin", testToken))
	})

	t.Run("wrong arity", func(t *testing.T) {
		assert.Equal(t, "-ERR wrong number of arguments for 'auth' command\r\n",
			exchange(t, srv, "AUTH"))
		assert.Equal(t, "-ERR wrong number of arguments for 'auth' command\r\n",
			exchange(t, srv, "AUTH", DefaultUser, testToken, "extra"))
	})
}

// TestAuthUnlocksTheConnection checks that authentication actually opens the
// gate, and only for the connection that passed it.
func TestAuthUnlocksTheConnection(t *testing.T) {
	srv, _ := newAuthServer(t)

	conn, reader := dial(t, srv)

	send(t, conn, "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n")
	assert.Equal(t, replyNoAuth, readReply(t, conn, reader))

	send(t, conn, "*2\r\n$4\r\nAUTH\r\n$"+strconv.Itoa(len(testToken))+"\r\n"+testToken+"\r\n")
	assert.Equal(t, replyOK, readReply(t, conn, reader))

	send(t, conn, "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n")
	assert.Equal(t, replyOK, readReply(t, conn, reader))

	send(t, conn, "*2\r\n$3\r\nGET\r\n$1\r\nk\r\n")
	assert.Equal(t, "$1\r\n", readReply(t, conn, reader))
	assert.Equal(t, "v\r\n", readReply(t, conn, reader))

	t.Run("a second connection is still gated", func(t *testing.T) {
		// Auth state is per-connection. A shared flag would let one client
		// authenticate on behalf of everyone reaching the port.
		other, otherReader := dial(t, srv)
		send(t, other, "*2\r\n$3\r\nGET\r\n$1\r\nk\r\n")
		assert.Equal(t, replyNoAuth, readReply(t, other, otherReader))
	})

	t.Run("a failed attempt does not close the connection", func(t *testing.T) {
		other, otherReader := dial(t, srv)
		send(t, other, "*2\r\n$4\r\nAUTH\r\n$5\r\nwrong\r\n")
		assert.Equal(t, replyWrongPass, readReply(t, other, otherReader))

		// Still usable: a mistyped password is not grounds for a hang-up, and a
		// client that has to reconnect per attempt cannot report a clean error.
		send(t, other, "*1\r\n$4\r\nPING\r\n")
		assert.Equal(t, "+PONG\r\n", readReply(t, other, otherReader))
	})
}

// TestAuthAgainstPasswordlessServer covers the row that exists for the sake of
// a diagnostic: a client configured with a password, pointed at a server with
// none, must be told exactly that.
func TestAuthAgainstPasswordlessServer(t *testing.T) {
	srv, serveErr := newTestServer(t)
	defer shutdownServer(t, srv, serveErr)

	want := "-ERR Client sent AUTH, but no password is set\r\n"
	assert.Equal(t, want, exchange(t, srv, "AUTH", "anything"))
	assert.Equal(t, want, exchange(t, srv, "AUTH", DefaultUser, "anything"))

	// And nothing else is gated, which is the default configuration (ADR-0009).
	assert.Equal(t, replyOK, exchange(t, srv, "SET", "k", "v"))
}

// TestAuthHotReload checks the rotation contract: later authentications use the
// new token, and connections that already authenticated are left alone.
func TestAuthHotReload(t *testing.T) {
	auth := NewAuthenticator(true, testToken)

	srv, err := New(context.Background(), "127.0.0.1:0", zerolog.Nop(), newFakeStore(), WithAuth(auth))
	require.NoError(t, err)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()
	defer shutdownServer(t, srv, serveErr)

	conn, reader := dial(t, srv)
	send(t, conn, "*2\r\n$4\r\nAUTH\r\n$"+strconv.Itoa(len(testToken))+"\r\n"+testToken+"\r\n")
	require.Equal(t, replyOK, readReply(t, conn, reader))

	auth.Set(true, "rotated-token")

	send(t, conn, "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n")
	assert.Equal(t, replyOK, readReply(t, conn, reader),
		"a rotation must not disconnect or re-gate a connection that had authenticated")

	assert.Equal(t, replyWrongPass, exchange(t, srv, "AUTH", testToken), "the old token no longer authenticates")
	assert.Equal(t, replyOK, exchange(t, srv, "AUTH", "rotated-token"))
}

// TestTokenNeverReachesTheLog is the one nobody notices until an incident.
//
// The server log is the most-copied artifact in an outage: it goes into tickets,
// chat and bug reports. A token in it is a token disclosed, and a *failed*
// attempt is the dangerous one, because failures are what get logged verbosely
// and a failed attempt is usually a near-miss of the real secret.
func TestTokenNeverReachesTheLog(t *testing.T) {
	srv, logs := newAuthServer(t)

	require.Equal(t, replyWrongPass, exchange(t, srv, "AUTH", testToken+"-typo"))
	require.Equal(t, replyWrongPass, exchange(t, srv, "AUTH", "admin", testToken))
	require.Equal(t, replyOK, exchange(t, srv, "AUTH", testToken))
	require.Equal(t, "*12\r\n", exchange(t, srv, "HELLO", "2", "AUTH", DefaultUser, testToken))

	written := logs.String()
	assert.NotContains(t, written, testToken, "the configured token reached the log")
	assert.NotContains(t, written, testToken+"-typo", "a supplied token reached the log")

	// The log is not empty of the useful part: the outcome is recorded.
	assert.Contains(t, written, "authentication failed")
	assert.Contains(t, written, "client authenticated")
	assert.Contains(t, written, "remote_addr")
}

// TestHelloAuthenticates covers the combined negotiation clients use to save a
// round trip, and the reason HELLO is on the pre-auth allowlist at all.
func TestHelloAuthenticates(t *testing.T) {
	srv, _ := newAuthServer(t)

	t.Run("HELLO 2 AUTH authenticates and answers the properties map", func(t *testing.T) {
		conn, reader := dial(t, srv)

		hello := "*5\r\n$5\r\nHELLO\r\n$1\r\n2\r\n$4\r\nAUTH\r\n$7\r\ndefault\r\n" +
			"$" + strconv.Itoa(len(testToken)) + "\r\n" + testToken + "\r\n"
		send(t, conn, hello)
		assert.Equal(t, "*12\r\n", readReply(t, conn, reader), "the properties map, not +OK")

		// Drain the map, then check the connection is authenticated.
		for i := 0; i < 12; i++ {
			line := readReply(t, conn, reader)
			if strings.HasPrefix(line, "$") {
				readReply(t, conn, reader)
			}
		}

		send(t, conn, "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n")
		assert.Equal(t, replyOK, readReply(t, conn, reader))
	})

	t.Run("a wrong token in HELLO is WRONGPASS and leaves the connection gated", func(t *testing.T) {
		assert.Equal(t, replyWrongPass, exchange(t, srv, "HELLO", "2", "AUTH", DefaultUser, "wrong"))

		conn, reader := dial(t, srv)
		send(t, conn, "*5\r\n$5\r\nHELLO\r\n$1\r\n2\r\n$4\r\nAUTH\r\n$7\r\ndefault\r\n$5\r\nwrong\r\n")
		require.Equal(t, replyWrongPass, readReply(t, conn, reader))

		send(t, conn, "*2\r\n$3\r\nGET\r\n$1\r\nk\r\n")
		assert.Equal(t, replyNoAuth, readReply(t, conn, reader))
	})

	t.Run("a bare HELLO is still allowed before authenticating", func(t *testing.T) {
		assert.Equal(t, "*12\r\n", exchange(t, srv, "HELLO"))
	})

	t.Run("HELLO 3 AUTH is still NOPROTO", func(t *testing.T) {
		// The version is settled before the options are read: a client probing
		// for RESP3 needs the reply it knows how to fall back from, whether or
		// not its credentials were right.
		assert.Equal(t, "-NOPROTO unsupported protocol version\r\n",
			exchange(t, srv, "HELLO", "3", "AUTH", DefaultUser, testToken))
	})
}
