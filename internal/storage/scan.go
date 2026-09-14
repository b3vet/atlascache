package storage

import (
	"container/list"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"strconv"
	"sync"
	"time"
)

// Scanning: the guarantee, and what bounds it (ADR-0017, FEAT-0016)
//
// A scan starts by presenting ScanCursorStart. The engine snapshots the key
// list of the shard it is about to walk, caches it under a generated cursor id,
// and hands back the first page. Later pages read from that snapshot and drop,
// at read time, the keys that have been deleted or have expired since it was
// taken. When one shard's snapshot runs out the next shard is snapshotted, so
// what is held is one shard's key list rather than the whole keyspace.
//
// # The guarantee
//
// Every key present when the scan began is returned exactly once, unless it is
// deleted or expires mid-scan. Keys created after the scan began may or may not
// appear.
//
// That is weaker than Redis's guarantee, and saying so plainly is the point.
// Redis walks hash buckets in reverse-binary order, which tolerates the table
// resizing underneath it; Go's built-in map does not expose bucket structure, so
// that algorithm is not available here (ADR-0017 records the alternatives and
// why they were rejected for v0.1.0).
//
// Exactly-once falls out of two facts: a map's keys are unique, so one shard's
// snapshot cannot list a key twice; and a key's shard is fixed by its hash, so
// no key appears in two shards' snapshots. A cursor is dropped the moment its
// scan completes, which is why an exhausted cursor errors rather than silently
// restarting — a scan that quietly reset would hand the caller duplicates it has
// no way to detect.
//
// Shards are snapshotted one at a time, as the scan reaches them, rather than
// all at once when it starts. That is what bounds the memory a cursor holds to
// the largest shard instead of the whole keyspace, and it costs nothing against
// the guarantee: a key present at the start is still in its own shard when the
// scan gets there, because keys do not move between shards.
//
// It does give "may or may not appear" a concrete shape, worth knowing before
// anyone builds on it. A key created mid-scan in a shard the scan has not
// reached yet will be in that shard's snapshot and will appear; one created in a
// shard already walked will not. Neither is a guarantee to rely on.
//
// # Bounds
//
// A server-side snapshot is memory a client can ask for and then walk away
// from, so all three of these are required rather than optional. Without them,
// opening thousands of scans and never finishing them is a memory-exhaustion
// attack:
//
//   - an idle timeout, after which the snapshot is dropped and the cursor errors;
//   - a cap on concurrent cursors per connection;
//   - a global cap on snapshot bytes, past which the least recently used cursor
//     is evicted and its cursor errors.
//
// Evicting one connection's cursor to admit another's is a deliberate choice
// (ADR-0017): the alternative — refusing the new scan — lets one client fill the
// cap and lock everyone else out for as long as it likes.
//
// # Snapshot memory is not charged to max_memory
//
// It is tracked separately, and reported by Stats as ScanCursors and
// ScanSnapshotBytes. Charging it to max_memory would let a large scan trigger
// the eviction of the very keys it is scanning.
//
// # Cursor ids
//
// Ids are the decimal text of 64 random bits from crypto/rand, and are bound to
// the connection that created them. A cursor presented by another connection is
// answered exactly as an unknown one is, so the reply does not confirm that some
// other client's cursor exists.
//
// Decimal, not hex, and that is a compatibility requirement rather than a
// preference (ADR-0017, revised). Redis sends the SCAN cursor as a bulk string,
// but essentially every mainstream client parses it back as an unsigned
// integer — go-redis with ParseUint, redis-py with int(), `redis-cli --scan`
// with strtoull. A hex id fails or silently truncates in all three, and the
// failure is invisible to a single-page test because a scan that finishes in one
// page returns "0" either way.
//
// 64 bits is ample: ids are already scoped to one connection, so an id is not a
// capability another client could use even if it guessed one. Zero is skipped,
// because it is the start-and-finished sentinel.

// ScanCursorStart is the cursor a client presents to begin a scan, and the
// cursor the engine returns once one has finished. It is deliberately not a
// generated id: "start over" and "there is no more" are the only two cursor
// values a client may invent.
const ScanCursorStart = "0"

// Defaults for the scan bounds. Each is a bound on something a client would
// otherwise be able to consume without limit.
const (
	// DefaultScanIdleTimeout is how long a cursor may go unused before its
	// snapshot is dropped.
	DefaultScanIdleTimeout = 60 * time.Second

	// DefaultMaxScanCursorsPerConn is how many scans one connection may have
	// open at once.
	DefaultMaxScanCursorsPerConn = 16

	// DefaultMaxScanSnapshotBytes caps the total accounted across every live
	// snapshot. It is separate from max_memory on purpose; see the note above.
	DefaultMaxScanSnapshotBytes = 64 << 20 // 64 MiB
)

