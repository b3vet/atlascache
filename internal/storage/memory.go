package storage

import (
	"runtime"
	"sync/atomic"
)

// MemoryTracker tracks memory usage across shards
type MemoryTracker struct {
	maxMemory   uint64
	usedMemory  uint64
	evictions   uint64
	oomRejected uint64
}

// NewMemoryTracker creates a new memory tracker
func NewMemoryTracker(maxMemory uint64) *MemoryTracker {
	return &MemoryTracker{
		maxMemory: maxMemory,
	}
}

// SetMaxMemory updates the maximum memory limit
func (m *MemoryTracker) SetMaxMemory(max uint64) {
	atomic.StoreUint64(&m.maxMemory, max)
}

// GetMaxMemory returns the maximum memory limit
func (m *MemoryTracker) GetMaxMemory() uint64 {
	return atomic.LoadUint64(&m.maxMemory)
}

// GetUsedMemory returns the current used memory
func (m *MemoryTracker) GetUsedMemory() uint64 {
	return atomic.LoadUint64(&m.usedMemory)
}

// Add adds bytes to the used memory counter
func (m *MemoryTracker) Add(bytes uint64) {
	atomic.AddUint64(&m.usedMemory, bytes)
}

// Sub subtracts bytes from the used memory counter
func (m *MemoryTracker) Sub(bytes uint64) {
	// Use compare-and-swap to prevent underflow
	for {
		current := atomic.LoadUint64(&m.usedMemory)
		var newVal uint64
		if bytes > current {
			newVal = 0
		} else {
			newVal = current - bytes
		}
		if atomic.CompareAndSwapUint64(&m.usedMemory, current, newVal) {
			break
		}
	}
}

// Set sets the used memory to a specific value
func (m *MemoryTracker) Set(bytes uint64) {
	atomic.StoreUint64(&m.usedMemory, bytes)
}

// IsOverLimit checks if memory usage exceeds the limit
func (m *MemoryTracker) IsOverLimit() bool {
	max := atomic.LoadUint64(&m.maxMemory)
	if max == 0 {
		return false // No limit
	}
	return atomic.LoadUint64(&m.usedMemory) >= max
}

// AvailableMemory returns how many bytes are available before hitting limit
func (m *MemoryTracker) AvailableMemory() uint64 {
	max := atomic.LoadUint64(&m.maxMemory)
	if max == 0 {
		return ^uint64(0) // Max uint64 if no limit
	}
	used := atomic.LoadUint64(&m.usedMemory)
	if used >= max {
		return 0
	}
	return max - used
}

// RecordEviction records an eviction event
func (m *MemoryTracker) RecordEviction() {
	atomic.AddUint64(&m.evictions, 1)
}

// RecordOOMRejection records an out-of-memory rejection
func (m *MemoryTracker) RecordOOMRejection() {
	atomic.AddUint64(&m.oomRejected, 1)
}

// GetEvictions returns the total eviction count
func (m *MemoryTracker) GetEvictions() uint64 {
	return atomic.LoadUint64(&m.evictions)
}

// GetOOMRejections returns the total OOM rejection count
func (m *MemoryTracker) GetOOMRejections() uint64 {
	return atomic.LoadUint64(&m.oomRejected)
}

// MemoryStats holds memory statistics
type MemoryStats struct {
	Used         uint64
	Max          uint64
	Available    uint64
	Evictions    uint64
	OOMRejected  uint64
	HeapAlloc    uint64
	HeapSys      uint64
	HeapInuse    uint64
	NumGC        uint32
}

// Stats returns memory statistics
func (m *MemoryTracker) Stats() MemoryStats {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	max := atomic.LoadUint64(&m.maxMemory)
	used := atomic.LoadUint64(&m.usedMemory)

	var available uint64
	if max > 0 && max > used {
		available = max - used
	} else if max == 0 {
		available = ^uint64(0)
	}

	return MemoryStats{
		Used:        used,
		Max:         max,
		Available:   available,
		Evictions:   atomic.LoadUint64(&m.evictions),
		OOMRejected: atomic.LoadUint64(&m.oomRejected),
		HeapAlloc:   memStats.HeapAlloc,
		HeapSys:     memStats.HeapSys,
		HeapInuse:   memStats.HeapInuse,
		NumGC:       memStats.NumGC,
	}
}
