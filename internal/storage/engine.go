package storage

import (
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cespare/xxhash/v2"
)

// StorageEngine defines the interface for the key-value storage.
//
// # Ownership of key and value bytes (ADR-0013)
//
// Writes copy. Set and SetNX copy the key and value into memory the entry
// owns, so a caller may reuse its buffers the instant the call returns — which
// is what a connection handler reading into a shared buffer needs.
//
// Reads do not copy. Get returns the engine's own slice, under a contract the
// caller must honor:
//
//   - do not mutate the returned slice; it is the stored value, and writing to
//     it corrupts the cache from what looks like a read-only operation;
//   - do not retain it past the current operation; copy it if it must outlive
//     the request.
//
// This is safe because no code path mutates a live entry's backing array. Set
// builds a new Entry and swaps the map pointer rather than writing over the
// old one, so a reader holding a superseded view sees valid, if stale, bytes,
// and the garbage collector keeps the array alive for as long as the view
// does. Expiration and eviction likewise only drop references.
//
// That invariant is load-bearing, not incidental. Any in-place update path — an
// APPEND command, a value patched in place, a pooled entry buffer — breaks the
// read contract for every concurrent reader and cannot be added without
// revisiting ADR-0013.
type StorageEngine interface {
	// Get returns the stored value as a read-only view owned by the engine.
	// The caller must neither mutate the result nor retain it past the current
	// operation. See the ownership note on this interface.
	Get(key []byte) (value []byte, ttl time.Duration, exists bool)

	// Set stores a copy of key and value. The caller may reuse both buffers as
	// soon as it returns.
	Set(key, value []byte, ttl time.Duration) error

	// SetNX stores a copy of key and value if no live entry holds the key,
	// under the same memory limit as Set.
	SetNX(key, value []byte, ttl time.Duration) (bool, error)

	Delete(key []byte) bool
	Exists(key []byte) bool

	// TTL operations
	GetTTL(key []byte) (time.Duration, bool)
	SetTTL(key []byte, ttl time.Duration) bool

	// Iteration
	Keys(pattern string) [][]byte
	Scan(cursor uint64, count int) (keys [][]byte, nextCursor uint64)

	// Internal access
	GetEntry(key []byte) (*Entry, bool)
	GetShard(key []byte) *Shard
	GetAllShards() []*Shard

	// Statistics
	Stats() Stats

	// Memory management
	MemoryUsed() uint64
	MaxMemory() uint64

	// Lifecycle
	Close() error
}

// Stats contains storage statistics
type Stats struct {
	Keys        uint64 // Total number of keys
	KeysWithTTL uint64 // Keys with TTL set
	MemoryUsed  uint64 // Bytes used by entries
	MemoryMax   uint64 // Maximum memory (0 = unlimited)
	Gets        uint64 // Total GET operations
	Sets        uint64 // Total SET operations
	Deletes     uint64 // Total DELETE operations
	Hits        uint64 // Cache hits
	Misses      uint64 // Cache misses
	Evictions   uint64 // Keys evicted
	Expirations uint64 // Keys expired
	OOMRejected uint64 // Requests rejected due to OOM
}

// EngineConfig holds configuration for the storage engine
type EngineConfig struct {
	ShardCount   int
	MaxMemory    uint64
	MaxValueSize uint64
}

// DefaultEngineConfig returns default configuration
func DefaultEngineConfig() EngineConfig {
	return EngineConfig{
		ShardCount:   64,
		MaxMemory:    0,               // Unlimited
		MaxValueSize: 1 * 1024 * 1024, // 1MB
	}
}

var _ StorageEngine = (*ShardedEngine)(nil)

// ShardedEngine implements StorageEngine with sharding for concurrency
type ShardedEngine struct {
	shards     []*Shard
	shardCount uint64
	shardMask  uint64

	maxValueSize uint64
	memory       *MemoryTracker

	// Expiration tracking
	expirations uint64

	// Lifecycle
	closed int32
}

