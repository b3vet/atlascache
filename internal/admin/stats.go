package admin

import (
	"net/http"
	"time"

	"github.com/b3vet/atlascache/internal/server"
)

// StatsSource is where /stats and /stats/memory get their figures.
//
// It is deliberately the server's own types. The STATS command reads a
// server.Stats from the keyspace and the composition root hands this API the
// same object, so the two surfaces are one source and cannot drift: a counter
// that moves moves in both, and a counter that is renamed fails to compile in
// both. Assembling a parallel set of figures here would produce two numbers
// with one name, which is worse than having no admin API at all — an operator
// who cannot reconcile two readings stops trusting either.
//
// Stats must stay cheap enough to poll: it is O(shards) and the process-memory
// half comes from a sampler, never from a runtime.ReadMemStats on this path
// (ISSUE-0015).
type StatsSource interface {
	Stats() server.Stats
	Limits() server.ConnLimits

	// ConnStats carries the ten connection figures that live only in
	// internal/server. They are read through here rather than counted again on
	// this side: two endpoints disagreeing about how many clients are connected
	// is the kind of thing that costs an hour during an incident.
	ConnStats() server.ConnStats
}

// statsResponse is the accounting /stats renders.
//
// The field names are the STATS command's names, exactly, so a script can move
// between the two without a translation table.
//
// Not here, and visibly so: connections_received, commands_processed,
// connected_clients, rejected_connections, idle_closed, output_limit_closed,
// stalled_closed, request_limit_closed, handler_panics and uptime_seconds.
// STATS reads those from counters that internal/server keeps unexported, and
// there is no accessor to reach them through. Reconstructing them here would
// mean a second set of counters incremented on a second path, which is the
// exact drift this type exists to avoid; the honest response is to report what
// has one source and to say plainly what does not. Exporting a single accessor
// on server.Server closes the gap when that package is next open for change.
type statsResponse struct {
	Keys        uint64 `json:"keys"`
	KeysWithTTL uint64 `json:"keys_with_ttl"`

	MemoryUsed uint64 `json:"memory_used"`
	MemoryMax  uint64 `json:"memory_max"`

	Gets    uint64 `json:"gets"`
	Sets    uint64 `json:"sets"`
	Deletes uint64 `json:"deletes"`
	Hits    uint64 `json:"hits"`
	Misses  uint64 `json:"misses"`

	Evictions   uint64 `json:"evictions"`
	Expirations uint64 `json:"expirations"`
	OOMRejected uint64 `json:"oom_rejected"`

	ScanCursors       uint64 `json:"scan_cursors"`
	ScanSnapshotBytes uint64 `json:"scan_snapshot_bytes"`

	MaxConnections int `json:"max_connections"`

	// Connection accounting, matching the STATS command's names exactly. These
	// come from internal/server rather than being counted again here, so the
	// two endpoints cannot drift apart.
	ConnectionsReceived uint64 `json:"connections_received"`
	CommandsProcessed   uint64 `json:"commands_processed"`
	ConnectedClients    int64  `json:"connected_clients"`
	RejectedConnections uint64 `json:"rejected_connections"`
	IdleClosed          uint64 `json:"idle_closed"`
	OutputLimitClosed   uint64 `json:"output_limit_closed"`
	StalledClosed       uint64 `json:"stalled_closed"`
	RequestLimitClosed  uint64 `json:"request_limit_closed"`
	HandlerPanics       uint64 `json:"handler_panics"`
	UptimeSeconds       uint64 `json:"uptime_seconds"`
}

// memoryResponse is the detail /stats/memory renders: the cache's own
// accounting first, the process's second, and the two never mixed.
//
// used_memory is what max_memory is enforced against and what capacity is
// planned on; the Go heap is a different quantity that happens to be measured
// in the same unit. INFO keeps them apart for the same reason and with the same
// prefix discipline, and anyone who plots one believing it is the other will
// size a deployment wrong.
type memoryResponse struct {
	MemoryUsed uint64 `json:"memory_used"`
	MemoryMax  uint64 `json:"memory_max"`

	ScanCursors       uint64 `json:"scan_cursors"`
	ScanSnapshotBytes uint64 `json:"scan_snapshot_bytes"`

	// Process is absent until the sampler has taken a reading. Reporting zeros
	// would be reporting a measurement that was never made.
	Process *processResponse `json:"process,omitempty"`
}

