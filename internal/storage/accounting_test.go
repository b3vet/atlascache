package storage

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertAccounting checks the two invariants that have to hold on every path:
// the global tracker equals the sum of the per-shard counters, and the
// incremental TTL counter equals a real count of entries carrying an expiry.
func assertAccounting(t *testing.T, engine *ShardedEngine) {
	t.Helper()

	var shardMemory uint64
	var counted, actual int64

	for _, shard := range engine.GetAllShards() {
		shardMemory += shard.MemoryUsed()
		counted += shard.KeysWithTTL()

		shard.mu.RLock()
		for _, entry := range shard.data {
			if entry.HasTTL() {
				actual++
			}
		}
		shard.mu.RUnlock()
	}

	assert.Equal(t, shardMemory, engine.MemoryUsed(),
		"global tracker must equal the sum of per-shard counters")
	assert.Equal(t, actual, counted,
		"keysWithTTL must equal the number of resident entries with an expiry")
	assert.Equal(t, uint64(actual), engine.Stats().KeysWithTTL)

	if limit := engine.MaxMemory(); limit > 0 {
		assert.LessOrEqual(t, engine.MemoryUsed(), limit,
			"max_memory is a hard limit, so no path may leave it exceeded")
	}
}

func TestAccountingInvariant(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{ShardCount: 8, MaxValueSize: 1024})
	defer engine.Close()

	assertAccounting(t, engine)

	t.Run("after set", func(t *testing.T) {
		for i := 0; i < 50; i++ {
			ttl := time.Duration(0)
			if i%2 == 0 {
				ttl = time.Hour
			}
			require.NoError(t, engine.Set([]byte(fmt.Sprintf("key:%d", i)), []byte("value"), ttl))
		}
		assertAccounting(t, engine)
		assert.Equal(t, uint64(25), engine.Stats().KeysWithTTL)
	})

	t.Run("after overwrite", func(t *testing.T) {
		for i := 0; i < 50; i++ {
			// Flip every key's TTL state and change its size at the same time.
			ttl := time.Hour
			if i%2 == 0 {
				ttl = 0
			}
			require.NoError(t, engine.Set([]byte(fmt.Sprintf("key:%d", i)), []byte("a-longer-value"), ttl))
		}
		assertAccounting(t, engine)
		assert.Equal(t, uint64(25), engine.Stats().KeysWithTTL)
	})

	t.Run("after setnx over an absent key", func(t *testing.T) {
		for i := 50; i < 70; i++ {
			ok, err := engine.SetNX([]byte(fmt.Sprintf("key:%d", i)), []byte("value"), time.Hour)
			require.NoError(t, err)
			require.True(t, ok)
		}
		assertAccounting(t, engine)
	})

	t.Run("after a refused setnx", func(t *testing.T) {
		before := engine.MemoryUsed()

		ok, err := engine.SetNX([]byte("key:50"), []byte("much-longer-value"), time.Hour)
		require.NoError(t, err)
		assert.False(t, ok)

		assert.Equal(t, before, engine.MemoryUsed(), "a refused write changes nothing")
		assertAccounting(t, engine)
	})

	t.Run("after setnx replaces an expired entry", func(t *testing.T) {
		key := []byte("short-lived")
		require.NoError(t, engine.Set(key, []byte("a-fairly-long-dead-value"), time.Millisecond))
		time.Sleep(5 * time.Millisecond)

		ok, err := engine.SetNX(key, []byte("v"), 0)
		require.NoError(t, err)
		require.True(t, ok)

		assertAccounting(t, engine)
	})

	t.Run("after ttl updates", func(t *testing.T) {
		assert.True(t, engine.SetTTL([]byte("key:1"), time.Hour))
		assert.True(t, engine.SetTTL([]byte("key:3"), 0))
		assert.False(t, engine.SetTTL([]byte("no-such-key"), time.Hour))
		assertAccounting(t, engine)
	})

	t.Run("after delete", func(t *testing.T) {
		for i := 0; i < 30; i++ {
			engine.Delete([]byte(fmt.Sprintf("key:%d", i)))
		}
		assertAccounting(t, engine)
	})

	t.Run("after expire", func(t *testing.T) {
		for i := 0; i < 20; i++ {
			require.NoError(t, engine.Set([]byte(fmt.Sprintf("doomed:%d", i)), []byte("value"), time.Millisecond))
		}
		assertAccounting(t, engine)
		time.Sleep(5 * time.Millisecond)

		reaped := 0
		for i := 0; i < 20; i++ {
			key := []byte(fmt.Sprintf("doomed:%d", i))
			if engine.Expire(shardIndexOf(engine, key), string(key)) {
				reaped++
			}
		}
		assert.Equal(t, 20, reaped)
		assertAccounting(t, engine)
	})

	t.Run("after evict", func(t *testing.T) {
		for i := 0; i < 20; i++ {
			require.NoError(t, engine.Set([]byte(fmt.Sprintf("victim:%d", i)), []byte("value"), 0))
		}
		assertAccounting(t, engine)

		for i := 0; i < 20; i++ {
			key := []byte(fmt.Sprintf("victim:%d", i))
			assert.True(t, engine.GetShard(key).evict(string(key)))
		}
		assert.Equal(t, uint64(20), engine.Stats().Evictions)
		assertAccounting(t, engine)
	})

	t.Run("after single-key expiry deletion", func(t *testing.T) {
		key := []byte("doomed-single")
		require.NoError(t, engine.Set(key, []byte("value"), time.Millisecond))
		time.Sleep(5 * time.Millisecond)

		assert.True(t, engine.DeleteExpired(key))
		assert.False(t, engine.DeleteExpired(key))
		assertAccounting(t, engine)
	})

	t.Run("after clear", func(t *testing.T) {
		for _, shard := range engine.GetAllShards() {
			shard.Clear()
		}
		// Deliberately not zeroing the tracker by hand: Clear returns each
		// shard's bytes to it, and this is where that would show up if it did
		// not.
		assert.Equal(t, uint64(0), engine.MemoryUsed())
		assertAccounting(t, engine)
		assert.Equal(t, uint64(0), engine.Stats().KeysWithTTL)
	})
}

