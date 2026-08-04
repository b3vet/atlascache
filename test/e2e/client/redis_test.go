package client_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/b3vet/atlascache/test/e2e/client"
	"github.com/b3vet/atlascache/test/e2e/runner"
)

// redisAddrEnv names a real Redis server to cross-check the client against.
//
//	docker run --rm -p 16379:6379 redis:7-alpine
//	ATLAS_E2E_REDIS_ADDR=127.0.0.1:16379 go test ./client/
//
// This is the check that makes the independence claim of [[ADR-0003]] real
// rather than nominal. The client is written from the RESP specification and
// shares nothing with AtlasCache, but "shares no code" only matters if the
// client is independently *right*. Driving the reference implementation of the
// protocol proves that: if the client can talk to Redis, its RESP is correct,
// and any disagreement with AtlasCache is AtlasCache's bug.
//
// It is opt-in because a machine without Docker should not fail the suite. Port
// 6379 is deliberately not the default: a developer machine very often has a
// real Redis on it already, and a test that quietly talked to it would be
// testing someone's data.
const redisAddrEnv = "ATLAS_E2E_REDIS_ADDR"

func dialRedis(t *testing.T) (*client.Client, context.Context) {
	t.Helper()

	addr := os.Getenv(redisAddrEnv)
	if addr == "" {
		t.Skipf("set %s to a real Redis to run the cross-check", redisAddrEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	conn, err := client.Dial(ctx, addr)
	if err != nil {
		t.Fatalf("dialing Redis at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if _, err := conn.Do(ctx, "FLUSHALL"); err != nil {
		t.Fatalf("FLUSHALL: %v", err)
	}
	return conn, ctx
}

func do(t *testing.T, conn *client.Client, ctx context.Context, cmd string) runner.Reply {
	t.Helper()
	reply, err := conn.Do(ctx, cmd)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return reply
}

// TestAgainstRealRedisRESP2 drives every RESP2 reply type through a real Redis.
func TestAgainstRealRedisRESP2(t *testing.T) {
	conn, ctx := dialRedis(t)

	tests := []struct {
		cmd  string
		kind runner.ReplyKind
		want string
	}{
		{"PING", runner.KindStatus, "PONG"},
		{"PING hello", runner.KindBulk, "hello"},
		{"SET k1 hello", runner.KindStatus, "OK"},
		{"GET k1", runner.KindBulk, "hello"},
		{"GET missing", runner.KindNil, "NIL"},
		{"EXISTS k1", runner.KindInteger, "1"},
		{"DEL k1", runner.KindInteger, "1"},
		{"GET k1", runner.KindNil, "NIL"},
		{"INCR counter", runner.KindInteger, "1"},
		{"INCRBY counter 41", runner.KindInteger, "42"},
		{`SET spaced "hello world"`, runner.KindStatus, "OK"},
		{"GET spaced", runner.KindBulk, "hello world"},
		{`SET empty ""`, runner.KindStatus, "OK"},
		{"GET empty", runner.KindBulk, ""},
		{"RPUSH list a b c", runner.KindInteger, "3"},
		{"LRANGE list 0 -1", runner.KindArray, "[a b c]"},
		{"LRANGE list 5 6", runner.KindArray, "[]"},
		{"TYPE list", runner.KindStatus, "list"},
		{"FROB", runner.KindError, ""},
		{"GET", runner.KindError, ""},
		{"INCR list", runner.KindError, ""},
	}

	for _, tc := range tests {
		reply := do(t, conn, ctx, tc.cmd)
		if reply.Kind != tc.kind {
			t.Errorf("%s: kind = %s, want %s (reply %q)", tc.cmd, reply.Kind, tc.kind, reply.String())
			continue
		}
		if tc.kind != runner.KindError && reply.String() != tc.want {
			t.Errorf("%s = %q, want %q", tc.cmd, reply.String(), tc.want)
		}
		if tc.kind == runner.KindError && !strings.HasPrefix(reply.Text, "ERR") && !strings.HasPrefix(reply.Text, "WRONGTYPE") {
			t.Errorf("%s: error text = %q", tc.cmd, reply.Text)
		}
	}
}

// TestAgainstRealRedisBinaryAndLargeValues checks the parts of the wire format
// that a naive line-oriented reader gets wrong: values containing NUL, CR, LF,
// and values larger than the read buffer.
func TestAgainstRealRedisBinaryAndLargeValues(t *testing.T) {
	conn, ctx := dialRedis(t)

	if reply := do(t, conn, ctx, `SET binary "a\x00b\r\nc"`); reply.String() != "OK" {
		t.Fatalf("SET binary = %q", reply.String())
	}
	reply := do(t, conn, ctx, "GET binary")
	if want := "a\x00b\r\nc"; reply.String() != want {
		t.Errorf("GET binary = %q, want %q", reply.String(), want)
	}
	if reply := do(t, conn, ctx, "STRLEN binary"); reply.String() != "6" {
		t.Errorf("STRLEN binary = %q, want 6", reply.String())
	}

	large := strings.Repeat("abcdefghij", 20000) // 200KB, past the read buffer
	if _, err := conn.Call(ctx, "SET", "large", large); err != nil {
		t.Fatalf("SET large: %v", err)
	}
	if reply := do(t, conn, ctx, "GET large"); reply.String() != large {
		t.Errorf("GET large returned %d bytes, want %d", len(reply.String()), len(large))
	}
}

// TestAgainstRealRedisRESP3 drives the RESP3 reply types, which no RESP2-only
// reader would decode: maps, sets, doubles, booleans, verbatim strings and the
// distinct null.
func TestAgainstRealRedisRESP3(t *testing.T) {
	conn, ctx := dialRedis(t)

	hello := do(t, conn, ctx, "HELLO 3")
	if hello.Kind != runner.KindMap {
		t.Fatalf("HELLO 3 = %s %q, want a map", hello.Kind, hello.String())
	}
	if version := hello.Fields()["proto"]; version != "3" {
		t.Fatalf("HELLO 3 reported proto %q; the rest of this test assumes RESP3", version)
	}

	if reply := do(t, conn, ctx, "GET missing"); reply.Kind != runner.KindNil {
		t.Errorf("RESP3 GET missing = %s %q, want nil", reply.Kind, reply.String())
	}

	do(t, conn, ctx, "ZADD scores 1.5 member")
	if reply := do(t, conn, ctx, "ZSCORE scores member"); reply.String() != "1.5" {
		t.Errorf("ZSCORE = %s %q, want 1.5", reply.Kind, reply.String())
	}

	do(t, conn, ctx, "SADD colors red green")
	if reply := do(t, conn, ctx, "SISMEMBER colors red"); reply.Kind != runner.KindInteger || reply.String() != "1" {
		t.Errorf("SISMEMBER = %s %q, want integer 1", reply.Kind, reply.String())
	}
	if reply := do(t, conn, ctx, "SISMEMBER colors blue"); reply.String() != "0" {
		t.Errorf("SISMEMBER absent = %q, want 0", reply.String())
	}
	if reply := do(t, conn, ctx, "SMEMBERS colors"); reply.Kind != runner.KindArray || len(reply.Items) != 2 {
		t.Errorf("SMEMBERS = %s %q, want a 2-element set", reply.Kind, reply.String())
	}

	if reply := do(t, conn, ctx, "CONFIG GET maxmemory"); reply.Fields()["maxmemory"] == "" {
		t.Errorf("CONFIG GET = %s %q, want a map holding maxmemory", reply.Kind, reply.String())
	}

	// LOLWUT is Redis's verbatim-string reply, which carries a format prefix
	// this client must strip.
	if reply := do(t, conn, ctx, "LOLWUT"); reply.Kind != runner.KindBulk || strings.HasPrefix(reply.Text, "txt:") {
		t.Errorf("LOLWUT = %s %q, want a verbatim string with its prefix removed", reply.Kind, reply.String())
	}

	if reply := do(t, conn, ctx, "FROB"); reply.Kind != runner.KindError {
		t.Errorf("RESP3 FROB = %s %q, want an error", reply.Kind, reply.String())
	}
}

// TestAgainstRealRedisClosesOnQuit checks the behavior the harness relies on
// when it reconnects: the server answers QUIT and then hangs up.
func TestAgainstRealRedisClosesOnQuit(t *testing.T) {
	conn, ctx := dialRedis(t)

	if reply := do(t, conn, ctx, "QUIT"); reply.String() != "OK" {
		t.Fatalf("QUIT = %q, want OK", reply.String())
	}
	if _, err := conn.Do(ctx, "PING"); err == nil {
		t.Error("PING after QUIT succeeded; the connection should be gone")
	} else if !client.IsConnError(err) {
		t.Errorf("PING after QUIT failed with %v, which is not a transport error", err)
	}
}
