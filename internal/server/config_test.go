package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CONFIG GET is ISSUE-0021: redis-benchmark probes it before it runs anything,
// and prints "WARNING: Could not fetch server CONFIG" when the probe fails.
//
// The assertions below are about the two things that make the reply worth
// having: it is the shape a Redis client expects, and every value in it is one
// AtlasCache actually has.

// newConfigServer starts a server with limits and memory figures a test can
// recognize in a CONFIG GET reply.
func newConfigServer(t *testing.T) (*Server, *fakeStore) {
	t.Helper()

	store := newFakeStore()
	store.stats.MemoryMax = 1073741824 // 1GiB, the figure a CONFIG GET must echo

	limits := DefaultConnLimits()
	limits.MaxConnections = 4242
	limits.IdleTimeout = 45 * time.Second

	srv, err := New(context.Background(), "127.0.0.1:0", zerolog.Nop(), store, WithConnLimits(limits))
	require.NoError(t, err)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()
	t.Cleanup(func() { shutdownServer(t, srv, serveErr) })

	return srv, store
}

func TestConfigGetReportsWhatTheServerHas(t *testing.T) {
	srv, store := newConfigServer(t)
	ask := conversation(t, srv)

	t.Run("maxmemory is the configured ceiling, in bytes", func(t *testing.T) {
		// Bytes as a decimal string is Redis's format for this parameter, and
		// dashboards parse it as one.
		assert.Equal(t, "[$maxmemory $1073741824]", ask("CONFIG", "GET", "maxmemory"))
	})

	t.Run("maxmemory follows a reload rather than a snapshot", func(t *testing.T) {
		// max_memory is hot-reloadable, so a value captured at startup would
		// report a ceiling the server had stopped enforcing.
		store.mu.Lock()
		store.stats.MemoryMax = 536870912
		store.mu.Unlock()

		assert.Equal(t, "[$maxmemory $536870912]", ask("CONFIG", "GET", "maxmemory"))
	})

	t.Run("maxclients and timeout are the connection limits", func(t *testing.T) {
		assert.Equal(t, "[$maxclients $4242]", ask("CONFIG", "GET", "maxclients"))
		// Seconds, as Redis's `timeout` is.
		assert.Equal(t, "[$timeout $45]", ask("CONFIG", "GET", "timeout"))
	})

	t.Run("save and appendonly are empty, not invented", func(t *testing.T) {
		// The pair is present because redis-benchmark reads a reply of fewer
		// than two elements as a failed probe and warns. The value is empty
		// because persistence is a later phase, and "appendonly:no" would be a
		// claim about durability with nothing behind it.
		assert.Equal(t, "[$save $]", ask("CONFIG", "GET", "save"))
		assert.Equal(t, "[$appendonly $]", ask("CONFIG", "GET", "appendonly"))
	})

	t.Run("a parameter this server does not have is empty, not an error", func(t *testing.T) {
		// Redis answers an unknown parameter with an empty array too; tools
		// probe for parameters they are not sure exist.
		assert.Equal(t, "[]", ask("CONFIG", "GET", "appendfsync"))
		assert.Equal(t, "[]", ask("CONFIG", "GET", "maxmemory-policy"))
	})
}

