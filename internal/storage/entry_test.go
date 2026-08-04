package storage

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewEntryOwnsItsBytes(t *testing.T) {
	t.Run("mutating the source buffers leaves the entry alone", func(t *testing.T) {
		key := []byte("entry-key!")
		value := []byte("entry-value")

		entry := NewEntry(key, value, 0)

		copy(key, "XXXXXXXXXX")
		copy(value, "mutated!!!!")

		assert.Equal(t, "entry-key!", string(entry.Key))
		assert.Equal(t, "entry-value", string(entry.Value))
	})

	t.Run("key capacity is capped so an append cannot reach the value", func(t *testing.T) {
		entry := NewEntry([]byte("key"), []byte("value"), 0)

		require.Equal(t, len(entry.Key), cap(entry.Key))

		grown := entry.Key
		grown = append(grown, 'X')

		assert.Equal(t, "value", string(entry.Value))
		assert.Equal(t, "keyX", string(grown))
	})

	t.Run("size is computed from the owned buffer", func(t *testing.T) {
		key := []byte("size-key")
		value := []byte("size-value")

		entry := NewEntry(key, value, 0)

		assert.Equal(t, uint32(len(key)+len(value)+EntryOverhead), entry.Size)
		assert.Equal(t, CalculateSize(entry.Key, entry.Value), entry.Size)
	})

	t.Run("empty key and value", func(t *testing.T) {
		entry := NewEntry(nil, nil, 0)

		assert.Empty(t, entry.Key)
		assert.Empty(t, entry.Value)
		assert.Equal(t, uint32(EntryOverhead), entry.Size)
	})
}

func TestEntryTTL(t *testing.T) {
	t.Run("no ttl", func(t *testing.T) {
		entry := NewEntry([]byte("k"), []byte("v"), 0)

		assert.False(t, entry.HasTTL())
		assert.False(t, entry.IsExpired())
		assert.Equal(t, time.Duration(-1), entry.TTL())
		assert.Zero(t, entry.ExpireAt.Load())
	})

	t.Run("live ttl", func(t *testing.T) {
		entry := NewEntry([]byte("k"), []byte("v"), time.Hour)

		assert.True(t, entry.HasTTL())
		assert.False(t, entry.IsExpired())
		assert.Positive(t, entry.TTL())
		assert.LessOrEqual(t, entry.TTL(), time.Hour)
	})

	t.Run("expired ttl", func(t *testing.T) {
		entry := NewEntry([]byte("k"), []byte("v"), time.Nanosecond)
		time.Sleep(time.Millisecond)

		assert.True(t, entry.HasTTL())
		assert.True(t, entry.IsExpired())
		assert.Equal(t, time.Duration(0), entry.TTL())
	})

	t.Run("negative ttl is treated as no ttl", func(t *testing.T) {
		entry := NewEntry([]byte("k"), []byte("v"), -time.Hour)

		assert.False(t, entry.HasTTL())
		assert.False(t, entry.IsExpired())
	})
}

func TestEntryUpdateExpiry(t *testing.T) {
	entry := NewEntry([]byte("k"), []byte("v"), 0)

	entry.UpdateExpiry(time.Hour)
	assert.True(t, entry.HasTTL())
	assert.Greater(t, entry.TTL(), 59*time.Minute)

	entry.UpdateExpiry(0)
	assert.False(t, entry.HasTTL())
	assert.Equal(t, time.Duration(-1), entry.TTL())

	entry.UpdateExpiry(-time.Second)
	assert.False(t, entry.HasTTL())
}

func TestEntryRecordAccess(t *testing.T) {
	entry := NewEntry([]byte("k"), []byte("v"), 0)

	created := entry.LastAccess.Load()
	require.Equal(t, uint32(1), entry.AccessCnt.Load())
	require.Equal(t, entry.CreatedAt, created)

	time.Sleep(time.Millisecond)
	entry.RecordAccess()

	assert.Equal(t, uint32(2), entry.AccessCnt.Load())
	assert.Greater(t, entry.LastAccess.Load(), created)
	assert.Equal(t, created, entry.CreatedAt, "CreatedAt is fixed at construction")
}

func TestCalculateSize(t *testing.T) {
	assert.Equal(t, uint32(EntryOverhead), CalculateSize(nil, nil))
	assert.Equal(t, uint32(3+5+EntryOverhead), CalculateSize([]byte("abc"), []byte("value")))
	assert.Less(t, CalculateSize([]byte("abc"), []byte("value")), uint32(math.MaxUint32))
}