const (
	// defaultScanCount is the page size for a request that does not ask for one.
	defaultScanCount = 10

	// maxScanCount bounds the work one page may do, so a client cannot ask for
	// a page that walks a whole shard in a single call.
	maxScanCount = 10_000

	// scanCursorOverhead is charged per open cursor, whatever its snapshot
	// holds. It is what makes the byte cap bound the number of cursors too:
	// without it, cursors over empty shards would be free and unlimited.
	scanCursorOverhead = 128

	// scanKeyOverhead is the string header a snapshotted key costs. The key's
	// bytes are shared with the map key rather than copied, so the header is the
	// only guaranteed cost — but the snapshot also keeps those bytes alive after
	// a delete, so the key length is charged alongside it as the honest worst
	// case rather than the best one.
	scanKeyOverhead = 16

	// cursorIDBytes is the entropy in a cursor id, rendered as decimal text.
	cursorIDBytes = 8

	// cursorIDAttempts bounds the retries on a generated id that collides with
	// a live one. At 128 bits this never runs, and the loop is bounded so a
	// broken generator cannot spin forever.
	cursorIDAttempts = 4
)

// Scan errors. All three are conditions a client can provoke, so each says what
// happened in terms the client can act on.
var (
	// ErrScanCursorUnknown means the cursor is not one this connection holds:
	// it was never issued, it belongs to another connection, the scan it
	// belonged to has already finished, it went idle past the timeout, or it was
	// dropped to reclaim snapshot memory. The caller must start a new scan; it
	// must not assume the pages it already has are complete.
	ErrScanCursorUnknown = errors.New("scan cursor is unknown, expired, or already finished")

	// ErrTooManyScanCursors means the connection is at its cursor limit. Finish
	// or abandon a scan and the limit frees up.
	ErrTooManyScanCursors = errors.New("too many open scan cursors for this connection")

	// ErrScanMemoryExhausted means the snapshot does not fit under the global
	// cap even after every other cursor was evicted — the shard holds more keys
	// than scan_max_snapshot_bytes allows for. It is reported rather than
	// papered over: exceeding the cap is how a scan turns into an outage.
	ErrScanMemoryExhausted = errors.New("scan snapshot memory limit reached")
)

// ScanOwner identifies the connection a cursor belongs to. The server assigns
// one per connection; anything that scans without a connection behind it (a
// migration job, a test) may use any value it likes, as long as it is its own.
type ScanOwner uint64

// scanCursor is one in-flight scan: a snapshot of one shard's keys, and how far
// through it the client has read.
type scanCursor struct {
	id    string
	owner ScanOwner

	// shard is the index the snapshot came from, -1 before the first one is
	// taken. keys is that shard's key list as it was at snapshot time, and pos
	// is how much of it has been handed out.
	shard int
	keys  []string
	pos   int

	// bytes is what this cursor is accounted at, snapshot plus overhead. It is
	// the amount dropping the cursor gives back.
	bytes uint64

	lastUsed time.Time
	elem     *list.Element
}

// scanRegistry holds every live cursor for one engine.
//
// One mutex covers the whole registry, and it is held for the length of a page
// — including the shard snapshot a page may have to take. Scanning is an
// administrative operation, not a hot path (ADR-0017 rejected the designs that
// would have taxed writes to make it faster), so the simplicity of a single
// lock is worth more here than the concurrency of several. The lock order is
// registry then shard, and nothing takes them the other way round.
type scanRegistry struct {
	idleTimeout time.Duration
	maxPerOwner int
	maxBytes    uint64

	// now and newID are fields so tests can drive the clock and force a
	// generator failure. Production wiring leaves both at their defaults.
	now   func() time.Time
	newID func() (string, error)

	mu      sync.Mutex
	cursors map[string]*scanCursor
	perConn map[ScanOwner]int
	// lru orders cursors most recently used first, so the sweep and the byte
	// cap both take from the back and neither has to walk the map.
	lru   *list.List
	bytes uint64
}

