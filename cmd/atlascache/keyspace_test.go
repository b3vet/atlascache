package main

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b3vet/atlascache/internal/server"
	"github.com/b3vet/atlascache/internal/storage"
)

// TestKeyspaceTranslatesEngineFailures pins the translation the server's replies
// are built from. Getting it wrong is invisible in a unit test of either side:
// the engine still returns its error and the server still returns an error, and
// only the wording the client sees is wrong.
func TestKeyspaceTranslatesEngineFailures(t *testing.T) {
	t.Run("a value over max_value_size", func(t *testing.T) {
		k := keyspace{engine: storage.NewShardedEngine(storage.EngineConfig{
			ShardCount:   1,
			MaxValueSize: 8,
		})}
		defer func() { assert.NoError(t, k.engine.Close()) }()

		err := k.Set([]byte("k"), []byte("nine byte"), 0)

		assert.ErrorIs(t, err, server.ErrValueTooLarge)
	})

	t.Run("a write with nowhere to put it", func(t *testing.T) {
		// No eviction controller, so max_memory is a wall rather than a
		// prompt to make room.
		k := keyspace{engine: storage.NewShardedEngine(storage.EngineConfig{
			ShardCount:   1,
			MaxMemory:    128,
			MaxValueSize: 1024,
		})}
		defer func() { assert.NoError(t, k.engine.Close()) }()

		require.NoError(t, k.Set([]byte("first"), []byte("value"), 0))
		err := k.Set([]byte("second"), []byte("value"), 0)

		assert.ErrorIs(t, err, server.ErrOutOfMemory)
	})

	t.Run("an empty key is not a failure", func(t *testing.T) {
		// ISSUE-0013: Redis stores an empty key like any other, and the engine
		// no longer refuses one. The sentinel is still translated, for whatever
		// key-level limit lands here next.
		k := newTestKeyspace(t)

		assert.NoError(t, k.Set(nil, []byte("value"), 0))
		assert.True(t, k.Exists([]byte("")))

		assert.ErrorIs(t, translate(storage.ErrInvalidKey), server.ErrInvalidKey)
	})

	t.Run("anything else passes through", func(t *testing.T) {
		k := newTestKeyspace(t)
		require.NoError(t, k.engine.Close())

		err := k.Set([]byte("k"), []byte("v"), 0)

		assert.ErrorIs(t, err, storage.ErrEngineClosed)
	})

	t.Run("and success is still success", func(t *testing.T) {
		assert.NoError(t, translate(nil))
	})
}

// TestKeyspaceReadsAndWrites covers the adapter's own methods against the real
// engine, including the ownership contract Get is documented under.
func TestKeyspaceReadsAndWrites(t *testing.T) {
	k := newTestKeyspace(t)

	require.NoError(t, k.Set([]byte("k"), []byte("value"), 0))

	value, exists := k.Get([]byte("k"))
	require.True(t, exists)
	assert.Equal(t, "value", string(value))

	ttl, exists := k.GetTTL([]byte("k"))
	require.True(t, exists)
	assert.Negative(t, ttl, "a key with no expiry reports a negative TTL, not a missing one")

	assert.True(t, k.SetTTL([]byte("k"), time.Minute))
	ttl, exists = k.GetTTL([]byte("k"))
	require.True(t, exists)
	assert.Positive(t, ttl)

	assert.False(t, k.SetTTL([]byte("absent"), time.Minute))
	_, exists = k.GetTTL([]byte("absent"))
	assert.False(t, exists)

	assert.True(t, k.Delete([]byte("k")))
	assert.False(t, k.Delete([]byte("k")))

	_, exists = k.Get([]byte("k"))
	assert.False(t, exists)
}

// TestWiredServerServesDataCommands is the wiring FEAT-0017 is about: a real
// engine behind a real server, driven over a real socket. The E2E suite proves
// this against the shipped binary; this proves it without one, so a broken
// wiring fails at `go test` rather than at `make e2e`.
func TestWiredServerServesDataCommands(t *testing.T) {
	c, err := newCore(testConfig())
	require.NoError(t, err)
	c.start(zerolog.Nop())
	defer c.close()

	srv, err := server.New(context.Background(), "127.0.0.1:0", zerolog.Nop(), keyspace{engine: c.engine})
	require.NoError(t, err)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()

	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(context.Background(), "tcp", srv.Addr())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)

	call := func(args ...string) string {
		t.Helper()
		require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)))

		var request strings.Builder
		request.WriteString("*" + strconv.Itoa(len(args)) + "\r\n")
		for _, arg := range args {
			request.WriteString("$" + strconv.Itoa(len(arg)) + "\r\n" + arg + "\r\n")
		}
		_, err := conn.Write([]byte(request.String()))
		require.NoError(t, err)

		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		if strings.HasPrefix(line, "$") && line != "$-1\r\n" {
			payload, err := reader.ReadString('\n')
			require.NoError(t, err)
			line += payload
		}
		return line
	}

	assert.Equal(t, "+OK\r\n", call("SET", "wired", "value"))
	assert.Equal(t, "$5\r\nvalue\r\n", call("GET", "wired"))
	assert.Equal(t, ":-1\r\n", call("TTL", "wired"))
	assert.Equal(t, ":1\r\n", call("EXPIRE", "wired", "60"))
	assert.Equal(t, ":60\r\n", call("TTL", "wired"))
	assert.Equal(t, "+OK\r\n", call("SET", "brief", "value", "PX", "40"))

	// The TTL manager the binary starts is running behind this server, so the
	// key goes away on its own with nothing reading it.
	assert.True(t, eventually(t, 2*time.Second, func() bool {
		return c.engine.Stats().Keys == 1
	}), "active expiration reclaimed the short-lived key")
	assert.Equal(t, "$-1\r\n", call("GET", "brief"))
	assert.Equal(t, ":-2\r\n", call("TTL", "brief"))

	assert.Equal(t, ":1\r\n", call("DEL", "wired", "brief"))
	assert.Equal(t, "$-1\r\n", call("GET", "wired"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, srv.Shutdown(ctx))
	require.NoError(t, <-serveErr)
	require.NoError(t, c.stop(ctx))
}

