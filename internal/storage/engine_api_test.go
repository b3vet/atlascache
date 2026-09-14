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

// TestEmptyKeyIsAKey closes ISSUE-0013. Redis stores an empty key like any
// other; the engine used to refuse one, which made a legal Redis operation fail
// against a server whose whole premise is that existing clients work unmodified
// (ADR-0006). It was never a deliberate divergence, and nothing in the engine
// needs it — an empty string is a perfectly good map key.
func TestEmptyKeyIsAKey(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{ShardCount: 4, MaxValueSize: 64})
	defer engine.Close()

	empty := []byte("")

	require.NoError(t, engine.Set(empty, []byte("v"), 0))
	assert.True(t, engine.Exists(empty))

	value, _, exists := engine.Get(empty)
	assert.True(t, exists)
	assert.Equal(t, "v", string(value))

	// It participates in everything else a key participates in.
	stored, err := engine.SetNX(empty, []byte("other"), 0)
	require.NoError(t, err)
	assert.False(t, stored, "an empty key already holding a value blocks SetNX like any other")

	assert.True(t, engine.SetTTL(empty, time.Hour))
	ttl, exists := engine.GetTTL(empty)
	assert.True(t, exists)
	assert.Positive(t, ttl)

	assert.True(t, engine.Delete(empty))
	assert.False(t, engine.Exists(empty))

	// A nil key is the same key: both are zero bytes long.
	require.NoError(t, engine.Set(nil, []byte("v"), 0))
	assert.True(t, engine.Exists(empty))
	assert.True(t, engine.Delete(nil))
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

	keys, cursor, err := engine.Scan(1, ScanCursorStart, 10, "")
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

	assert.Len(t, engine.Keys("*"), 4)
	assert.Len(t, engine.Keys("user:*"), 2)
	assert.Len(t, engine.Keys("user:?"), 2)
	assert.Len(t, engine.Keys("session:1"), 1)
	assert.Empty(t, engine.Keys("nothing:*"))

	// Keys containing a separator are the case filepath.Match got wrong: it
	// refuses to let '*' cross one, and cache keys are made of them.
	t.Run("a wildcard crosses path separators", func(t *testing.T) {
		require.NoError(t, engine.Set([]byte("cdn/eu/logo.png"), []byte("v"), 0))

		assert.Len(t, engine.Keys("cdn/*"), 1)
		assert.Len(t, engine.Keys("*.png"), 1)
		assert.Len(t, engine.Keys("*/*/*"), 1)
	})

	// An empty pattern is not a match-all pattern in Redis; it matches the
	// empty key and nothing else. Only a bare "*" is short circuited.
	t.Run("an empty pattern is not a wildcard", func(t *testing.T) {
		assert.Empty(t, engine.Keys(""))

		require.NoError(t, engine.Set([]byte(""), []byte("v"), 0))
		assert.Len(t, engine.Keys(""), 1, "the empty pattern matches the empty key")
		assert.Contains(t, stringKeys(engine.Keys("*")), "", "a bare star returns the empty key too")

		assert.True(t, engine.Delete([]byte("")))
	})

	// Redis tolerates a malformed pattern rather than rejecting it, and
	// tolerating it does not mean matching it literally: none of these find
	// "a[b". Every expectation here was read off redis:7-alpine.
	t.Run("malformed patterns match what Redis matches", func(t *testing.T) {
		assert.Empty(t, engine.Keys("*a[b*"))
		assert.Empty(t, engine.Keys("*a[b"))
		assert.Empty(t, engine.Keys("a[b*"))
		assert.Empty(t, engine.Keys("a[b"))
		assert.Empty(t, engine.Keys("z[q"))

		assert.Len(t, engine.Keys(`a\[b`), 1, "an escaped bracket is a literal bracket")
	})
}

// stringKeys renders a Keys result for assertions that care about membership.
func stringKeys(keys [][]byte) []string {
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, string(key))
	}
	return out
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