// newScanRegistry builds a registry from cfg, substituting the default for
// every bound the config leaves at zero.
func newScanRegistry(cfg EngineConfig) *scanRegistry {
	idle := cfg.ScanIdleTimeout
	if idle <= 0 {
		idle = DefaultScanIdleTimeout
	}
	perConn := cfg.MaxScanCursorsPerConn
	if perConn <= 0 {
		perConn = DefaultMaxScanCursorsPerConn
	}
	maxBytes := cfg.MaxScanSnapshotBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxScanSnapshotBytes
	}

	return &scanRegistry{
		idleTimeout: idle,
		maxPerOwner: perConn,
		maxBytes:    maxBytes,
		now:         time.Now,
		newID:       randomCursorID,
		cursors:     make(map[string]*scanCursor),
		perConn:     make(map[ScanOwner]int),
		lru:         list.New(),
	}
}

// randomCursorID returns 64 unguessable bits as decimal text. A client must not
// be able to guess another connection's cursor, and connection scoping alone
// would not stop it from guessing its own past cursors back into existence.
//
// Zero is the one value it never returns: that is the sentinel meaning "start"
// on the way in and "finished" on the way out, and an id equal to it would end
// a scan that had not ended.
func randomCursorID() (string, error) {
	var buf [cursorIDBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}

	id := binary.BigEndian.Uint64(buf[:])
	if id == 0 {
		id = 1
	}
	return strconv.FormatUint(id, 10), nil
}

// scanStats is what the registry contributes to Stats.
type scanStats struct {
	Cursors uint64
	Bytes   uint64
}

func (r *scanRegistry) stats() scanStats {
	r.mu.Lock()
	defer r.mu.Unlock()

	// len() of a map is never negative, so the conversion cannot wrap.
	return scanStats{
		Cursors: uint64(len(r.cursors)),
		Bytes:   r.bytes,
	}
}

// page returns the next page of a scan, and the cursor to present for the one
// after it. See ShardedEngine.Scan for the contract this implements.
//
// match, when non-empty, is a glob filter applied as the page is assembled. It
// removes keys from the page and does nothing else: the count of entries
// examined is unchanged, so a heavily filtered scan walks the keyspace at the
// same rate an unfiltered one does and simply returns less of it.
func (r *scanRegistry) page(
	shards []*Shard, owner ScanOwner, id string, count int, match string,
) ([][]byte, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.sweepLocked()

	cursor, err := r.resolveLocked(owner, id)
	if err != nil {
		return nil, ScanCursorStart, err
	}

	keys := make([][]byte, 0, count)
	for examined := 0; examined < count; {
		if cursor.pos >= len(cursor.keys) {
			advanced, err := r.advanceLocked(cursor, shards)
			if err != nil {
				return nil, ScanCursorStart, err
			}
			if !advanced {
				// Every shard has been walked. Dropping the cursor here is what
				// makes a second attempt to use it an error rather than a silent
				// restart that would duplicate keys the caller already has.
				r.dropLocked(cursor)
				return keys, ScanCursorStart, nil
			}
			continue
		}

		take := count - examined
		if remaining := len(cursor.keys) - cursor.pos; take > remaining {
			take = remaining
		}
		batch := cursor.keys[cursor.pos : cursor.pos+take]
		cursor.pos += take
		examined += take

		keys = shards[cursor.shard].liveKeys(batch, match, keys)
	}

	return keys, cursor.id, nil
}

// resolveLocked opens a new cursor for the start sentinel, or finds an existing
// one. A cursor belonging to another connection is reported exactly as an
// unknown one is, so the answer does not confirm that it exists.
func (r *scanRegistry) resolveLocked(owner ScanOwner, id string) (*scanCursor, error) {
	if id == ScanCursorStart {
		return r.openLocked(owner)
	}

	cursor, ok := r.cursors[id]
	if !ok || cursor.owner != owner {
		return nil, ErrScanCursorUnknown
	}
	r.touchLocked(cursor)

	return cursor, nil
}

// openLocked registers a new cursor for owner, before any snapshot is taken.
func (r *scanRegistry) openLocked(owner ScanOwner) (*scanCursor, error) {
	if r.perConn[owner] >= r.maxPerOwner {
		return nil, ErrTooManyScanCursors
	}

	id, err := r.freshIDLocked()
	if err != nil {
		return nil, err
	}
	if !r.makeRoomLocked(nil, scanCursorOverhead) {
		return nil, ErrScanMemoryExhausted
	}

	cursor := &scanCursor{
		id:       id,
		owner:    owner,
		shard:    -1,
		bytes:    scanCursorOverhead,
		lastUsed: r.now(),
	}
	cursor.elem = r.lru.PushFront(cursor)
	r.cursors[id] = cursor
	r.perConn[owner]++
	r.bytes += scanCursorOverhead

	return cursor, nil
}

