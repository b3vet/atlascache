package storage

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestShard builds a shard with its own tracker, which is what a shard
// exercised outside an engine gets.
func newTestShard(capacity int) *Shard {
	return NewShard(capacity, nil)
}

// newLimitedShard builds a shard whose tracker enforces a memory limit.
func newLimitedShard(capacity int, maxMemory uint64) *Shard {
	return NewShard(capacity, NewMemoryTracker(maxMemory))
}

func mustSet(t *testing.T, shard *Shard, key string, entry *Entry) {
	t.Helper()
	require.NoError(t, shard.Set(key, entry))
}

// TestShardWritePathsAgree covers what ISSUE-0010 was: Set and SetNX kept their
// own accounting, and the two did not agree. They now share one write path, so
// the cases below are the same store seen through two doors.
func TestShardWritePathsAgree(t *testing.T) {
	t.Run("Set over an absent key stores", func(t *testing.T) {
		shard := newTestShard(4)

		require.NoError(t, shard.Set("k", NewEntry([]byte("k"), []byte("v"), 0)))

		assert.Equal(t, 1, shard.Len())
	})

	t.Run("Set over a live key replaces it and charges only the difference", func(t *testing.T) {
		shard := newTestShard(4)
		first := NewEntry([]byte("k"), []byte("v1"), 0)
		mustSet(t, shard, "k", first)

		replacement := NewEntry([]byte("k"), []byte("a-much-longer-value"), 0)
		require.NoError(t, shard.Set("k", replacement))

		assert.Equal(t, 1, shard.Len())
		assert.Equal(t, uint64(replacement.Size), shard.MemoryUsed())
		assert.Equal(t, uint64(replacement.Size), shard.mem.GetUsedMemory())
	})

	t.Run("SetNX over an absent key stores", func(t *testing.T) {
		shard := newTestShard(4)

		stored, err := shard.SetNX("k", NewEntry([]byte("k"), []byte("v"), 0))

		require.NoError(t, err)
		assert.True(t, stored)
		assert.Equal(t, 1, shard.Len())
	})

	t.Run("SetNX refuses a live key without touching memory", func(t *testing.T) {
		shard := newTestShard(4)
		first := NewEntry([]byte("k"), []byte("v1"), 0)
		mustSet(t, shard, "k", first)
		before := shard.MemoryUsed()

		stored, err := shard.SetNX("k", NewEntry([]byte("k"), []byte("v2"), 0))

		require.NoError(t, err)
		assert.False(t, stored)
		assert.Equal(t, before, shard.MemoryUsed(), "a refused write reserves nothing")

		entry, ok := shard.GetEntry("k")
		require.True(t, ok)
		assert.Equal(t, "v1", string(entry.Value), "the refused write left the value alone")
	})

	t.Run("SetNX displaces an expired entry and credits its bytes", func(t *testing.T) {
		shard := newTestShard(4)
		dead := NewEntry([]byte("k"), []byte("dead-value"), time.Nanosecond)
		mustSet(t, shard, "k", dead)
		time.Sleep(time.Millisecond)

		replacement := NewEntry([]byte("k"), []byte("v"), 0)
		stored, err := shard.SetNX("k", replacement)

		require.NoError(t, err)
		assert.True(t, stored, "an expired entry does not block the write")
		assert.Equal(t, uint64(replacement.Size), shard.MemoryUsed())
		assert.Equal(t, uint64(replacement.Size), shard.mem.GetUsedMemory())
	})
}

