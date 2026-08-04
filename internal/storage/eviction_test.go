package storage

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeController stands in for internal/eviction, which cannot be imported here
// — it imports this package, and the seam exists precisely so the dependency
// runs one way. It ranks by key so the victim is predictable, which is what
// these tests need; the policies themselves are covered in internal/eviction.
type fakeController struct {
	calls atomic.Int64

	// empty makes every selection come back with nothing, which is how a shard
	// with no live entries, or a none policy, looks to the engine.
	empty bool
}

func (f *fakeController) SelectVictims(shard *Shard, needed uint64) []string {
	f.calls.Add(1)
	if f.empty || shard == nil {
		return nil
	}

	// A real controller samples; this one looks at everything and sorts, so the
	// choice is deterministic and the test can name the victim it expects.
	entries := shard.Sample(1 << 20)
	keys := make([]string, 0, len(entries))
	sizes := make(map[string]uint64, len(entries))
	for _, entry := range entries {
		key := string(entry.Key)
		keys = append(keys, key)
		sizes[key] = uint64(entry.Size)
	}
	sort.Strings(keys)

	victims := make([]string, 0, len(keys))
	var freed uint64
	for _, key := range keys {
		victims = append(victims, key)
		freed += sizes[key]
		if freed >= needed {
			break
		}
	}

	return victims
}

// newEvictingEngine builds a single-shard engine at a fixed limit with victim
// selection installed. One shard because eviction samples the shard the write
// is bound for, so a test that spreads a handful of keys over 64 shards is
// testing the hash, not the policy.
func newEvictingEngine(maxMemory uint64) (*ShardedEngine, *fakeController) {
	engine := NewShardedEngine(EngineConfig{
		ShardCount:   1,
		MaxMemory:    maxMemory,
		MaxValueSize: 1 << 20,
	})
	controller := &fakeController{}
	engine.SetEvictionController(controller)

	return engine, controller
}

// entrySize is the accounted size of a key/value pair, so a test can state a
// memory limit as a number of entries rather than a magic byte count.
func entrySize(key, value string) uint64 {
	return uint64(CalculateSize([]byte(key), []byte(value)))
}

// TestWritePastLimitEvicts is the headline of FEAT-0013: max_memory stops being
// a rejection threshold and becomes a limit the cache lives within.
func TestWritePastLimitEvicts(t *testing.T) {
	const value = "0123456789"
	limit := 10 * entrySize("key:0000", value)

	engine, _ := newEvictingEngine(limit)
	defer engine.Close()

	for i := 0; i < 500; i++ {
		require.NoError(t, engine.Set([]byte(fmt.Sprintf("key:%04d", i)), []byte(value), 0),
			"a full cache makes room instead of refusing the write")
	}

	stats := engine.Stats()
	assert.LessOrEqual(t, stats.MemoryUsed, limit)
	assert.Equal(t, uint64(10), stats.Keys, "the cache holds what fits and no more")
	assert.Equal(t, uint64(0), stats.OOMRejected)
	assert.Positive(t, stats.Evictions)
	assertAccounting(t, engine)
}

// TestMemoryNeverExceedsLimit is the load-bearing assertion of ADR-0018: the
// limit is never crossed, not even for an instant. It is sampled throughout the
// run rather than at the end, because a check-then-act admission passes an
// end-of-run assertion and still overshoots while the writers are going.
func TestMemoryNeverExceedsLimit(t *testing.T) {
	const (
		writers  = 8
		duration = 300 * time.Millisecond
	)
	value := make([]byte, 64)
	limit := 200 * uint64(CalculateSize([]byte("key:00000000"), value))

	engine := NewShardedEngine(EngineConfig{ShardCount: 4, MaxMemory: limit, MaxValueSize: 1 << 20})
	defer engine.Close()
	engine.SetEvictionController(&fakeController{})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var peak atomic.Uint64
	var samples atomic.Int64

	record := func() {
		used := engine.MemoryUsed()
		for {
			current := peak.Load()
			if used <= current || peak.CompareAndSwap(current, used) {
				break
			}
		}
		samples.Add(1)
	}

	wg.Add(writers + 1)
	for w := 0; w < writers; w++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				key := []byte(fmt.Sprintf("key:%04d%04d", id, i%500))
				if err := engine.Set(key, value, 0); err != nil {
					assert.ErrorIs(t, err, ErrOutOfMemory)
				}
			}
		}(w)
	}
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			record()
			assert.LessOrEqual(t, engine.MemoryUsed(), limit,
				"max_memory must hold at every instant, not just at rest")
		}
	}()

	time.Sleep(duration)
	close(stop)
	wg.Wait()

	assert.Positive(t, samples.Load(), "the limit was actually sampled during the run")
	assert.LessOrEqual(t, peak.Load(), limit)
	assert.Positive(t, engine.Stats().Evictions)
	assertAccounting(t, engine)
}

