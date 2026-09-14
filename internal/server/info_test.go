package server

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// infoServer starts a server whose store reports stats, and returns a function
// that asks it for INFO.
func infoServer(t *testing.T, stats Stats) (*Server, func(sections ...string) string) {
	t.Helper()

	store := newFakeStore()
	store.stats = stats

	srv, serveErr := newTestServerWith(t, store)
	t.Cleanup(func() { shutdownServer(t, srv, serveErr) })

	return srv, func(sections ...string) string {
		t.Helper()
		reply := ask(t, srv, append([]string{"INFO"}, sections...)...)
		return strings.TrimPrefix(reply, "$")
	}
}

// TestInfoIsRedisFormat checks the framing every exporter depends on: CRLF
// endings, `# Section` headers, `key:value` lines, and one blank line between
// sections. It is asserted at the byte level because "roughly Redis's format"
// is not a format.
func TestInfoIsRedisFormat(t *testing.T) {
	_, info := infoServer(t, Stats{Keys: 3, KeysWithTTL: 1, MemoryUsed: 2048})

	text := info()

	require.True(t, strings.HasPrefix(text, "# Server\r\n"), "INFO opens with the Server header")
	assert.Contains(t, text, "\r\n\r\n# Memory\r\n", "sections are separated by one blank line")
	assert.True(t, strings.HasSuffix(text, "\r\n"), "the last line is terminated like every other")
	assert.NotContains(t, text, "\r\n\r\n\r\n", "there is exactly one blank line between sections")

	for _, line := range strings.Split(strings.TrimSuffix(text, "\r\n"), "\r\n") {
		if line == "" || strings.HasPrefix(line, "# ") {
			continue
		}
		assert.Containsf(t, line, ":", "every content line is key:value, got %q", line)
		assert.NotContainsf(t, line, "\n", "no line carries a bare newline: %q", line)
	}

	// Every section is present, in Redis's order: Server first, Keyspace last.
	var order []string
	for _, line := range strings.Split(text, "\r\n") {
		if strings.HasPrefix(line, "# ") {
			order = append(order, strings.ToLower(strings.TrimPrefix(line, "# ")))
		}
	}
	assert.Equal(t, infoSections, order)
}

// TestInfoFieldNames is the compatibility contract from FEAT-0021: Redis's
// spelling where there is a direct equivalent, an atlascache_ prefix on
// everything else. A dashboard built for Redis reads the first group unchanged,
// and the prefix on the second means a field Redis adds later cannot collide
// with one invented here.
func TestInfoFieldNames(t *testing.T) {
	_, info := infoServer(t, Stats{
		Keys: 1000, KeysWithTTL: 50, MemoryUsed: 1 << 20, MemoryMax: 1 << 30,
		Hits: 1000, Misses: 37, Evictions: 9, Expirations: 4, OOMRejected: 2,
		Gets: 2000, Sets: 1500, Deletes: 100,
		ScanCursors: 3, ScanSnapshotBytes: 4096,
		Process: ProcessStats{HeapAlloc: 123, HeapSys: 456, NumGC: 7, Goroutines: 8, SampledAt: time.Now()},
	})

	fields := ParseInfo(info())

	// Redis's own names, spelled exactly as Redis spells them.
	for name, want := range map[string]string{
		"used_memory":       "1048576",
		"used_memory_human": "1.00M",
		"maxmemory":         "1073741824",
		"maxmemory_human":   "1.00G",
		"keyspace_hits":     "1000",
		"keyspace_misses":   "37",
		"evicted_keys":      "9",
		"expired_keys":      "4",
		"connected_clients": "1",
		"arch_bits":         "64",
		"uptime_in_days":    "0",
		"db0":               "keys=1000,expires=50,avg_ttl=0",
	} {
		assert.Equalf(t, want, fields[name], "field %s", name)
	}

	for _, name := range []string{"total_connections_received", "total_commands_processed", "uptime_in_seconds",
		"process_id", "tcp_port", "go_version", "os", "blocked_clients"} {
		assert.Containsf(t, fields, name, "Redis-named field %s is missing", name)
	}

	// Everything AtlasCache invented carries the prefix.
	for name, want := range map[string]string{
		"atlascache_version":             Version,
		"atlascache_mode":                serverMode,
		"atlascache_role":                serverRole,
		"atlascache_scan_cursors":        "3",
		"atlascache_scan_snapshot_bytes": "4096",
		"atlascache_heap_alloc":          "123",
		"atlascache_heap_sys":            "456",
		"atlascache_num_gc":              "7",
		"atlascache_goroutines":          "8",
		"atlascache_gets":                "2000",
		"atlascache_sets":                "1500",
		"atlascache_deletes":             "100",
		"atlascache_oom_rejected":        "2",
	} {
		assert.Equalf(t, want, fields[name], "field %s", name)
	}

	// Nothing claims to be Redis. A version check against redis_version would
	// otherwise pass on a server implementing a fraction of the command set.
	assert.NotContains(t, fields, "redis_version")
}

