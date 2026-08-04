package storage

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShardSetSymmetry(t *testing.T) {
	t.Run("Set over an absent key displaces nothing", func(t *testing.T) {
		shard := NewShard(4)

		old, existed := shard.Set("k", NewEntry([]byte("k"), []byte("v"), 0))

		assert.Nil(t, old)
		assert.False(t, existed)
	})

	t.Run("Set over a live key returns the displaced entry", func(t *testing.T) {
		shard := NewShard(4)
		first := NewEntry([]byte("k"), []byte("v1"), 0)
		shard.Set("k", first)

		old, existed := shard.Set("k", NewEntry([]byte("k"), []byte("v2"), 0))

		assert.True(t, existed)
		assert.Same(t, first, old)
	})

	t.Run("SetNX over an absent key displaces nothing", func(t *testing.T) {
		shard := NewShard(4)

		old, existed := shard.SetNX("k", NewEntry([]byte("k"), []byte("v"), 0))

		assert.Nil(t, old)
		assert.False(t, existed)
		assert.Equal(t, 1, shard.Len())
	})

	t.Run("SetNX refuses a live key and returns it", func(t *testing.T) {
		shard := NewShard(4)
		first := NewEntry([]byte("k"), []byte("v1"), 0)
		shard.Set("k", first)

		old, existed := shard.SetNX("k", NewEntry([]byte("k"), []byte("v2"), 0))

		assert.True(t, existed)
		assert.Same(t, first, old)

		stored, ok := shard.GetEntry("k")
		require.True(t, ok)
		assert.Equal(t, "v1", string(stored.Value), "the refused write left the value alone")
	})

	t.Run("SetNX displaces an expired entry and reports it for accounting", func(t *testing.T) {
		shard := NewShard(4)
		dead := NewEntry([]byte("k"), []byte("dead-value"), time.Nanosecond)
		shard.Set("k", dead)
		time.Sleep(time.Millisecond)

		before := shard.MemoryUsed()
		replacement := NewEntry([]byte("k"), []byte("v"), 0)
		old, existed := shard.SetNX("k", replacement)

		assert.False(t, existed, "an expired entry does not block the write")
		assert.Same(t, dead, old, "the displaced entry comes back so the engine can subtract it")
		assert.Equal(t, before-uint64(dead.Size)+uint64(replacement.Size), shard.MemoryUsed())
	})
}

// TestShardKeysWithTTLTransitions walks every transition the counter has to
// track. A miss in any one of them is a statistic that drifts silently
// (ISSUE-0012), so each gets its own case.
func TestShardKeysWithTTLTransitions(t *testing.T) {
	t.Run("set with TTL over an absent key adds one", func(t *testing.T) {
		shard := NewShard(4)

		shard.Set("k", NewEntry([]byte("k"), []byte("v"), time.Hour))

		assert.Equal(t, int64(1), shard.KeysWithTTL())
	})

	t.Run("set without TTL over a key that had one subtracts", func(t *testing.T) {
		shard := NewShard(4)
		shard.Set("k", NewEntry([]byte("k"), []byte("v"), time.Hour))

		shard.Set("k", NewEntry([]byte("k"), []byte("v"), 0))

		assert.Equal(t, int64(0), shard.KeysWithTTL())
	})

	t.Run("set with TTL over a key that had none adds one", func(t *testing.T) {
		shard := NewShard(4)
		shard.Set("k", NewEntry([]byte("k"), []byte("v"), 0))

		shard.Set("k", NewEntry([]byte("k"), []byte("v"), time.Hour))

		assert.Equal(t, int64(1), shard.KeysWithTTL())
	})

	t.Run("set with TTL over a key that had one nets zero", func(t *testing.T) {
		shard := NewShard(4)
		shard.Set("k", NewEntry([]byte("k"), []byte("v"), time.Hour))

		shard.Set("k", NewEntry([]byte("k"), []byte("v"), time.Minute))

		assert.Equal(t, int64(1), shard.KeysWithTTL())
	})

	t.Run("SetTTL on a key with no TTL adds one", func(t *testing.T) {
		shard := NewShard(4)
		shard.Set("k", NewEntry([]byte("k"), []byte("v"), 0))

		assert.True(t, shard.SetTTL("k", time.Hour))
		assert.Equal(t, int64(1), shard.KeysWithTTL())
	})

	t.Run("clearing a TTL subtracts", func(t *testing.T) {
		shard := NewShard(4)
		shard.Set("k", NewEntry([]byte("k"), []byte("v"), time.Hour))

		assert.True(t, shard.SetTTL("k", 0))
		assert.Equal(t, int64(0), shard.KeysWithTTL())
	})

	t.Run("replacing one TTL with another nets zero", func(t *testing.T) {
		shard := NewShard(4)
		shard.Set("k", NewEntry([]byte("k"), []byte("v"), time.Hour))

		assert.True(t, shard.SetTTL("k", time.Minute))
		assert.Equal(t, int64(1), shard.KeysWithTTL())
	})

	t.Run("deleting a key with a TTL subtracts", func(t *testing.T) {
		shard := NewShard(4)
		shard.Set("k", NewEntry([]byte("k"), []byte("v"), time.Hour))

		_, existed := shard.Delete("k")

		assert.True(t, existed)
		assert.Equal(t, int64(0), shard.KeysWithTTL())
	})

	t.Run("reaping an expired key subtracts", func(t *testing.T) {
		shard := NewShard(4)
		entry := NewEntry([]byte("k"), []byte("v"), time.Nanosecond)
		shard.Set("k", entry)
		time.Sleep(time.Millisecond)
		require.Equal(t, int64(1), shard.KeysWithTTL(), "an expired but resident key still counts")

		expired, freed := shard.ExpireKeys(0)

		assert.Equal(t, 1, expired)
		assert.Equal(t, uint64(entry.Size), freed)
		assert.Equal(t, int64(0), shard.KeysWithTTL())
	})

	t.Run("SetNX displacing an expired entry nets its TTL out", func(t *testing.T) {
		shard := NewShard(4)
		shard.Set("k", NewEntry([]byte("k"), []byte("v"), time.Nanosecond))
		time.Sleep(time.Millisecond)

		shard.SetNX("k", NewEntry([]byte("k"), []byte("v"), 0))

		assert.Equal(t, int64(0), shard.KeysWithTTL())
	})

	t.Run("SetTTL on a missing or dead key changes nothing", func(t *testing.T) {
		shard := NewShard(4)
		shard.Set("dead", NewEntry([]byte("dead"), []byte("v"), time.Nanosecond))
		time.Sleep(time.Millisecond)

		assert.False(t, shard.SetTTL("absent", time.Hour))
		assert.False(t, shard.SetTTL("dead", time.Hour))
		assert.Equal(t, int64(1), shard.KeysWithTTL(), "the dead entry is still resident and still counted")
	})

	t.Run("Clear resets the counter", func(t *testing.T) {
		shard := NewShard(4)
		shard.Set("k", NewEntry([]byte("k"), []byte("v"), time.Hour))

		shard.Clear()

		assert.Equal(t, int64(0), shard.KeysWithTTL())
		assert.Equal(t, uint64(0), shard.MemoryUsed())
		assert.Equal(t, 0, shard.Len())
	})
}

