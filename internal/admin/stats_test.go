package admin

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b3vet/atlascache/internal/server"
)

// fakeStats is a StatsSource with distinct values in every field, so that a
// handler wiring `Misses` into `hits` fails instead of passing on two zeros.
// ConnStats lets the fake satisfy StatsSource; the connection figures come
// from internal/server and are asserted against the real server elsewhere.
func (f fakeStats) ConnStats() server.ConnStats { return f.conns }

type fakeStats struct {
	stats  server.Stats
	limits server.ConnLimits
	conns  server.ConnStats
}

func (f fakeStats) Stats() server.Stats       { return f.stats }
func (f fakeStats) Limits() server.ConnLimits { return f.limits }

func distinctStats() fakeStats {
	return fakeStats{
		stats: server.Stats{
			Keys:              11,
			KeysWithTTL:       12,
			MemoryUsed:        13,
			MemoryMax:         14,
			Gets:              15,
			Sets:              16,
			Deletes:           17,
			Hits:              18,
			Misses:            19,
			Evictions:         20,
			Expirations:       21,
			OOMRejected:       22,
			ScanCursors:       23,
			ScanSnapshotBytes: 24,
		},
		limits: server.ConnLimits{MaxConnections: 25},
		conns: server.ConnStats{
			ConnectionsReceived: 26,
			CommandsProcessed:   27,
			Connected:           28,
			Rejected:            29,
			IdleClosed:          30,
			OutputClosed:        31,
			StalledClosed:       32,
			RequestClosed:       33,
			HandlerPanics:       34,
			UptimeSeconds:       35,
		},
	}
}

// TestStatsRendersEveryFieldFromTheSource checks the mapping field by field.
//
// The values are all different, which is what makes the check worth running: a
// transposition between two counters is invisible against a source of zeros,
// and a dashboard built on a transposed counter is worse than one built on no
// counter at all.
func TestStatsRendersEveryFieldFromTheSource(t *testing.T) {
	srv := newTestServer(t, WithStats(distinctStats()))
	srv.SetReady(true)

	fields := get(t, srv, "/stats").fields(t)

	assert.Equal(t, map[string]any{
		"keys":                 float64(11),
		"keys_with_ttl":        float64(12),
		"memory_used":          float64(13),
		"memory_max":           float64(14),
		"gets":                 float64(15),
		"sets":                 float64(16),
		"deletes":              float64(17),
		"hits":                 float64(18),
		"misses":               float64(19),
		"evictions":            float64(20),
		"expirations":          float64(21),
		"oom_rejected":         float64(22),
		"scan_cursors":         float64(23),
		"scan_snapshot_bytes":  float64(24),
		"max_connections":      float64(25),
		"connections_received": float64(26),
		"commands_processed":   float64(27),
		"connected_clients":    float64(28),
		"rejected_connections": float64(29),
		"idle_closed":          float64(30),
		"output_limit_closed":  float64(31),
		"stalled_closed":       float64(32),
		"request_limit_closed": float64(33),
		"handler_panics":       float64(34),
		"uptime_seconds":       float64(35),
	}, fields)
}

// TestStatsFieldNamesMatchTheStatsCommand is the anti-drift check.
//
// The admin API and the STATS command are two renderings of one set of numbers,
// and a script that moves between them must not need a translation table. The
// names asserted here are the names internal/server's stats handler builds, and
// the list is the subset the admin API can reach: the connection counters STATS
// also reports live in unexported fields with no accessor, and inventing a
// second set here is exactly the drift this test exists to prevent.
func TestStatsFieldNamesMatchTheStatsCommand(t *testing.T) {
	srv := newTestServer(t, WithStats(distinctStats()))
	srv.SetReady(true)

	fields := get(t, srv, "/stats").fields(t)

	for _, name := range []string{
		"keys", "keys_with_ttl", "memory_used", "memory_max",
		"gets", "sets", "deletes", "hits", "misses",
		"evictions", "expirations", "oom_rejected",
		"scan_cursors", "scan_snapshot_bytes", "max_connections",
	} {
		assert.Contains(t, fields, name, "the STATS command reports %s", name)
	}
}

// TestStatsMemoryOmitsAProcessSampleThatWasNeverTaken keeps a measurement that
// has not happened from looking like one that read zero.
func TestStatsMemoryOmitsAProcessSampleThatWasNeverTaken(t *testing.T) {
	srv := newTestServer(t, WithStats(distinctStats()))
	srv.SetReady(true)

	fields := get(t, srv, "/stats/memory").fields(t)

	assert.Equal(t, float64(13), fields["memory_used"])
	assert.Equal(t, float64(14), fields["memory_max"])
	assert.NotContains(t, fields, "process", "an unsampled process is absent, not zero")
}

// TestStatsMemoryReportsTheSampleAndItsAge checks the half that matters when a
// memory problem is being diagnosed: the figures are a sample taken on a timer
// (ISSUE-0015), and a reader has to be able to tell how old they are.
func TestStatsMemoryReportsTheSampleAndItsAge(t *testing.T) {
	sampledAt := time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC)

	source := distinctStats()
	source.stats.Process = server.ProcessStats{
		HeapAlloc:   101,
		HeapSys:     102,
		HeapInuse:   103,
		HeapObjects: 104,
		StackInuse:  105,
		Sys:         106,
		NumGC:       107,
		Goroutines:  108,
		SampledAt:   sampledAt,
	}

	srv := newTestServer(t, WithStats(source))
	srv.SetReady(true)

	fields := get(t, srv, "/stats/memory").fields(t)
	process, ok := fields["process"].(map[string]any)
	require.True(t, ok, "the sample is reported: %v", fields)

	assert.Equal(t, map[string]any{
		"heap_alloc":   float64(101),
		"heap_sys":     float64(102),
		"heap_inuse":   float64(103),
		"heap_objects": float64(104),
		"stack_inuse":  float64(105),
		"sys":          float64(106),
		"num_gc":       float64(107),
		"goroutines":   float64(108),
		"sampled_at":   sampledAt.Format(time.RFC3339Nano),
	}, process)
}

// TestStatsWithoutASourceIsUnavailableRatherThanEmpty keeps "nothing is wired"
// from being plotted as "the cache is empty" — which is the reading a
// dashboard would take from a 200 full of zeros, and the one that sends an
// operator looking for a data loss that never happened.
func TestStatsWithoutASourceIsUnavailableRatherThanEmpty(t *testing.T) {
	srv := newTestServer(t)
	srv.SetReady(true)

	for _, path := range []string{"/stats", "/stats/memory"} {
		resp := get(t, srv, path)
		assert.Equal(t, http.StatusServiceUnavailable, resp.status, "GET %s", path)
		assert.Contains(t, resp.fields(t)["error"], "not available")
	}
}

// TestStatsSourceCanBeSuppliedAfterBinding is the startup ordering: the admin
// listener answers before the engine exists, so the source arrives late.
func TestStatsSourceCanBeSuppliedAfterBinding(t *testing.T) {
	srv := newTestServer(t)

	assert.Equal(t, http.StatusServiceUnavailable, get(t, srv, "/stats").status)

	srv.SetStats(distinctStats())
	srv.SetReady(true)

	resp := get(t, srv, "/stats")
	require.Equal(t, http.StatusOK, resp.status)
	assert.Equal(t, float64(11), resp.fields(t)["keys"])
}