// TestSetNXEnforcesMaxMemory covers the first half of ISSUE-0010: SetNX skipped
// the memory check entirely, so max_memory was not a limit for any traffic
// using it.
func TestSetNXEnforcesMaxMemory(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{ShardCount: 8, MaxMemory: 1024, MaxValueSize: 512})
	defer engine.Close()

	var rejected int
	for i := 0; i < 1000; i++ {
		ok, err := engine.SetNX([]byte(fmt.Sprintf("key:%d", i)), make([]byte, 100), 0)
		if err != nil {
			require.ErrorIs(t, err, ErrOutOfMemory)
			assert.False(t, ok)
			rejected++
			continue
		}
		require.True(t, ok)
	}

	assert.Positive(t, rejected, "SetNX must reject once max_memory is reached")
	assert.LessOrEqual(t, engine.MemoryUsed(), engine.MaxMemory())
	assert.Positive(t, engine.Stats().OOMRejected)
	assertAccounting(t, engine)
}

// TestSetNXOverExpiredKeepsTrackerExact covers the second half of ISSUE-0010:
// the shard subtracted the displaced expired entry from its own counter while
// the engine only ever added, so the global tracker over-counted for good.
func TestSetNXOverExpiredKeepsTrackerExact(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{ShardCount: 4, MaxValueSize: 1024})
	defer engine.Close()

	key := []byte("recycled")
	for i := 0; i < 100; i++ {
		require.NoError(t, engine.Set(key, make([]byte, 128), time.Millisecond))
		time.Sleep(2 * time.Millisecond)

		ok, err := engine.SetNX(key, make([]byte, 64), time.Millisecond)
		require.NoError(t, err)
		require.True(t, ok, "an expired entry must not block SetNX")
		time.Sleep(2 * time.Millisecond)
	}

	assertAccounting(t, engine)

	shard := engine.GetShard(key)
	assert.Equal(t, uint64(CalculateSize(key, make([]byte, 64))), shard.MemoryUsed(),
		"100 recycles must leave exactly one entry's worth of memory accounted")
}

func TestStatsKeysWithTTLTransitionsThroughEngine(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{ShardCount: 4, MaxValueSize: 1024})
	defer engine.Close()

	key := []byte("ttl-transitions")
	value := []byte("value")

	withTTL := func() uint64 { return engine.Stats().KeysWithTTL }

	require.NoError(t, engine.Set(key, value, time.Hour))
	assert.Equal(t, uint64(1), withTTL(), "set with TTL over an absent key")

	require.NoError(t, engine.Set(key, value, 0))
	assert.Equal(t, uint64(0), withTTL(), "set without TTL over a key that had one")

	require.NoError(t, engine.Set(key, value, time.Hour))
	assert.Equal(t, uint64(1), withTTL(), "set with TTL over a key that had none")

	require.True(t, engine.SetTTL(key, 0))
	assert.Equal(t, uint64(0), withTTL(), "TTL cleared")

	require.True(t, engine.SetTTL(key, time.Hour))
	assert.Equal(t, uint64(1), withTTL(), "EXPIRE on a key with no TTL")

	assert.True(t, engine.Delete(key))
	assert.Equal(t, uint64(0), withTTL(), "delete of a key with a TTL")

	require.NoError(t, engine.Set(key, value, time.Millisecond))
	require.Equal(t, uint64(1), withTTL())
	time.Sleep(5 * time.Millisecond)
	assert.True(t, engine.DeleteExpired(key))
	assert.Equal(t, uint64(0), withTTL(), "expiry of a key with a TTL")

	assertAccounting(t, engine)
}

// TestStatsCountsFromCountersNotTraversal guards ISSUE-0012's fix from
// regressing. The counter tracks entries that are resident, whereas the walk it
// replaced skipped expired ones — so an expired-but-unreaped key is the visible
// fingerprint of which implementation is in place. Stats().Keys has always
// counted such entries, so this also makes the two figures agree.
//
// Lazy expiration is off here for the same reason: with it on the read would
// reclaim the entry, and there would be no unreaped key to tell the two
// implementations apart.
func TestStatsCountsFromCountersNotTraversal(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{ShardCount: 4, MaxValueSize: 1024, DisableLazyExpiration: true})
	defer engine.Close()

	key := []byte("resident")
	require.NoError(t, engine.Set(key, []byte("value"), time.Millisecond))
	time.Sleep(5 * time.Millisecond)

	_, _, exists := engine.Get(key)
	require.False(t, exists, "the key reads as gone")

	stats := engine.Stats()
	assert.Equal(t, uint64(1), stats.Keys, "but it is still resident until reaped")
	assert.Equal(t, uint64(1), stats.KeysWithTTL, "so it still counts toward KeysWithTTL")

	require.True(t, engine.Expire(shardIndexOf(engine, key), string(key)))

	stats = engine.Stats()
	assert.Equal(t, uint64(0), stats.Keys)
	assert.Equal(t, uint64(0), stats.KeysWithTTL)
}

// shardIndexOf reports the index of the shard a key lands in, which is what the
// TTL manager is handed and hands back.
func shardIndexOf(engine *ShardedEngine, key []byte) int {
	idx, _ := engine.shardFor(key)
	return idx
}
