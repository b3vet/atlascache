package storage

import (
	"errors"
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

	// Scan returns one page of a snapshot scan owned by owner. See
	// ShardedEngine.Scan and the package note in scan.go for the guarantee it
	// offers and the bounds that hold it in place.
	Scan(owner ScanOwner, cursor string, count int) (keys [][]byte, nextCursor string, err error)

	// ReleaseScans drops every scan cursor owned by owner. A connection handler
	// must call it when the connection goes away.
	ReleaseScans(owner ScanOwner)

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

	// ScanCursors is the number of scans currently open, and
	// ScanSnapshotBytes what their snapshots are accounted at.
	//
	// Neither is part of MemoryUsed and neither counts against max_memory
	// (ADR-0017). Charging snapshots to the data budget would let a large scan
	// evict the very keys it is scanning, so they are reported here instead —
	// visible, and separate.
	ScanCursors       uint64
	ScanSnapshotBytes uint64
}

// EngineConfig holds configuration for the storage engine
type EngineConfig struct {
	ShardCount   int
	MaxMemory    uint64
	MaxValueSize uint64

	// DisableLazyExpiration stops reads from reclaiming the expired entries
	// they find, leaving them for active expiration. It is phrased as an opt-out
	// on purpose: an engine built from a bare EngineConfig has to reclaim on
	// access, or ISSUE-0007 comes back through a zero value.
	DisableLazyExpiration bool

	// Scan cursor bounds. Zero means the default for each, and every one of them
	// is required rather than optional: a server-side snapshot is memory a
	// client can ask for and then abandon. See scan.go.
	ScanIdleTimeout       time.Duration // 0 = DefaultScanIdleTimeout
	MaxScanCursorsPerConn int           // 0 = DefaultMaxScanCursorsPerConn
	MaxScanSnapshotBytes  uint64        // 0 = DefaultMaxScanSnapshotBytes
}

// DefaultEngineConfig returns default configuration
func DefaultEngineConfig() EngineConfig {
	return EngineConfig{
		ShardCount:   64,
		MaxMemory:    0,               // Unlimited
		MaxValueSize: 1 * 1024 * 1024, // 1MB
	}
}

// EvictionController selects the keys a write may drop to make room for itself.
//
// The engine declares the seam it needs rather than importing the package that
// implements it, so the dependency runs one way — internal/eviction imports
// internal/storage to sample a shard, and nothing imports back.
type EvictionController interface {
	// SelectVictims returns keys to drop from shard, best candidate first,
	// covering at least needed bytes where the sample allows it. An empty
	// result means the policy found nothing to take, and the caller must not
	// ask again in a loop.
	SelectVictims(shard *Shard, needed uint64) []string
}

// ExpiryScheduler is told about every expiry the engine creates, so the TTL
// manager can schedule the key for collection. ttl.Manager satisfies it, and
// this seam is why internal/storage does not import internal/ttl.
type ExpiryScheduler interface {
	Add(shard int, key string, expireAt int64)
}

// evictionHolder and schedulerHolder box an interface so it can live in an
// atomic.Pointer, which is what makes both swappable at runtime — the eviction
// policy on config reload, the scheduler at wiring time.
type evictionHolder struct{ controller EvictionController }

type schedulerHolder struct{ scheduler ExpiryScheduler }

const (
	// maxEvictionsPerWrite bounds the victims one admission pass may take.
	// Without it a single large value could evict a whole shard trying to fit,
	// so ADR-0018 caps the attempts and returns ErrOutOfMemory on reaching the
	// cap rather than evicting further.
	maxEvictionsPerWrite = 64

	// admitAttempts bounds how many times a write repeats [make room, store]
	// after losing the room it just made to a concurrent writer, so the worst
	// case for one write is admitAttempts passes of the cap above. Each pass
	// evicts more, so losing repeatedly is vanishingly unlikely; the bound is
	// there so the loop is provably finite rather than merely improbable.
	admitAttempts = 4
)

var _ StorageEngine = (*ShardedEngine)(nil)

