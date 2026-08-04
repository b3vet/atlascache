package ttl

import (
	"sync"
	"time"
)

// fakeKey identifies an entry in the fake keyspace.
type fakeKey struct {
	key   string
	shard int
}

// fakeKeyspace stands in for the storage engine that FEAT-0012 will supply.
// It models the only thing the wheel is allowed to care about: the entry's
// current expiry, and whether the key is there at all.
type fakeKeyspace struct {
	mu      sync.Mutex
	entries map[fakeKey]int64
	refuse  map[fakeKey]bool
	lookups int
	expired []fakeKey

	// beforeLookup, when set, runs before every ExpiryOf. It exists so a test
	// can hold the tick loop still inside hint validation.
	beforeLookup func()
}

func newFakeKeyspace() *fakeKeyspace {
	return &fakeKeyspace{
		entries: make(map[fakeKey]int64),
		refuse:  make(map[fakeKey]bool),
	}
}

// put makes the key present with the given expiry; an expireAt of zero models
// a key that carries no TTL.
func (f *fakeKeyspace) put(shard int, key string, expireAt int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[fakeKey{key: key, shard: shard}] = expireAt
}

// remove models a key that has been deleted or evicted out from under a hint.
func (f *fakeKeyspace) remove(shard int, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, fakeKey{key: key, shard: shard})
}

// refuseExpiry models FEAT-0012 declining a deletion because its re-check
// under the write lock found a live value.
func (f *fakeKeyspace) refuseExpiry(shard int, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refuse[fakeKey{key: key, shard: shard}] = true
}

func (f *fakeKeyspace) ExpiryOf(shard int, key string) (int64, bool) {
	if f.beforeLookup != nil {
		f.beforeLookup()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	expireAt, ok := f.entries[fakeKey{key: key, shard: shard}]
	return expireAt, ok
}

func (f *fakeKeyspace) Expire(shard int, key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := fakeKey{key: key, shard: shard}
	if f.refuse[k] {
		return false
	}
	delete(f.entries, k)
	f.expired = append(f.expired, k)
	return true
}

func (f *fakeKeyspace) expiredKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.expired))
	for _, k := range f.expired {
		out = append(out, k.key)
	}
	return out
}

func (f *fakeKeyspace) expiredCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.expired)
}

func (f *fakeKeyspace) lookupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups
}

// ---- test-only views into the wheel ----

// levelCounts reports how many hints each level holds, with the overflow list
// in the final position.
func (w *wheel) levelCounts() [levelCount + 1]int {
	w.mu.Lock()
	defer w.mu.Unlock()

	var out [levelCount + 1]int
	for i := range w.levels {
		for _, slot := range w.levels[i].slots {
			out[i] += len(slot)
		}
	}
	out[levelCount] = len(w.overflow)
	return out
}

// levelCounts aggregates levelCounts across every stripe.
func (m *manager) levelCounts() [levelCount + 1]int {
	var out [levelCount + 1]int
	for _, w := range m.stripes {
		c := w.levelCounts()
		for i := range out {
			out[i] += c[i]
		}
	}
	return out
}

// levelOf reports which level holds the wheel's single hint, with levelCount
// meaning the overflow list. It fails if the count is not exactly one.
func (m *manager) levelOf() int {
	counts := m.levelCounts()
	found := -1
	for i, n := range counts {
		if n == 1 && found == -1 {
			found = i
		} else if n != 0 {
			return -1
		}
	}
	return found
}

// jumpTo pins the wheel's logical clock at tick, walking the epoch back to
// match so that absolute deadlines stay consistent. It is only valid before
// any hint has been added, and it lets a cascade test start a few ticks below
// a boundary that is otherwise 38 days away.
func (m *manager) jumpTo(tick uint64) {
	m.epoch = m.epoch.Add(-time.Duration(tick) * m.tick)
	m.epochNano = m.epoch.UnixNano()
	m.current.Store(tick)
	for _, w := range m.stripes {
		w.current = tick
	}
}

// expireAtTick is the absolute expiry, in Unix nanoseconds, of the instant the
// given tick falls due.
func (m *manager) expireAtTick(tick uint64) int64 {
	return m.deadline(tick).UnixNano()
}

// advanceTo drives the manager one tick at a time on a virtual clock pinned
// just past each tick's deadline, which is how the real loop wakes.
func advanceTo(m *manager, tick uint64) {
	for m.current.Load() < tick {
		m.catchUp(m.deadline(m.current.Load() + 1).Add(time.Microsecond))
	}
}

// newTestManager builds a single-stripe manager on a fixed epoch, so every
// tick and deadline in a test is deterministic.
func newTestManager(ks Keyspace) *manager {
	return newManager(time.Now(), Config{Tick: DefaultTick, Stripes: 1}, ks)
}
