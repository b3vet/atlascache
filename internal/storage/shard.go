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

	// mem is the engine-wide tracker. The shard owns both halves of the memory
	// accounting — its own counter and the global one — and moves them together
	// under the write lock, so the global total is the sum of the per-shard
	// ones at every instant. Splitting that bookkeeping across two layers is
	// what let ISSUE-0010's leak through.
	mem *MemoryTracker

	// Memory tracking
	memoryUsed uint64

	// keysWithTTL counts resident entries carrying an expiry. It is maintained
	// incrementally at every transition rather than recomputed, so Stats() does
	// not walk the keyspace (ISSUE-0012). Every adjustment happens under the
	// write lock; it is atomic only so readers need not take the lock. Signed
	// deliberately: a missed transition then shows up as a negative count
	// rather than as an enormous unsigned one.
	keysWithTTL atomic.Int64

	// lazyExpiration reports whether a read reclaims the entry it finds expired
	// rather than only reporting a miss. It is on unless ttl.lazy_expiration
	// turns it off; either way the read reports a miss.
	lazyExpiration atomic.Bool

	// Statistics (atomic)
	gets        uint64
	sets        uint64
	deletes     uint64
	hits        uint64
	misses      uint64
	expirations uint64
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
	Expirations uint64
}

// NewShard creates a new shard with the given initial capacity, accounting its
// memory into mem. A nil mem gives the shard a private unlimited tracker, which
// is what a shard exercised on its own wants.
func NewShard(initialCapacity int, mem *MemoryTracker) *Shard {
	if mem == nil {
		mem = NewMemoryTracker(0)
	}

	s := &Shard{
		data: make(map[string]*Entry, initialCapacity),
		mem:  mem,
	}
	s.lazyExpiration.Store(true)

	return s
}

// SetLazyExpiration turns passive reclamation on or off. With it off, a read of
// an expired key still misses, but the entry is left for active expiration to
// collect — which is what ttl.lazy_expiration: false asks for.
func (s *Shard) SetLazyExpiration(enabled bool) {
	s.lazyExpiration.Store(enabled)
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

	if entry.IsExpired() {
		// Passive expiration (ISSUE-0007): reclaim the entry now instead of
		// leaving a tombstone for the wheel to find, so a hot expired key costs
		// its memory for one read rather than until its slot fires. expire
		// re-checks under the write lock, so a key that came back to life in
		// the gap between the read above and the upgrade survives.
		if s.lazyExpiration.Load() {
			s.expire(key)
		}
		atomic.AddUint64(&s.misses, 1)
		return nil, false
	}

	entry.RecordAccess()
	atomic.AddUint64(&s.hits, 1)

	return entry, true
}

// Set stores a value in the shard, replacing whatever held the key.
//
// It returns ErrOutOfMemory when the store would push the engine past
// max_memory. The engine evicts and retries rather than handing that straight
// back to the caller.
func (s *Shard) Set(key string, entry *Entry) error {
	_, err := s.put(key, entry, false)
	return err
}

// SetNX stores a value only if no live entry holds the key, reporting whether
// it stored one.
//
// An expired entry does not block the write: it is displaced, and its memory is
// credited against the incoming entry. A live one does, and then nothing is
// stored and no memory is reserved — which is why a refused SetNX cannot
// trigger an eviction.
func (s *Shard) SetNX(key string, entry *Entry) (bool, error) {
	return s.put(key, entry, true)
}

// put is the one write path, shared by Set and SetNX so the two cannot diverge
// the way they did in ISSUE-0010.
//
// The reservation and the store happen in a single hold of the write lock. That
// is what makes the credit for a displaced entry exact: no concurrent writer
// can replace the resident entry between the two steps, so the global tracker
// and the shard counter move by the same amount or not at all.
func (s *Shard) put(key string, entry *Entry, onlyIfAbsent bool) (stored bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	old, present := s.data[key]
	if onlyIfAbsent && present && !old.IsExpired() {
		return false, nil
	}

	// An overwrite gives back what it displaces, so only the difference is new.
	var credit uint64
	if present {
		credit = uint64(old.Size)
	}
	if !s.mem.Reserve(uint64(entry.Size), credit) {
		return false, ErrOutOfMemory
	}

	if present {
		s.release(old)
	}
	s.store(key, entry)

	atomic.AddUint64(&s.sets, 1)

	return true, nil
}