// TestShardReservationHoldsTheLimit pins the reservation itself: the shard
// refuses a store it cannot account for, and an overwrite is charged the
// difference rather than the whole entry — the double charge FEAT-0013 called
// out, which rejected rewrites of a key at the limit.
func TestShardReservationHoldsTheLimit(t *testing.T) {
	entry := NewEntry([]byte("k"), []byte("value"), 0)
	shard := newLimitedShard(4, uint64(entry.Size))

	require.NoError(t, shard.Set("k", entry))

	t.Run("a second key does not fit", func(t *testing.T) {
		err := shard.Set("other", NewEntry([]byte("other"), []byte("value"), 0))

		require.ErrorIs(t, err, ErrOutOfMemory)
		assert.Equal(t, 1, shard.Len())
		assert.Equal(t, uint64(entry.Size), shard.mem.GetUsedMemory(), "a refused store reserves nothing")
	})

	t.Run("rewriting the same key at the limit fits", func(t *testing.T) {
		require.NoError(t, shard.Set("k", NewEntry([]byte("k"), []byte("valu2"), 0)))

		assert.Equal(t, uint64(entry.Size), shard.mem.GetUsedMemory())
	})

	t.Run("SetNX is held to the same limit", func(t *testing.T) {
		stored, err := shard.SetNX("other", NewEntry([]byte("other"), []byte("value"), 0))

		require.ErrorIs(t, err, ErrOutOfMemory)
		assert.False(t, stored)
	})
}

// TestShardKeysWithTTLTransitions walks every transition the counter has to
// track. A miss in any one of them is a statistic that drifts silently
// (ISSUE-0012), so each gets its own case.
func TestShardKeysWithTTLTransitions(t *testing.T) {
	t.Run("set with TTL over an absent key adds one", func(t *testing.T) {
		shard := newTestShard(4)

		mustSet(t, shard, "k", NewEntry([]byte("k"), []byte("v"), time.Hour))

		assert.Equal(t, int64(1), shard.KeysWithTTL())
	})

	t.Run("set without TTL over a key that had one subtracts", func(t *testing.T) {
		shard := newTestShard(4)
		mustSet(t, shard, "k", NewEntry([]byte("k"), []byte("v"), time.Hour))

		mustSet(t, shard, "k", NewEntry([]byte("k"), []byte("v"), 0))

		assert.Equal(t, int64(0), shard.KeysWithTTL())
	})

	t.Run("set with TTL over a key that had none adds one", func(t *testing.T) {
		shard := newTestShard(4)
		mustSet(t, shard, "k", NewEntry([]byte("k"), []byte("v"), 0))

		mustSet(t, shard, "k", NewEntry([]byte("k"), []byte("v"), time.Hour))

		assert.Equal(t, int64(1), shard.KeysWithTTL())
	})

	t.Run("set with TTL over a key that had one nets zero", func(t *testing.T) {
		shard := newTestShard(4)
		mustSet(t, shard, "k", NewEntry([]byte("k"), []byte("v"), time.Hour))

		mustSet(t, shard, "k", NewEntry([]byte("k"), []byte("v"), time.Minute))

		assert.Equal(t, int64(1), shard.KeysWithTTL())
	})

	t.Run("SetTTL on a key with no TTL adds one", func(t *testing.T) {
		shard := newTestShard(4)
		mustSet(t, shard, "k", NewEntry([]byte("k"), []byte("v"), 0))

		assert.True(t, shard.SetTTL("k", time.Hour))
		assert.Equal(t, int64(1), shard.KeysWithTTL())
	})

	t.Run("clearing a TTL subtracts", func(t *testing.T) {
		shard := newTestShard(4)
		mustSet(t, shard, "k", NewEntry([]byte("k"), []byte("v"), time.Hour))

		assert.True(t, shard.SetTTL("k", 0))
		assert.Equal(t, int64(0), shard.KeysWithTTL())
	})

	t.Run("replacing one TTL with another nets zero", func(t *testing.T) {
		shard := newTestShard(4)
		mustSet(t, shard, "k", NewEntry([]byte("k"), []byte("v"), time.Hour))

		assert.True(t, shard.SetTTL("k", time.Minute))
		assert.Equal(t, int64(1), shard.KeysWithTTL())
	})

	t.Run("deleting a key with a TTL subtracts", func(t *testing.T) {
		shard := newTestShard(4)
		mustSet(t, shard, "k", NewEntry([]byte("k"), []byte("v"), time.Hour))

		_, existed := shard.Delete("k")

		assert.True(t, existed)
		assert.Equal(t, int64(0), shard.KeysWithTTL())
	})

	t.Run("reaping an expired key subtracts", func(t *testing.T) {
		shard := newTestShard(4)
		entry := NewEntry([]byte("k"), []byte("v"), time.Nanosecond)
		mustSet(t, shard, "k", entry)
		time.Sleep(time.Millisecond)
		require.Equal(t, int64(1), shard.KeysWithTTL(), "an expired but resident key still counts")

		assert.True(t, shard.expire("k"))

		assert.Equal(t, int64(0), shard.KeysWithTTL())
		assert.Equal(t, uint64(0), shard.MemoryUsed())
	})

	t.Run("SetNX displacing an expired entry nets its TTL out", func(t *testing.T) {
		shard := newTestShard(4)
		mustSet(t, shard, "k", NewEntry([]byte("k"), []byte("v"), time.Nanosecond))
		time.Sleep(time.Millisecond)

		stored, err := shard.SetNX("k", NewEntry([]byte("k"), []byte("v"), 0))

		require.NoError(t, err)
		require.True(t, stored)
		assert.Equal(t, int64(0), shard.KeysWithTTL())
	})

	t.Run("SetTTL on a missing or dead key changes nothing", func(t *testing.T) {
		shard := newTestShard(4)
		mustSet(t, shard, "dead", NewEntry([]byte("dead"), []byte("v"), time.Nanosecond))
		time.Sleep(time.Millisecond)

		assert.False(t, shard.SetTTL("absent", time.Hour))
		assert.False(t, shard.SetTTL("dead", time.Hour))
		assert.Equal(t, int64(1), shard.KeysWithTTL(), "the dead entry is still resident and still counted")
	})

	t.Run("Clear resets the counter", func(t *testing.T) {
		shard := newTestShard(4)
		mustSet(t, shard, "k", NewEntry([]byte("k"), []byte("v"), time.Hour))

		shard.Clear()

		assert.Equal(t, int64(0), shard.KeysWithTTL())
		assert.Equal(t, uint64(0), shard.MemoryUsed())
		assert.Equal(t, 0, shard.Len())
	})
}

