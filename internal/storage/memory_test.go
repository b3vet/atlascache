package storage

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMemoryTrackerAccounting(t *testing.T) {
	tracker := NewMemoryTracker(1000)

	assert.Equal(t, uint64(1000), tracker.GetMaxMemory())
	assert.Equal(t, uint64(0), tracker.GetUsedMemory())

	tracker.Add(400)
	assert.Equal(t, uint64(400), tracker.GetUsedMemory())
	assert.Equal(t, uint64(600), tracker.AvailableMemory())
	assert.False(t, tracker.IsOverLimit())

	tracker.Sub(150)
	assert.Equal(t, uint64(250), tracker.GetUsedMemory())

	tracker.Sub(9999)
	assert.Equal(t, uint64(0), tracker.GetUsedMemory(), "Sub clamps at zero rather than wrapping")

	tracker.Set(1000)
	assert.True(t, tracker.IsOverLimit())
	assert.Equal(t, uint64(0), tracker.AvailableMemory())

	tracker.SetMaxMemory(2000)
	assert.False(t, tracker.IsOverLimit())
	assert.Equal(t, uint64(1000), tracker.AvailableMemory())
}

func TestMemoryTrackerUnlimited(t *testing.T) {
	tracker := NewMemoryTracker(0)

	tracker.Add(1 << 40)

	assert.False(t, tracker.IsOverLimit())
	assert.Equal(t, ^uint64(0), tracker.AvailableMemory())

	stats := tracker.Stats()
	assert.Equal(t, uint64(0), stats.Max)
	assert.Equal(t, ^uint64(0), stats.Available)
	assert.Equal(t, uint64(1<<40), stats.Used)
}

func TestMemoryTrackerCounters(t *testing.T) {
	tracker := NewMemoryTracker(100)

	tracker.RecordEviction()
	tracker.RecordEviction()
	tracker.RecordOOMRejection()

	assert.Equal(t, uint64(2), tracker.GetEvictions())
	assert.Equal(t, uint64(1), tracker.GetOOMRejections())

	stats := tracker.Stats()
	assert.Equal(t, uint64(2), stats.Evictions)
	assert.Equal(t, uint64(1), stats.OOMRejected)
	assert.Equal(t, uint64(100), stats.Max)
}

func TestMemoryTrackerConcurrentSub(t *testing.T) {
	tracker := NewMemoryTracker(0)
	tracker.Set(10_000)

	var wg sync.WaitGroup
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				tracker.Sub(10)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, uint64(0), tracker.GetUsedMemory())
}