// ShardedEngine implements StorageEngine with sharding for concurrency
type ShardedEngine struct {
	shards     []*Shard
	shardCount uint64
	shardMask  uint64

	maxValueSize uint64
	memory       *MemoryTracker

	// scans holds the live SCAN snapshots. Its memory is accounted inside the
	// registry and never through memory above, which is what keeps a scan from
	// evicting the keys it is scanning (ADR-0017).
	scans *scanRegistry

	// Swappable collaborators, both optional: with no controller a full cache
	// rejects writes as it did before eviction existed, and with no scheduler
	// expiry falls back to the passive path alone.
	eviction  atomic.Pointer[evictionHolder]
	scheduler atomic.Pointer[schedulerHolder]

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

	memory := NewMemoryTracker(cfg.MaxMemory)

	shards := make([]*Shard, shardCount)
	for i := range shards {
		shards[i] = NewShard(1024, memory)
		shards[i].SetLazyExpiration(!cfg.DisableLazyExpiration)
	}

	// nextPowerOfTwo guarantees shardCount >= 1, so these conversions cannot wrap.
	return &ShardedEngine{
		shards:       shards,
		shardCount:   uint64(shardCount),     //nolint:gosec // bounded positive by nextPowerOfTwo
		shardMask:    uint64(shardCount - 1), //nolint:gosec // bounded positive by nextPowerOfTwo
		maxValueSize: cfg.MaxValueSize,
		memory:       memory,
		scans:        newScanRegistry(cfg),
	}
}

// SetEvictionController installs the policy that picks victims when a write
// needs room. A nil controller removes it, which makes max_memory a rejection
// threshold again.
func (e *ShardedEngine) SetEvictionController(controller EvictionController) {
	if controller == nil {
		e.eviction.Store(nil)
		return
	}
	e.eviction.Store(&evictionHolder{controller: controller})
}

// SetExpiryScheduler installs the TTL manager the engine reports new expiries
// to. A nil scheduler removes it; expiry then depends on the passive path.
func (e *ShardedEngine) SetExpiryScheduler(scheduler ExpiryScheduler) {
	if scheduler == nil {
		e.scheduler.Store(nil)
		return
	}
	e.scheduler.Store(&schedulerHolder{scheduler: scheduler})
}

// evictionController returns the installed controller, or nil.
func (e *ShardedEngine) evictionController() EvictionController {
	if holder := e.eviction.Load(); holder != nil {
		return holder.controller
	}
	return nil
}