// TestSetNXHeldToTheSameLimit is the other half of ISSUE-0010: SetNX ignored
// max_memory entirely, so any SETNX traffic walked straight past it.
func TestSetNXHeldToTheSameLimit(t *testing.T) {
	const value = "0123456789"
	limit := 10 * entrySize("key:0000", value)

	t.Run("with eviction it makes room, exactly as Set does", func(t *testing.T) {
		engine, _ := newEvictingEngine(limit)
		defer engine.Close()

		for i := 0; i < 200; i++ {
			stored, err := engine.SetNX([]byte(fmt.Sprintf("key:%04d", i)), []byte(value), 0)
			require.NoError(t, err)
			require.True(t, stored)
			require.LessOrEqual(t, engine.MemoryUsed(), limit)
		}

		assert.Equal(t, uint64(10), engine.Stats().Keys)
		assertAccounting(t, engine)
	})

	t.Run("with no policy it is refused, exactly as Set is", func(t *testing.T) {
		engine := NewShardedEngine(EngineConfig{ShardCount: 1, MaxMemory: limit, MaxValueSize: 1 << 20})
		defer engine.Close()

		var refused int
		for i := 0; i < 200; i++ {
			stored, err := engine.SetNX([]byte(fmt.Sprintf("key:%04d", i)), []byte(value), 0)
			if err != nil {
				require.ErrorIs(t, err, ErrOutOfMemory)
				assert.False(t, stored)
				refused++
			}
		}

		assert.Positive(t, refused)
		assert.LessOrEqual(t, engine.MemoryUsed(), limit)
		assertAccounting(t, engine)
	})

	t.Run("a refused SetNX costs no victims", func(t *testing.T) {
		engine, controller := newEvictingEngine(limit)
		defer engine.Close()

		key := []byte("key:0000")
		require.NoError(t, engine.Set(key, []byte(value), 0))
		for i := 1; i < 10; i++ {
			require.NoError(t, engine.Set([]byte(fmt.Sprintf("key:%04d", i)), []byte(value), 0))
		}
		before := engine.Stats().Evictions
		controller.calls.Store(0)

		// The cache is exactly full and this key is taken, so the write cannot
		// happen. It must not pay for room it will never use.
		stored, err := engine.SetNX(key, []byte("a-much-longer-value-than-the-others"), 0)

		require.NoError(t, err)
		assert.False(t, stored)
		assert.Equal(t, int64(0), controller.calls.Load(), "no selection was asked for")
		assert.Equal(t, before, engine.Stats().Evictions)
	})
}

// TestOverwriteIsChargedTheDifference covers the double charge FEAT-0013 called
// out: admission compared used+newSize against the limit without crediting the
// entry being replaced, so rewriting a key in a full cache evicted its
// neighbors to make room for memory it was about to give back.
func TestOverwriteIsChargedTheDifference(t *testing.T) {
	const value = "0123456789"
	limit := 2 * entrySize("key:a", value)

	engine, controller := newEvictingEngine(limit)
	defer engine.Close()

	require.NoError(t, engine.Set([]byte("key:a"), []byte(value), 0))
	require.NoError(t, engine.Set([]byte("key:b"), []byte(value), 0))
	require.Equal(t, limit, engine.MemoryUsed(), "the cache is exactly full")
	controller.calls.Store(0)

	require.NoError(t, engine.Set([]byte("key:a"), []byte("9876543210"), 0))

	assert.Equal(t, int64(0), controller.calls.Load(), "a same-size overwrite needs no room")
	assert.Equal(t, uint64(0), engine.Stats().Evictions)
	assert.True(t, engine.Exists([]byte("key:b")), "the neighbor survived")

	got, _, ok := engine.Get([]byte("key:a"))
	require.True(t, ok)
	assert.Equal(t, "9876543210", string(got))
	assertAccounting(t, engine)
}