func TestShardConcurrentTTLTransitions(t *testing.T) {
	shard := newTestShard(64)
	const keys = 32

	for i := 0; i < keys; i++ {
		mustSet(t, shard, fmt.Sprintf("k%d", i), NewEntry([]byte("k"), []byte("v"), 0))
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

// TestShardExpireRechecksUnderTheWriteLock covers the one case that makes
// expiry dangerous: the key the caller saw expired may have been overwritten
// with a live value by the time the write lock is taken, and deleting it then
// destroys data that has nothing to do with the expiry.
func TestShardExpireRechecksUnderTheWriteLock(t *testing.T) {
	shard := newTestShard(8)

	t.Run("an expired key goes", func(t *testing.T) {
		mustSet(t, shard, "dead", NewEntry([]byte("dead"), []byte("v"), time.Nanosecond))
		time.Sleep(time.Millisecond)

		assert.True(t, shard.expire("dead"))
		assert.False(t, shard.expire("dead"), "and only once")
	})

	t.Run("a live key stays", func(t *testing.T) {
		mustSet(t, shard, "live", NewEntry([]byte("live"), []byte("v"), time.Hour))

		assert.False(t, shard.expire("live"))
		assert.Equal(t, 1, shard.Len())
	})

	t.Run("a key overwritten between the read and the delete survives", func(t *testing.T) {
		mustSet(t, shard, "revived", NewEntry([]byte("revived"), []byte("old"), time.Nanosecond))
		time.Sleep(time.Millisecond)

		// What the racing writer does: the caller has already decided this key
		// is expired, and the value lands before expire takes the write lock.
		mustSet(t, shard, "revived", NewEntry([]byte("revived"), []byte("new"), time.Hour))

		assert.False(t, shard.expire("revived"), "the re-check refuses to delete live data")

		entry, ok := shard.GetEntry("revived")
		require.True(t, ok)
		assert.Equal(t, "new", string(entry.Value))
	})

	t.Run("an absent key is not an expiry", func(t *testing.T) {
		assert.False(t, shard.expire("never-existed"))
	})
}

// TestShardGetReclaimsExpired is ISSUE-0007 at the shard level: the read used to
// report a miss and leave the entry, which is how a TTL'd keyspace leaked.
func TestShardGetReclaimsExpired(t *testing.T) {
	t.Run("lazy expiration on", func(t *testing.T) {
		shard := newTestShard(8)
		mustSet(t, shard, "dead", NewEntry([]byte("dead"), []byte("v"), time.Nanosecond))
		time.Sleep(time.Millisecond)

		_, ok := shard.Get("dead")

		assert.False(t, ok)
		assert.Equal(t, 0, shard.Len(), "the read reclaimed it")
		assert.Equal(t, uint64(0), shard.MemoryUsed())
		assert.Equal(t, uint64(0), shard.mem.GetUsedMemory())
		assert.Equal(t, int64(0), shard.KeysWithTTL())
		assert.Equal(t, uint64(1), shard.Stats().Expirations)
	})

	t.Run("lazy expiration off", func(t *testing.T) {
		shard := newTestShard(8)
		shard.SetLazyExpiration(false)
		mustSet(t, shard, "dead", NewEntry([]byte("dead"), []byte("v"), time.Nanosecond))
		time.Sleep(time.Millisecond)

		_, ok := shard.Get("dead")

		assert.False(t, ok, "the read still misses")
		assert.Equal(t, 1, shard.Len(), "but leaves the entry for active expiration")
		assert.Equal(t, uint64(0), shard.Stats().Expirations)
	})
}

// TestShardConcurrentExpireAndOverwrite runs the same race for real: readers
// reclaiming a key that writers keep bringing back. Nothing may be lost and the
// accounting may not drift.
func TestShardConcurrentExpireAndOverwrite(t *testing.T) {
	shard := newTestShard(8)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			mustSet(t, shard, "k", NewEntry([]byte("k"), []byte("v"), time.Millisecond))
		}
	}()
	for g := 0; g < 2; g++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				shard.Get("k")
				shard.expire("k")
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()

	assert.Equal(t, shard.MemoryUsed(), shard.mem.GetUsedMemory(),
		"the shard counter and the tracker must not drift under the race")
	assert.Equal(t, int64(shard.Len()), shard.KeysWithTTL())
}

