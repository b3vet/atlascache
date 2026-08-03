package storage

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewShardedEngine(t *testing.T) {
	t.Run("default config", func(t *testing.T) {
		cfg := DefaultEngineConfig()
		engine := NewShardedEngine(cfg)
		defer engine.Close()

		assert.NotNil(t, engine)
		assert.Equal(t, 64, len(engine.shards)) // Power of 2
	})

	t.Run("custom shard count rounds to power of 2", func(t *testing.T) {
		cfg := EngineConfig{ShardCount: 100}
		engine := NewShardedEngine(cfg)
		defer engine.Close()

		assert.Equal(t, 128, len(engine.shards)) // Next power of 2
	})
}

func TestBasicOperations(t *testing.T) {
	engine := NewShardedEngine(DefaultEngineConfig())
	defer engine.Close()

	key := []byte("test-key")
	value := []byte("test-value")

	t.Run("set and get", func(t *testing.T) {
		err := engine.Set(key, value, 0)
		require.NoError(t, err)

		got, ttl, exists := engine.Get(key)
		assert.True(t, exists)
		assert.Equal(t, value, got)
		assert.Equal(t, time.Duration(-1), ttl) // No TTL
	})

	t.Run("exists", func(t *testing.T) {
		assert.True(t, engine.Exists(key))
		assert.False(t, engine.Exists([]byte("nonexistent")))
	})

	t.Run("delete", func(t *testing.T) {
		deleted := engine.Delete(key)
		assert.True(t, deleted)
		assert.False(t, engine.Exists(key))

		deleted = engine.Delete(key)
		assert.False(t, deleted) // Already deleted
	})
}

func TestSetNX(t *testing.T) {
	engine := NewShardedEngine(DefaultEngineConfig())
	defer engine.Close()

	key := []byte("setnx-key")
	value1 := []byte("value1")
	value2 := []byte("value2")

	t.Run("set when not exists", func(t *testing.T) {
		ok, err := engine.SetNX(key, value1, 0)
		require.NoError(t, err)
		assert.True(t, ok)

		got, _, exists := engine.Get(key)
		assert.True(t, exists)
		assert.Equal(t, value1, got)
	})

	t.Run("fail when exists", func(t *testing.T) {
		ok, err := engine.SetNX(key, value2, 0)
		require.NoError(t, err)
		assert.False(t, ok)

		// Value should still be original
		got, _, _ := engine.Get(key)
		assert.Equal(t, value1, got)
	})
}

func TestTTL(t *testing.T) {
	engine := NewShardedEngine(DefaultEngineConfig())
	defer engine.Close()

	key := []byte("ttl-key")
	value := []byte("ttl-value")

	t.Run("set with TTL", func(t *testing.T) {
		err := engine.Set(key, value, 100*time.Millisecond)
		require.NoError(t, err)

		_, ttl, exists := engine.Get(key)
		assert.True(t, exists)
		assert.True(t, ttl > 0 && ttl <= 100*time.Millisecond)
	})

	t.Run("expires after TTL", func(t *testing.T) {
		time.Sleep(150 * time.Millisecond)

		_, _, exists := engine.Get(key)
		assert.False(t, exists)
	})

	t.Run("get and set TTL", func(t *testing.T) {
		key2 := []byte("ttl-key2")
		err := engine.Set(key2, value, 1*time.Hour)
		require.NoError(t, err)

		ttl, exists := engine.GetTTL(key2)
		assert.True(t, exists)
		assert.True(t, ttl > 59*time.Minute)

		// Update TTL
		ok := engine.SetTTL(key2, 10*time.Minute)
		assert.True(t, ok)

		ttl, exists = engine.GetTTL(key2)
		assert.True(t, exists)
		assert.True(t, ttl > 9*time.Minute && ttl <= 10*time.Minute)
	})
}

func TestKeys(t *testing.T) {
	engine := NewShardedEngine(DefaultEngineConfig())
	defer engine.Close()

	// Set some keys
	for i := 0; i < 10; i++ {
		key := []byte(fmt.Sprintf("key:%d", i))
		engine.Set(key, []byte("value"), 0)
	}
	for i := 0; i < 5; i++ {
		key := []byte(fmt.Sprintf("other:%d", i))
		engine.Set(key, []byte("value"), 0)
	}

	t.Run("all keys", func(t *testing.T) {
		keys := engine.Keys("*")
		assert.Equal(t, 15, len(keys))
	})

	t.Run("pattern matching", func(t *testing.T) {
		keys := engine.Keys("key:*")
		assert.Equal(t, 10, len(keys))

		keys = engine.Keys("other:*")
		assert.Equal(t, 5, len(keys))
	})
}

