package storage

import (
	"fmt"
	"math/rand"
	"strconv"
	"testing"
	"time"
)

// The engine benchmark suite (FEAT-0018).
//
// The numbers these produce are only worth having if the workload resembles the
// one the engine will actually see, so three things are parameterized rather
// than fixed:
//
// Key distribution. Sequential keys give a benchmark unrealistically good cache
// locality and a perfectly even spread across shards. Real cache traffic is
// skewed, so reads are measured under a Zipfian draw as well as a uniform one,
// and the gap between the two is the interesting part.
//
// Value size. 64B, 1KB and 64KB. Set copies the value into memory the entry
// owns (ADR-0013), so the cost of that copy scales with the value — a single
// size would hide exactly the tradeoff the ADR accepted.
//
// What is being measured. A Set below max_memory measures the write path; a Set
// at a tight max_memory measures eviction. Reporting one number for both would
// average two unrelated things, so they are separate benchmarks (the eviction
// half is in policy_bench_test.go, where the real policies live).
//
// Every benchmark reports allocations. The per-key overhead target is under 100
// bytes and allocations per operation is the figure most likely to regress
// without anyone noticing.

// benchValueSizes are the three value sizes every value-sensitive benchmark
// runs at.
var benchValueSizes = []int{64, 1024, 64 << 10}

// benchDataBudget bounds the bytes a populated benchmark keyspace holds, so a
// 64KB value size does not need a hundred times the memory a 64B one does.
const benchDataBudget = 64 << 20

// benchKeyspace is how many keys a benchmark populates at a given value size:
// enough to be more than a handful, few enough to stay inside the budget.
func benchKeyspace(valueSize int) int {
	keys := benchDataBudget / (valueSize + EntryOverhead)
	switch {
	case keys < 1_000:
		return 1_000
	case keys > 100_000:
		return 100_000
	default:
		return keys
	}
}

// benchKeys builds a fixed pool of keys, so a benchmark measures the engine
// rather than strconv.
func benchKeys(n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = "bench:key:" + strconv.Itoa(i)
	}
	return keys
}

// benchDraw is a precomputed sequence of indexes into a keyspace. Drawing is
// done up front so the distribution costs nothing inside the timed loop.
const benchDrawLen = 1 << 16

// The generators are seeded rather than random: a benchmark that draws a
// different sequence on every run is not comparable with the one before it.

// uniformDraw is the flat baseline: every key equally likely.
func uniformDraw(keyspace int) []int {
	rng := rand.New(rand.NewSource(1))
	draw := make([]int, benchDrawLen)
	for i := range draw {
		draw[i] = rng.Intn(keyspace)
	}
	return draw
}

// zipfianDraw is what cache traffic actually looks like: a small set of hot keys
// and a long tail. s = 1.1 is a mild skew, near what production caches report.
func zipfianDraw(keyspace int) []int {
	rng := rand.New(rand.NewSource(1))
	zipf := rand.NewZipf(rng, 1.1, 1, uint64(keyspace-1))
	draw := make([]int, benchDrawLen)
	for i := range draw {
		draw[i] = int(zipf.Uint64())
	}
	return draw
}

// benchDistributions is the pair every read-side benchmark runs under.
var benchDistributions = []struct {
	name string
	draw func(keyspace int) []int
}{
	{"uniform", uniformDraw},
	{"zipfian", zipfianDraw},
}

// populate fills an engine with keyspace keys of the given value size.
func populate(b *testing.B, engine *ShardedEngine, keys []string, valueSize int) {
	b.Helper()

	value := make([]byte, valueSize)
	for _, key := range keys {
		if err := engine.Set([]byte(key), value, 0); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGet measures the read path at each value size under both
// distributions. Get returns the engine's own slice rather than a copy
// (ADR-0013), so a regression here shows up as allocations appearing where
// there were none.
func BenchmarkGet(b *testing.B) {
	for _, size := range benchValueSizes {
		keyspace := benchKeyspace(size)
		keys := benchKeys(keyspace)

		engine := NewShardedEngine(DefaultEngineConfig())
		populate(b, engine, keys, size)

		for _, dist := range benchDistributions {
			draw := dist.draw(keyspace)

			b.Run(fmt.Sprintf("%s/value-%s", dist.name, byteLabel(size)), func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, _, ok := engine.Get([]byte(keys[draw[i&(benchDrawLen-1)]])); !ok {
						b.Fatal("missing key")
					}
				}
			})
		}

		_ = engine.Close()
	}
}