func TestShardGetAndStats(t *testing.T) {
	shard := newTestShard(8)
	mustSet(t, shard, "live", NewEntry([]byte("live"), []byte("v"), 0))
	mustSet(t, shard, "dead", NewEntry([]byte("dead"), []byte("v"), time.Nanosecond))
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
		_, ok := shard.GetEntry("dead")
		assert.False(t, ok)

		assert.False(t, shard.Exists("dead"))
		assert.False(t, shard.Exists("absent"))
		assert.True(t, shard.Exists("live"))

		// Last, because it reclaims what the assertions above look at.
		_, ok = shard.Get("dead")
		assert.False(t, ok)
	})

	t.Run("stats", func(t *testing.T) {
		stats := shard.Stats()
		assert.Equal(t, 1, stats.Keys, "the expired entry was reclaimed by the read above")
		assert.Equal(t, int64(0), stats.KeysWithTTL)
		assert.Equal(t, shard.MemoryUsed(), stats.MemoryUsed)
		assert.Equal(t, uint64(2), stats.Sets)
		assert.Equal(t, uint64(1), stats.Hits)
		assert.Equal(t, uint64(2), stats.Misses)
		assert.Equal(t, uint64(3), stats.Gets)
		assert.Equal(t, uint64(0), stats.Deletes)
		assert.Equal(t, uint64(1), stats.Expirations)
	})

	t.Run("delete of an absent key", func(t *testing.T) {
		entry, existed := shard.Delete("absent")
		assert.Nil(t, entry)
		assert.False(t, existed)
	})
}

func TestShardIteration(t *testing.T) {
	shard := newTestShard(8)
	for i := 0; i < 5; i++ {
		mustSet(t, shard, fmt.Sprintf("live%d", i), NewEntry([]byte("k"), []byte("v"), 0))
	}
	mustSet(t, shard, "dead", NewEntry([]byte("dead"), []byte("v"), time.Nanosecond))
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
