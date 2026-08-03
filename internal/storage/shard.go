package storage

import (
	"sync"
	"sync/atomic"
)

// Shard represents a single partition of the storage
type Shard struct {
	mu   sync.RWMutex
	data map[string]*Entry

	// Memory tracking
	memoryUsed uint64

	// Statistics (atomic)
	gets    uint64
	sets    uint64
	deletes uint64
	hits    uint64
	misses  uint64
}

// ShardStats holds statistics for a single shard
type ShardStats struct {
	Keys       int
	MemoryUsed uint64
	Gets       uint64
	Sets       uint64
	Deletes    uint64
	Hits       uint64
	Misses     uint64
}

// NewShard creates a new shard with the given initial capacity
func NewShard(initialCapacity int) *Shard {
	return &Shard{
		data: make(map[string]*Entry, initialCapacity),
	}
}

// Get retrieves a value from the shard
// Returns the entry and whether it exists (and is not expired)
func (s *Shard) Get(key string) (*Entry, bool) {
	s.mu.RLock()
	entry, exists := s.data[key]
	s.mu.RUnlock()

	atomic.AddUint64(&s.gets, 1)

	if !exists {
		atomic.AddUint64(&s.misses, 1)
		return nil, false
	}

	// Passive expiration check
	if entry.IsExpired() {
		// Don't delete here - let active expiration handle it
		// Just report as not found
		atomic.AddUint64(&s.misses, 1)
		return nil, false
	}

	entry.RecordAccess()
	atomic.AddUint64(&s.hits, 1)

	return entry, true
}

// Set stores a value in the shard
// Returns the old entry if it existed, and whether an old entry was replaced
func (s *Shard) Set(key string, entry *Entry) (*Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	old, existed := s.data[key]
	s.data[key] = entry

	// Update memory tracking
	if existed {
		s.memoryUsed -= uint64(old.Size)
	}
	s.memoryUsed += uint64(entry.Size)

	atomic.AddUint64(&s.sets, 1)

	return old, existed
}

// SetNX stores a value only if the key does not exist
// Returns true if the value was set, false if key already exists
func (s *Shard) SetNX(key string, entry *Entry) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, exists := s.data[key]
	if exists && !existing.IsExpired() {
		return false
	}

	// If there was an expired entry, clean it up
	if exists {
		s.memoryUsed -= uint64(existing.Size)
	}

	s.data[key] = entry
	s.memoryUsed += uint64(entry.Size)

	atomic.AddUint64(&s.sets, 1)

	return true
}

// Delete removes a value from the shard
// Returns the deleted entry and whether it existed
func (s *Shard) Delete(key string) (*Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, exists := s.data[key]
	if exists {
		delete(s.data, key)
		s.memoryUsed -= uint64(entry.Size)
		atomic.AddUint64(&s.deletes, 1)
	}

	return entry, exists
}

// Exists checks if a key exists (without updating access stats)
func (s *Shard) Exists(key string) bool {
	s.mu.RLock()
	entry, exists := s.data[key]
	s.mu.RUnlock()

	if !exists {
		return false
	}

	return !entry.IsExpired()
}

// GetEntry retrieves an entry without updating stats (for internal use)
func (s *Shard) GetEntry(key string) (*Entry, bool) {
	s.mu.RLock()
	entry, exists := s.data[key]
	s.mu.RUnlock()

	if !exists || entry.IsExpired() {
		return nil, false
	}

	return entry, true
}

// Len returns the number of entries in the shard
func (s *Shard) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data)
}

// MemoryUsed returns the memory used by this shard
func (s *Shard) MemoryUsed() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.memoryUsed
}

// Keys returns all non-expired keys in this shard
func (s *Shard) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	keys := make([]string, 0, len(s.data))
	for k, entry := range s.data {
		if !entry.IsExpired() {
			keys = append(keys, k)
		}
	}
	return keys
}

// AllKeys returns all keys including expired ones (for internal use)
func (s *Shard) AllKeys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	keys := make([]string, 0, len(s.data))
	for k := range s.data {
		keys = append(keys, k)
	}
	return keys
}

// ExpireKeys removes expired keys and returns count
func (s *Shard) ExpireKeys(maxCount int) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	expired := 0
	for key, entry := range s.data {
		if entry.IsExpired() {
			delete(s.data, key)
			s.memoryUsed -= uint64(entry.Size)
			expired++
			if maxCount > 0 && expired >= maxCount {
				break
			}
		}
	}
	return expired
}

// Sample returns a random sample of entries for eviction
// The map iteration is pseudo-random which is sufficient for sampling
func (s *Shard) Sample(count int) []*Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	samples := make([]*Entry, 0, count)
	for _, entry := range s.data {
		if !entry.IsExpired() {
			samples = append(samples, entry)
			if len(samples) >= count {
				break
			}
		}
	}
	return samples
}

// ForEach iterates over all non-expired entries
func (s *Shard) ForEach(fn func(key string, entry *Entry) bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for k, entry := range s.data {
		if !entry.IsExpired() {
			if !fn(k, entry) {
				break
			}
		}
	}
}

// Stats returns statistics for this shard
func (s *Shard) Stats() ShardStats {
	s.mu.RLock()
	keyCount := len(s.data)
	memUsed := s.memoryUsed
	s.mu.RUnlock()

	return ShardStats{
		Keys:       keyCount,
		MemoryUsed: memUsed,
		Gets:       atomic.LoadUint64(&s.gets),
		Sets:       atomic.LoadUint64(&s.sets),
		Deletes:    atomic.LoadUint64(&s.deletes),
		Hits:       atomic.LoadUint64(&s.hits),
		Misses:     atomic.LoadUint64(&s.misses),
	}
}

// Clear removes all entries from the shard
func (s *Shard) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data = make(map[string]*Entry)
	s.memoryUsed = 0
}
