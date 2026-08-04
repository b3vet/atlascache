package ttl

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// benchKeys is a fixed pool so the benchmarks measure insertion, not string
// formatting.
var benchKeys = func() []string {
	keys := make([]string, 1024)
	for i := range keys {
		keys[i] = "bench:key:" + strconv.Itoa(i)
	}
	return keys
}()

const benchKeyMask = 1023

// benchResetEvery bounds how many hints a benchmark accumulates before the
// wheel is rebuilt. Without it a long -benchtime would measure allocator
// pressure rather than insertion.
const benchResetEvery = 1 << 20

// BenchmarkAdd measures insertion into each wheel level and into the overflow
// list. The deltas are chosen one tick past each level's lower bound so the
// insertion scan walks the same number of levels every iteration.
func BenchmarkAdd(b *testing.B) {
	levels := []struct {
		name  string
		delta uint64
	}{
		{"L0", 10},
		{"L1", l0Span + 10},
		{"L2", l1Span + 10},
		{"L3", l2Span + 10},
		{"Overflow", l3Span + 10},
	}

	for _, lv := range levels {
		b.Run(lv.name, func(b *testing.B) {
			m := newManager(time.Now(), Config{Stripes: 1}, nil)
			expireAt := m.expireAtTick(lv.delta)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if i%benchResetEvery == 0 && i > 0 {
					b.StopTimer()
					m = newManager(time.Now(), Config{Stripes: 1}, nil)
					expireAt = m.expireAtTick(lv.delta)
					b.StartTimer()
				}
				m.Add(0, benchKeys[i&benchKeyMask], expireAt)
			}
		})
	}
}

// BenchmarkAddParallel measures Add against a live wheel: the tick loop is
// running and draining hints as they fall due, so this includes both stripe
// contention and contention with the loop itself.
func BenchmarkAddParallel(b *testing.B) {
	m := newManager(time.Now(), Config{Tick: time.Millisecond, MaxHintsPerTick: -1}, nil)
	if err := m.Start(); err != nil {
		b.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.Stop(ctx); err != nil {
			b.Error(err)
		}
	}()

	expireAt := m.expireAtTick(2)
	var next atomic.Int64

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		shard := int(next.Add(1))
		i := 0
		for pb.Next() {
			m.Add(shard, benchKeys[i&benchKeyMask], expireAt)
			i++
		}
	})
}

// BenchmarkAdvance measures the cost of one tick on an empty wheel, which is
// what the tick loop pays 10 times a second regardless of load.
func BenchmarkAdvance(b *testing.B) {
	w := newWheel()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.advance(nil)
	}
}