// NewShardedEngine creates a new sharded storage engine
func NewShardedEngine(cfg EngineConfig) *ShardedEngine {
	// Ensure shard count is power of 2 for efficient modulo
	shardCount := cfg.ShardCount
	if shardCount <= 0 {
		shardCount = 64
	}
	shardCount = nextPowerOfTwo(shardCount)

	shards := make([]*Shard, shardCount)
	for i := range shards {
		shards[i] = NewShard(1024)
	}

	// nextPowerOfTwo guarantees shardCount >= 1, so these conversions cannot wrap.
	return &ShardedEngine{
		shards:       shards,
		shardCount:   uint64(shardCount),     //nolint:gosec // bounded positive by nextPowerOfTwo
		shardMask:    uint64(shardCount - 1), //nolint:gosec // bounded positive by nextPowerOfTwo
		maxValueSize: cfg.MaxValueSize,
		memory:       NewMemoryTracker(cfg.MaxMemory),
	}
}

// nextPowerOfTwo returns the next power of 2 >= n
func nextPowerOfTwo(n int) int {
	if n <= 0 {
		return 1
	}
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n++
	return n
}

// getShard returns the shard for a given key using xxhash
func (e *ShardedEngine) getShard(key []byte) *Shard {
	hash := xxhash.Sum64(key)
	return e.shards[hash&e.shardMask]
}

// Get retrieves a value from the storage.
// The returned slice is the engine's own memory: read-only to the caller, and
// valid only for the current operation. See the StorageEngine ownership note.
func (e *ShardedEngine) Get(key []byte) (value []byte, ttl time.Duration, exists bool) {
	if atomic.LoadInt32(&e.closed) == 1 {
		return nil, 0, false
	}

	shard := e.getShard(key)
	entry, exists := shard.Get(string(key))
	if !exists {
		return nil, 0, false
	}

	return entry.Value, entry.TTL(), true
}

// validateWrite runs the checks Set and SetNX share, and builds the entry.
func (e *ShardedEngine) validateWrite(key, value []byte, ttl time.Duration) (*Entry, error) {
	if atomic.LoadInt32(&e.closed) == 1 {
		return nil, ErrEngineClosed
	}

	if len(key) == 0 {
		return nil, ErrInvalidKey
	}

	if uint64(len(value)) > e.maxValueSize {
		return nil, ErrValueTooLarge
	}

	if ttl < 0 {
		return nil, ErrInvalidTTL
	}

	return NewEntry(key, value, ttl), nil
}

// admit is the single place max_memory is enforced, shared by Set and SetNX so
// the two cannot diverge the way they did in ISSUE-0010.
//
// FEAT-0013 takes this over: it makes room by evicting instead of rejecting,
// and widens the signature to admit(shard *Shard, entry *Entry) error so the
// eviction controller can sample the shard the entry is bound for. Both call
// sites already pass through here, so that change stays inside this function.
func (e *ShardedEngine) admit(entry *Entry) error {
	maxMemory := e.memory.GetMaxMemory()
	if maxMemory == 0 {
		return nil
	}

	if e.memory.GetUsedMemory()+uint64(entry.Size) > maxMemory {
		// Memory would exceed limit - caller should handle eviction
		e.memory.RecordOOMRejection()
		return ErrOutOfMemory
	}

	return nil
}

// Set stores a value with optional TTL
func (e *ShardedEngine) Set(key, value []byte, ttl time.Duration) error {
	entry, err := e.validateWrite(key, value, ttl)
	if err != nil {
		return err
	}

	if err := e.admit(entry); err != nil {
		return err
	}

	shard := e.getShard(key)
	old, existed := shard.Set(string(key), entry)

	// Update memory tracker
	if existed {
		e.memory.Sub(uint64(old.Size))
	}
	e.memory.Add(uint64(entry.Size))

	return nil
}

