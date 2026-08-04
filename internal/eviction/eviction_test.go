package eviction

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b3vet/atlascache/internal/storage"
)

// shardWith builds a shard holding the given entries, and returns it with the
// size of one entry so a test can talk in entries rather than bytes.
func shardWith(t *testing.T, entries map[string]*storage.Entry) *storage.Shard {
	t.Helper()

	shard := storage.NewShard(len(entries), nil)
	for key, entry := range entries {
		require.NoError(t, shard.Set(key, entry))
	}

	return shard
}

// entryAt builds an entry and back-dates the access metadata the policies rank
// on. Writing the fields directly is what makes the ranking deterministic:
// waiting for real time to pass would make these tests slow and flaky.
func entryAt(key string, created, lastAccess time.Time, accesses uint32) *storage.Entry {
	entry := storage.NewEntry([]byte(key), []byte("value"), 0)
	entry.CreatedAt = created.UnixNano()
	entry.LastAccess.Store(lastAccess.UnixNano())
	entry.AccessCnt.Store(accesses)

	return entry
}

func mustNew(t *testing.T, cfg Config) Controller {
	t.Helper()

	controller, err := New(cfg)
	require.NoError(t, err)

	return controller
}

// TestPoliciesPickTheirVictim states what each policy is for, one case each. A
// sample size above the entry count makes the sample the whole shard, so what is
// under test is the ranking rather than which entries the draw happened to see.
func TestPoliciesPickTheirVictim(t *testing.T) {
	now := time.Now()

	// coldest was read longest ago; oldest was written first; rarest has been
	// read least. Each policy has exactly one right answer here.
	entries := map[string]*storage.Entry{
		"coldest": entryAt("coldest", now.Add(-2*time.Minute), now.Add(-time.Hour), 500),
		"oldest":  entryAt("oldest", now.Add(-time.Hour), now.Add(-time.Second), 500),
		"rarest":  entryAt("rarest", now.Add(-2*time.Minute), now.Add(-time.Second), 1),
		"hot":     entryAt("hot", now.Add(-time.Minute), now, 5000),
	}

	cases := []struct {
		policy string
		victim string
		why    string
	}{
		{PolicyLRU, "coldest", "lru takes the least recently read, however often it was read"},
		{PolicyLFU, "coldest", "lfu takes the least frequently read once age is discounted"},
		{PolicyFIFO, "oldest", "fifo takes the first one written, however hot it is now"},
	}

	for _, tc := range cases {
		t.Run(tc.policy, func(t *testing.T) {
			shard := shardWith(t, entries)
			controller := mustNew(t, Config{Policy: tc.policy, SampleSize: 100})

			victims := controller.SelectVictims(shard, 1)

			require.NotEmpty(t, victims)
			assert.Equal(t, tc.victim, victims[0], tc.why)
		})
	}

	t.Run(PolicyNone, func(t *testing.T) {
		shard := shardWith(t, entries)
		controller := mustNew(t, Config{Policy: PolicyNone, SampleSize: 100})

		assert.Empty(t, controller.SelectVictims(shard, 1), "none evicts nothing at all")
	})
}

// TestLFUDiscountsByAge covers the reason lfu needs decay: a raw counter never
// forgets, so a key that was hot last week outranks one that is hot now,
// forever. The discount is computed at sample time from LastAccess, so nothing
// walks the keyspace to age counters.
func TestLFUDiscountsByAge(t *testing.T) {
	now := time.Now()
	controller := mustNew(t, Config{Policy: PolicyLFU, SampleSize: 100, LFUHalfLife: time.Minute})

	t.Run("a stale hot key loses to a warm recent one", func(t *testing.T) {
		shard := shardWith(t, map[string]*storage.Entry{
			// 1024 accesses, but untouched for ten half-lives: 1024 >> 10 = 1.
			"stale-hot": entryAt("stale-hot", now.Add(-time.Hour), now.Add(-10*time.Minute), 1024),
			// Four accesses, all recent, so nothing is discounted.
			"warm-now": entryAt("warm-now", now.Add(-time.Hour), now, 4),
		})

		victims := controller.SelectVictims(shard, 1)

		require.NotEmpty(t, victims)
		assert.Equal(t, "stale-hot", victims[0])
	})

	t.Run("with equal idle time the raw counter decides", func(t *testing.T) {
		shard := shardWith(t, map[string]*storage.Entry{
			"busy": entryAt("busy", now, now, 100),
			"idle": entryAt("idle", now, now, 2),
		})

		victims := controller.SelectVictims(shard, 1)

		require.NotEmpty(t, victims)
		assert.Equal(t, "idle", victims[0])
	})

	t.Run("an entry idle beyond every halving scores zero rather than shifting out of range", func(t *testing.T) {
		shard := shardWith(t, map[string]*storage.Entry{
			"ancient": entryAt("ancient", now.Add(-time.Hour), now.Add(-1000*time.Hour), 4_000_000),
			"fresh":   entryAt("fresh", now, now, 1),
		})

		victims := controller.SelectVictims(shard, 1)

		require.NotEmpty(t, victims)
		assert.Equal(t, "ancient", victims[0])
	})
}

