package netbench

import (
	"math/bits"
	"sort"
)

// Latency is recorded in a log-linear histogram, not a slice of samples.
//
// A 200-connection, pipeline-100 run completes tens of millions of operations
// in five seconds; keeping every sample would cost more memory than the server
// under test and would put an allocation on the measurement path, which is the
// one path that must stay cheap. The histogram is fixed-size, allocation-free
// after construction, and one per worker so recording needs no lock.
//
// The layout is HdrHistogram's: 128 linear buckets per octave, so the bucket a
// value lands in is never more than 1/128 -- 0.78% -- wider than the value
// itself. Percentiles are therefore accurate to better than 1%, which is an
// order of magnitude below the run-to-run noise on a laptop-class machine.
// `max` is kept exactly, outside the bucketing, because the largest sample is
// the one figure a histogram would round in the direction that flatters.
const (
	histSubBits  = 7
	histSubCount = 1 << histSubBits
	// Enough octaves for values up to 2^40 ns, about 18 minutes. Anything
	// larger is clamped into the top bucket rather than lost.
	histOctaves = 40
	histBuckets = histSubCount * (histOctaves + 1)
)

type hist struct {
	counts []uint64
	count  uint64
	sum    uint64
	min    uint64
	max    uint64
}

func newHist() *hist {
	return &hist{counts: make([]uint64, histBuckets), min: ^uint64(0)}
}

// histIndex maps a value to its bucket. Values below histSubCount get their own
// bucket each; above that, each octave is split into histSubCount linear steps.
func histIndex(v uint64) int {
	if v < histSubCount {
		return int(v)
	}
	k := uint(bits.Len64(v)-1) - histSubBits
	i := histSubCount*(int(k)+1) + int(v>>k) - histSubCount
	if i >= histBuckets {
		return histBuckets - 1
	}
	return i
}

// histValue returns the highest value that lands in bucket i. Reporting the top
// of the bucket rather than its floor keeps a percentile from being quoted
// lower than a sample that actually contributed to it.
func histValue(i int) uint64 {
	if i < histSubCount {
		return uint64(i)
	}
	k := uint(i/histSubCount - 1)
	sub := uint64(i%histSubCount) + histSubCount
	return (sub << k) + (1<<k - 1)
}

func (h *hist) record(v uint64) {
	h.counts[histIndex(v)]++
	h.count++
	h.sum += v
	if v < h.min {
		h.min = v
	}
	if v > h.max {
		h.max = v
	}
}

func (h *hist) merge(o *hist) {
	for i, c := range o.counts {
		h.counts[i] += c
	}
	h.count += o.count
	h.sum += o.sum
	if o.count > 0 {
		if o.min < h.min {
			h.min = o.min
		}
		if o.max > h.max {
			h.max = o.max
		}
	}
}

// quantile returns the value at fraction q of the distribution, in the same
// units as the recorded samples. The result is never above the exact maximum.
func (h *hist) quantile(q float64) uint64 {
	if h.count == 0 {
		return 0
	}
	want := uint64(q*float64(h.count) + 0.5)
	if want == 0 {
		want = 1
	}
	if want > h.count {
		want = h.count
	}
	var seen uint64
	for i, c := range h.counts {
		if c == 0 {
			continue
		}
		seen += c
		if seen >= want {
			v := histValue(i)
			if v > h.max {
				return h.max
			}
			return v
		}
	}
	return h.max
}

func (h *hist) mean() float64 {
	if h.count == 0 {
		return 0
	}
	return float64(h.sum) / float64(h.count)
}

// exactQuantile is the reference implementation the histogram is checked
// against: sort the samples and index into them. It is only used by tests.
func exactQuantile(samples []uint64, q float64) uint64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]uint64(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := int(q*float64(len(sorted))+0.5) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}
