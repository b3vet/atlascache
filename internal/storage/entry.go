package storage

import (
	"math"
	"sync/atomic"
	"time"
)

// Entry represents a stored key-value pair.
//
// Key and Value are slices of a single buffer the entry owns, allocated by
// NewEntry. Nothing ever writes to that buffer after construction, which is
// what allows StorageEngine.Get to hand back an internal view instead of a
// copy — see the contract documented there and in ADR-0013.
//
// CreatedAt and Size are also fixed at construction. They need no atomic
// access: an entry is published into a shard under the shard's write lock and
// read under its read lock, which orders the construction before every read.
// ExpireAt, LastAccess and AccessCnt do change while an entry is live, so they
// are typed atomics — the untyped form let ISSUE-0008 mix an atomic store with
// a plain load, and the typed form makes that impossible to write.
type Entry struct {
	Key        []byte        // The key (stored for eviction reference)
	Value      []byte        // The actual value (max 1MB)
	ExpireAt   atomic.Int64  // Unix nanoseconds, 0 = no expiry
	LastAccess atomic.Int64  // Last access timestamp, for LRU
	AccessCnt  atomic.Uint32 // Access counter, for LFU
	CreatedAt  int64         // Creation timestamp (Unix nano)
	Size       uint32        // Total size in bytes (key + value + overhead)
}

// NewEntry creates a new Entry owning a copy of the given key and value.
//
// Both live in one buffer rather than two: it halves the allocation count and
// keeps the key next to its value in memory. Key is capped with a three-index
// slice so an append to it cannot reach into Value.
func NewEntry(key, value []byte, ttl time.Duration) *Entry {
	now := time.Now().UnixNano()

	buf := make([]byte, len(key)+len(value))
	copy(buf, key)
	copy(buf[len(key):], value)

	e := &Entry{
		Key:       buf[:len(key):len(key)],
		Value:     buf[len(key):],
		CreatedAt: now,
	}
	e.Size = CalculateSize(e.Key, e.Value)

	if ttl > 0 {
		e.ExpireAt.Store(now + int64(ttl))
	}
	e.LastAccess.Store(now)
	e.AccessCnt.Store(1)

	return e
}

// HasTTL reports whether the entry carries an expiry.
func (e *Entry) HasTTL() bool {
	return e.ExpireAt.Load() != 0
}

// IsExpired checks if the entry has expired
func (e *Entry) IsExpired() bool {
	expireAt := e.ExpireAt.Load()
	if expireAt == 0 {
		return false
	}
	return time.Now().UnixNano() > expireAt
}

// TTL returns the remaining time-to-live
// Returns -1 if no expiry, 0 if expired, positive duration otherwise
func (e *Entry) TTL() time.Duration {
	expireAt := e.ExpireAt.Load()
	if expireAt == 0 {
		return -1 // No expiry
	}
	remaining := expireAt - time.Now().UnixNano()
	if remaining <= 0 {
		return 0 // Expired
	}
	return time.Duration(remaining)
}

// RecordAccess updates access metadata
func (e *Entry) RecordAccess() {
	e.LastAccess.Store(time.Now().UnixNano())
	e.AccessCnt.Add(1)
}

// UpdateExpiry updates the expiration time.
// Callers must hold the owning shard's write lock: the shard's keysWithTTL
// counter is derived from this transition and would drift if two goroutines
// changed the same entry's expiry concurrently.
func (e *Entry) UpdateExpiry(ttl time.Duration) {
	if ttl <= 0 {
		e.ExpireAt.Store(0)
		return
	}
	e.ExpireAt.Store(time.Now().UnixNano() + int64(ttl))
}

// EntryOverhead is the fixed memory overhead per entry (bytes)
// Calculated as: struct fields + map entry overhead + slice headers
const EntryOverhead = 80 // Approximate: 24 (key slice) + 24 (value slice) + 8+8+8+4+4 + map overhead (~10)

// CalculateSize computes the total memory footprint of an entry.
// Saturates at MaxUint32 rather than wrapping: an entry large enough to wrap
// would otherwise be accounted as tiny and slip past the memory limit.
func CalculateSize(key, value []byte) uint32 {
	total := len(key) + len(value) + EntryOverhead
	if total < 0 || total > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(total)
}
