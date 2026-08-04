package storage

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeScheduler records the hints the engine hands out. It stands in for
// ttl.Manager, which cannot be imported here — the ExpiryScheduler seam is what
// keeps internal/storage from depending on internal/ttl at all.
type fakeScheduler struct {
	mu    sync.Mutex
	hints []hint
}

type hint struct {
	shard    int
	key      string
	expireAt int64
}

func (f *fakeScheduler) Add(shard int, key string, expireAt int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hints = append(f.hints, hint{shard: shard, key: key, expireAt: expireAt})
}

func (f *fakeScheduler) snapshot() []hint {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]hint(nil), f.hints...)
}

// expireAll plays the part of the TTL manager's tick: it validates every hint
// it was given against the keyspace and reclaims the ones that are genuinely
// due, exactly as ttl.Manager does through the same two methods.
func (f *fakeScheduler) expireAll(engine *ShardedEngine) int {
	now := time.Now().UnixNano()
	reclaimed := 0

	for _, h := range f.snapshot() {
		expireAt, present := engine.ExpiryOf(h.shard, h.key)
		if !present || expireAt == 0 || now <= expireAt {
			continue
		}
		if engine.Expire(h.shard, h.key) {
			reclaimed++
		}
	}

	return reclaimed
}

// TestExpiredKeysAreReclaimed is the regression test for ISSUE-0007: expiry was
// detected and then ignored, so every TTL'd key leaked and max_memory filled
// with tombstones. Both routes are checked on their own, because each has to
// work when the other is turned off.
func TestExpiredKeysAreReclaimed(t *testing.T) {
	const keys = 2000

	t.Run("passive path alone", func(t *testing.T) {
		// No scheduler installed: nothing but the reads reclaims anything.
		engine := NewShardedEngine(EngineConfig{ShardCount: 8, MaxValueSize: 1024})
		defer engine.Close()

		baselineKeys := engine.Stats().Keys
		baselineMemory := engine.MemoryUsed()

		for i := 0; i < keys; i++ {
			require.NoError(t, engine.Set([]byte(fmt.Sprintf("key:%d", i)), []byte("value"), 20*time.Millisecond))
		}
		require.Equal(t, uint64(keys), engine.Stats().Keys)
		require.Positive(t, engine.MemoryUsed())

		time.Sleep(30 * time.Millisecond)

		for i := 0; i < keys; i++ {
			_, _, exists := engine.Get([]byte(fmt.Sprintf("key:%d", i)))
			require.False(t, exists)
		}

		stats := engine.Stats()
		assert.Equal(t, baselineKeys, stats.Keys, "key count returns to baseline")
		assert.Equal(t, baselineMemory, engine.MemoryUsed(), "memory returns to baseline")
		assert.Equal(t, uint64(0), stats.KeysWithTTL)
		assert.Equal(t, uint64(keys), stats.Expirations, "each key expired exactly once")
		assertAccounting(t, engine)
	})

	t.Run("active path alone", func(t *testing.T) {
		// Lazy expiration off: a read reports a miss and leaves the entry, so
		// only the scheduler's hints can reclaim anything.
		engine := NewShardedEngine(EngineConfig{ShardCount: 8, MaxValueSize: 1024, DisableLazyExpiration: true})
		defer engine.Close()

		scheduler := &fakeScheduler{}
		engine.SetExpiryScheduler(scheduler)

		for i := 0; i < keys; i++ {
			require.NoError(t, engine.Set([]byte(fmt.Sprintf("key:%d", i)), []byte("value"), 20*time.Millisecond))
		}
		require.Len(t, scheduler.snapshot(), keys, "every TTL'd write is scheduled")

		time.Sleep(30 * time.Millisecond)

		_, _, exists := engine.Get([]byte("key:0"))
		require.False(t, exists)
		require.Equal(t, uint64(keys), engine.Stats().Keys, "the read left it for the wheel")

		assert.Equal(t, keys, scheduler.expireAll(engine))

		stats := engine.Stats()
		assert.Equal(t, uint64(0), stats.Keys, "key count returns to baseline")
		assert.Equal(t, uint64(0), engine.MemoryUsed(), "memory returns to baseline")
		assert.Equal(t, uint64(0), stats.KeysWithTTL)
		assert.Equal(t, uint64(keys), stats.Expirations)
		assertAccounting(t, engine)
	})
}