// TestKeyspaceIntrospectionAndIteration covers the adapter methods P2 added.
// They are thin, and thin is exactly where a mistranslation hides: a swapped
// return value or an unmapped sentinel here reaches a client as a wrong answer
// with nothing in between to catch it.
func TestKeyspaceIntrospectionAndIteration(t *testing.T) {
	k := newTestKeyspace(t)

	t.Run("SetNX reports whether it stored, and the failure if it could not", func(t *testing.T) {
		stored, err := k.SetNX([]byte("nx"), []byte("first"), 0)
		require.NoError(t, err)
		assert.True(t, stored)

		stored, err = k.SetNX([]byte("nx"), []byte("second"), 0)
		require.NoError(t, err)
		assert.False(t, stored, "a live key blocks the write")

		value, exists := k.Get([]byte("nx"))
		assert.True(t, exists)
		assert.Equal(t, "first", string(value))

		// The engine's sentinel arrives as the server's, so the handler can
		// answer it without importing the storage package.
		stored, err = k.SetNX([]byte("big"), make([]byte, 2048), 0)
		assert.False(t, stored)
		assert.ErrorIs(t, err, server.ErrValueTooLarge)
	})

	t.Run("Keys matches Redis globs", func(t *testing.T) {
		for _, key := range []string{"user:1", "user:2", "cdn/eu/logo.png"} {
			require.NoError(t, k.Set([]byte(key), []byte("v"), 0))
		}

		assert.Len(t, k.Keys("user:*"), 2)
		assert.Len(t, k.Keys("cdn/*"), 1, "a wildcard crosses a path separator")
		assert.Empty(t, k.Keys("nothing:*"))
	})

	t.Run("Scan pages, and its cursors belong to one connection", func(t *testing.T) {
		const owner = 7

		seen := map[string]bool{}
		cursor := server.ScanCursorStart
		for i := 0; ; i++ {
			require.Less(t, i, 1000, "the scan never finished")

			keys, next, err := k.Scan(owner, cursor, 1, "user:*")
			require.NoError(t, err)
			for _, key := range keys {
				assert.False(t, seen[string(key)], "a key came back twice")
				seen[string(key)] = true
			}
			if next == server.ScanCursorStart {
				break
			}
			cursor = next
		}
		assert.Len(t, seen, 2)

		// A cursor nobody issued is refused rather than silently restarted.
		_, _, err := k.Scan(owner, "123456789", 10, "*")
		assert.ErrorIs(t, err, server.ErrScanCursorUnknown)

		// And a connection's cursors go when the connection does.
		_, cursor, err = k.Scan(owner, server.ScanCursorStart, 1, "*")
		require.NoError(t, err)
		require.NotEqual(t, server.ScanCursorStart, cursor)

		k.ReleaseScans(owner)
		_, _, err = k.Scan(owner, cursor, 1, "*")
		assert.ErrorIs(t, err, server.ErrScanCursorUnknown)
	})

	t.Run("Stats carries the engine's accounting and the process sample", func(t *testing.T) {
		stats := k.Stats()
		assert.Equal(t, uint64(4), stats.Keys)
		assert.Positive(t, stats.MemoryUsed)
		assert.Positive(t, stats.Sets)
		assert.Zero(t, stats.Process.SampledAt, "with no sampler wired there is no process reading to report")

		sampler := storage.NewProcessMemorySampler(time.Hour)
		sampler.Start()
		defer sampler.Stop()

		sampled := keyspace{engine: k.engine, procmem: sampler}.Stats()
		assert.Positive(t, sampled.Process.HeapAlloc)
		assert.Positive(t, sampled.Process.Goroutines)
		assert.False(t, sampled.Process.SampledAt.IsZero())
	})
}

func newTestKeyspace(t *testing.T) keyspace {
	t.Helper()

	engine := storage.NewShardedEngine(storage.EngineConfig{ShardCount: 1, MaxValueSize: 1024})
	t.Cleanup(func() { _ = engine.Close() })

	return keyspace{engine: engine}
}
