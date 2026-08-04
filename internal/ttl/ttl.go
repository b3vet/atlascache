// Package ttl implements AtlasCache's expiry scheduler: a four-level
// hierarchical time wheel whose slots hold hints rather than truth.
//
// # Hints, not truth
//
// The load-bearing invariant of this package (ADR-0016) is that
// storage.Entry.ExpireAt is the sole source of truth about when a key expires.
// A wheel slot holds only a scheduling hint. When a slot fires, the manager
// re-reads the entry through the Keyspace seam and acts only if the key is
// genuinely expired:
//
//	genuinely expired      hand off to Keyspace.Expire
//	key gone               drop the hint
//	overwritten, no TTL    drop the hint
//	TTL extended           re-insert a hint at the new deadline
//
// A stale hint therefore costs one wasted lookup and nothing else. That is why
// there is no Remove: deletion, overwrite, eviction, and TTL changes never
// touch the wheel, so the wheel cannot drift out of step with the keyspace and
// expire live data early — the classic failure of this data structure.
//
// # Geometry
//
//	Level  Slots  Resolution  Span
//	L0     256    100ms       25.6s
//	L1      64    25.6s       ~27m
//	L2      64    ~27m        ~29h
//	L3      32    ~29h        ~38d
//
// Deadlines beyond L3's span are parked in an overflow list and re-inserted
// once they come into range.
package ttl

import (
	"context"
	"errors"
	"time"
)

// DefaultTick is the wheel's base resolution: the L0 slot width, and therefore
// the precision with which an expiry is detected.
const DefaultTick = 100 * time.Millisecond

// DefaultStripes is the number of independent wheels the keyspace is spread
// across. Striping keeps Add off a single global mutex; every stripe is
// advanced by the same tick loop, so they share one logical clock.
const DefaultStripes = 16

// DefaultMaxHintsPerTick bounds how many hints one tick may validate, so a mass
// expiry cannot stall the tick loop. Hints past the limit are deferred to the
// next tick rather than dropped.
const DefaultMaxHintsPerTick = 10000

// maxStripes caps Config.Stripes; beyond this the per-stripe slot arrays cost
// more than the lock contention they remove.
const maxStripes = 1024

// Errors returned by the manager lifecycle.
var (
	// ErrAlreadyRunning is returned by Start when the manager is already running.
	ErrAlreadyRunning = errors.New("ttl: manager already running")
	// ErrStopped is returned by Start when the manager has already been stopped.
	// A stopped manager is not restartable; construct a new one.
	ErrStopped = errors.New("ttl: manager stopped")
)

// Manager schedules and detects key expiry.
//
// Add is O(1) and safe for concurrent use. There is deliberately no Remove:
// see the package documentation.
type Manager interface {
	// Add registers a hint that the key in the given shard expires at expireAt,
	// expressed in Unix nanoseconds. An expireAt of zero or less means "no
	// expiry" and is ignored. Adding the same key twice is harmless.
	Add(shard int, key string, expireAt int64)

	// Start launches the tick loop. It returns ErrAlreadyRunning if the manager
	// is already running and ErrStopped if it has been stopped.
	Start() error

	// Stop signals the tick loop to finish its current tick and waits for it to
	// exit, returning ctx.Err() if the context expires first. Stop is safe to
	// call on a manager that was never started, and is idempotent.
	Stop(ctx context.Context) error

	// Stats returns a snapshot of the manager's counters.
	Stats() Stats
}

// Keyspace is the seam between the wheel and the keyspace whose expiries it
// schedules. The wheel owns this interface rather than importing
// internal/storage, so that FEAT-0012 can supply the real implementation
// (backed by storage.ShardedEngine) without this package depending on it, and
// so that tests can drive every hint-validation path from a fake.
//
// Implementations must be safe for concurrent use.
type Keyspace interface {
	// ExpiryOf reports the key's current expiry in Unix nanoseconds together
	// with whether the key is present. A present key that carries no TTL
	// reports an expireAt of zero. This is the "re-read the entry" half of the
	// hints-not-truth invariant: the manager trusts this answer, never the hint.
	ExpiryOf(shard int, key string) (expireAt int64, present bool)

	// Expire is called only for a key the manager has confirmed is genuinely
	// expired. The implementation is responsible for re-checking expiry under
	// its own write lock before deleting — the key may have been overwritten
	// with a live value since ExpiryOf answered — and for the memory and
	// counter accounting. It reports whether the key was actually removed.
	Expire(shard int, key string) bool
}

// nopKeyspace answers as though every key is gone, so every hint is dropped.
// It stands in when a Manager is constructed without a Keyspace, which is
// useful for benchmarking the wheel in isolation before FEAT-0012 wires the
// engine in.
type nopKeyspace struct{}

func (nopKeyspace) ExpiryOf(int, string) (int64, bool) { return 0, false }
func (nopKeyspace) Expire(int, string) bool            { return false }

// Config tunes the manager. The zero value is valid and yields the defaults.
type Config struct {
	// Tick is the wheel's base resolution. Zero means DefaultTick.
	Tick time.Duration

	// Stripes is the number of independent wheels. Zero means DefaultStripes.
	// Values are rounded up to a power of two and capped at 1024, so that a
	// shard maps to its stripe with a mask.
	Stripes int

	// MaxHintsPerTick bounds the hints validated in a single tick. Zero means
	// DefaultMaxHintsPerTick; a negative value means unlimited.
	MaxHintsPerTick int
}

// Stats is a snapshot of the manager's counters.
type Stats struct {
	// Running reports whether the tick loop is live.
	Running bool
	// Ticks is the number of ticks processed since Start.
	Ticks uint64
	// CurrentTick is the wheel's logical clock, in ticks since the epoch.
	CurrentTick uint64
	// Added is the number of hints accepted by Add.
	Added uint64
	// Ignored is the number of Add calls carrying no expiry.
	Ignored uint64
	// Fired is the number of hints taken out of a slot and validated.
	Fired uint64
	// Expired is the number of hints the keyspace confirmed and removed.
	Expired uint64
	// Dropped is the number of stale hints discarded: the key was gone, had
	// been overwritten without a TTL, or lost the race in Keyspace.Expire.
	Dropped uint64
	// Reinserted is the number of hints re-scheduled because the entry's TTL
	// had been extended.
	Reinserted uint64
	// Deferred is the number of hints pushed to the next tick by the per-tick
	// batch limit.
	Deferred uint64
	// LateTicks is the number of ticks processed as catch-up, that is, ticks
	// whose deadline had already passed when the loop got to them.
	LateTicks uint64
	// MaxLag is the largest observed delay between a tick's deadline and the
	// loop reaching it.
	MaxLag time.Duration
	// Pending is the number of hints currently held across all levels.
	Pending uint64
	// Overflow is the number of hints parked beyond L3's span.
	Overflow uint64
}

// normalize fills in defaults and clamps out-of-range values.
func (c Config) normalize() Config {
	if c.Tick <= 0 {
		c.Tick = DefaultTick
	}
	if c.Stripes == 0 {
		c.Stripes = DefaultStripes
	}
	c.Stripes = roundUpPow2(c.Stripes)
	if c.MaxHintsPerTick == 0 {
		c.MaxHintsPerTick = DefaultMaxHintsPerTick
	}
	if c.MaxHintsPerTick < 0 {
		c.MaxHintsPerTick = 0 // unlimited
	}
	return c
}

// roundUpPow2 rounds n up to a power of two within [1, maxStripes].
func roundUpPow2(n int) int {
	if n <= 1 {
		return 1
	}
	if n >= maxStripes {
		return maxStripes
	}
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}