// schedule hands a new expiry to the TTL manager. The hint is advisory
// (ADR-0016), so a dropped one costs a late reclamation and nothing else.
func (e *ShardedEngine) schedule(shard int, key string, expireAt int64) {
	if expireAt == 0 {
		return
	}
	if holder := e.scheduler.Load(); holder != nil {
		holder.scheduler.Add(shard, key, expireAt)
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

// shardFor returns the shard for a given key using xxhash, with its index —
// which is what the TTL manager schedules against.
func (e *ShardedEngine) shardFor(key []byte) (int, *Shard) {
	idx := xxhash.Sum64(key) & e.shardMask
	return int(idx), e.shards[idx] //nolint:gosec // masked below shardCount, which is at most 4096
}

// getShard returns the shard for a given key using xxhash
func (e *ShardedEngine) getShard(key []byte) *Shard {
	_, shard := e.shardFor(key)
	return shard
}

// shardAt returns the shard with the given index, or nil when the index is out
// of range. The TTL manager replays indexes from hints it may have held for
// days, so they are bounds-checked rather than trusted.
func (e *ShardedEngine) shardAt(idx int) *Shard {
	if idx < 0 || idx >= len(e.shards) {
		return nil
	}
	return e.shards[idx]
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
// It makes room rather than refusing it: victims are sampled from the shard the
// entry is bound for and dropped under the active policy (ADR-0012) until the
// entry fits. It returns ErrOutOfMemory only when it genuinely cannot — no
// controller is installed, the policy is none, sampling found no candidates, or
// the write reached its eviction cap.
//
// The shortfall credits whatever the write is about to displace: an overwrite
// only has to find room for the difference between the two entries, not for the
// whole incoming one.
func (e *ShardedEngine) admit(shard *Shard, entry *Entry) error {
	maxMemory := e.memory.GetMaxMemory()
	if maxMemory == 0 {
		return nil
	}

	size := uint64(entry.Size)
	if size > maxMemory {
		// Emptying the cache would not help: the entry still would not fit.
		return ErrOutOfMemory
	}

	controller := e.evictionController()
	if controller == nil {
		return ErrOutOfMemory
	}

	key := string(entry.Key)

	for evicted := 0; ; {
		used := e.memory.GetUsedMemory()
		credit := shard.residentSize(key)
		if used+size <= maxMemory+credit {
			return nil
		}
		if evicted >= maxEvictionsPerWrite {
			return ErrOutOfMemory
		}

		victims := controller.SelectVictims(shard, used+size-credit-maxMemory)
		if len(victims) == 0 {
			// The policy has nothing to give. Asking again would only spin.
			return ErrOutOfMemory
		}

		for _, victim := range victims {
			shard.evict(victim)

			// Counted whether or not the key was still there: an attempt that
			// found nothing is exactly the case the cap has to stop.
			evicted++
			if evicted >= maxEvictionsPerWrite {
				break
			}
		}
	}
}

// admitAndStore makes room and stores, retrying when the two race.
//
// The store is attempted first: only a refused reservation triggers eviction,
// so a SetNX that a live key blocks never costs a victim. The shard's own
// reservation stays the authority on the limit — admit works from a sampled
// view of memory that another writer can invalidate, and the reservation cannot.
func (e *ShardedEngine) admitAndStore(shard *Shard, key string, entry *Entry, onlyIfAbsent bool) (bool, error) {
	for attempt := 0; attempt < admitAttempts; attempt++ {
		stored, err := shard.put(key, entry, onlyIfAbsent)
		if !errors.Is(err, ErrOutOfMemory) {
			return stored, err
		}
		if err := e.admit(shard, entry); err != nil {
			break
		}
	}

	e.memory.RecordOOMRejection()
	return false, ErrOutOfMemory
}

// Set stores a value with optional TTL
func (e *ShardedEngine) Set(key, value []byte, ttl time.Duration) error {
	entry, err := e.validateWrite(key, value, ttl)
	if err != nil {
		return err
	}

	idx, shard := e.shardFor(key)
	name := string(key)

	if _, err := e.admitAndStore(shard, name, entry, false); err != nil {
		return err
	}

	e.schedule(idx, name, entry.ExpireAt.Load())

	return nil
}

// SetNX sets a value only if no live entry holds the key
func (e *ShardedEngine) SetNX(key, value []byte, ttl time.Duration) (bool, error) {
	entry, err := e.validateWrite(key, value, ttl)
	if err != nil {
		return false, err
	}

	idx, shard := e.shardFor(key)
	name := string(key)

	stored, err := e.admitAndStore(shard, name, entry, true)
	if err != nil || !stored {
		return false, err
	}

	e.schedule(idx, name, entry.ExpireAt.Load())

	return true, nil
}

// Delete removes a key from the storage
func (e *ShardedEngine) Delete(key []byte) bool {
	if atomic.LoadInt32(&e.closed) == 1 {
		return false
	}

	shard := e.getShard(key)
	_, existed := shard.Delete(string(key))

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

	idx, shard := e.shardFor(key)
	name := string(key)
	if !shard.SetTTL(name, ttl) {
		return false
	}

	// Re-read rather than compute the deadline: the entry is the source of
	// truth for it, and a hint that fires late would delay the reclamation.
	if expireAt, present := shard.expiryOf(name); present {
		e.schedule(idx, name, expireAt)
	}

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

// Scan returns one page of a snapshot scan, and the cursor to present for the
// page after it.
//
// Present ScanCursorStart to begin. The returned cursor is ScanCursorStart when
// the scan has finished, and any other value is an opaque id to hand back.
//
// # What it guarantees
//
// Every key present when the scan began is returned exactly once, unless it is
// deleted or expires mid-scan. Keys created after the scan began may or may not
// appear. This is weaker than Redis's guarantee; scan.go says why, and what the
// snapshot costs.
//
// # What count means
//
// count bounds the work one page does, not the size of the result: it is the
// number of snapshot entries examined, and entries that have been deleted or
// have expired since the snapshot was taken are dropped from the page rather
// than replaced. So a page may be short, or empty, without the scan being over.
// Only a returned cursor of ScanCursorStart means that. A count above
// maxScanCount is clamped, and a non-positive one means defaultScanCount.
//
// # Errors
//
// ErrScanCursorUnknown for a cursor this connection does not hold — never
// issued, another connection's, already finished, idle past the timeout, or
// dropped to reclaim snapshot memory. It is an error rather than a silent
// restart on purpose: a scan that quietly resets hands back duplicates the
// caller has no way to detect. ErrTooManyScanCursors when the connection is at
// its cursor limit, and ErrScanMemoryExhausted when a shard's snapshot does not
// fit under the global cap.
func (e *ShardedEngine) Scan(owner ScanOwner, cursor string, count int) (keys [][]byte, nextCursor string, err error) {
	if atomic.LoadInt32(&e.closed) == 1 {
		return nil, ScanCursorStart, ErrEngineClosed
	}

	switch {
	case count <= 0:
		count = defaultScanCount
	case count > maxScanCount:
		count = maxScanCount
	}

	return e.scans.page(e.shards, owner, cursor, count)
}

// ReleaseScans drops every scan cursor owned by owner, giving back the snapshot
// memory immediately rather than at the idle timeout. A connection handler calls
// it when the connection goes away: an abandoned scan has no one left to finish
// it, and holding its snapshot for another minute serves nobody.
func (e *ShardedEngine) ReleaseScans(owner ScanOwner) {
	e.scans.release(owner)
}

// Stats returns aggregated statistics
func (e *ShardedEngine) Stats() Stats {
	var stats Stats

	memStats := e.memory.Stats()
	stats.MemoryUsed = memStats.Used
	stats.MemoryMax = memStats.Max
	stats.Evictions = memStats.Evictions
	stats.OOMRejected = memStats.OOMRejected

	// Reported beside the data figures, and deliberately not folded into them.
	scanStats := e.scans.stats()
	stats.ScanCursors = scanStats.Cursors
	stats.ScanSnapshotBytes = scanStats.Bytes

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
		stats.Expirations += shardStats.Expirations
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

// RecordEviction records an eviction event
func (e *ShardedEngine) RecordEviction() {
	e.memory.RecordEviction()
}

// expireKey reclaims key from shard when it is genuinely expired, reporting
// whether it deleted anything.
//
// This is the engine's half of expiration and it holds no logic of its own: the
// deletion, the re-check under the write lock, and the accounting all live in
// Shard.expire, which the passive path in Shard.Get reaches directly. One
// implementation, two entry points, so the two cannot drift apart.
func (e *ShardedEngine) expireKey(shard *Shard, key string) bool {
	if atomic.LoadInt32(&e.closed) == 1 {
		return false
	}

	return shard.expire(key)
}

// ExpiryOf reports a key's expiry and presence.
//
// It is one half of the ttl.Keyspace seam the TTL manager validates its hints
// against (ADR-0016). An expired-but-resident entry reports present with an
// expiry in the past, which is precisely the case the manager acts on.
func (e *ShardedEngine) ExpiryOf(shard int, key string) (expireAt int64, present bool) {
	if atomic.LoadInt32(&e.closed) == 1 {
		return 0, false
	}

	target := e.shardAt(shard)
	if target == nil {
		return 0, false
	}

	return target.expiryOf(key)
}

// Expire reclaims a key the TTL manager has found expired, reporting whether it
// removed one.
//
// It is the other half of the ttl.Keyspace seam. A false answer is the manager's
// signal that the hint was stale — the key was overwritten with a live value
// between the lookup and this call — and is counted as a dropped hint rather
// than as an expiry.
func (e *ShardedEngine) Expire(shard int, key string) bool {
	target := e.shardAt(shard)
	if target == nil {
		return false
	}

	return e.expireKey(target, key)
}

// DeleteExpired removes a key that has expired, reporting whether it removed
// one. It runs the same path as active and passive expiration.
func (e *ShardedEngine) DeleteExpired(key []byte) bool {
	return e.expireKey(e.getShard(key), string(key))
}

// Close shuts down the engine
func (e *ShardedEngine) Close() error {
	if !atomic.CompareAndSwapInt32(&e.closed, 0, 1) {
		return ErrEngineClosed
	}

	// Each shard returns its own bytes to the tracker, so a drift between the
	// two counters survives Close and shows up rather than being zeroed away.
	for _, shard := range e.shards {
		shard.Clear()
	}
	e.scans.closeAll()

	return nil
}

// IsClosed returns true if the engine is closed
func (e *ShardedEngine) IsClosed() bool {
	return atomic.LoadInt32(&e.closed) == 1
}
