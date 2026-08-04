package ttl

import "sync"

// Wheel geometry. Every level holds a power-of-two number of slots so that
// slot selection is a shift and a mask, and so that a level's span is exactly
// the slot width of the level above it.
//
//	Level  Bits  Slots  Slot width      Span
//	L0        8    256  1 tick          256 ticks      25.6s at a 100ms tick
//	L1        6     64  256 ticks       16384 ticks    ~27m
//	L2        6     64  16384 ticks     2^20 ticks     ~29h
//	L3        5     32  2^20 ticks      2^25 ticks     ~38d
const (
	l0Bits = 8
	l1Bits = 6
	l2Bits = 6
	l3Bits = 5

	levelCount = 4
)

// Level spans in ticks. A deadline delta below a level's span fits in that
// level; anything at or beyond l3Span goes to the overflow list.
const (
	l0Span = uint64(1) << l0Bits
	l1Span = uint64(1) << (l0Bits + l1Bits)
	l2Span = uint64(1) << (l0Bits + l1Bits + l2Bits)
	l3Span = uint64(1) << (l0Bits + l1Bits + l2Bits + l3Bits)
)

// levelSpans indexes the spans above by level, for the insertion scan.
var levelSpans = [levelCount]uint64{l0Span, l1Span, l2Span, l3Span}

// hint is a scheduling hint: the key it points at may or may not still exist,
// and may or may not still expire when expireTick says it does. expireTick is
// carried because the cascade has to redistribute a hint by remaining time
// without consulting the keyspace; it is never treated as truth.
type hint struct {
	key        string
	expireTick uint64
	shard      int
}

// level is one ring of slots.
type level struct {
	slots [][]hint
	shift uint
	mask  uint64
}

// wheel is a single striped instance of the four-level wheel. Its mutex covers
// both the slots and the logical clock, which is what keeps a concurrent Add
// from scheduling into a slot the tick loop has just drained.
type wheel struct {
	mu       sync.Mutex
	current  uint64
	levels   [levelCount]level
	overflow []hint
	pending  uint64
}

func newWheel() *wheel {
	w := &wheel{}
	specs := [levelCount]struct{ bits, shift uint }{
		{l0Bits, 0},
		{l1Bits, l0Bits},
		{l2Bits, l0Bits + l1Bits},
		{l3Bits, l0Bits + l1Bits + l2Bits},
	}
	for i, s := range specs {
		w.levels[i] = level{
			slots: make([][]hint, 1<<s.bits),
			shift: s.shift,
			mask:  uint64(1)<<s.bits - 1,
		}
	}
	return w
}

// add schedules a hint.
func (w *wheel) add(h hint) {
	w.mu.Lock()
	// The current tick's L0 slot has already been drained by the time an
	// external caller can reach it, so the earliest schedulable tick is the
	// next one. That floor is also what makes re-insertion of a fired hint
	// terminate: the hint always moves at least one tick into the future.
	w.insert(h, w.current+1)
	w.pending++
	w.mu.Unlock()
}

// requeue re-schedules a hint for the very next tick, used by the per-tick
// batch limit. It does not count as a new addition.
func (w *wheel) requeue(h hint) {
	w.mu.Lock()
	h.expireTick = w.current + 1
	w.insert(h, w.current+1)
	w.pending++
	w.mu.Unlock()
}

// insert places a hint in the lowest level whose span covers its remaining
// time, or in the overflow list. A deadline before earliest is pulled forward
// to it. The caller holds w.mu.
//
// earliest is w.current+1 for external additions and w.current during a
// cascade: a cascade runs before the tick's L0 slot is read, so a hint that
// falls due on exactly this tick must be allowed into that slot. Pushing it to
// the next tick instead would delay every level boundary by one tick.
func (w *wheel) insert(h hint, earliest uint64) {
	if h.expireTick < earliest {
		h.expireTick = earliest
	}
	delta := h.expireTick - w.current
	for i, span := range levelSpans {
		if delta < span {
			l := &w.levels[i]
			idx := (h.expireTick >> l.shift) & l.mask
			l.slots[idx] = append(l.slots[idx], h)
			return
		}
	}
	w.overflow = append(w.overflow, h)
}

// advance moves the wheel on by one tick and appends the hints that fired to
// dst, which it returns. Cascading from the higher levels happens before the
// L0 slot is read, so a hint that cascades into the slot about to fire is not
// missed for a full rotation.
func (w *wheel) advance(dst []hint) []hint {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.current++
	idx := w.current & w.levels[0].mask
	if idx == 0 {
		w.cascadeChain()
	}

	slot := w.levels[0].slots[idx]
	if len(slot) == 0 {
		return dst
	}
	dst = append(dst, slot...)
	w.pending -= uint64(len(slot))
	w.levels[0].slots[idx] = slot[:0]
	return dst
}

// cascadeChain redistributes higher levels downwards when L0 wraps. L1's due
// slot always cascades; L2's cascades only when L1 itself wrapped, and so on,
// which is what makes each level cascade exactly once per rotation of the
// level below it. The overflow list is rescanned when L3 wraps, once every
// l3Span ticks — exactly the interval that guarantees nothing parked there can
// fall due while it waits.
//
// The caller holds w.mu and has already advanced w.current.
func (w *wheel) cascadeChain() {
	for lv := 1; lv < levelCount; lv++ {
		if w.cascade(lv) != 0 {
			return
		}
	}
	w.drainOverflow()
}

// cascade empties level lv's due slot back through insert, which redistributes
// each hint into a lower level by its remaining time. It returns the slot
// index it drained so cascadeChain can tell whether the level wrapped.
func (w *wheel) cascade(lv int) uint64 {
	l := &w.levels[lv]
	idx := (w.current >> l.shift) & l.mask
	slot := l.slots[idx]
	if len(slot) > 0 {
		for _, h := range slot {
			w.insert(h, w.current)
		}
		l.slots[idx] = slot[:0]
	}
	return idx
}

// drainOverflow re-inserts every parked hint. Those still beyond L3's span
// land back in the overflow list; the rest enter the wheel proper.
func (w *wheel) drainOverflow() {
	if len(w.overflow) == 0 {
		return
	}
	// The slice is released rather than truncated: insert appends the
	// still-distant hints back onto w.overflow, and reusing the backing array
	// we are ranging over would overwrite entries not yet read.
	due := w.overflow
	w.overflow = nil
	for _, h := range due {
		w.insert(h, w.current)
	}
}

// counts reports how many hints the wheel holds and how many of those are
// parked in overflow.
func (w *wheel) counts() (pending, overflow uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pending, uint64(len(w.overflow))
}
