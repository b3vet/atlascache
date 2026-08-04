package storage

import (
	"sync"
	"sync/atomic"
	"time"
)

// Shard represents a single partition of the storage
type Shard struct {
	mu   sync.RWMutex
	data map[string]*Entry

	// Memory tracking
	memoryUsed uint64

	// keysWithTTL counts resident entries carrying an expiry. It is maintained
	// incrementally at every transition rather than recomputed, so Stats() does
	// not walk the keyspace (ISSUE-0012). Every adjustment happens under the
	// write lock; it is atomic only so readers need not take the lock. Signed
	// deliberately: a missed transition then shows up as a negative count
	// rather than as an enormous unsigned one.
	keysWithTTL atomic.Int64

	// Statistics (atomic)
	gets    uint64
	sets    uint64
	deletes uint64
	hits    uint64
	misses  uint64
}

// ShardStats holds statistics for a single shard
type ShardStats struct {
	Keys        int
	KeysWithTTL int64
	MemoryUsed  uint64
	Gets        uint64
	Sets        uint64
	Deletes     uint64
	Hits        uint64
	Misses      uint64
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

// Set stores a value in the shard, replacing whatever held the key.
//
// Returns the entry that was already under the key and whether there was one.
// SetNX returns the same shape and gives the two results the same meaning; the
// asymmetry between them was the root cause of ISSUE-0010.
func (s *Shard) Set(key string, entry *Entry) (old *Entry, existed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	old, existed = s.data[key]
	if existed {
		s.release(old)
	}
	s.store(key, entry)

	atomic.AddUint64(&s.sets, 1)

	return old, existed
}

// SetNX stores a value only if no live entry holds the key.
//
// Returns the entry that was already under the key and whether that entry
// blocked the write. An expired entry does not block: it is displaced and
// returned with existed false, so the caller can subtract its size from the
// global tracker the way it does after Set. Nothing is stored when existed is
// true, and the returned entry must not be subtracted in that case.
func (s *Shard) SetNX(key string, entry *Entry) (old *Entry, existed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	old, present := s.data[key]
	if present && !old.IsExpired() {
		return old, true
	}

	if present {
		s.release(old)
	}
	s.store(key, entry)

	atomic.AddUint64(&s.sets, 1)

	return old, false
}

// store inserts an entry and takes on its accounting.
// The caller must hold the write lock and have released any entry it displaces.
func (s *Shard) store(key string, entry *Entry) {
	s.data[key] = entry
	s.memoryUsed += uint64(entry.Size)
	if entry.HasTTL() {
		s.keysWithTTL.Add(1)
	}
}

// release drops the accounting for an entry leaving the shard.
// The caller must hold the write lock and remove the entry itself: release is
// used both by deletion and by the overwrite paths, which replace rather than
// delete the map slot.
func (s *Shard) release(entry *Entry) {
	s.memoryUsed -= uint64(entry.Size)
	if entry.HasTTL() {
		s.keysWithTTL.Add(-1)
	}
}

// SetTTL updates the expiry of a live entry, reporting whether it found one.
//
// This runs under the write lock rather than beside it: keysWithTTL is derived
// from the before/after state of the entry's expiry, and two concurrent
// updates reading the same "had no TTL" would each add one.
func (s *Shard) SetTTL(key string, ttl time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, exists := s.data[key]
	if !exists || entry.IsExpired() {
		return false
	}

	had := entry.HasTTL()
	entry.UpdateExpiry(ttl)

	switch has := entry.HasTTL(); {
	case !had && has:
		s.keysWithTTL.Add(1)
	case had && !has:
		s.keysWithTTL.Add(-1)
	}

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
		s.release(entry)
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

// KeysWithTTL returns the number of resident entries carrying an expiry,
// including entries that have expired but not yet been reaped.
func (s *Shard) KeysWithTTL() int64 {
	return s.keysWithTTL.Load()
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

// ExpireKeys removes expired keys, returning how many went and how many bytes
// they freed so the caller can settle the global memory tracker.
func (s *Shard) ExpireKeys(maxCount int) (expired int, freed uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key, entry := range s.data {
		if !entry.IsExpired() {
			continue
		}
		delete(s.data, key)
		s.release(entry)
		freed += uint64(entry.Size)
		expired++
		if maxCount > 0 && expired >= maxCount {
			break
		}
	}
	return expired, freed
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
		Keys:        keyCount,
		KeysWithTTL: s.keysWithTTL.Load(),
		MemoryUsed:  memUsed,
		Gets:        atomic.LoadUint64(&s.gets),
		Sets:        atomic.LoadUint64(&s.sets),
		Deletes:     atomic.LoadUint64(&s.deletes),
		Hits:        atomic.LoadUint64(&s.hits),
		Misses:      atomic.LoadUint64(&s.misses),
	}
}

// Clear removes all entries from the shard
func (s *Shard) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data = make(map[string]*Entry)
	s.memoryUsed = 0
	s.keysWithTTL.Store(0)
}