func TestInfoSectionSelection(t *testing.T) {
	_, info := infoServer(t, Stats{Keys: 2, KeysWithTTL: 1})

	t.Run("one section is only that section", func(t *testing.T) {
		text := info("memory")

		assert.True(t, strings.HasPrefix(text, "# Memory\r\n"))
		assert.NotContains(t, text, "# Server")
		assert.NotContains(t, text, "# Keyspace")
		assert.Contains(t, ParseInfo(text), "used_memory")
	})

	t.Run("section names are case-insensitive", func(t *testing.T) {
		assert.Equal(t, info("memory"), info("MEMORY"))
		assert.Equal(t, info("memory"), info("Memory"))
	})

	t.Run("several sections come back in render order, not argument order", func(t *testing.T) {
		assert.Equal(t, info("memory", "server"), info("server", "memory"))
		assert.True(t, strings.HasPrefix(info("keyspace", "server"), "# Server\r\n"))
	})

	t.Run("a repeated section is rendered once", func(t *testing.T) {
		assert.Equal(t, info("memory"), info("memory", "memory"))
	})

	t.Run("default, all and everything each mean every section", func(t *testing.T) {
		full := info()
		for _, alias := range []string{"default", "all", "everything"} {
			assert.Equalf(t, strings.Count(full, "# "), strings.Count(info(alias), "# "), "alias %q", alias)
		}
	})

	t.Run("an unknown section is empty, not an error", func(t *testing.T) {
		// Tools probe for sections they are not sure exist, and Redis answers
		// them with an empty string rather than refusing.
		assert.Empty(t, info("nosuchsection"))
		assert.Empty(t, info("nosuchsection", "alsonot"))
	})

	t.Run("a known section beside an unknown one still renders", func(t *testing.T) {
		assert.Equal(t, info("memory"), info("nosuchsection", "memory"))
	})
}

// TestInfoKeyspaceOmitsAnEmptyDatabase follows Redis: no keys, no db0 line.
// Several exporters read the absence of the line as "empty" and a line reading
// keys=0 as a database that exists and is broken.
func TestInfoKeyspaceOmitsAnEmptyDatabase(t *testing.T) {
	_, empty := infoServer(t, Stats{})
	assert.Equal(t, "# Keyspace\r\n", empty("keyspace"))
	assert.NotContains(t, ParseInfo(empty()), "db0")

	store := newFakeStore()
	require.NoError(t, store.Set([]byte("k"), []byte("v"), 0))
	srv, serveErr := newTestServerWith(t, store)
	defer shutdownServer(t, srv, serveErr)

	text := strings.TrimPrefix(ask(t, srv, "INFO", "keyspace"), "$")
	assert.Equal(t, "keys=1,expires=0,avg_ttl=0", ParseInfo(text)["db0"])
}

// TestInfoCountsConnectionsAndCommands checks the two counters a monitoring
// dashboard graphs first, and that they count the whole server rather than the
// connection asking.
func TestInfoCountsConnectionsAndCommands(t *testing.T) {
	srv, info := infoServer(t, Stats{})

	before := ParseInfo(info())
	firstConnections := mustInt(t, before["total_connections_received"])
	firstCommands := mustInt(t, before["total_commands_processed"])

	assert.Equal(t, "+PONG", ask(t, srv, "PING"))
	assert.Equal(t, "+PONG", ask(t, srv, "PING"))

	after := ParseInfo(info())
	assert.Greater(t, mustInt(t, after["total_connections_received"]), firstConnections,
		"connections received is cumulative and never goes down")
	assert.Greater(t, mustInt(t, after["total_commands_processed"]), firstCommands,
		"every command counts, including the INFO that reports the count")
}