func TestShardConcurrentTTLTransitions(t *testing.T) {
	shard := NewShard(64)
	const keys = 32

	for i := 0; i < keys; i++ {
		shard.Set(fmt.Sprintf("k%d", i), NewEntry([]byte("k"), []byte("v"), 0))
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < keys; i++ {
				key := fmt.Sprintf("k%d", i)
				shard.SetTTL(key, time.Hour)
				shard.SetTTL(key, 0)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(0), shard.KeysWithTTL(),
		"concurrent TTL transitions must not double-count")
}

func TestShardExpireKeysRespectsMax(t *testing.T) {
	shard := NewShard(16)
	for i := 0; i < 10; i++ {
		shard.Set(fmt.Sprintf("k%d", i), NewEntry([]byte("k"), []byte("v"), time.Nanosecond))
	}
	shard.Set("live", NewEntry([]byte("live"), []byte("v"), 0))
	time.Sleep(time.Millisecond)

	expired, freed := shard.ExpireKeys(3)

	assert.Equal(t, 3, expired)
	assert.Positive(t, freed)
	assert.Equal(t, 8, shard.Len())
	assert.Equal(t, int64(7), shard.KeysWithTTL())
}

func TestShardGetAndStats(t *testing.T) {
	shard := NewShard(8)
	shard.Set("live", NewEntry([]byte("live"), []byte("v"), 0))
	shard.Set("dead", NewEntry([]byte("dead"), []byte("v"), time.Nanosecond))
	time.Sleep(time.Millisecond)

	t.Run("hit", func(t *testing.T) {
		entry, ok := shard.Get("live")
		require.True(t, ok)
		assert.Equal(t, "v", string(entry.Value))
	})

	t.Run("miss on absent", func(t *testing.T) {
		_, ok := shard.Get("absent")
		assert.False(t, ok)
	})

	t.Run("miss on expired", func(t *testing.T) {
		_, ok := shard.Get("dead")
		assert.False(t, ok)

		_, ok = shard.GetEntry("dead")
		assert.False(t, ok)

		assert.False(t, shard.Exists("dead"))
		assert.False(t, shard.Exists("absent"))
		assert.True(t, shard.Exists("live"))
	})

	t.Run("stats", func(t *testing.T) {
		stats := shard.Stats()
		assert.Equal(t, 2, stats.Keys)
		assert.Equal(t, int64(1), stats.KeysWithTTL)
		assert.Equal(t, shard.MemoryUsed(), stats.MemoryUsed)
		assert.Equal(t, uint64(2), stats.Sets)
		assert.Equal(t, uint64(1), stats.Hits)
		assert.Equal(t, uint64(2), stats.Misses)
		assert.Equal(t, uint64(3), stats.Gets)
		assert.Equal(t, uint64(0), stats.Deletes)
	})

	t.Run("delete of an absent key", func(t *testing.T) {
		entry, existed := shard.Delete("absent")
		assert.Nil(t, entry)
		assert.False(t, existed)
	})
}

func TestShardIteration(t *testing.T) {
	shard := NewShard(8)
	for i := 0; i < 5; i++ {
		shard.Set(fmt.Sprintf("live%d", i), NewEntry([]byte("k"), []byte("v"), 0))
	}
	shard.Set("dead", NewEntry([]byte("dead"), []byte("v"), time.Nanosecond))
	time.Sleep(time.Millisecond)

	assert.Len(t, shard.Keys(), 5, "Keys skips expired entries")
	assert.Len(t, shard.AllKeys(), 6, "AllKeys includes them")
	assert.Len(t, shard.Sample(3), 3)
	assert.Len(t, shard.Sample(100), 5, "sampling never returns expired entries")

	seen := 0
	shard.ForEach(func(string, *Entry) bool {
		seen++
		return true
	})
	assert.Equal(t, 5, seen)

	stopped := 0
	shard.ForEach(func(string, *Entry) bool {
		stopped++
		return false
	})
	assert.Equal(t, 1, stopped, "returning false stops the walk")
}