func TestConfigGetMatchesPatternsTheWayRedisDoes(t *testing.T) {
	srv, _ := newConfigServer(t)
	ask := conversation(t, srv)

	t.Run("a glob selects every parameter it matches, in table order", func(t *testing.T) {
		// `CONFIG GET max*` is what monitoring agents send when they want the
		// memory and connection ceilings in one round trip.
		assert.Equal(t, "[$maxclients $4242 $maxmemory $1073741824]", ask("CONFIG", "GET", "max*"))
	})

	t.Run("a bare star is every parameter", func(t *testing.T) {
		assert.Equal(t,
			"[$appendonly $ $maxclients $4242 $maxmemory $1073741824 $save $ $timeout $45]",
			ask("CONFIG", "GET", "*"))
	})

	t.Run("a glob that matches nothing is empty", func(t *testing.T) {
		assert.Equal(t, "[]", ask("CONFIG", "GET", "appendf*"))
	})

	t.Run("a malformed pattern is answered, never refused", func(t *testing.T) {
		// Redis never returns an error for a pattern — its matcher reads what
		// it can. internal/glob is the same port KEYS uses.
		assert.Equal(t, "[]", ask("CONFIG", "GET", "[a"))
	})

	t.Run("names are matched case-insensitively", func(t *testing.T) {
		// And the reply echoes the spelling the client used, as Redis's does,
		// so a client that looks the reply up by the string it sent finds it.
		assert.Equal(t, "[$MAXMEMORY $1073741824]", ask("CONFIG", "GET", "MAXMEMORY"))
		assert.Equal(t, "[$MaxClients $4242]", ask("CONFIG", "GET", "MaxClients"))
		// A pattern has no single spelling to echo, so it reports canonical
		// names.
		assert.Equal(t, "[$maxclients $4242 $maxmemory $1073741824]", ask("CONFIG", "GET", "MAX*"))
	})

	t.Run("several parameters may be asked for at once", func(t *testing.T) {
		assert.Equal(t, "[$maxclients $4242 $timeout $45]", ask("CONFIG", "GET", "timeout", "maxclients"))
	})

	t.Run("a parameter selected twice is reported once", func(t *testing.T) {
		// Redis keys its match set by name, so overlapping patterns do not
		// produce a reply with the same parameter in it twice — which would
		// break a client building a map out of the pairs.
		assert.Equal(t, "[$maxmemory $1073741824]", ask("CONFIG", "GET", "maxmemory", "MAXMEMORY"))
		assert.Equal(t, "[$maxclients $4242 $maxmemory $1073741824]",
			ask("CONFIG", "GET", "maxmemory", "max*"))
	})
}

func TestConfigRefusalsNameThePathThatWorks(t *testing.T) {
	srv, _ := newConfigServer(t)
	ask := conversation(t, srv)

	t.Run("CONFIG SET names hot reload", func(t *testing.T) {
		// "Unknown subcommand" would be true and useless: configuration is a
		// file, and the operator needs to be told that rather than sent looking
		// for a build that has CONFIG SET.
		reply := ask("CONFIG", "SET", "maxmemory", "100mb")
		assert.True(t, strings.HasPrefix(reply, "-ERR CONFIG SET is not supported"), reply)
		assert.Contains(t, reply, "config file")
		assert.Contains(t, reply, "SIGHUP")
	})

	t.Run("an unknown subcommand is Redis's error, and points at HELP", func(t *testing.T) {
		assert.Equal(t, "-ERR unknown subcommand 'RESETSTAT'. Try CONFIG HELP.",
			ask("CONFIG", "RESETSTAT"))
		assert.Equal(t, "-ERR unknown subcommand 'REWRITE'. Try CONFIG HELP.",
			ask("CONFIG", "REWRITE"))
	})

	t.Run("CONFIG HELP is answered, so that advice leads somewhere", func(t *testing.T) {
		reply := ask("CONFIG", "HELP")
		assert.Contains(t, reply, "GET <pattern>")
		assert.Contains(t, reply, "Not supported")
	})

	t.Run("arity errors name the subcommand, as Redis's do", func(t *testing.T) {
		assert.Equal(t, "-ERR wrong number of arguments for 'config|get' command", ask("CONFIG", "GET"))
		assert.Equal(t, "-ERR wrong number of arguments for 'config' command", ask("CONFIG"))
	})

	t.Run("the connection survives every refusal", func(t *testing.T) {
		// A CONFIG a client got wrong must not cost it the connection: that is
		// the same rule every other command follows.
		assert.Equal(t, "+PONG", ask("PING"))
	})
}