// TestSelectVictimsCoversTheShortfall checks the other half of selection: enough
// victims to free what the write needs, and no more than the sample holds.
func TestSelectVictimsCoversTheShortfall(t *testing.T) {
	now := time.Now()
	entries := make(map[string]*storage.Entry, 10)
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("key:%02d", i)
		entries[key] = entryAt(key, now, now.Add(-time.Duration(10-i)*time.Minute), 1)
	}
	shard := shardWith(t, entries)
	one := uint64(storage.CalculateSize([]byte("key:00"), []byte("value")))

	controller := mustNew(t, Config{Policy: PolicyLRU, SampleSize: 100})

	t.Run("one entry's worth takes one victim", func(t *testing.T) {
		assert.Len(t, controller.SelectVictims(shard, one), 1)
	})

	t.Run("three entries' worth takes three, coldest first", func(t *testing.T) {
		victims := controller.SelectVictims(shard, 3*one)

		require.Len(t, victims, 3)
		assert.Equal(t, []string{"key:00", "key:01", "key:02"}, victims)
	})

	t.Run("more than the shard holds takes everything it saw", func(t *testing.T) {
		assert.Len(t, controller.SelectVictims(shard, 1_000_000), 10)
	})

	t.Run("nothing needed takes nothing", func(t *testing.T) {
		assert.Empty(t, controller.SelectVictims(shard, 0))
	})
}

// TestSamplingBoundsTheDraw is what keeps selection off the O(n) path: the shard
// is sampled, never scanned, however many keys it holds.
func TestSamplingBoundsTheDraw(t *testing.T) {
	now := time.Now()
	entries := make(map[string]*storage.Entry, 500)
	for i := 0; i < 500; i++ {
		key := fmt.Sprintf("key:%03d", i)
		entries[key] = entryAt(key, now, now, 1)
	}
	shard := shardWith(t, entries)

	controller := mustNew(t, Config{Policy: PolicyLRU, SampleSize: 5})
	victims := controller.SelectVictims(shard, 1_000_000)

	assert.Len(t, victims, 5, "a selection never looks at more than sample_size entries")
}

func TestSelectVictimsEdgeCases(t *testing.T) {
	controller := mustNew(t, Config{Policy: PolicyLRU})

	t.Run("an empty shard yields nothing, so the caller stops", func(t *testing.T) {
		assert.Empty(t, controller.SelectVictims(storage.NewShard(4, nil), 1024))
	})

	t.Run("a shard holding only expired entries yields nothing", func(t *testing.T) {
		shard := storage.NewShard(4, nil)
		require.NoError(t, shard.Set("dead", storage.NewEntry([]byte("dead"), []byte("v"), time.Nanosecond)))
		time.Sleep(time.Millisecond)

		assert.Empty(t, controller.SelectVictims(shard, 1024),
			"an expired entry is the wheel's to reclaim, not a victim to spend an attempt on")
	})

	t.Run("no shard yields nothing", func(t *testing.T) {
		assert.Empty(t, controller.SelectVictims(nil, 1024))
	})
}

func TestPolicyNames(t *testing.T) {
	t.Run("the zero config is lru", func(t *testing.T) {
		assert.Equal(t, PolicyLRU, mustNew(t, Config{}).Policy())
	})

	t.Run("names are matched loosely", func(t *testing.T) {
		assert.Equal(t, PolicyLFU, mustNew(t, Config{Policy: " LFU "}).Policy())
	})

	t.Run("an unknown name is refused", func(t *testing.T) {
		_, err := New(Config{Policy: "random"})

		require.ErrorIs(t, err, ErrUnknownPolicy)
		assert.Contains(t, err.Error(), "random")
	})
}

// TestSetPolicySwitchesAtRuntime covers the hot-reload path: the policy changes
// under live traffic without rebuilding anything.
func TestSetPolicySwitchesAtRuntime(t *testing.T) {
	now := time.Now()
	entries := map[string]*storage.Entry{
		"coldest": entryAt("coldest", now.Add(-time.Minute), now.Add(-time.Hour), 100),
		"oldest":  entryAt("oldest", now.Add(-time.Hour), now, 100),
	}
	shard := shardWith(t, entries)
	controller := mustNew(t, Config{Policy: PolicyLRU, SampleSize: 100})

	require.Equal(t, "coldest", controller.SelectVictims(shard, 1)[0])

	require.NoError(t, controller.SetPolicy(PolicyFIFO))
	assert.Equal(t, PolicyFIFO, controller.Policy())
	assert.Equal(t, "oldest", controller.SelectVictims(shard, 1)[0])

	require.ErrorIs(t, controller.SetPolicy("nonsense"), ErrUnknownPolicy)
	assert.Equal(t, PolicyFIFO, controller.Policy(), "a refused switch leaves the policy alone")
}

