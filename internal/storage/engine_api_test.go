package storage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteValidation(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{ShardCount: 4, MaxValueSize: 64})
	defer engine.Close()

	cases := []struct {
		name  string
		key   []byte
		value []byte
		ttl   time.Duration
		want  error
	}{
		{"empty key", nil, []byte("v"), 0, ErrInvalidKey},
		{"value too large", []byte("k"), make([]byte, 65), 0, ErrValueTooLarge},
		{"negative ttl", []byte("k"), []byte("v"), -time.Second, ErrInvalidTTL},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.ErrorIs(t, engine.Set(tc.key, tc.value, tc.ttl), tc.want)

			ok, err := engine.SetNX(tc.key, tc.value, tc.ttl)
			assert.ErrorIs(t, err, tc.want)
			assert.False(t, ok)
		})
	}
}

func TestClosedEngineRejectsEverything(t *testing.T) {
	engine := NewShardedEngine(DefaultEngineConfig())
	require.NoError(t, engine.Set([]byte("k"), []byte("v"), time.Hour))
	require.NoError(t, engine.Close())

	assert.True(t, engine.IsClosed())

	assert.ErrorIs(t, engine.Set([]byte("k"), []byte("v"), 0), ErrEngineClosed)

	ok, err := engine.SetNX([]byte("k"), []byte("v"), 0)
	assert.ErrorIs(t, err, ErrEngineClosed)
	assert.False(t, ok)

	_, _, exists := engine.Get([]byte("k"))
	assert.False(t, exists)
	assert.False(t, engine.Delete([]byte("k")))
	assert.False(t, engine.Exists([]byte("k")))
	assert.False(t, engine.SetTTL([]byte("k"), time.Hour))

	_, exists = engine.GetTTL([]byte("k"))
	assert.False(t, exists)

	_, exists = engine.GetEntry([]byte("k"))
	assert.False(t, exists)

	assert.Nil(t, engine.Keys("*"))

	keys, cursor, err := engine.Scan(1, ScanCursorStart, 10)
	assert.ErrorIs(t, err, ErrEngineClosed)
	assert.Nil(t, keys)
	assert.Equal(t, ScanCursorStart, cursor)

	assert.Equal(t, uint64(0), engine.MemoryUsed())
}

func TestEngineTTLAccessors(t *testing.T) {
	engine := NewShardedEngine(DefaultEngineConfig())
	defer engine.Close()

	require.NoError(t, engine.Set([]byte("no-ttl"), []byte("v"), 0))
	require.NoError(t, engine.Set([]byte("with-ttl"), []byte("v"), time.Hour))
	require.NoError(t, engine.Set([]byte("dead"), []byte("v"), time.Millisecond))
	time.Sleep(5 * time.Millisecond)

	ttl, exists := engine.GetTTL([]byte("no-ttl"))
	assert.True(t, exists)
	assert.Equal(t, time.Duration(-1), ttl)

	ttl, exists = engine.GetTTL([]byte("with-ttl"))
	assert.True(t, exists)
	assert.Positive(t, ttl)

	_, exists = engine.GetTTL([]byte("dead"))
	assert.False(t, exists)

	_, exists = engine.GetTTL([]byte("absent"))
	assert.False(t, exists)

	assert.False(t, engine.SetTTL([]byte("dead"), time.Hour), "a dead entry cannot be revived by EXPIRE")
	assert.False(t, engine.SetTTL([]byte("absent"), time.Hour))

	entry, exists := engine.GetEntry([]byte("with-ttl"))
	require.True(t, exists)
	assert.Equal(t, "v", string(entry.Value))

	_, exists = engine.GetEntry([]byte("absent"))
	assert.False(t, exists)
}

func TestEngineShardRouting(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{ShardCount: 8, MaxValueSize: 1024})
	defer engine.Close()

	assert.Len(t, engine.GetAllShards(), 8)

	key := []byte("routed")
	require.NoError(t, engine.Set(key, []byte("v"), 0))

	shard := engine.GetShard(key)
	require.NotNil(t, shard)
	assert.Equal(t, 1, shard.Len())
	assert.Same(t, shard, engine.GetShard(key), "routing is stable")
}

