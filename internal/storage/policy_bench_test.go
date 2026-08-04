package storage_test

// The eviction half of the benchmark suite (FEAT-0018).
//
// It lives in the external test package because the real policies are in
// internal/eviction, which imports internal/storage — measuring the policies
// the server actually runs is worth an extra file, and a hand-rolled stand-in
// would produce numbers about the stand-in.
//
// The comparison that matters is unlimited against tight. Both run the same
// keyspace, the same value size and the same key order; the only difference is
// max_memory. The gap is what a write pays for eviction (ADR-0018), and running
// all four policies through it says how much of that gap is the policy's ranking
// rather than the machinery around it.

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/b3vet/atlascache/internal/eviction"
	"github.com/b3vet/atlascache/internal/storage"
)

const (
	// evictionBenchValueSize is one kilobyte: small enough that a tight limit
	// holds thousands of entries, large enough that the copy is not noise.
	evictionBenchValueSize = 1024

	// evictionBenchLimit is the tight max_memory. At the value size above it
	// holds roughly four thousand entries, so a keyspace many times larger
	// forces essentially every write to make room for itself.
	evictionBenchLimit = 4 << 20

	// evictionBenchKeyspace is deliberately far larger than what fits.
	evictionBenchKeyspace = 200_000
)

func evictionBenchKeys() []string {
	keys := make([]string, evictionBenchKeyspace)
	for i := range keys {
		keys[i] = "bench:evict:" + strconv.Itoa(i)
	}
	return keys
}

// BenchmarkSetMemoryPressure is the FEAT-0018 comparison: the same write path
// with max_memory unset, and then at a limit under each of the four policies.
//
// unlimited is the control. It is the write path and nothing else — no
// sampling, no victim selection, no second reservation attempt. Every other
// case is that plus whatever making room costs.
//
// none is not a no-op: it is the rejection path. Once the cache is full every
// write is refused, so the number is what an OOM costs rather than what an
// eviction costs, and the two are worth telling apart.
func BenchmarkSetMemoryPressure(b *testing.B) {
	keys := evictionBenchKeys()
	value := make([]byte, evictionBenchValueSize)

	b.Run("unlimited", func(b *testing.B) {
		engine := storage.NewShardedEngine(storage.EngineConfig{
			ShardCount:   16,
			MaxValueSize: 1 << 20,
		})
		defer engine.Close()

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := engine.Set([]byte(keys[i%len(keys)]), value, 0); err != nil {
				b.Fatal(err)
			}
		}
	})

	policies := []string{eviction.PolicyLRU, eviction.PolicyLFU, eviction.PolicyFIFO, eviction.PolicyNone}
	for _, policy := range policies {
		b.Run("at-limit/"+policy, func(b *testing.B) {
			engine := storage.NewShardedEngine(storage.EngineConfig{
				ShardCount:   16,
				MaxValueSize: 1 << 20,
				MaxMemory:    evictionBenchLimit,
			})
			defer engine.Close()

			controller, err := eviction.New(eviction.Config{Policy: policy})
			if err != nil {
				b.Fatal(err)
			}
			engine.SetEvictionController(controller)

			// Fill to the limit before timing, so the measurement is of a full
			// cache rather than of the first few thousand free insertions.
			for i := 0; i < len(keys) && engine.MemoryUsed()+evictionBenchValueSize < evictionBenchLimit; i++ {
				_ = engine.Set([]byte(keys[i]), value, 0)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// ErrOutOfMemory is the expected answer under the none policy
				// and a legitimate one under the others when a write reaches
				// its eviction cap, so it is counted rather than fatal.
				_ = engine.Set([]byte(keys[i%len(keys)]), value, 0)
			}
			b.StopTimer()

			stats := engine.Stats()
			b.ReportMetric(float64(stats.Evictions)/float64(b.N), "evictions/op")
			b.ReportMetric(float64(stats.OOMRejected)/float64(b.N), "oom/op")
		})
	}
}

// BenchmarkSetAtLimitByValueSize asks whether eviction gets more expensive as
// values grow. It should not by much: a bigger value means fewer victims are
// needed to free the bytes, but each sampled entry costs the same to rank.
func BenchmarkSetAtLimitByValueSize(b *testing.B) {
	keys := evictionBenchKeys()

	for _, size := range []int{64, 1024, 64 << 10} {
		value := make([]byte, size)

		b.Run(fmt.Sprintf("value-%dB", size), func(b *testing.B) {
			engine := storage.NewShardedEngine(storage.EngineConfig{
				ShardCount:   16,
				MaxValueSize: 1 << 20,
				MaxMemory:    evictionBenchLimit,
			})
			defer engine.Close()

			controller, err := eviction.New(eviction.Config{Policy: eviction.PolicyLRU})
			if err != nil {
				b.Fatal(err)
			}
			engine.SetEvictionController(controller)

			for i := 0; i < len(keys) && engine.MemoryUsed()+uint64(size) < evictionBenchLimit; i++ {
				_ = engine.Set([]byte(keys[i]), value, 0)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = engine.Set([]byte(keys[i%len(keys)]), value, 0)
			}
		})
	}
}
