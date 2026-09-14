package main

import (
	"errors"
	"math"
	"time"

	"github.com/b3vet/atlascache/internal/server"
	"github.com/b3vet/atlascache/internal/storage"
)

// keyspace joins the storage engine to the seam the server declares.
//
// It exists so the dependency keeps running one way. internal/server names the
// keyspace it needs and the failures it has a RESP reply for; internal/storage
// knows nothing of RESP. This adapter is the single place that speaks both
// vocabularies, and it lives in the composition root because that is the only
// place already entitled to know about both.
type keyspace struct {
	engine *storage.ShardedEngine

	// procmem is where INFO's process-level figures come from. It is a sampler
	// rather than a reader: runtime.ReadMemStats stops the world, and INFO is
	// on a path a dashboard polls every second (ISSUE-0015). A nil sampler is
	// allowed and reports nothing, which is what a test wiring wants.
	procmem *storage.ProcessMemorySampler
}

// The server's contract is satisfied here rather than by the engine itself, so
// a change to either side fails at the build rather than at the first GET.
var _ server.Store = keyspace{}

// Get returns the engine's read-only view of the value. The server encodes it
// into the reply before the command completes and never retains it, which is
// the contract StorageEngine documents (ADR-0013).
// MaxValueSize hands the engine's configured ceiling to the protocol layer, so
// the parser refuses a bulk string the engine would reject anyway rather than
// buffering it first (ISSUE-0016).
func (k keyspace) MaxValueSize() int {
	size := k.engine.MaxValueSize()
	if size > math.MaxInt {
		return math.MaxInt
	}
	return int(size)
}

func (k keyspace) Get(key []byte) (value []byte, exists bool) {
	value, _, exists = k.engine.Get(key)
	return value, exists
}

// Set stores a copy of key and value, so the connection's read buffer is free
// to be reused the moment this returns (ISSUE-0009).
func (k keyspace) Set(key, value []byte, ttl time.Duration) error {
	return translate(k.engine.Set(key, value, ttl))
}

// Delete removes a key, reporting whether it removed one.
func (k keyspace) Delete(key []byte) bool {
	return k.engine.Delete(key)
}

// GetTTL reports the remaining life of a live key.
func (k keyspace) GetTTL(key []byte) (time.Duration, bool) {
	return k.engine.GetTTL(key)
}

// SetTTL attaches an expiry to a live key.
func (k keyspace) SetTTL(key []byte, ttl time.Duration) bool {
	return k.engine.SetTTL(key, ttl)
}

// SetNX stores only if the key is free, through the same admission path as Set
// so that max_memory and eviction apply identically to both (ISSUE-0010).
func (k keyspace) SetNX(key, value []byte, ttl time.Duration) (bool, error) {
	stored, err := k.engine.SetNX(key, value, ttl)
	return stored, translate(err)
}

// Exists reports whether a live key holds the name.
func (k keyspace) Exists(key []byte) bool {
	return k.engine.Exists(key)
}

// Keys returns every live key matching a Redis glob.
func (k keyspace) Keys(pattern string) [][]byte {
	return k.engine.Keys(pattern)
}

// Scan returns one page of a snapshot scan owned by the connection.
//
// The owner is the server's connection id widened to the engine's ScanOwner.
// Both are opaque identifiers of the same thing, and keeping the two types
// distinct is what stops a caller elsewhere passing a shard index or a cursor
// by mistake.
func (k keyspace) Scan(owner uint64, cursor string, count int, match string) ([][]byte, string, error) {
	keys, next, err := k.engine.Scan(storage.ScanOwner(owner), cursor, count, match)
	return keys, next, translate(err)
}

// ReleaseScans drops every cursor a departing connection held.
func (k keyspace) ReleaseScans(owner uint64) {
	k.engine.ReleaseScans(storage.ScanOwner(owner))
}

// Stats maps the engine's accounting onto the shape the server renders, and
// folds in the process-level sample.
//
// The two halves are joined here rather than in the engine on purpose: the
// cache's memory accounting is the engine's business, and the Go runtime's is
// the process's. Conflating them is what put a stop-the-world under every
// STATS call in the first place.
func (k keyspace) Stats() server.Stats {
	stats := k.engine.Stats()

	out := server.Stats{
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
	}

	if k.procmem != nil {
		sample := k.procmem.Sample()
		out.Process = server.ProcessStats{
			HeapAlloc:   sample.HeapAlloc,
			HeapSys:     sample.HeapSys,
			HeapInuse:   sample.HeapInuse,
			HeapObjects: sample.HeapObjects,
			StackInuse:  sample.StackInuse,
			Sys:         sample.Sys,
			NumGC:       sample.NumGC,
			Goroutines:  sample.Goroutines,
			SampledAt:   sample.SampledAt,
		}
	}

	return out
}

// translate maps the engine's sentinels onto the server's. Anything unmapped is
// passed through: the server reports an unrecognized failure as a generic RESP
// error rather than swallowing it.
func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, storage.ErrValueTooLarge):
		return server.ErrValueTooLarge
	case errors.Is(err, storage.ErrOutOfMemory):
		return server.ErrOutOfMemory
	case errors.Is(err, storage.ErrInvalidKey):
		return server.ErrInvalidKey
	case errors.Is(err, storage.ErrScanCursorUnknown):
		return server.ErrScanCursorUnknown
	case errors.Is(err, storage.ErrTooManyScanCursors):
		return server.ErrTooManyScanCursors
	case errors.Is(err, storage.ErrScanMemoryExhausted):
		return server.ErrScanMemoryExhausted
	default:
		return err
	}
}
