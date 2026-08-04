package storage

import (
	"math"
	"sync/atomic"
	"time"
)

// Entry represents a stored key-value pair
type Entry struct {
	Key        []byte // The key (stored for eviction reference)
	Value      []byte // The actual value (max 1MB)
	ExpireAt   int64  // Unix nanoseconds, 0 = no expiry
	CreatedAt  int64  // Creation timestamp (Unix nano)
	LastAccess int64  // Last access timestamp (atomic, for LRU)
	AccessCnt  uint32 // Access counter (atomic, for LFU)
	Size       uint32 // Total size in bytes (key + value + overhead)
}

// NewEntry creates a new Entry with the given key, value, and TTL
func NewEntry(key, value []byte, ttl time.Duration) *Entry {
	now := time.Now().UnixNano()

	var expireAt int64
	if ttl > 0 {
		expireAt = now + int64(ttl)
	}

	return &Entry{
		Key:        key,
		Value:      value,
		ExpireAt:   expireAt,
		CreatedAt:  now,
		LastAccess: now,
		AccessCnt:  1,
		Size:       CalculateSize(key, value),
	}
}

// IsExpired checks if the entry has expired
func (e *Entry) IsExpired() bool {
	if e.ExpireAt == 0 {
		return false
	}
	return time.Now().UnixNano() > e.ExpireAt
}

// TTL returns the remaining time-to-live
// Returns -1 if no expiry, 0 if expired, positive duration otherwise
func (e *Entry) TTL() time.Duration {
	if e.ExpireAt == 0 {
		return -1 // No expiry
	}
	remaining := e.ExpireAt - time.Now().UnixNano()
	if remaining <= 0 {
		return 0 // Expired
	}
	return time.Duration(remaining)
}

// RecordAccess updates access metadata atomically
func (e *Entry) RecordAccess() {
	atomic.StoreInt64(&e.LastAccess, time.Now().UnixNano())
	atomic.AddUint32(&e.AccessCnt, 1)
}

// GetLastAccess returns the last access time atomically
func (e *Entry) GetLastAccess() int64 {
	return atomic.LoadInt64(&e.LastAccess)
}

// GetAccessCount returns the access count atomically
func (e *Entry) GetAccessCount() uint32 {
	return atomic.LoadUint32(&e.AccessCnt)
}

// UpdateExpiry updates the expiration time
func (e *Entry) UpdateExpiry(ttl time.Duration) {
	if ttl <= 0 {
		atomic.StoreInt64(&e.ExpireAt, 0)
	} else {
		atomic.StoreInt64(&e.ExpireAt, time.Now().UnixNano()+int64(ttl))
	}
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
