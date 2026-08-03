package storage

import (
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cespare/xxhash/v2"
)

// StorageEngine defines the interface for the key-value storage
type StorageEngine interface {
	// Basic operations
	Get(key []byte) (value []byte, ttl time.Duration, exists bool)
	Set(key, value []byte, ttl time.Duration) error
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
	Keys           uint64 // Total number of keys
	KeysWithTTL    uint64 // Keys with TTL set
	MemoryUsed     uint64 // Bytes used by entries
	MemoryMax      uint64 // Maximum memory (0 = unlimited)
	Gets           uint64 // Total GET operations
	Sets           uint64 // Total SET operations
	Deletes        uint64 // Total DELETE operations
	Hits           uint64 // Cache hits
	Misses         uint64 // Cache misses
	Evictions      uint64 // Keys evicted
	Expirations    uint64 // Keys expired
	OOMRejected    uint64 // Requests rejected due to OOM
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
		MaxMemory:    0, // Unlimited
		MaxValueSize: 1 * 1024 * 1024, // 1MB
	}
}

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
	mu     sync.RWMutex
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

	return &ShardedEngine{
		shards:       shards,
		shardCount:   uint64(shardCount),
		shardMask:    uint64(shardCount - 1),
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

// getShardByString returns the shard for a string key
func (e *ShardedEngine) getShardByString(key string) *Shard {
	hash := xxhash.Sum64String(key)
	return e.shards[hash&e.shardMask]
}

// Get retrieves a value from the storage
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

// Set stores a value with optional TTL
func (e *ShardedEngine) Set(key, value []byte, ttl time.Duration) error {
	if atomic.LoadInt32(&e.closed) == 1 {
		return ErrEngineClosed
	}

	if len(key) == 0 {
		return ErrInvalidKey
	}

	if uint64(len(value)) > e.maxValueSize {
		return ErrValueTooLarge
	}

	if ttl < 0 {
		return ErrInvalidTTL
	}

	entry := NewEntry(key, value, ttl)

	// Check memory before setting
	if e.memory.GetMaxMemory() > 0 {
		newSize := uint64(entry.Size)
		if e.memory.GetUsedMemory()+newSize > e.memory.GetMaxMemory() {
			// Memory would exceed limit - caller should handle eviction
			e.memory.RecordOOMRejection()
			return ErrOutOfMemory
		}
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

// SetNX sets a value only if the key does not exist
func (e *ShardedEngine) SetNX(key, value []byte, ttl time.Duration) (bool, error) {
	if atomic.LoadInt32(&e.closed) == 1 {
		return false, ErrEngineClosed
	}

	if len(key) == 0 {
		return false, ErrInvalidKey
	}

	if uint64(len(value)) > e.maxValueSize {
		return false, ErrValueTooLarge
	}

	if ttl < 0 {
		return false, ErrInvalidTTL
	}

	entry := NewEntry(key, value, ttl)

	shard := e.getShard(key)
	if !shard.SetNX(string(key), entry) {
		return false, nil
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

	shard := e.getShard(key)
	entry, exists := shard.GetEntry(string(key))
	if !exists {
		return false
	}

	entry.UpdateExpiry(ttl)
	return true
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

	for _, shard := range e.shards {
		shardStats := shard.Stats()
		stats.Keys += uint64(shardStats.Keys)
		stats.Gets += shardStats.Gets
		stats.Sets += shardStats.Sets
		stats.Deletes += shardStats.Deletes
		stats.Hits += shardStats.Hits
		stats.Misses += shardStats.Misses
	}

	// Count keys with TTL
	for _, shard := range e.shards {
		shard.ForEach(func(key string, entry *Entry) bool {
			if entry.ExpireAt > 0 {
				stats.KeysWithTTL++
			}
			return true
		})
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

	shard := e.shards[shardIdx]

	// Get expired keys
	shard.mu.Lock()
	expired := 0
	for key, entry := range shard.data {
		if entry.IsExpired() {
			delete(shard.data, key)
			shard.memoryUsed -= uint64(entry.Size)
			e.memory.Sub(uint64(entry.Size))
			atomic.AddUint64(&e.expirations, 1)
			expired++
			if maxCount > 0 && expired >= maxCount {
				break
			}
		}
	}
	shard.mu.Unlock()

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