// TestInfoConnectedClientsTracksLiveConnections is the other counter, and the
// one that has to go down as well as up.
func TestInfoConnectedClientsTracksLiveConnections(t *testing.T) {
	srv, _ := infoServer(t, Stats{})

	// One connection, reused: a reader that opened a fresh connection per
	// reading would be counting itself.
	reading := conversation(t, srv)
	live := func() int {
		t.Helper()
		text := strings.TrimPrefix(reading("INFO", "clients"), "$")
		return mustInt(t, ParseInfo(text)["connected_clients"])
	}

	baseline := live()
	assert.Positive(t, baseline, "the connection doing the asking is itself connected")

	conn, reader := dial(t, srv)
	send(t, conn, "*1\r\n$4\r\nPING\r\n")
	require.Equal(t, "+PONG\r\n", readReply(t, conn, reader))

	assert.Greater(t, live(), baseline, "an open connection is counted")

	require.NoError(t, conn.Close())
	assert.Eventually(t, func() bool { return live() <= baseline }, 2*time.Second, 10*time.Millisecond,
		"a closed connection stops being counted")
}

func mustInt(t *testing.T, text string) int {
	t.Helper()

	n, err := strconv.Atoi(text)
	require.NoErrorf(t, err, "expected a number, got %q", text)
	return n
}

// TestInfoOmitsProcessMemoryUntilSampled keeps INFO honest. An unsampled
// process reports no heap figures at all rather than a convincing row of zeros,
// because a dashboard cannot tell a zero from an absence and would plot one.
func TestInfoOmitsProcessMemoryUntilSampled(t *testing.T) {
	_, unsampled := infoServer(t, Stats{MemoryUsed: 10})
	assert.NotContains(t, ParseInfo(unsampled()), "atlascache_heap_alloc")

	_, sampled := infoServer(t, Stats{MemoryUsed: 10, Process: ProcessStats{SampledAt: time.Now()}})
	assert.Contains(t, ParseInfo(sampled()), "atlascache_heap_alloc")
	assert.Contains(t, ParseInfo(sampled()), "atlascache_memory_sampled_at")
}

// TestHumanBytes follows Redis's bytesToHuman, because used_memory_human is
// displayed verbatim by redis-cli and by every dashboard that shows it.
func TestHumanBytes(t *testing.T) {
	cases := map[uint64]string{
		0:         "0B",
		1:         "1B",
		1023:      "1023B",
		1024:      "1.00K",
		1536:      "1.50K",
		1 << 20:   "1.00M",
		1_500_000: "1.43M",
		1 << 30:   "1.00G",
		1 << 40:   "1.00T",
		1 << 50:   "1.00P",
		1 << 60:   "1.00E",
	}
	for in, want := range cases {
		assert.Equalf(t, want, humanBytes(in), "humanBytes(%d)", in)
	}
}

func TestParseInfoSkipsWhatIsNotAField(t *testing.T) {
	text := "# Server\r\natlascache_version:0.1.0\r\n\r\n# Keyspace\r\ndb0:keys=1,expires=0\r\nnotafield\r\n"

	assert.Equal(t, map[string]string{
		"atlascache_version": "0.1.0",
		"db0":                "keys=1,expires=0",
	}, ParseInfo(text))

	assert.Empty(t, ParseInfo(""))
}

func TestListenPort(t *testing.T) {
	assert.Equal(t, "6379", listenPort("127.0.0.1:6379"))
	assert.Equal(t, "6379", listenPort("[::1]:6379"))
	assert.Equal(t, "0", listenPort("not-an-address"))
}

func TestInfoSectionsIsStable(t *testing.T) {
	names := InfoSections()
	assert.Len(t, names, len(infoSections))
	assert.Equal(t, []string{"clients", "keyspace", "memory", "server", "stats"}, names)

	names[0] = "mutated"
	assert.NotContains(t, infoSections, "mutated", "the caller gets a copy, not the render order itself")
}
