package netbench

import (
	"bufio"
	"context"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// The measurement machinery is tested, because a percentile tracker nobody
// checked is not evidence. These run in the normal suite; the sweep itself does
// not.

func TestHistogramMatchesSortedSamples(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	samples := make([]uint64, 200_000)
	h := newHist()
	for i := range samples {
		// A long-tailed shape, which is what latency looks like and what a
		// linear histogram would get wrong at the top.
		v := uint64(math.Abs(rng.NormFloat64())*20_000) + uint64(rng.Intn(50))
		if rng.Intn(1000) == 0 {
			v *= 50
		}
		samples[i] = v
		h.record(v)
	}

	for _, q := range []float64{0.5, 0.95, 0.99, 0.999} {
		got, want := h.quantile(q), exactQuantile(samples, q)
		// 0.78% is the bucket width the layout guarantees; allow one bucket of
		// slack on top of it for the rank rounding.
		tolerance := float64(want) * 0.02
		if math.Abs(float64(got)-float64(want)) > tolerance+1 {
			t.Errorf("q%.3f: histogram said %d, sorted samples say %d (tolerance %.0f)", q, got, want, tolerance)
		}
	}

	if want := exactQuantile(samples, 1.0); h.max != want {
		t.Errorf("max: histogram said %d, sorted samples say %d", h.max, want)
	}
	if h.count != uint64(len(samples)) {
		t.Errorf("count: got %d, want %d", h.count, len(samples))
	}
}

func TestHistogramBucketsAreMonotonic(t *testing.T) {
	prev := uint64(0)
	for i := 0; i < histBuckets; i++ {
		v := histValue(i)
		if i > 0 && v <= prev {
			t.Fatalf("bucket %d has value %d, which is not above bucket %d's %d", i, v, i-1, prev)
		}
		prev = v
	}
	for _, v := range []uint64{0, 1, 127, 128, 255, 256, 1023, 1_000_000, 5_000_000_000} {
		i := histIndex(v)
		if histValue(i) < v {
			t.Errorf("value %d landed in bucket %d whose top is %d", v, i, histValue(i))
		}
		if i > 0 && histValue(i-1) >= v {
			t.Errorf("value %d should not fit in bucket %d, whose top is %d", v, i-1, histValue(i-1))
		}
	}
}

func TestHistogramMergeIsAdditive(t *testing.T) {
	a, b, both := newHist(), newHist(), newHist()
	for i := uint64(1); i <= 1000; i++ {
		a.record(i)
		both.record(i)
	}
	for i := uint64(5000); i <= 6000; i++ {
		b.record(i)
		both.record(i)
	}
	a.merge(b)
	if a.count != both.count || a.sum != both.sum || a.max != both.max || a.min != both.min {
		t.Fatalf("merged %+v, want count=%d sum=%d min=%d max=%d", a, both.count, both.sum, both.min, both.max)
	}
	if a.quantile(0.99) != both.quantile(0.99) {
		t.Errorf("merged p99 %d, want %d", a.quantile(0.99), both.quantile(0.99))
	}
}

func TestEncodeCmdIsRESP2(t *testing.T) {
	got := string(encodeStrs("SET", "k", "hello"))
	want := "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$5\r\nhello\r\n"
	if got != want {
		t.Errorf("encodeCmd produced %q, want %q", got, want)
	}
}

func TestReadReply(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wire  string
		kind  byte
		n     int64
		isErr bool
	}{
		{name: "simple string", wire: "+OK\r\n", kind: '+'},
		{name: "integer", wire: ":1\r\n", kind: ':', n: 1},
		{name: "integer zero", wire: ":0\r\n", kind: ':'},
		{name: "bulk", wire: "$5\r\nhello\r\n", kind: '$', n: 5},
		{name: "null bulk", wire: "$-1\r\n", kind: '$', n: -1},
		{name: "empty bulk", wire: "$0\r\n\r\n", kind: '$'},
		{name: "array", wire: "*2\r\n$1\r\na\r\n:7\r\n", kind: '*', n: 2},
		{name: "error", wire: "-ERR nope\r\n", kind: '-', isErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(tc.wire))
			rep, err := readReply(r)
			if tc.isErr {
				if err == nil {
					t.Fatal("an error reply must surface as a Go error, not as throughput")
				}
				return
			}
			if err != nil {
				t.Fatalf("readReply: %v", err)
			}
			if rep.kind != tc.kind || rep.n != tc.n {
				t.Errorf("got kind %q n %d, want kind %q n %d", rep.kind, rep.n, tc.kind, tc.n)
			}
			if r.Buffered() != 0 {
				t.Errorf("%d bytes left unconsumed; a pipelined run would desynchronize", r.Buffered())
			}
		})
	}
}

