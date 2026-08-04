package storage

import (
	"fmt"
	"testing"
	"time"
)

func BenchmarkSet(b *testing.B) {
	sizes := []int{16, 256, 4096}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("value-%dB", size), func(b *testing.B) {
			engine := NewShardedEngine(DefaultEngineConfig())
			defer engine.Close()

			key := []byte("bench-key")
			value := make([]byte, size)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := engine.Set(key, value, 0); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSetParallel(b *testing.B) {
	engine := NewShardedEngine(DefaultEngineConfig())
	defer engine.Close()

	value := make([]byte, 256)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		key := []byte("bench-parallel-key-000000")
		i := 0
		for pb.Next() {
			key[len(key)-1] = byte(i)
			if err := engine.Set(key, value, 0); err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
}

func BenchmarkGet(b *testing.B) {
	engine := NewShardedEngine(DefaultEngineConfig())
	defer engine.Close()

	key := []byte("bench-key")
	if err := engine.Set(key, make([]byte, 256), 0); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, ok := engine.Get(key); !ok {
			b.Fatal("missing key")
		}
	}
}

func BenchmarkStats(b *testing.B) {
	counts := []int{1_000, 1_000_000}

	for _, count := range counts {
		b.Run(fmt.Sprintf("keys-%d", count), func(b *testing.B) {
			engine := NewShardedEngine(DefaultEngineConfig())
			defer engine.Close()

			value := make([]byte, 16)
			for i := 0; i < count; i++ {
				key := []byte(fmt.Sprintf("stats-key:%d", i))
				// Half the keys carry a TTL so KeysWithTTL has real work to do.
				var ttl time.Duration
				if i%2 == 0 {
					ttl = time.Hour
				}
				if err := engine.Set(key, value, ttl); err != nil {
					b.Fatal(err)
				}
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = engine.Stats()
			}
		})
	}
}
