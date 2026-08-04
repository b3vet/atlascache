package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWritesCopyCallerBuffers is the regression test for ISSUE-0009. A server
// reading into a per-connection buffer reuses it across requests, so anything
// the engine keeps a reference to is rewritten by the next command.
func TestWritesCopyCallerBuffers(t *testing.T) {
	t.Run("Set", func(t *testing.T) {
		engine := NewShardedEngine(DefaultEngineConfig())
		defer engine.Close()

		key := []byte("reuse-key1")
		value := []byte("original-value")
		require.NoError(t, engine.Set(key, value, 0))

		copy(key, "reuse-key2")
		copy(value, "clobbered-!!!!")

		got, _, exists := engine.Get([]byte("reuse-key1"))
		require.True(t, exists, "the stored key survived its buffer being reused")
		assert.Equal(t, "original-value", string(got))
		assert.False(t, engine.Exists([]byte("reuse-key2")))
	})

	t.Run("SetNX", func(t *testing.T) {
		engine := NewShardedEngine(DefaultEngineConfig())
		defer engine.Close()

		key := []byte("reuse-key3")
		value := []byte("original-value")
		ok, err := engine.SetNX(key, value, 0)
		require.NoError(t, err)
		require.True(t, ok)

		copy(key, "reuse-key4")
		copy(value, "clobbered-!!!!")

		got, _, exists := engine.Get([]byte("reuse-key3"))
		require.True(t, exists)
		assert.Equal(t, "original-value", string(got))
		assert.False(t, engine.Exists([]byte("reuse-key4")))
	})

	t.Run("the entry does not alias the caller's slices", func(t *testing.T) {
		engine := NewShardedEngine(DefaultEngineConfig())
		defer engine.Close()

		key := []byte("alias-key")
		value := []byte("alias-value")
		require.NoError(t, engine.Set(key, value, 0))

		entry, ok := engine.GetEntry(key)
		require.True(t, ok)
		assert.NotSame(t, &key[0], &entry.Key[0])
		assert.NotSame(t, &value[0], &entry.Value[0])
	})
}

// TestGetReturnsAnInternalView pins the read half of ADR-0013: Get hands back
// storage's own memory rather than a copy, which is why the interface carries a
// do-not-mutate, do-not-retain contract. If someone changes Get to copy, this
// fails and the decision gets made deliberately.
func TestGetReturnsAnInternalView(t *testing.T) {
	engine := NewShardedEngine(DefaultEngineConfig())
	defer engine.Close()

	key := []byte("view-key")
	require.NoError(t, engine.Set(key, []byte("view-value"), 0))

	entry, ok := engine.GetEntry(key)
	require.True(t, ok)

	got, _, exists := engine.Get(key)
	require.True(t, exists)
	require.NotEmpty(t, got)

	assert.Same(t, &entry.Value[0], &got[0])
}

// TestOverwriteLeavesReadersValidBytes exercises the invariant the read
// contract rests on: a write builds a new entry and swaps the map pointer, so a
// view taken before the write keeps reading valid, if stale, bytes.
func TestOverwriteLeavesReadersValidBytes(t *testing.T) {
	engine := NewShardedEngine(DefaultEngineConfig())
	defer engine.Close()

	key := []byte("swap-key")
	require.NoError(t, engine.Set(key, []byte("first-value"), 0))

	stale, _, exists := engine.Get(key)
	require.True(t, exists)

	require.NoError(t, engine.Set(key, []byte("second-valu"), 0))

	assert.Equal(t, "first-value", string(stale), "the superseded array was not written over")

	fresh, _, exists := engine.Get(key)
	require.True(t, exists)
	assert.Equal(t, "second-valu", string(fresh))
}
