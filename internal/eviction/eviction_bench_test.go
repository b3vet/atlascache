package eviction

import (
	"fmt"
	"testing"
	"time"

	"github.com/b3vet/atlascache/internal/storage"
)

// benchShard fills a shard with entries spread over access times and counts, so
// ranking has something to discriminate between.
func benchShard(b *testing.B, keys int) *storage.Shard {
	b.Helper()

	shard := storage.NewShard(keys, nil)
	now := time.Now()
	for i := 0; i < keys; i++ {
		key := fmt.Sprintf("bench:key:%06d", i)
		entry := storage.NewEntry([]byte(key), make([]byte, 64), 0)
		entry.CreatedAt = now.Add(-time.Duration(i) * time.Millisecond).UnixNano()
		entry.LastAccess.Store(now.Add(-time.Duration(i%600) * time.Second).UnixNano())
		entry.AccessCnt.Store(uint32(i%1000) + 1)
		if err := shard.Set(key, entry); err != nil {
			b.Fatal(err)
		}
	}

	return shard
}

// BenchmarkSelectVictims is the cost eviction adds to a write at the memory
// limit, per policy. It is expected to be flat in the number of keys: the draw
// is sample_size entries whatever the shard holds.
func BenchmarkSelectVictims(b *testing.B) {
	policies := []string{PolicyLRU, PolicyLFU, PolicyFIFO, PolicyNone}
	counts := []int{1_000, 100_000}

	for _, count := range counts {
		shard := benchShard(b, count)
		for _, name := range policies {
			b.Run(fmt.Sprintf("%s/keys-%d", name, count), func(b *testing.B) {
				controller, err := New(Config{Policy: name})
				if err != nil {
					b.Fatal(err)
				}

				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					controller.SelectVictims(shard, 1024)
				}
			})
		}
	}
}
