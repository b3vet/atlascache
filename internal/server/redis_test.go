package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Differential testing of the replies AtlasCache claims are Redis's.
//
// ADR-0006 makes byte-for-byte compatibility the premise of speaking RESP at
// all, and a string copied from a document is not a measurement — ISSUE-0020
// found three that had drifted, and correcting them from the issue's own table
// would have introduced a fourth, because Redis 7 had changed one of them
// again. So the strings are asked of a real server here, and the answers are
// compared with what this package sends.
//
// The tests are skipped unless the environment names a Redis to compare
// against, because CI cannot assume one. Two are needed: an open one for
// CONFIG, and one with a password for the authentication errors.
//
//	docker run -d --name atlascache-diff-redis -p 6399:6379 redis:7-alpine
//	docker run -d --name atlascache-diff-redis-auth -p 6398:6379 redis:7-alpine \
//	    redis-server --requirepass diffsecret
//
//	ATLASCACHE_REDIS_ADDR=127.0.0.1:6399 \
//	ATLASCACHE_REDIS_AUTH_ADDR=127.0.0.1:6398 \
//	ATLASCACHE_REDIS_AUTH_TOKEN=diffsecret \
//	    go test ./internal/server -run Redis -v
const (
	redisAddrEnv      = "ATLASCACHE_REDIS_ADDR"
	redisAuthAddrEnv  = "ATLASCACHE_REDIS_AUTH_ADDR"
	redisAuthTokenEnv = "ATLASCACHE_REDIS_AUTH_TOKEN"
)

// askRedis sends one command on a fresh connection and renders the reply in the
// same text form readFullReply produces for AtlasCache, so the two can be
// compared as strings.
//
// A fresh connection per command is not laziness: every authentication
// assertion here is about what an *unauthenticated* connection is told, and a
// reused connection would carry the state of the command before it.
func askRedis(t *testing.T, addr string, args ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	require.NoError(t, err, "dial %s", addr)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))

	var request strings.Builder
	fmt.Fprintf(&request, "*%d\r\n", len(args))
	for _, arg := range args {
		fmt.Fprintf(&request, "$%d\r\n%s\r\n", len(arg), arg)
	}
	_, err = conn.Write([]byte(request.String()))
	require.NoError(t, err)

	return readRedisReply(t, bufio.NewReader(conn))
}

// readRedisReply decodes one RESP2 reply into the text form the AtlasCache-side
// helpers use. It shares no code with the codec under test, for the reason
// ADR-0003 gives about the E2E client: a shared misreading would make both
// sides agree and prove nothing.
func readRedisReply(t *testing.T, r *bufio.Reader) string {
	t.Helper()

	line, err := r.ReadString('\n')
	require.NoError(t, err)
	line = strings.TrimRight(line, "\r\n")
	require.NotEmpty(t, line)

	switch line[0] {
	case '+', '-', ':':
		return line

	case '$':
		size, err := strconv.Atoi(line[1:])
		require.NoError(t, err)
		if size < 0 {
			return "NIL"
		}
		buf := make([]byte, size+2)
		_, err = io.ReadFull(r, buf)
		require.NoError(t, err)
		return "$" + string(buf[:size])

	case '*':
		count, err := strconv.Atoi(line[1:])
		require.NoError(t, err)
		items := make([]string, 0, count)
		for i := 0; i < count; i++ {
			items = append(items, readRedisReply(t, r))
		}
		return "[" + strings.Join(items, " ") + "]"
	}

	t.Fatalf("unexpected reply frame %q", line)
	return ""
}

func redisAddr(t *testing.T, key string) string {
	t.Helper()

	addr := os.Getenv(key)
	if addr == "" {
		t.Skipf("set %s to a throwaway Redis to run the differential comparison", key)
	}
	return addr
}

// TestAuthErrorsMatchRealRedis is ISSUE-0020's third checkbox. Every string
// here was wrong by a full stop or a clause before it was measured.
func TestAuthErrorsMatchRealRedis(t *testing.T) {
	addr := redisAddr(t, redisAuthAddrEnv)
	token := os.Getenv(redisAuthTokenEnv)
	require.NotEmpty(t, token, "%s names the password %s was started with", redisAuthTokenEnv, redisAuthAddrEnv)

	t.Run("a gated command", func(t *testing.T) {
		assert.Equal(t, askRedis(t, addr, "GET", "k"), replyErrorText(errNoAuth()))
	})

	t.Run("a wrong password", func(t *testing.T) {
		assert.Equal(t, askRedis(t, addr, "AUTH", "not-the-password"), replyErrorText(errWrongPass()))
		assert.Equal(t, askRedis(t, addr, "AUTH", "someone", token), replyErrorText(errWrongPass()))
	})

	t.Run("a bare HELLO", func(t *testing.T) {
		// The properties map names the server, its version, its mode and its
		// role. Redis does not disclose it before authentication, and this is
		// the sentence it sends instead.
		assert.Equal(t, askRedis(t, addr, "HELLO"), replyErrorText(errNoAuthHello()))
		assert.Equal(t, askRedis(t, addr, "HELLO", "2"), replyErrorText(errNoAuthHello()))
	})

	t.Run("a wrong password inside HELLO", func(t *testing.T) {
		assert.Equal(t, askRedis(t, addr, "HELLO", "2", "AUTH", "default", "wrong"),
			replyErrorText(errWrongPass()))
	})

	t.Run("an unsupported protocol version outranks the auth check", func(t *testing.T) {
		// This is the ordering AtlasCache copies for HELLO 3. Redis speaks
		// RESP3, so the version it rejects is 4 — and it rejects it before
		// asking whether the client has authenticated, which is what makes
		// -NOPROTO the faithful answer to an unauthenticated HELLO 3 here.
		assert.Equal(t, "-NOPROTO unsupported protocol version", askRedis(t, addr, "HELLO", "4"))
	})
}