// TestConcurrentPolicySwitch is the same switch under load: a reload lands while
// writes are selecting victims, and neither side may see a torn policy.
func TestConcurrentPolicySwitch(t *testing.T) {
	now := time.Now()
	entries := make(map[string]*storage.Entry, 50)
	for i := 0; i < 50; i++ {
		key := fmt.Sprintf("key:%02d", i)
		entries[key] = entryAt(key, now, now, uint32(i+1))
	}
	shard := shardWith(t, entries)
	controller := mustNew(t, Config{Policy: PolicyLRU})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	policies := []string{PolicyLRU, PolicyLFU, PolicyFIFO, PolicyNone}

	wg.Add(4)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			require.NoError(t, controller.SetPolicy(policies[i%len(policies)]))
		}
	}()
	for g := 0; g < 3; g++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				controller.SelectVictims(shard, 1024)
				_ = controller.Policy()
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestSampleSizeIsClamped(t *testing.T) {
	now := time.Now()
	entries := make(map[string]*storage.Entry, MaxSampleSize+50)
	for i := 0; i < MaxSampleSize+50; i++ {
		key := fmt.Sprintf("key:%03d", i)
		entries[key] = entryAt(key, now, now, 1)
	}
	shard := shardWith(t, entries)

	t.Run("zero falls back to the default", func(t *testing.T) {
		controller := mustNew(t, Config{Policy: PolicyLRU, SampleSize: 0})

		assert.Len(t, controller.SelectVictims(shard, 1_000_000), DefaultSampleSize)
	})

	t.Run("an oversized sample is capped", func(t *testing.T) {
		controller := mustNew(t, Config{Policy: PolicyLRU, SampleSize: 10_000})

		assert.Len(t, controller.SelectVictims(shard, 1_000_000), MaxSampleSize)
	})
}

// TestEngineEvictsUnderEachPolicy is the integration the storage package cannot
// run itself: the real controller wired into the real engine, one policy at a
// time, each asked to give up the entry it is supposed to give up.
func TestEngineEvictsUnderEachPolicy(t *testing.T) {
	value := []byte("0123456789")
	entry := uint64(storage.CalculateSize([]byte("key:x"), value))

	cases := []struct {
		policy   string
		survivor string
		victim   string
	}{
		{PolicyLRU, "key:b", "key:a"},
		{PolicyLFU, "key:b", "key:a"},
		{PolicyFIFO, "key:b", "key:a"},
	}

	for _, tc := range cases {
		t.Run(tc.policy, func(t *testing.T) {
			controller := mustNew(t, Config{Policy: tc.policy, SampleSize: 100})
			engine := storage.NewShardedEngine(storage.EngineConfig{
				ShardCount:   1,
				MaxMemory:    2 * entry,
				MaxValueSize: 1 << 20,
			})
			defer engine.Close()
			engine.SetEvictionController(controller)

			// key:a is written first and left alone; key:b is written after and
			// then read, so it is the newer, hotter and more recently used of
			// the two under every policy.
			require.NoError(t, engine.Set([]byte("key:a"), value, 0))
			time.Sleep(2 * time.Millisecond)
			require.NoError(t, engine.Set([]byte("key:b"), value, 0))
			for i := 0; i < 10; i++ {
				_, _, ok := engine.Get([]byte("key:b"))
				require.True(t, ok)
			}

			require.NoError(t, engine.Set([]byte("key:c"), value, 0), "the write makes room for itself")

			assert.False(t, engine.Exists([]byte(tc.victim)), "%s was evicted", tc.victim)
			assert.True(t, engine.Exists([]byte(tc.survivor)), "%s was kept", tc.survivor)
			assert.True(t, engine.Exists([]byte("key:c")))
			assert.LessOrEqual(t, engine.MemoryUsed(), engine.MaxMemory())
			assert.Equal(t, uint64(1), engine.Stats().Evictions)
		})
	}

	t.Run(PolicyNone, func(t *testing.T) {
		controller := mustNew(t, Config{Policy: PolicyNone})
		engine := storage.NewShardedEngine(storage.EngineConfig{
			ShardCount:   1,
			MaxMemory:    2 * entry,
			MaxValueSize: 1 << 20,
		})
		defer engine.Close()
		engine.SetEvictionController(controller)

		require.NoError(t, engine.Set([]byte("key:a"), value, 0))
		require.NoError(t, engine.Set([]byte("key:b"), value, 0))

		require.ErrorIs(t, engine.Set([]byte("key:c"), value, 0), storage.ErrOutOfMemory)
		assert.True(t, engine.Exists([]byte("key:a")), "none keeps everything and refuses the write")
		assert.True(t, engine.Exists([]byte("key:b")))
		assert.Equal(t, uint64(0), engine.Stats().Evictions)
		assert.Positive(t, engine.Stats().OOMRejected)
	})
}