// processResponse is the Go runtime's view of this process, as of SampledAt.
type processResponse struct {
	HeapAlloc   uint64 `json:"heap_alloc"`
	HeapSys     uint64 `json:"heap_sys"`
	HeapInuse   uint64 `json:"heap_inuse"`
	HeapObjects uint64 `json:"heap_objects"`
	StackInuse  uint64 `json:"stack_inuse"`
	Sys         uint64 `json:"sys"`
	NumGC       uint32 `json:"num_gc"`
	Goroutines  int    `json:"goroutines"`

	// SampledAt is when the figures above were read. It is here because they
	// are a sample and not a live reading, and an operator diagnosing a memory
	// problem needs to know how old the number in front of them is.
	SampledAt time.Time `json:"sampled_at"`
}

// handleStats renders the full statistics document.
func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	source := s.statsSource()
	if source == nil {
		s.statsUnavailable(w)
		return
	}

	stats := source.Stats()
	conns := source.ConnStats()
	s.writeJSON(w, http.StatusOK, statsResponse{
		Keys:              stats.Keys,
		KeysWithTTL:       stats.KeysWithTTL,
		MemoryUsed:        stats.MemoryUsed,
		MemoryMax:         stats.MemoryMax,
		Gets:              stats.Gets,
		Sets:              stats.Sets,
		Deletes:           stats.Deletes,
		Hits:              stats.Hits,
		Misses:            stats.Misses,
		Evictions:         stats.Evictions,
		Expirations:       stats.Expirations,
		OOMRejected:       stats.OOMRejected,
		ScanCursors:       stats.ScanCursors,
		ScanSnapshotBytes: stats.ScanSnapshotBytes,
		MaxConnections:    source.Limits().MaxConnections,

		ConnectionsReceived: conns.ConnectionsReceived,
		CommandsProcessed:   conns.CommandsProcessed,
		ConnectedClients:    conns.Connected,
		RejectedConnections: conns.Rejected,
		IdleClosed:          conns.IdleClosed,
		OutputLimitClosed:   conns.OutputClosed,
		StalledClosed:       conns.StalledClosed,
		RequestLimitClosed:  conns.RequestClosed,
		HandlerPanics:       conns.HandlerPanics,
		UptimeSeconds:       conns.UptimeSeconds,
	})
}

// handleStatsMemory renders the memory detail.
func (s *Server) handleStatsMemory(w http.ResponseWriter, _ *http.Request) {
	source := s.statsSource()
	if source == nil {
		s.statsUnavailable(w)
		return
	}

	stats := source.Stats()
	response := memoryResponse{
		MemoryUsed:        stats.MemoryUsed,
		MemoryMax:         stats.MemoryMax,
		ScanCursors:       stats.ScanCursors,
		ScanSnapshotBytes: stats.ScanSnapshotBytes,
	}

	if !stats.Process.SampledAt.IsZero() {
		response.Process = &processResponse{
			HeapAlloc:   stats.Process.HeapAlloc,
			HeapSys:     stats.Process.HeapSys,
			HeapInuse:   stats.Process.HeapInuse,
			HeapObjects: stats.Process.HeapObjects,
			StackInuse:  stats.Process.StackInuse,
			Sys:         stats.Process.Sys,
			NumGC:       stats.Process.NumGC,
			Goroutines:  stats.Process.Goroutines,
			SampledAt:   stats.Process.SampledAt.UTC(),
		}
	}

	s.writeJSON(w, http.StatusOK, response)
}

// statsUnavailable is the answer when no source was wired. It is a 503 rather
// than a 200 with zeros, because zeros are a reading and this is the absence of
// one — a dashboard must not plot "no source" as "an empty cache".
func (s *Server) statsUnavailable(w http.ResponseWriter) {
	s.writeJSON(w, http.StatusServiceUnavailable, errorResponse{
		Error: "statistics are not available on this node",
	})
}
