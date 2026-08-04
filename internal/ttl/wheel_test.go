package ttl

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// levelName labels a level index for test output; levelCount is the overflow
// list.
func levelName(level int) string {
	if level == levelCount {
		return "overflow"
	}
	return fmt.Sprintf("L%d", level)
}

// TestGeometry pins the wheel's shape to what ADR-0016 specifies, so a change
// to the constants cannot silently move the level boundaries.
func TestGeometry(t *testing.T) {
	w := newWheel()

	assert.Len(t, w.levels[0].slots, 256, "L0 slots")
	assert.Len(t, w.levels[1].slots, 64, "L1 slots")
	assert.Len(t, w.levels[2].slots, 64, "L2 slots")
	assert.Len(t, w.levels[3].slots, 32, "L3 slots")

	assert.Equal(t, uint64(256), l0Span, "L0 span in ticks")
	assert.Equal(t, uint64(16384), l1Span, "L1 span in ticks")
	assert.Equal(t, uint64(1048576), l2Span, "L2 span in ticks")
	assert.Equal(t, uint64(33554432), l3Span, "L3 span in ticks")

	// The spans above, rendered at the 100ms base resolution.
	t.Logf("L0 span %v, L1 span %v, L2 span %v, L3 span %v",
		DefaultTick*256, DefaultTick*16384, DefaultTick*1048576, DefaultTick*33554432)
}

// TestInsertLevelSelection walks every level transition: one tick under the
// boundary, the boundary itself, and one tick over.
func TestInsertLevelSelection(t *testing.T) {
	cases := []struct {
		name  string
		delta uint64
		level int
	}{
		{"one tick", 1, 0},
		{"last tick inside L0", l0Span - 1, 0},
		{"L0/L1 boundary exactly", l0Span, 1},
		{"one tick past the L0/L1 boundary", l0Span + 1, 1},
		{"last tick inside L1", l1Span - 1, 1},
		{"L1/L2 boundary exactly", l1Span, 2},
		{"one tick past the L1/L2 boundary", l1Span + 1, 2},
		{"last tick inside L2", l2Span - 1, 2},
		{"L2/L3 boundary exactly", l2Span, 3},
		{"one tick past the L2/L3 boundary", l2Span + 1, 3},
		{"last tick inside L3", l3Span - 1, 3},
		{"L3/overflow boundary exactly", l3Span, levelCount},
		{"one tick past the L3/overflow boundary", l3Span + 1, levelCount},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWheel()
			w.add(hint{key: "k", expireTick: tc.delta})

			counts := w.levelCounts()
			assert.Equal(t, 1, counts[tc.level],
				"delta %d ticks belongs in level %d, counts %v", tc.delta, tc.level, counts)

			total := 0
			for _, n := range counts {
				total += n
			}
			assert.Equal(t, 1, total, "hint must be in exactly one place")
			t.Logf("delta %10d ticks (%12s) -> %s", tc.delta, DefaultTick*time.Duration(tc.delta), levelName(tc.level))
		})
	}
}

// TestInsertPullsPastDeadlineForward proves a deadline already in the past is
// scheduled for the next tick rather than dropped into a slot that has already
// fired this rotation.
func TestInsertPullsPastDeadlineForward(t *testing.T) {
	w := newWheel()
	w.current = 5

	w.add(hint{key: "stale", expireTick: 0})

	assert.Len(t, w.levels[0].slots[6], 1, "clamped to the next tick's slot")
	assert.Equal(t, uint64(1), w.pending)

	fired := w.advance(nil)
	require.Len(t, fired, 1, "fires on the very next tick, never later")
	assert.Equal(t, "stale", fired[0].key)
	assert.Equal(t, uint64(6), w.current)
	assert.Zero(t, w.pending)
}

