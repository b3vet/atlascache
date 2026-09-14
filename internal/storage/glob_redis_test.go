package storage

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Differential testing of the glob matcher against a real Redis.
//
// The matcher is a port, and the only honest way to check a port is to ask the
// original. This test runs generated patterns against generated keys on both
// implementations and diffs the answers. It is what the table in glob_test.go
// was derived from, and it is how a future edit to the matcher gets checked
// rather than merely reviewed.
//
// It is skipped unless ATLASCACHE_REDIS_ADDR names a Redis to compare against,
// because CI cannot assume one:
//
//	docker run -d --name atlascache-diff-redis -p 6399:6379 redis:7-alpine
//	ATLASCACHE_REDIS_ADDR=127.0.0.1:6399 go test ./internal/storage -run Redis -v
//
// The Redis it points at is flushed repeatedly. Do not point it at anything
// that matters.
const redisAddrEnv = "ATLASCACHE_REDIS_ADDR"

// redisConn is the smallest RESP client that can drive KEYS: connect, send one
// command, read one reply. It shares no code with the server's codec, for the
// same reason the E2E suite's client does not (ADR-0003) — a shared misreading
// would make both sides agree and prove nothing.
type redisConn struct {
	conn net.Conn
	r    *bufio.Reader
}

func dialRedis(t *testing.T) *redisConn {
	t.Helper()

	addr := os.Getenv(redisAddrEnv)
	if addr == "" {
		t.Skipf("set %s to a throwaway Redis to run the differential comparison", redisAddrEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	require.NoError(t, err, "dial %s", addr)
	t.Cleanup(func() { _ = conn.Close() })

	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Minute)))

	return &redisConn{conn: conn, r: bufio.NewReader(conn)}
}

// do sends one command and returns the reply as a flat list of strings: a bulk
// or status reply is one element, an array is its elements, a nil is empty.
// That is all KEYS, SET and FLUSHALL need between them.
func (c *redisConn) do(t *testing.T, args ...string) []string {
	t.Helper()

	var req strings.Builder
	fmt.Fprintf(&req, "*%d\r\n", len(args))
	for _, arg := range args {
		fmt.Fprintf(&req, "$%d\r\n%s\r\n", len(arg), arg)
	}
	_, err := c.conn.Write([]byte(req.String()))
	require.NoError(t, err)

	return c.readReply(t)
}

func (c *redisConn) readReply(t *testing.T) []string {
	t.Helper()

	line, err := c.r.ReadString('\n')
	require.NoError(t, err)
	line = strings.TrimRight(line, "\r\n")
	require.NotEmpty(t, line)

	switch line[0] {
	case '+', ':':
		return []string{line[1:]}
	case '-':
		t.Fatalf("redis replied with an error: %s", line[1:])
	case '$':
		n, convErr := strconv.Atoi(line[1:])
		require.NoError(t, convErr)
		if n < 0 {
			return nil
		}
		buf := make([]byte, n+2)
		_, err = io.ReadFull(c.r, buf)
		require.NoError(t, err)
		return []string{string(buf[:n])}
	case '*':
		n, convErr := strconv.Atoi(line[1:])
		require.NoError(t, convErr)
		if n < 0 {
			return nil
		}
		out := make([]string, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, c.readReply(t)...)
		}
		return out
	}

	t.Fatalf("unexpected reply frame %q", line)
	return nil
}

// TestGlobMatchesRealRedis is the differential run. Every pattern is asked of
// every key on both sides, and every disagreement is reported rather than only
// the first, so one run says how wrong the matcher is rather than merely that
// it is.
func TestGlobMatchesRealRedis(t *testing.T) {
	redis := dialRedis(t)

	keys := differentialKeys()
	patterns := differentialPatterns()

	t.Logf("comparing %d patterns against %d keys (%d cases)", len(patterns), len(keys), len(patterns)*len(keys))

	// One key resident at a time, so KEYS answers the question "does this
	// pattern match this key" and nothing else.
	var mismatches int
	for _, key := range keys {
		redis.do(t, "FLUSHALL")
		redis.do(t, "SET", key, "v")

		for _, pattern := range patterns {
			want := len(redis.do(t, "KEYS", pattern)) == 1
			got := MatchesEveryKey(pattern) || MatchGlob(pattern, key)

			if want != got && !assert.Equalf(t, want, got, "pattern %q against key %q", pattern, key) {
				mismatches++
			}
		}
	}
	assert.Zero(t, mismatches, "the matcher disagreed with Redis %d times", mismatches)
}

// differentialKeys are the subjects. The list is deliberately full of the
// characters a pattern can interpret — brackets, backslashes, stars, dashes,
// carets — because those are where a port drifts, plus the separators real
// cache keys are made of.
func differentialKeys() []string {
	fixed := []string{
		"", "a", "b", "z", "ab", "abc", "aXc",
		"user:1", "user:12", "user:1:session",
		"cdn/eu/asset.png", "a/b/c", "//", "/",
		"a[b", "a]b", "a[bc]d", "[abc]",
		`a\b`, `\`, `a\`,
		"a*b", "*", "**", "a?b", "?",
		"a-c", "-", "^abc", "^",
		"A", "Z", "0", "9", "0-9",
		" ", "a b", "\t", "é", "ü",
		strings.Repeat("a", 40),
	}

	// A handful of random byte strings over the interesting alphabet, so the
	// run is not limited to the cases a human thought of.
	rng := rand.New(rand.NewSource(20260914))
	alphabet := []byte(`abcz019:/*?[]\-^. `)
	for i := 0; i < 60; i++ {
		length := 1 + rng.Intn(8)
		buf := make([]byte, length)
		for j := range buf {
			buf[j] = alphabet[rng.Intn(len(alphabet))]
		}
		fixed = append(fixed, string(buf))
	}

	return dedupe(fixed)
}

// differentialPatterns are the questions. Every construct the matcher claims to
// support appears, along with the malformed spellings Redis tolerates rather
// than rejects — an unterminated class, a trailing backslash, an inverted
// range — since those are precisely what filepath.Match got wrong.
func differentialPatterns() []string {
	fixed := []string{
		"*", "**", "***", "?", "??", "",
		"a", "abc", "a*", "*a", "*a*", "a*c", "a?c", "?bc", "ab?",
		"user:*", "user:?", "user:1*", "*:1",
		"*/*", "a/*", "*/c", "a/*/c", "cdn/*/*.png", "*.png",
		"[abc]", "[a-c]", "[^abc]", "[^a-c]", "[c-a]", "[]", "[^]",
		"[abc", "[a-c", "[^abc", "a[b", "*a[b", "a[b*", "*a[b*",
		"[a-]", "[-a]", "[a\\]b]", "[\\]]", "[\\\\]",
		"\\*", "\\?", "\\[", "\\\\", "a\\", "\\",
		"a\\*b", "a\\?b", "*\\**",
		"[0-9]*", "*[0-9]", "[A-Za-z]*", "[^/]*", "*[^/]",
		"a[bc]d", "[abc]*", "*[abc]*",
		"z*", "*z", "nothing*",
	}

	rng := rand.New(rand.NewSource(514229))
	alphabet := []byte(`abcz09:/*?[]\-^`)
	for i := 0; i < 120; i++ {
		length := 1 + rng.Intn(6)
		buf := make([]byte, length)
		for j := range buf {
			buf[j] = alphabet[rng.Intn(len(alphabet))]
		}
		fixed = append(fixed, string(buf))
	}

	return dedupe(fixed)
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