// SetNX sets a value only if no live entry holds the key
func (e *ShardedEngine) SetNX(key, value []byte, ttl time.Duration) (bool, error) {
	entry, err := e.validateWrite(key, value, ttl)
	if err != nil {
		return false, err
	}

	if err := e.admit(entry); err != nil {
		return false, err
	}

	shard := e.getShard(key)
	old, existed := shard.SetNX(string(key), entry)
	if existed {
		return false, nil
	}

	// An expired entry may have been displaced; the shard has already dropped
	// it from its own counter, so the global tracker has to hear about it too.
	if old != nil {
		e.memory.Sub(uint64(old.Size))
	}
	e.memory.Add(uint64(entry.Size))

	return true, nil
}

// Delete removes a key from the storage
func (e *ShardedEngine) Delete(key []byte) bool {
	if atomic.LoadInt32(&e.closed) == 1 {
		return false
	}

	shard := e.getShard(key)
	old, existed := shard.Delete(string(key))

	if existed {
		e.memory.Sub(uint64(old.Size))
	}

	return existed
}

// Exists checks if a key exists
func (e *ShardedEngine) Exists(key []byte) bool {
	if atomic.LoadInt32(&e.closed) == 1 {
		return false
	}

	shard := e.getShard(key)
	return shard.Exists(string(key))
}

// GetTTL returns the remaining TTL for a key
func (e *ShardedEngine) GetTTL(key []byte) (time.Duration, bool) {
	if atomic.LoadInt32(&e.closed) == 1 {
		return 0, false
	}

	shard := e.getShard(key)
	entry, exists := shard.GetEntry(string(key))
	if !exists {
		return 0, false
	}

	return entry.TTL(), true
}

// SetTTL updates the TTL for a key
func (e *ShardedEngine) SetTTL(key []byte, ttl time.Duration) bool {
	if atomic.LoadInt32(&e.closed) == 1 {
		return false
	}

	return e.getShard(key).SetTTL(string(key), ttl)
}

// GetEntry returns the entry for internal use
func (e *ShardedEngine) GetEntry(key []byte) (*Entry, bool) {
	if atomic.LoadInt32(&e.closed) == 1 {
		return nil, false
	}

	shard := e.getShard(key)
	return shard.GetEntry(string(key))
}

// GetShard returns the shard for a key
func (e *ShardedEngine) GetShard(key []byte) *Shard {
	return e.getShard(key)
}

// GetAllShards returns all shards
func (e *ShardedEngine) GetAllShards() []*Shard {
	return e.shards
}

// Keys returns all keys matching a pattern
// Pattern supports * (match any characters) and ? (match single character)
func (e *ShardedEngine) Keys(pattern string) [][]byte {
	if atomic.LoadInt32(&e.closed) == 1 {
		return nil
	}

	var keys [][]byte
	matchAll := pattern == "*" || pattern == ""

	for _, shard := range e.shards {
		shardKeys := shard.Keys()
		for _, key := range shardKeys {
			if matchAll || matchPattern(pattern, key) {
				keys = append(keys, []byte(key))
			}
		}
	}

	return keys
}

// matchPattern matches a key against a glob pattern
func matchPattern(pattern, key string) bool {
	// Use filepath.Match for glob-style matching
	matched, err := filepath.Match(pattern, key)
	if err != nil {
		// If pattern is invalid, try simple prefix/suffix matching
		if strings.HasPrefix(pattern, "*") && strings.HasSuffix(pattern, "*") {
			return strings.Contains(key, pattern[1:len(pattern)-1])
		}
		if strings.HasPrefix(pattern, "*") {
			return strings.HasSuffix(key, pattern[1:])
		}
		if strings.HasSuffix(pattern, "*") {
			return strings.HasPrefix(key, pattern[:len(pattern)-1])
		}
		return key == pattern
	}
	return matched
}