// TestCascadeL1IntoL0 drives the wheel honestly across the first L0 wrap and
// checks the hint arrives in L0 with the right remaining time.
func TestCascadeL1IntoL0(t *testing.T) {
	w := newWheel()
	w.add(hint{key: "k", expireTick: 300})
	require.Equal(t, 1, w.levelCounts()[1], "300 ticks starts in L1")

	for i := 0; i < 256; i++ {
		require.Empty(t, w.advance(nil), "fired early at tick %d", i+1)
	}

	assert.Equal(t, 1, w.levelCounts()[0], "cascaded into L0 at the wrap")
	assert.Len(t, w.levels[0].slots[300&255], 1, "landed in the slot for tick 300")

	for i := 257; i < 300; i++ {
		require.Empty(t, w.advance(nil), "fired early at tick %d", i)
	}
	fired := w.advance(nil)
	require.Len(t, fired, 1)
	assert.Equal(t, uint64(300), w.current, "fires on its own tick, not before or after")
}

// TestCascadeChainStopsAtTheFirstNonZeroLevel documents why L2 only cascades
// when L1 itself wrapped: each level must cascade exactly once per rotation of
// the level below it.
func TestCascadeChainStopsAtTheFirstNonZeroLevel(t *testing.T) {
	w := newWheel()

	// A hint due in the second L1 slot, which must survive the first few L0
	// wraps untouched.
	w.add(hint{key: "k", expireTick: l1Span + 500})
	require.Equal(t, 1, w.levelCounts()[2], "beyond L1's span, so it starts in L2")

	for w.current < l1Span-1 {
		require.Empty(t, w.advance(nil))
	}
	assert.Equal(t, 1, w.levelCounts()[2], "still in L2 one tick before the L1 wrap")

	require.Empty(t, w.advance(nil)) // tick l1Span: L1 wraps, so L2 cascades
	assert.Equal(t, 1, w.levelCounts()[1], "cascaded L2 -> L1")
}

// TestOverflowDrainsIntoTheWheel proves a deadline past L3's span is parked and
// then re-entered when it comes into range.
func TestOverflowDrainsIntoTheWheel(t *testing.T) {
	w := newWheel()
	w.add(hint{key: "far", expireTick: l3Span + 5})
	require.Equal(t, 1, w.levelCounts()[levelCount], "parked in overflow")

	// Sit one tick below the overflow rescan, which happens once per L3 span.
	w.current = l3Span - 1
	require.Empty(t, w.advance(nil))

	assert.Zero(t, w.levelCounts()[levelCount], "overflow drained")
	assert.Equal(t, 1, w.levelCounts()[0], "re-entered L0 with 5 ticks left")

	for i := 0; i < 4; i++ {
		require.Empty(t, w.advance(nil), "fired early")
	}
	fired := w.advance(nil)
	require.Len(t, fired, 1)
	assert.Equal(t, "far", fired[0].key)
	assert.Equal(t, l3Span+5, w.current)
}

// TestOverflowKeepsWhatIsStillOutOfRange proves the rescan is a filter, not a
// flush.
func TestOverflowKeepsWhatIsStillOutOfRange(t *testing.T) {
	w := newWheel()
	w.add(hint{key: "soon", expireTick: l3Span + 5})
	w.add(hint{key: "distant", expireTick: 3 * l3Span})
	require.Equal(t, 2, w.levelCounts()[levelCount])

	w.current = l3Span - 1
	require.Empty(t, w.advance(nil))

	assert.Equal(t, 1, w.levelCounts()[levelCount], "the distant hint stays parked")
	assert.Equal(t, 1, w.levelCounts()[0], "the near one enters the wheel")
	assert.Equal(t, uint64(2), w.pending, "nothing was lost")
}

// TestRequeueSchedulesForTheNextTick covers the path the per-tick batch limit
// uses to defer work.
func TestRequeueSchedulesForTheNextTick(t *testing.T) {
	w := newWheel()
	w.current = 100

	w.requeue(hint{key: "deferred", expireTick: 9999})

	assert.Len(t, w.levels[0].slots[101&255], 1)
	fired := w.advance(nil)
	require.Len(t, fired, 1)
	assert.Equal(t, "deferred", fired[0].key)
}

// TestCountsReportsOverflowSeparately covers the Stats plumbing.
func TestCountsReportsOverflowSeparately(t *testing.T) {
	w := newWheel()
	w.add(hint{key: "near", expireTick: 3})
	w.add(hint{key: "far", expireTick: l3Span + 1})

	pending, overflow := w.counts()
	assert.Equal(t, uint64(2), pending)
	assert.Equal(t, uint64(1), overflow)
}