func TestPartitionExhaustCoversEveryRequestOnce(t *testing.T) {
	spec := loadSpec{requests: make([][]byte, 1000), exhaust: true}
	for _, workers := range []int{1, 3, 50, 1500} {
		shares := partition(spec, workers)
		total := 0
		for _, s := range shares {
			total += s.n
			if s.off+s.n > len(spec.requests) {
				t.Fatalf("workers=%d: share %+v runs past the table", workers, s)
			}
		}
		if total != len(spec.requests) {
			t.Errorf("workers=%d: shares cover %d requests, want %d", workers, total, len(spec.requests))
		}
	}
}

func TestPartitionCycleSpreadsStartOffsets(t *testing.T) {
	spec := loadSpec{requests: make([][]byte, 1000)}
	shares := partition(spec, 4)
	want := []int{0, 250, 500, 750}
	for i, s := range shares {
		if s.off != want[i] || s.n != 0 {
			t.Errorf("worker %d got %+v, want offset %d and no limit", i, s, want[i])
		}
	}
}

func TestParseCPUTime(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want float64
		ok   bool
	}{
		{in: "0:02.34", want: 2.34, ok: true},
		{in: "1:00.00", want: 60, ok: true},
		{in: "1:01:01.50", want: 3661.5, ok: true},
		{in: "", ok: false},
		{in: "nonsense", ok: false},
	} {
		got, ok := parseCPUTime(tc.in)
		if ok != tc.ok || (ok && math.Abs(got-tc.want) > 1e-6) {
			t.Errorf("parseCPUTime(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestLoadAgainstNullServer exercises the whole generator -- connect, pipeline,
// warmup, measure, merge -- in under a second, so a broken harness fails in the
// normal suite rather than ten minutes into a sweep.
func TestLoadAgainstNullServer(t *testing.T) {
	ns, err := startNullServer(bulkReply(16))
	if err != nil {
		t.Fatalf("null server: %v", err)
	}
	defer ns.Stop()

	spec := loadSpec{
		name:     "selftest",
		conns:    4,
		pipeline: 8,
		duration: 200 * time.Millisecond,
		warmup:   50 * time.Millisecond,
		requests: getTable(64),
		addr:     ns.addr,
	}
	res, err := runLoad(context.Background(), spec, ns)
	if err != nil {
		t.Fatalf("runLoad: %v", err)
	}
	if res.ops == 0 || res.lat.count != res.ops {
		t.Fatalf("ops=%d but %d latency samples; every completed op must be timed", res.ops, res.lat.count)
	}
	if res.opsPerSec() <= 0 || res.lat.quantile(0.99) == 0 {
		t.Fatalf("degenerate result: %.0f ops/s, p99 %d ns", res.opsPerSec(), res.lat.quantile(0.99))
	}
}

func TestLoadExhaustSendsEveryRequestExactlyOnce(t *testing.T) {
	ns, err := startNullServer([]byte(":1\r\n"))
	if err != nil {
		t.Fatalf("null server: %v", err)
	}
	defer ns.Stop()

	const n = 5000
	spec := loadSpec{
		name:     "selftest-exhaust",
		conns:    7,
		pipeline: 16,
		requests: delTable(n),
		exhaust:  true,
		addr:     ns.addr,
	}
	res, err := runLoad(context.Background(), spec, ns)
	if err != nil {
		t.Fatalf("runLoad: %v", err)
	}
	if res.ops != n {
		t.Errorf("sent %d requests, want %d", res.ops, n)
	}
	if res.ones != n {
		t.Errorf("counted %d `:1` replies, want %d", res.ones, n)
	}
}