// BenchmarkGetMiss is the other half of the read path: a lookup that finds
// nothing still pays the hash and the shard lock.
func BenchmarkGetMiss(b *testing.B) {
	engine := NewShardedEngine(DefaultEngineConfig())
	defer engine.Close()

	populate(b, engine, benchKeys(10_000), 64)
	missing := []byte("bench:absent:0000000")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, ok := engine.Get(missing); ok {
			b.Fatal("unexpected hit")
		}
	}
}

// BenchmarkSet measures the write path with max_memory unset, so nothing here
// is eviction: this is the copy, the reservation and the map store.
//
// Writes rotate over a fixed keyspace rather than inserting forever, which keeps
// the benchmark's memory bounded and matches how a cache is written to. The
// first pass inserts and the rest overwrite.
func BenchmarkSet(b *testing.B) {
	for _, size := range benchValueSizes {
		b.Run("value-"+byteLabel(size), func(b *testing.B) {
			engine := NewShardedEngine(DefaultEngineConfig())
			defer engine.Close()

			keys := benchKeys(benchKeyspace(size))
			value := make([]byte, size)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := engine.Set([]byte(keys[i%len(keys)]), value, 0); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSetWithTTL is Set plus the expiry bookkeeping: the deadline on the
// entry and the keysWithTTL transition, both of which happen under the write
// lock.
func BenchmarkSetWithTTL(b *testing.B) {
	engine := NewShardedEngine(DefaultEngineConfig())
	defer engine.Close()

	keys := benchKeys(benchKeyspace(64))
	value := make([]byte, 64)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := engine.Set([]byte(keys[i%len(keys)]), value, time.Hour); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSetNX measures both outcomes separately. A refused SetNX still
// validates and builds the entry — that happens before the shard is consulted —
// but it reserves no memory and can therefore never trigger an eviction. The
// gap between these two numbers is the store and the reservation; the
// allocations they share are the entry neither of them avoids.
func BenchmarkSetNX(b *testing.B) {
	value := make([]byte, 64)

	b.Run("absent", func(b *testing.B) {
		engine := NewShardedEngine(DefaultEngineConfig())
		defer engine.Close()

		keys := benchKeys(benchKeyspace(64))

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			// Emptying the keyspace once per pass keeps every iteration an
			// insert rather than one insert and b.N-1 refusals. It happens off
			// the clock so the number is SetNX alone.
			if i%len(keys) == 0 {
				b.StopTimer()
				for _, key := range keys {
					engine.Delete([]byte(key))
				}
				b.StartTimer()
			}
			stored, err := engine.SetNX([]byte(keys[i%len(keys)]), value, 0)
			if err != nil {
				b.Fatal(err)
			}
			if !stored {
				b.Fatal("an absent key must accept SetNX")
			}
		}
	})

	b.Run("present", func(b *testing.B) {
		engine := NewShardedEngine(DefaultEngineConfig())
		defer engine.Close()

		keys := benchKeys(benchKeyspace(64))
		populate(b, engine, keys, 64)

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			stored, err := engine.SetNX([]byte(keys[i%len(keys)]), value, 0)
			if err != nil {
				b.Fatal(err)
			}
			if stored {
				b.Fatal("a live key must refuse SetNX")
			}
		}
	})
}

// BenchmarkDelete measures removal, which settles both memory counters under the
// write lock.
//
// The present case refills the keyspace once per pass rather than once per
// iteration: stopping the timer around every single Set costs more than the
// Delete being measured and would dominate the run.
func BenchmarkDelete(b *testing.B) {
	b.Run("present", func(b *testing.B) {
		engine := NewShardedEngine(DefaultEngineConfig())
		defer engine.Close()

		keys := benchKeys(benchKeyspace(64))

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if i%len(keys) == 0 {
				b.StopTimer()
				populate(b, engine, keys, 64)
				b.StartTimer()
			}
			if !engine.Delete([]byte(keys[i%len(keys)])) {
				b.Fatal("delete found nothing")
			}
		}
	})

	b.Run("absent", func(b *testing.B) {
		engine := NewShardedEngine(DefaultEngineConfig())
		defer engine.Close()

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			engine.Delete([]byte("bench:absent"))
		}
	})
}

// BenchmarkMixed is the workload shape a cache actually serves: mostly reads,
// skewed, with a minority of writes. It is the closest thing in the suite to an
// end-to-end engine number.
func BenchmarkMixed(b *testing.B) {
	ratios := []struct {
		name  string
		reads int // out of 10
	}{
		{"90r10w", 9},
		{"70r30w", 7},
		{"50r50w", 5},
	}

	keyspace := benchKeyspace(1024)
	keys := benchKeys(keyspace)
	draw := zipfianDraw(keyspace)
	value := make([]byte, 1024)

	for _, ratio := range ratios {
		b.Run(ratio.name, func(b *testing.B) {
			engine := NewShardedEngine(DefaultEngineConfig())
			defer engine.Close()
			populate(b, engine, keys, 1024)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				key := []byte(keys[draw[i&(benchDrawLen-1)]])
				if i%10 < ratio.reads {
					engine.Get(key)
					continue
				}
				if err := engine.Set(key, value, 0); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// benchShardCounts are the shard counts the concurrent benchmarks sweep. One
// shard is the contention floor — every operation on one lock — and the rest
// show what sharding buys before it stops buying anything.
var benchShardCounts = []int{1, 8, 64, 256}

// BenchmarkGetParallel measures read throughput under contention across shard
// counts, drawn Zipfian so the hot keys collide the way real ones do.
func BenchmarkGetParallel(b *testing.B) {
	keyspace := benchKeyspace(64)
	keys := benchKeys(keyspace)
	draw := zipfianDraw(keyspace)

	for _, shards := range benchShardCounts {
		b.Run(fmt.Sprintf("shards-%d", shards), func(b *testing.B) {
			engine := NewShardedEngine(EngineConfig{ShardCount: shards, MaxValueSize: 1 << 20})
			defer engine.Close()
			populate(b, engine, keys, 64)

			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					engine.Get([]byte(keys[draw[i&(benchDrawLen-1)]]))
					i++
				}
			})
		})
	}
}

// BenchmarkSetParallel is the write side of the same sweep. Writes take the
// shard's write lock, so this is where the shard count earns its keep.
func BenchmarkSetParallel(b *testing.B) {
	keyspace := benchKeyspace(64)
	keys := benchKeys(keyspace)
	draw := uniformDraw(keyspace)
	value := make([]byte, 64)

	for _, shards := range benchShardCounts {
		b.Run(fmt.Sprintf("shards-%d", shards), func(b *testing.B) {
			engine := NewShardedEngine(EngineConfig{ShardCount: shards, MaxValueSize: 1 << 20})
			defer engine.Close()

			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					if err := engine.Set([]byte(keys[draw[i&(benchDrawLen-1)]]), value, 0); err != nil {
						b.Fatal(err)
					}
					i++
				}
			})
		})
	}
}