// TestEvictionAttemptsAreCapped covers ADR-0018's cap: one large value must not
// be able to empty the cache trying to fit.
func TestEvictionAttemptsAreCapped(t *testing.T) {
	const (
		value = "0123456789"
		keys  = 200
	)
	limit := keys * entrySize("key:0000", value)

	engine, _ := newEvictingEngine(limit)
	defer engine.Close()

	for i := 0; i < keys; i++ {
		require.NoError(t, engine.Set([]byte(fmt.Sprintf("key:%04d", i)), []byte(value), 0))
	}
	before := engine.Stats().Keys

	// Large enough that covering it would take more victims than the cap allows.
	huge := make([]byte, (maxEvictionsPerWrite+20)*int(entrySize("key:0000", value)))
	err := engine.Set([]byte("huge"), huge, 0)

	require.ErrorIs(t, err, ErrOutOfMemory)
	assert.False(t, engine.Exists([]byte("huge")))
	assert.Positive(t, engine.Stats().OOMRejected)
	assert.LessOrEqual(t, before-engine.Stats().Keys, uint64(maxEvictionsPerWrite),
		"the write took no more victims than the cap allows")
	assertAccounting(t, engine)
}

// TestValueLargerThanTheLimitEvictsNothing is the degenerate case of the same
// rule: a value that cannot fit in an empty cache must not empty it trying.
func TestValueLargerThanTheLimitEvictsNothing(t *testing.T) {
	const value = "0123456789"
	limit := 4 * entrySize("key:0000", value)

	engine, controller := newEvictingEngine(limit)
	defer engine.Close()

	for i := 0; i < 4; i++ {
		require.NoError(t, engine.Set([]byte(fmt.Sprintf("key:%04d", i)), []byte(value), 0))
	}
	controller.calls.Store(0)

	err := engine.Set([]byte("oversized"), make([]byte, limit+1), 0)

	require.ErrorIs(t, err, ErrOutOfMemory)
	assert.Equal(t, int64(0), controller.calls.Load(), "nothing was sampled for a hopeless write")
	assert.Equal(t, uint64(4), engine.Stats().Keys, "the cache is intact")
	assertAccounting(t, engine)
}

// TestEmptySelectionDoesNotSpin covers the other termination condition: when the
// policy has nothing to give, the write gives up rather than asking again.
func TestEmptySelectionDoesNotSpin(t *testing.T) {
	const value = "0123456789"
	limit := entrySize("key:0000", value)

	engine, controller := newEvictingEngine(limit)
	defer engine.Close()
	controller.empty = true

	require.NoError(t, engine.Set([]byte("key:0000"), []byte(value), 0))
	controller.calls.Store(0)

	err := engine.Set([]byte("key:0001"), []byte(value), 0)

	require.ErrorIs(t, err, ErrOutOfMemory)
	assert.Equal(t, int64(1), controller.calls.Load(),
		"one selection, then the write stops; a retry loop here is an infinite one")
	assertAccounting(t, engine)
}

// TestNoControllerRejects keeps the pre-eviction behavior available: with no
// policy installed a full cache refuses writes rather than losing data, which
// is what eviction.policy: none asks for.
func TestNoControllerRejects(t *testing.T) {
	const value = "0123456789"
	limit := entrySize("key:0000", value)

	engine, _ := newEvictingEngine(limit)
	defer engine.Close()
	engine.SetEvictionController(nil)

	require.NoError(t, engine.Set([]byte("key:0000"), []byte(value), 0))

	require.ErrorIs(t, engine.Set([]byte("key:0001"), []byte(value), 0), ErrOutOfMemory)
	assert.True(t, engine.Exists([]byte("key:0000")), "nothing was evicted to make room")
	assert.Positive(t, engine.Stats().OOMRejected)
	assertAccounting(t, engine)
}

// TestUnlimitedMemorySkipsAdmission keeps the common configuration free of the
// eviction path entirely.
func TestUnlimitedMemorySkipsAdmission(t *testing.T) {
	engine, controller := newEvictingEngine(0)
	defer engine.Close()

	for i := 0; i < 1000; i++ {
		require.NoError(t, engine.Set([]byte(fmt.Sprintf("key:%04d", i)), []byte("value"), 0))
	}

	assert.Equal(t, int64(0), controller.calls.Load())
	assert.Equal(t, uint64(1000), engine.Stats().Keys)
}