func TestEngineCounters(t *testing.T) {
	engine := NewShardedEngine(DefaultEngineConfig())
	defer engine.Close()

	engine.SetMaxMemory(4096)
	assert.Equal(t, uint64(4096), engine.MaxMemory())

	for _, key := range []string{"doomed:1", "doomed:2"} {
		require.NoError(t, engine.Set([]byte(key), []byte("v"), time.Millisecond))
	}
	time.Sleep(5 * time.Millisecond)
	for _, key := range []string{"doomed:1", "doomed:2"} {
		require.True(t, engine.DeleteExpired([]byte(key)))
	}

	engine.RecordEviction()

	stats := engine.Stats()
	assert.Equal(t, uint64(2), stats.Expirations)
	assert.Equal(t, uint64(1), stats.Evictions)
	assert.Equal(t, uint64(4096), stats.MemoryMax)
}

// TestKeyspaceSeamBounds covers the ttl.Keyspace side of the engine. The shard
// index comes back from a hint the manager may have held for days, so it is
// bounds-checked rather than trusted.
func TestKeyspaceSeamBounds(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{ShardCount: 4, MaxValueSize: 1024})

	key := []byte("k")
	require.NoError(t, engine.Set(key, []byte("v"), time.Millisecond))
	idx := shardIndexOf(engine, key)

	t.Run("out of range indexes answer as absent", func(t *testing.T) {
		_, present := engine.ExpiryOf(-1, "k")
		assert.False(t, present)

		_, present = engine.ExpiryOf(4, "k")
		assert.False(t, present)

		assert.False(t, engine.Expire(-1, "k"))
		assert.False(t, engine.Expire(4, "k"))
	})

	t.Run("a resident key reports its expiry", func(t *testing.T) {
		expireAt, present := engine.ExpiryOf(idx, "k")
		assert.True(t, present)
		assert.Positive(t, expireAt)

		_, present = engine.ExpiryOf(idx, "absent")
		assert.False(t, present)
	})

	t.Run("a live key is not expired", func(t *testing.T) {
		assert.False(t, engine.Expire(idx, "k"), "the deadline has not passed yet")
	})

	t.Run("an expired key is reclaimed once", func(t *testing.T) {
		time.Sleep(5 * time.Millisecond)

		assert.True(t, engine.Expire(idx, "k"))
		assert.False(t, engine.Expire(idx, "k"))
		assert.Equal(t, uint64(0), engine.MemoryUsed())
	})

	t.Run("a closed engine expires nothing", func(t *testing.T) {
		require.NoError(t, engine.Close())

		_, present := engine.ExpiryOf(idx, "k")
		assert.False(t, present)
		assert.False(t, engine.Expire(idx, "k"))
		assert.False(t, engine.DeleteExpired(key))
	})
}

func TestKeysPatternMatching(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{ShardCount: 4, MaxValueSize: 1024})
	defer engine.Close()

	for _, key := range []string{"user:1", "user:2", "session:1", "a[b"} {
		require.NoError(t, engine.Set([]byte(key), []byte("v"), 0))
	}

	assert.Len(t, engine.Keys(""), 4, "an empty pattern matches everything")
	assert.Len(t, engine.Keys("*"), 4)
	assert.Len(t, engine.Keys("user:*"), 2)
	assert.Len(t, engine.Keys("user:?"), 2)
	assert.Len(t, engine.Keys("session:1"), 1)
	assert.Empty(t, engine.Keys("nothing:*"))

	// filepath.Match rejects an unterminated character class, and matchPattern
	// falls back to literal prefix/suffix/contains handling.
	t.Run("malformed patterns fall back to literal matching", func(t *testing.T) {
		assert.Len(t, engine.Keys("*a[b*"), 1, "contains")
		assert.Len(t, engine.Keys("*a[b"), 1, "suffix")
		assert.Len(t, engine.Keys("a[b*"), 1, "prefix")
		assert.Len(t, engine.Keys("a[b"), 1, "equality")
		assert.Empty(t, engine.Keys("z[q"))
	})
}

func TestNextPowerOfTwo(t *testing.T) {
	cases := map[int]int{-4: 1, 0: 1, 1: 1, 2: 2, 3: 4, 64: 64, 65: 128, 1000: 1024}
	for in, want := range cases {
		assert.Equal(t, want, nextPowerOfTwo(in), "nextPowerOfTwo(%d)", in)
	}

	engine := NewShardedEngine(EngineConfig{ShardCount: 0})
	defer engine.Close()
	assert.Len(t, engine.GetAllShards(), 64, "a non-positive shard count falls back to the default")
}