// store inserts an entry and takes on its accounting.
// The caller must hold the write lock and have released any entry it displaces.
// It moves the shard's own counters only: the global tracker is moved by the
// Reserve that admitted the entry.
func (s *Shard) store(key string, entry *Entry) {
	s.data[key] = entry
	s.memoryUsed += uint64(entry.Size)
	if entry.HasTTL() {
		s.keysWithTTL.Add(1)
	}
}

// release drops the shard's accounting for an entry leaving it.
// The caller must hold the write lock and remove the entry itself: release is
// used both by deletion and by the overwrite paths, which replace rather than
// delete the map slot. Like store it leaves the global tracker alone; removal
// paths go through removeLocked, which settles both.
func (s *Shard) release(entry *Entry) {
	s.memoryUsed -= uint64(entry.Size)
	if entry.HasTTL() {
		s.keysWithTTL.Add(-1)
	}
}

// removeLocked drops the entry under key and settles both memory counters.
// The caller must hold the write lock, and must have read entry from the map
// under that same hold.
func (s *Shard) removeLocked(key string, entry *Entry) {
	delete(s.data, key)
	s.release(entry)
	s.mem.Sub(uint64(entry.Size))
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
		s.removeLocked(key, entry)
		atomic.AddUint64(&s.deletes, 1)
	}

	return entry, exists
}

// expire deletes the entry under key when it is genuinely expired, reporting
// whether it removed one.
//
// This is the single deletion path behind both expiration routes: the active
// one, which arrives through the engine's ttl.Keyspace seam, and the passive
// one in Get. Sharing it is what keeps the memory, TTL and expiration counters
// from diverging between the two.
//
// The expiry is re-checked here, under the write lock, rather than taken on
// trust from the caller: between the caller's read and this call another
// goroutine may have overwritten the key with a live value, and deleting then
// would destroy live data.
func (s *Shard) expire(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, exists := s.data[key]
	if !exists || !entry.IsExpired() {
		return false
	}

	s.removeLocked(key, entry)
	atomic.AddUint64(&s.expirations, 1)

	return true
}

// evict drops key to make room for a write, reporting whether it removed an
// entry. Unlike Delete this is not a client operation, so it counts as an
// eviction rather than as a delete.
func (s *Shard) evict(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, exists := s.data[key]
	if !exists {
		return false
	}

	s.removeLocked(key, entry)
	s.mem.RecordEviction()

	return true
}

// expiryOf reports the expiry of whatever holds key, in Unix nanoseconds,
// together with whether the key is there at all.
//
// An expired-but-resident entry reports present: that is exactly the case the
// TTL manager acts on, so hiding it the way GetEntry does would leave the
// active path unable to see anything to reclaim.
func (s *Shard) expiryOf(key string) (expireAt int64, present bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, exists := s.data[key]
	if !exists {
		return 0, false
	}

	return entry.ExpireAt.Load(), true
}

// residentSize reports the accounted size of whatever currently holds key,
// expired or not, and zero when the key is absent. It is what a write about to
// replace that entry may count against its own size.
func (s *Shard) residentSize(key string) uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, exists := s.data[key]
	if !exists {
		return 0
	}

	return uint64(entry.Size)
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

// liveKeys appends the candidates that are still resident, unexpired and
// matching to dst, as copies the caller owns. An empty match keeps everything.
//
// It is the read-time filter behind a scan page: the snapshot a cursor holds
// says which keys the shard had when the scan began, and this says which of
// them it still has. One lock acquisition covers the whole batch, so a page
// costs one round trip rather than one per key — and, more importantly, the
// keys in a batch are judged against a single consistent view of the shard.
//
// Expired-but-resident entries are treated as gone: a key that expired mid-scan
// must not surface just because nothing has reclaimed it yet.
//
// The glob is applied before the copy rather than after the page is returned,
// which is what keeps a filtered scan from allocating the keys it is about to
// discard. It costs the matcher a turn under the read lock; a glob over a page
// of at most maxScanCount keys is cheap beside the lock acquisition it shares.
func (s *Shard) liveKeys(candidates []string, match string, dst [][]byte) [][]byte {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, key := range candidates {
		entry, exists := s.data[key]
		if !exists || entry.IsExpired() {
			continue
		}
		if match != "" && !MatchGlob(match, key) {
			continue
		}
		dst = append(dst, []byte(key))
	}

	return dst
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
		Expirations: atomic.LoadUint64(&s.expirations),
	}
}

// Clear removes all entries from the shard
func (s *Shard) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.mem.Sub(s.memoryUsed)
	s.data = make(map[string]*Entry)
	s.memoryUsed = 0
	s.keysWithTTL.Store(0)
}