func TestScan(t *testing.T) {
	engine := NewShardedEngine(DefaultEngineConfig())
	defer engine.Close()

	// Set 100 keys
	for i := 0; i < 100; i++ {
		key := []byte(fmt.Sprintf("scan-key:%d", i))
		engine.Set(key, []byte("value"), 0)
	}

	t.Run("scan all keys", func(t *testing.T) {
		var allKeys [][]byte
		var cursor uint64

		for {
			keys, nextCursor := engine.Scan(cursor, 10)
			allKeys = append(allKeys, keys...)
			cursor = nextCursor

			if cursor == 0 {
				break
			}
		}

		assert.Equal(t, 100, len(allKeys))
	})
}

func TestMemoryLimit(t *testing.T) {
	cfg := EngineConfig{
		ShardCount:   16,
		MaxMemory:    1024, // 1KB limit
		MaxValueSize: 512,
	}
	engine := NewShardedEngine(cfg)
	defer engine.Close()

	t.Run("reject when over limit", func(t *testing.T) {
		// Fill up memory
		for i := 0; i < 100; i++ {
			key := []byte(fmt.Sprintf("key:%d", i))
			value := make([]byte, 100)
			err := engine.Set(key, value, 0)
			if err == ErrOutOfMemory {
				// This is expected once we hit the limit
				break
			}
		}

		// Verify we're at or near limit
		assert.True(t, engine.MemoryUsed() <= engine.MaxMemory()+200)
	})
}

func TestValueTooLarge(t *testing.T) {
	cfg := EngineConfig{
		ShardCount:   16,
		MaxValueSize: 100,
	}
	engine := NewShardedEngine(cfg)
	defer engine.Close()

	key := []byte("large-key")
	largeValue := make([]byte, 200)

	err := engine.Set(key, largeValue, 0)
	assert.Equal(t, ErrValueTooLarge, err)
}

func TestConcurrency(t *testing.T) {
	engine := NewShardedEngine(DefaultEngineConfig())
	defer engine.Close()

	const goroutines = 100
	const opsPerGoroutine = 1000

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				key := []byte(fmt.Sprintf("key:%d:%d", id, i))
				value := []byte(fmt.Sprintf("value:%d:%d", id, i))

				// Set
				err := engine.Set(key, value, 0)
				assert.NoError(t, err)

				// Get
				got, _, exists := engine.Get(key)
				assert.True(t, exists)
				assert.Equal(t, value, got)
			}
		}(g)
	}

	wg.Wait()

	// Verify stats
	stats := engine.Stats()
	assert.Equal(t, uint64(goroutines*opsPerGoroutine), stats.Sets)
}

func TestStats(t *testing.T) {
	engine := NewShardedEngine(DefaultEngineConfig())
	defer engine.Close()

	// Perform operations
	for i := 0; i < 100; i++ {
		key := []byte(fmt.Sprintf("key:%d", i))
		engine.Set(key, []byte("value"), 0)
	}

	for i := 0; i < 50; i++ {
		key := []byte(fmt.Sprintf("key:%d", i))
		engine.Get(key)
	}

	for i := 0; i < 10; i++ {
		key := []byte(fmt.Sprintf("nonexistent:%d", i))
		engine.Get(key)
	}

	stats := engine.Stats()
	assert.Equal(t, uint64(100), stats.Keys)
	assert.Equal(t, uint64(100), stats.Sets)
	assert.Equal(t, uint64(50), stats.Hits)
	assert.Equal(t, uint64(10), stats.Misses)
}

func TestClose(t *testing.T) {
	engine := NewShardedEngine(DefaultEngineConfig())

	// Set some data
	engine.Set([]byte("key"), []byte("value"), 0)

	// Close
	err := engine.Close()
	require.NoError(t, err)

	// Operations should fail
	err = engine.Set([]byte("key2"), []byte("value"), 0)
	assert.Equal(t, ErrEngineClosed, err)

	_, _, exists := engine.Get([]byte("key"))
	assert.False(t, exists)

	// Double close should return error
	err = engine.Close()
	assert.Equal(t, ErrEngineClosed, err)
}