// TestSustainedTTLChurnHoldsSteadyState is the same claim over time: memory
// under continuous write-and-expire traffic settles rather than climbing, which
// is what ISSUE-0007 made impossible.
func TestSustainedTTLChurnHoldsSteadyState(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{ShardCount: 8, MaxValueSize: 1024})
	defer engine.Close()

	scheduler := &fakeScheduler{}
	engine.SetExpiryScheduler(scheduler)

	var peak uint64
	for round := 0; round < 20; round++ {
		for i := 0; i < 200; i++ {
			key := []byte(fmt.Sprintf("round:%d:key:%d", round, i))
			require.NoError(t, engine.Set(key, []byte("value"), 5*time.Millisecond))
		}
		time.Sleep(6 * time.Millisecond)
		scheduler.expireAll(engine)

		if used := engine.MemoryUsed(); used > peak {
			peak = used
		}
	}

	// One round's worth of entries, give or take: nothing accumulates across
	// rounds the way it did when expiry never deleted.
	assert.Less(t, peak, 400*entrySize("round:00:key:000", "value"),
		"memory holds at roughly one round, rather than growing with every round")
	assertAccounting(t, engine)
}

// TestSchedulerSeamReceivesEveryExpiry pins what the TTL manager is told. A
// missed hand-off is a key that only the passive path can ever reclaim.
func TestSchedulerSeamReceivesEveryExpiry(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{ShardCount: 4, MaxValueSize: 1024})
	defer engine.Close()

	scheduler := &fakeScheduler{}
	engine.SetExpiryScheduler(scheduler)

	require.NoError(t, engine.Set([]byte("no-ttl"), []byte("v"), 0))
	assert.Empty(t, scheduler.snapshot(), "a write with no expiry schedules nothing")

	require.NoError(t, engine.Set([]byte("with-ttl"), []byte("v"), time.Hour))

	stored, err := engine.SetNX([]byte("setnx-ttl"), []byte("v"), time.Hour)
	require.NoError(t, err)
	require.True(t, stored)

	stored, err = engine.SetNX([]byte("with-ttl"), []byte("v"), time.Hour)
	require.NoError(t, err)
	require.False(t, stored)

	require.True(t, engine.SetTTL([]byte("no-ttl"), time.Hour))
	require.True(t, engine.SetTTL([]byte("with-ttl"), 0))

	hints := scheduler.snapshot()
	require.Len(t, hints, 3, "one hint per expiry created, and none for a refused write or a cleared TTL")

	for _, h := range hints {
		assert.Positive(t, h.expireAt)
		idx, _ := engine.shardFor([]byte(h.key))
		assert.Equal(t, idx, h.shard, "the hint carries the shard the key actually lives in")
	}

	t.Run("removing the scheduler is safe", func(t *testing.T) {
		engine.SetExpiryScheduler(nil)
		require.NoError(t, engine.Set([]byte("after"), []byte("v"), time.Hour))
		assert.Len(t, scheduler.snapshot(), 3)
	})
}

// TestExpiryDoesNotDeleteRevivedKeys is the race the re-check exists for: a key
// found expired, then overwritten with a live value before the deletion takes
// the write lock. Deleting it then would destroy data the client just wrote.
func TestExpiryDoesNotDeleteRevivedKeys(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{ShardCount: 4, MaxValueSize: 1024})
	defer engine.Close()

	key := []byte("revived")
	idx := shardIndexOf(engine, key)

	require.NoError(t, engine.Set(key, []byte("old"), time.Millisecond))
	time.Sleep(5 * time.Millisecond)

	expireAt, present := engine.ExpiryOf(idx, "revived")
	require.True(t, present)
	require.Less(t, expireAt, time.Now().UnixNano(), "the manager sees a genuinely expired key")

	// The client's write lands in the gap between that decision and the delete.
	require.NoError(t, engine.Set(key, []byte("new"), time.Hour))

	assert.False(t, engine.Expire(idx, "revived"),
		"a false answer is the manager's signal that the hint was stale")

	got, _, exists := engine.Get(key)
	require.True(t, exists)
	assert.Equal(t, "new", string(got))
	assert.Equal(t, uint64(0), engine.Stats().Expirations)
	assertAccounting(t, engine)
}

// TestConcurrentExpireAndAccess runs both expiry routes against live traffic on
// the same keys. Nothing may be double-counted and the accounting may not drift.
func TestConcurrentExpireAndAccess(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{ShardCount: 4, MaxValueSize: 1024})
	defer engine.Close()

	scheduler := &fakeScheduler{}
	engine.SetExpiryScheduler(scheduler)

	const keys = 64
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(3)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for i := 0; i < keys; i++ {
				require.NoError(t, engine.Set([]byte(fmt.Sprintf("k%d", i)), []byte("value"), 2*time.Millisecond))
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for i := 0; i < keys; i++ {
				engine.Get([]byte(fmt.Sprintf("k%d", i)))
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for i := 0; i < keys; i++ {
				engine.Expire(shardIndexOf(engine, []byte(fmt.Sprintf("k%d", i))), fmt.Sprintf("k%d", i))
			}
		}
	}()

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	assertAccounting(t, engine)
}