// TestAuthDisabledErrorDivergesFromRealRedisDeliberately pins the one string
// that is not Redis 7's.
//
// Redis 6 sent what AtlasCache sends. Redis 7 replaced it with a sentence that
// shares no prefix with it, and the Go SDK classifies this condition by the old
// prefix (pkg/client/errors.go) to produce "you authenticated against a server
// with no password set" rather than a bare protocol error. Following Redis 7
// here would silently break that diagnostic, so the divergence is deliberate,
// documented in docs/protocol.md, and asserted rather than left to be
// rediscovered.
func TestAuthDisabledErrorDivergesFromRealRedisDeliberately(t *testing.T) {
	addr := redisAddr(t, redisAddrEnv)

	const redis7 = "-ERR AUTH <password> called without any password configured for the default user. " +
		"Are you sure your configuration is correct?"
	assert.Equal(t, redis7, askRedis(t, addr, "AUTH", "anything"),
		"Redis changed this string again; revisit ISSUE-0020 before following it")

	srv, serveErr := newTestServer(t)
	defer shutdownServer(t, srv, serveErr)

	ours := strings.TrimSuffix(exchange(t, srv, "AUTH", "anything"), "\r\n")
	assert.Equal(t, "-ERR Client sent AUTH, but no password is set. "+
		"Did you mean AUTH <username> <password>?", ours)
	assert.Contains(t, ours, "Client sent AUTH, but no password is set",
		"the Go SDK classifies this condition by that prefix")
}

// TestConfigGetMatchesRealRedis covers ISSUE-0021's last checkbox: the
// parameters both servers have must answer in the same shape, and the errors
// around CONFIG must be the same bytes.
func TestConfigGetMatchesRealRedis(t *testing.T) {
	addr := redisAddr(t, redisAddrEnv)

	srv, _ := newConfigServer(t)
	ask := conversation(t, srv)

	t.Run("shared parameters answer in the same shape", func(t *testing.T) {
		// The values differ — they are two differently configured servers —
		// so what is compared is the shape a client parses: the name echoed
		// back, and a decimal string beside it.
		for _, name := range []string{"maxmemory", "maxclients", "timeout"} {
			theirs := askRedis(t, addr, "CONFIG", "GET", name)
			ours := ask("CONFIG", "GET", name)

			assert.Truef(t, strings.HasPrefix(theirs, "[$"+name+" $"), "redis answered %s with %s", name, theirs)
			assert.Truef(t, strings.HasPrefix(ours, "[$"+name+" $"), "atlascache answered %s with %s", name, ours)
			assertDecimalValue(t, name, theirs)
			assertDecimalValue(t, name, ours)
		}
	})

	t.Run("a parameter neither has is empty on both", func(t *testing.T) {
		assert.Equal(t, "[]", askRedis(t, addr, "CONFIG", "GET", "no-such-parameter"))
		assert.Equal(t, "[]", ask("CONFIG", "GET", "no-such-parameter"))
	})

	t.Run("a name is echoed in the spelling the client used", func(t *testing.T) {
		assert.True(t, strings.HasPrefix(askRedis(t, addr, "CONFIG", "GET", "MAXMEMORY"), "[$MAXMEMORY "))
		assert.True(t, strings.HasPrefix(ask("CONFIG", "GET", "MAXMEMORY"), "[$MAXMEMORY "))
	})

	t.Run("a malformed pattern is answered on both, never refused", func(t *testing.T) {
		assert.Equal(t, "[]", askRedis(t, addr, "CONFIG", "GET", "[a"))
		assert.Equal(t, "[]", ask("CONFIG", "GET", "[a"))
	})

	t.Run("the errors around CONFIG are the same bytes", func(t *testing.T) {
		for _, args := range [][]string{
			{"CONFIG"},
			{"CONFIG", "GET"},
			{"CONFIG", "NOSUCHSUB"},
		} {
			assert.Equalf(t, askRedis(t, addr, args...), ask(args...), "%v", args)
		}
	})

	t.Run("save and appendonly are present on both, and empty here", func(t *testing.T) {
		// Redis reports its save points and its AOF setting; AtlasCache has
		// neither and says so with an empty value rather than by being absent,
		// which is what keeps redis-benchmark from warning (ISSUE-0021).
		for _, name := range []string{"save", "appendonly"} {
			assert.Truef(t, strings.HasPrefix(askRedis(t, addr, "CONFIG", "GET", name), "[$"+name+" "),
				"redis must report %s as a pair", name)
			assert.Equalf(t, "[$"+name+" $]", ask("CONFIG", "GET", name),
				"%s must be present and empty, never fabricated", name)
		}
	})
}

// assertDecimalValue checks the second element of a CONFIG GET pair parses as
// an unsigned decimal, which is the format Redis reports these parameters in
// and the one every dashboard reads them with.
func assertDecimalValue(t *testing.T, name, reply string) {
	t.Helper()

	_, value, found := strings.Cut(strings.TrimSuffix(strings.TrimPrefix(reply, "[$"), "]"), " $")
	require.Truef(t, found, "%s: %s is not a name/value pair", name, reply)
	_, err := strconv.ParseUint(value, 10, 64)
	assert.NoErrorf(t, err, "%s reported %q, which is not a decimal", name, value)
}

// replyErrorText renders one of this package's error replies the way the wire
// carries it, so it can be compared with what a real Redis sent.
func replyErrorText(reply any) string {
	type errorReply interface{ Error() string }
	if err, ok := reply.(errorReply); ok {
		return "-" + err.Error()
	}
	return fmt.Sprintf("%v", reply)
}