// freshIDLocked returns an id no live cursor is using.
func (r *scanRegistry) freshIDLocked() (string, error) {
	for attempt := 0; attempt < cursorIDAttempts; attempt++ {
		id, err := r.newID()
		if err != nil {
			return "", err
		}
		if _, taken := r.cursors[id]; !taken {
			return id, nil
		}
	}
	return "", errors.New("could not generate an unused scan cursor id")
}

// advanceLocked moves the cursor to the next shard holding keys and snapshots
// it, reporting false once every shard has been walked.
func (r *scanRegistry) advanceLocked(cursor *scanCursor, shards []*Shard) (bool, error) {
	next := cursor.shard + 1
	if next >= len(shards) {
		return false, nil
	}

	keys := shards[next].Keys()

	// The old snapshot is released before the new one is admitted, so a cursor
	// stepping from one shard to the next is charged for one of them, not two.
	r.bytes -= cursor.bytes
	cursor.bytes = 0
	cursor.keys = nil

	need := scanCursorOverhead + snapshotBytes(keys)
	if !r.makeRoomLocked(cursor, need) {
		// The cursor cannot proceed and its caller is about to be told so;
		// leaving it registered would only hold the per-connection slot.
		r.dropLocked(cursor)
		return false, ErrScanMemoryExhausted
	}

	cursor.shard = next
	cursor.keys = keys
	cursor.pos = 0
	cursor.bytes = need
	r.bytes += need

	return true, nil
}

// snapshotBytes is what a snapshot of keys is accounted at. See scanKeyOverhead
// for why the key length is charged as well as the header.
func snapshotBytes(keys []string) uint64 {
	total := uint64(len(keys)) * scanKeyOverhead
	for _, key := range keys {
		total += uint64(len(key))
	}
	return total
}

// makeRoomLocked evicts least recently used cursors until want more bytes fit
// under the cap, leaving keep alone. It reports false when the want does not
// fit even with every other cursor gone.
func (r *scanRegistry) makeRoomLocked(keep *scanCursor, want uint64) bool {
	for r.bytes+want > r.maxBytes {
		victim := r.oldestLocked(keep)
		if victim == nil {
			return false
		}
		r.dropLocked(victim)
	}
	return true
}

// oldestLocked returns the least recently used cursor other than keep.
func (r *scanRegistry) oldestLocked(keep *scanCursor) *scanCursor {
	for elem := r.lru.Back(); elem != nil; elem = elem.Prev() {
		cursor, ok := elem.Value.(*scanCursor)
		if ok && cursor != keep {
			return cursor
		}
	}
	return nil
}

// sweepLocked drops every cursor that has gone idle past the timeout. The list
// is ordered by last use, so it stops at the first cursor still within it.
func (r *scanRegistry) sweepLocked() {
	deadline := r.now().Add(-r.idleTimeout)

	for elem := r.lru.Back(); elem != nil; {
		cursor, ok := elem.Value.(*scanCursor)
		if !ok || cursor.lastUsed.After(deadline) {
			return
		}
		prev := elem.Prev()
		r.dropLocked(cursor)
		elem = prev
	}
}

// touchLocked marks a cursor as used now, moving it clear of the sweep and of
// the byte cap's eviction order.
func (r *scanRegistry) touchLocked(cursor *scanCursor) {
	cursor.lastUsed = r.now()
	r.lru.MoveToFront(cursor.elem)
}

// dropLocked forgets a cursor and gives back everything it was accounted at.
func (r *scanRegistry) dropLocked(cursor *scanCursor) {
	delete(r.cursors, cursor.id)
	r.lru.Remove(cursor.elem)
	r.bytes -= cursor.bytes

	if remaining := r.perConn[cursor.owner] - 1; remaining > 0 {
		r.perConn[cursor.owner] = remaining
	} else {
		delete(r.perConn, cursor.owner)
	}

	cursor.keys = nil
	cursor.bytes = 0
}

// release drops every cursor owned by owner, and is what a connection handler
// calls when the connection goes away. Without it an abandoned scan would hold
// its snapshot until the idle timeout, for a connection that can never come
// back to finish it.
func (r *scanRegistry) release(owner ScanOwner) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, cursor := range r.cursors {
		if cursor.owner == owner {
			r.dropLocked(cursor)
		}
	}
}

// closeAll drops every cursor, for engine shutdown.
func (r *scanRegistry) closeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, cursor := range r.cursors {
		r.dropLocked(cursor)
	}
}