// Scan iterates over keys with cursor-based pagination
func (e *ShardedEngine) Scan(cursor uint64, count int) (keys [][]byte, nextCursor uint64) {
	if atomic.LoadInt32(&e.closed) == 1 {
		return nil, 0
	}

	if count <= 0 {
		count = 10
	}

	// Calculate starting shard and position within shard
	shardIdx := cursor / 1000000
	posInShard := cursor % 1000000

	keys = make([][]byte, 0, count)

	for shardIdx < e.shardCount && len(keys) < count {
		shard := e.shards[shardIdx]
		shardKeys := shard.Keys()

		// Skip to position in shard
		start := int(posInShard)
		if start >= len(shardKeys) {
			shardIdx++
			posInShard = 0
			continue
		}

		// Collect keys from this shard
		end := start + count - len(keys)
		if end > len(shardKeys) {
			end = len(shardKeys)
		}

		for i := start; i < end; i++ {
			keys = append(keys, []byte(shardKeys[i]))
		}

		// Update position
		posInShard = uint64(end)
		if posInShard >= uint64(len(shardKeys)) {
			shardIdx++
			posInShard = 0
		}
	}

	// Calculate next cursor
	if shardIdx >= e.shardCount {
		nextCursor = 0 // Scan complete
	} else {
		nextCursor = shardIdx*1000000 + posInShard
	}

	return keys, nextCursor
}

// Stats returns aggregated statistics
func (e *ShardedEngine) Stats() Stats {
	var stats Stats

	memStats := e.memory.Stats()
	stats.MemoryUsed = memStats.Used
	stats.MemoryMax = memStats.Max
	stats.Evictions = memStats.Evictions
	stats.OOMRejected = memStats.OOMRejected
	stats.Expirations = atomic.LoadUint64(&e.expirations)

	// One pass, and no traversal within it: every field below is a counter the
	// shard maintains as it goes, so Stats() costs O(shards) (ISSUE-0012).
	for _, shard := range e.shards {
		shardStats := shard.Stats()
		stats.Keys += uint64(shardStats.Keys) //nolint:gosec // len() of a map, never negative
		if shardStats.KeysWithTTL > 0 {
			stats.KeysWithTTL += uint64(shardStats.KeysWithTTL)
		}
		stats.Gets += shardStats.Gets
		stats.Sets += shardStats.Sets
		stats.Deletes += shardStats.Deletes
		stats.Hits += shardStats.Hits
		stats.Misses += shardStats.Misses
	}

	return stats
}

// MemoryUsed returns current memory usage
func (e *ShardedEngine) MemoryUsed() uint64 {
	return e.memory.GetUsedMemory()
}

// MaxMemory returns the maximum memory limit
func (e *ShardedEngine) MaxMemory() uint64 {
	return e.memory.GetMaxMemory()
}

// SetMaxMemory updates the maximum memory limit
func (e *ShardedEngine) SetMaxMemory(max uint64) {
	e.memory.SetMaxMemory(max)
}

// RecordExpiration increments the expiration counter
func (e *ShardedEngine) RecordExpiration() {
	atomic.AddUint64(&e.expirations, 1)
}

// RecordEviction records an eviction event
func (e *ShardedEngine) RecordEviction() {
	e.memory.RecordEviction()
}

// DeleteExpired removes an expired key and updates memory
func (e *ShardedEngine) DeleteExpired(key []byte) bool {
	shard := e.getShard(key)
	old, existed := shard.Delete(string(key))

	if existed {
		e.memory.Sub(uint64(old.Size))
		atomic.AddUint64(&e.expirations, 1)
	}

	return existed
}

// ExpireKeysInShard expires keys in a specific shard
func (e *ShardedEngine) ExpireKeysInShard(shardIdx int, maxCount int) int {
	if shardIdx < 0 || shardIdx >= len(e.shards) {
		return 0
	}

	expired, freed := e.shards[shardIdx].ExpireKeys(maxCount)
	if expired > 0 {
		e.memory.Sub(freed)
		atomic.AddUint64(&e.expirations, uint64(expired))
	}

	return expired
}

// Close shuts down the engine
func (e *ShardedEngine) Close() error {
	if !atomic.CompareAndSwapInt32(&e.closed, 0, 1) {
		return ErrEngineClosed
	}

	// Clear all shards
	for _, shard := range e.shards {
		shard.Clear()
	}

	e.memory.Set(0)

	return nil
}

// IsClosed returns true if the engine is closed
func (e *ShardedEngine) IsClosed() bool {
	return atomic.LoadInt32(&e.closed) == 1
}