// BenchmarkMixedParallel puts readers and writers on the same shards at once,
// which is the case a RWMutex is least good at and the one a server sees.
func BenchmarkMixedParallel(b *testing.B) {
	keyspace := benchKeyspace(64)
	keys := benchKeys(keyspace)
	draw := zipfianDraw(keyspace)
	value := make([]byte, 64)

	for _, shards := range benchShardCounts {
		b.Run(fmt.Sprintf("shards-%d", shards), func(b *testing.B) {
			engine := NewShardedEngine(EngineConfig{ShardCount: shards, MaxValueSize: 1 << 20})
			defer engine.Close()
			populate(b, engine, keys, 64)

			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					key := []byte(keys[draw[i&(benchDrawLen-1)]])
					if i%10 < 9 {
						engine.Get(key)
					} else if err := engine.Set(key, value, 0); err != nil {
						b.Fatal(err)
					}
					i++
				}
			})
		})
	}
}

// BenchmarkCopyOnWrite splits Set into its two halves, so the cost ADR-0013
// accepted is a measured number rather than an assumption.
//
//	entry-build  NewEntry alone: the copy, and only the copy
//	shard-put    storing a prebuilt entry: Set with the copy already paid for
//
// Their sum is what Set costs, and entry-build is what the copy adds. Running
// both at each value size is what shows the copy scaling with the value while
// the store does not.
func BenchmarkCopyOnWrite(b *testing.B) {
	for _, size := range benchValueSizes {
		key := []byte("bench:cow:key")
		value := make([]byte, size)

		b.Run(fmt.Sprintf("entry-build/value-%s", byteLabel(size)), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				entry := NewEntry(key, value, 0)
				if entry.Size == 0 {
					b.Fatal("empty entry")
				}
			}
		})

		b.Run(fmt.Sprintf("shard-put/value-%s", byteLabel(size)), func(b *testing.B) {
			shard := NewShard(64, nil)
			entry := NewEntry(key, value, 0)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := shard.Set("bench:cow:key", entry); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkStats is the ISSUE-0012 regression guard. Stats() reads counters the
// shards maintain as they go rather than walking the keyspace, so the cost must
// be flat in the number of keys: the 1M case should land within noise of the 1K
// one. A thousandfold gap is the bug coming back.
func BenchmarkStats(b *testing.B) {
	counts := []int{1_000, 1_000_000}

	for _, count := range counts {
		b.Run(fmt.Sprintf("keys-%d", count), func(b *testing.B) {
			engine := NewShardedEngine(DefaultEngineConfig())
			defer engine.Close()

			value := make([]byte, 16)
			for i := 0; i < count; i++ {
				key := []byte("stats-key:" + strconv.Itoa(i))
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

// BenchmarkScanPage measures the two costs a SCAN page can have (FEAT-0016):
// the first page of a shard pays for the snapshot, every later page only walks
// it. Separating them is the point — averaging a 100,000-key snapshot over the
// pages that follow it would hide how expensive starting a scan is.
func BenchmarkScanPage(b *testing.B) {
	const keyspace = 100_000

	engine := NewShardedEngine(EngineConfig{ShardCount: 1, MaxValueSize: 1024, MaxScanSnapshotBytes: 1 << 30})
	defer engine.Close()
	populate(b, engine, benchKeys(keyspace), 64)

	b.Run("first-page-snapshots-the-shard", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			owner := ScanOwner(i)
			if _, _, err := engine.Scan(owner, ScanCursorStart, 100); err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
			engine.ReleaseScans(owner)
			b.StartTimer()
		}
	})

	b.Run("later-page-walks-the-snapshot", func(b *testing.B) {
		_, cursor, err := engine.Scan(1, ScanCursorStart, 1)
		if err != nil {
			b.Fatal(err)
		}

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			keys, next, pageErr := engine.Scan(1, cursor, 100)
			if pageErr != nil {
				b.Fatal(pageErr)
			}
			if next == ScanCursorStart {
				// The scan ran out; start another so the loop keeps measuring
				// paging rather than restarting.
				b.StopTimer()
				engine.ReleaseScans(1)
				_, next, err = engine.Scan(1, ScanCursorStart, 1)
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
			cursor = next
			_ = keys
		}
		engine.ReleaseScans(1)
	})
}

// byteLabel renders a size the way a benchmark name should read.
func byteLabel(size int) string {
	switch {
	case size >= 1<<20 && size%(1<<20) == 0:
		return strconv.Itoa(size>>20) + "MB"
	case size >= 1<<10 && size%(1<<10) == 0:
		return strconv.Itoa(size>>10) + "KB"
	default:
		return strconv.Itoa(size) + "B"
	}
}
