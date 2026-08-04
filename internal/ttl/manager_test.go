package ttl

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// TestMain fails any test that leaves a goroutine behind, which is the
// acceptance criterion for Stop.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// ---- construction and configuration ----

func TestConfigNormalize(t *testing.T) {
	def := Config{}.normalize()
	assert.Equal(t, DefaultTick, def.Tick)
	assert.Equal(t, DefaultStripes, def.Stripes)
	assert.Equal(t, DefaultMaxHintsPerTick, def.MaxHintsPerTick)

	custom := Config{Tick: -1, Stripes: 5, MaxHintsPerTick: -1}.normalize()
	assert.Equal(t, DefaultTick, custom.Tick, "a non-positive tick falls back to the default")
	assert.Equal(t, 8, custom.Stripes, "stripes round up to a power of two")
	assert.Zero(t, custom.MaxHintsPerTick, "a negative batch limit means unlimited")

	assert.Equal(t, maxStripes, Config{Stripes: 1 << 20}.normalize().Stripes, "stripes are capped")
}

func TestRoundUpPow2(t *testing.T) {
	for in, want := range map[int]int{-4: 1, 0: 1, 1: 1, 2: 2, 3: 4, 16: 16, 17: 32, 1024: 1024, 4096: 1024} {
		assert.Equal(t, want, roundUpPow2(in), "roundUpPow2(%d)", in)
	}
}

func TestNewReturnsUsableManagerWithoutAKeyspace(t *testing.T) {
	m := New(Config{Stripes: 2}, nil)
	m.Add(0, "k", time.Now().Add(time.Hour).UnixNano())

	st := m.Stats()
	assert.Equal(t, uint64(1), st.Added)
	assert.Equal(t, uint64(1), st.Pending)
	assert.False(t, st.Running)
}

func TestStripeSelectionHandlesAnyShardIndex(t *testing.T) {
	m := newManager(time.Now(), Config{Stripes: 8}, newFakeKeyspace())
	for _, shard := range []int{0, 1, 7, 8, 63, -1, -17} {
		assert.NotNil(t, m.stripeFor(shard), "shard %d", shard)
	}
	assert.Same(t, m.stripeFor(0), m.stripeFor(8), "shards fold onto stripes by mask")
	assert.Same(t, m.stripeFor(7), m.stripeFor(-1), "a negative shard still lands in range")
}

// ---- Add ----

func TestAddIgnoresEntriesWithoutATTL(t *testing.T) {
	m := newTestManager(newFakeKeyspace())
	m.Add(0, "no-ttl", 0)
	m.Add(0, "negative", -5)

	st := m.Stats()
	assert.Equal(t, uint64(2), st.Ignored)
	assert.Zero(t, st.Added)
	assert.Zero(t, st.Pending)
}

func TestAddAfterStopIsIgnored(t *testing.T) {
	m := newTestManager(newFakeKeyspace())
	require.NoError(t, m.Stop(context.Background()))

	m.Add(0, "k", time.Now().Add(time.Hour).UnixNano())
	assert.Zero(t, m.Stats().Added)
}

func TestAddRoundsUpSoAHintNeverFiresEarly(t *testing.T) {
	m := newTestManager(newFakeKeyspace())
	// Half a tick past tick 4's deadline: the hint must be scheduled for tick
	// 5, not tick 4.
	assert.Equal(t, uint64(5), m.tickFor(m.expireAtTick(4)+int64(DefaultTick/2)))
	assert.Equal(t, uint64(4), m.tickFor(m.expireAtTick(4)), "an exact multiple is not rounded up")
	assert.Zero(t, m.tickFor(m.epochNano-1), "a deadline before the epoch maps to tick zero")
}

// ---- boundaries ----

// TestFiresWithinOneTickOfItsDeadline is the L0 acceptance criterion: a TTL
// under 25.6s fires within 100ms of its deadline, and never before it.
func TestFiresWithinOneTickOfItsDeadline(t *testing.T) {
	ks := newFakeKeyspace()
	m := newTestManager(ks)

	expireAt := m.expireAtTick(5)
	ks.put(0, "k", expireAt)
	m.Add(0, "k", expireAt)

	for tick := uint64(1); tick < 5; tick++ {
		advanceTo(m, tick)
		require.Zero(t, ks.expiredCount(), "expired early, at tick %d of 5", tick)
	}
	advanceTo(m, 5)
	assert.Equal(t, []string{"k"}, ks.expiredKeys(), "expired on its own tick")
	assert.Zero(t, m.Stats().Pending, "the hint left the wheel")
}

// TestLevelBoundaryFireTicks drives every L0 and L1 transition to the tick and
// asserts the exact tick a key expires on. An off-by-one in the cascade shows
// up here as an early expiry.
func TestLevelBoundaryFireTicks(t *testing.T) {
	cases := []struct {
		name  string
		ticks uint64
		level int
	}{
		{"just under the L0 span", l0Span - 1, 0},
		{"exactly the L0 span (25.6s)", l0Span, 1},
		{"just over the L0 span (25.7s)", l0Span + 1, 1},
		{"well inside L1", l0Span + 44, 1},
		{"just under the L1 span", l1Span - 1, 1},
		{"exactly the L1 span", l1Span, 2},
		{"just over the L1 span", l1Span + 1, 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ks := newFakeKeyspace()
			m := newTestManager(ks)

			expireAt := m.expireAtTick(tc.ticks)
			ks.put(0, "k", expireAt)
			m.Add(0, "k", expireAt)
			require.Equal(t, tc.level, m.levelOf(), "insertion level")

			advanceTo(m, tc.ticks-1)
			require.Zero(t, ks.expiredCount(),
				"expired early: TTL of %d ticks went at or before tick %d", tc.ticks, tc.ticks-1)

			advanceTo(m, tc.ticks)
			require.Equal(t, 1, ks.expiredCount(),
				"TTL of %d ticks had not expired by its own tick", tc.ticks)

			t.Logf("TTL %8d ticks (%12v) inserted in %s, fired on tick %d",
				tc.ticks, DefaultTick*time.Duration(tc.ticks), levelName(tc.level), m.current.Load())
		})
	}
}

// TestCascadeL2ToL0FiresOnTime starts the wheel just below an L2 cascade point
// so the ~29h boundary can be driven to the tick in milliseconds.
func TestCascadeL2ToL0FiresOnTime(t *testing.T) {
	ks := newFakeKeyspace()
	m := newTestManager(ks)
	m.jumpTo(l2Span - l1Span)

	due := l2Span + 5
	expireAt := m.expireAtTick(due)
	ks.put(0, "k", expireAt)
	m.Add(0, "k", expireAt)
	require.Equal(t, 2, m.levelOf(), "starts in L2")

	advanceTo(m, l2Span-1)
	require.Zero(t, ks.expiredCount(), "expired before the cascade")
	require.Equal(t, 2, m.levelOf(), "still in L2 one tick before the cascade")

	advanceTo(m, l2Span)
	require.Equal(t, 0, m.levelOf(), "cascaded L2 -> L0 with 5 ticks left")
	require.Zero(t, ks.expiredCount(), "cascading must not expire anything")

	advanceTo(m, due-1)
	require.Zero(t, ks.expiredCount(), "expired one tick early")
	advanceTo(m, due)
	assert.Equal(t, 1, ks.expiredCount())
	t.Logf("L2 hint cascaded at tick %d and fired on tick %d", l2Span, due)
}

// TestCascadeL3ToL0FiresOnTime does the same for the ~38d boundary.
func TestCascadeL3ToL0FiresOnTime(t *testing.T) {
	ks := newFakeKeyspace()
	m := newTestManager(ks)
	m.jumpTo(l3Span - l2Span)

	due := l3Span + 5
	expireAt := m.expireAtTick(due)
	ks.put(0, "k", expireAt)
	m.Add(0, "k", expireAt)
	require.Equal(t, 3, m.levelOf(), "starts in L3")

	advanceTo(m, l3Span-1)
	require.Equal(t, 3, m.levelOf(), "still in L3 one tick before the cascade")
	require.Zero(t, ks.expiredCount())

	advanceTo(m, l3Span)
	require.Equal(t, 0, m.levelOf(), "cascaded L3 -> L0")

	advanceTo(m, due-1)
	require.Zero(t, ks.expiredCount(), "expired one tick early")
	advanceTo(m, due)
	assert.Equal(t, 1, ks.expiredCount())
	t.Logf("L3 hint cascaded at tick %d and fired on tick %d", l3Span, due)
}

// TestOverflowReentersTheWheel is the beyond-38-days acceptance criterion,
// driven through the real cascade chain.
func TestOverflowReentersTheWheel(t *testing.T) {
	ks := newFakeKeyspace()
	m := newTestManager(ks)
	m.jumpTo(l3Span - 300)

	due := m.current.Load() + l3Span + 10
	expireAt := m.expireAtTick(due)
	ks.put(0, "k", expireAt)
	m.Add(0, "k", expireAt)
	require.Equal(t, levelCount, m.levelOf(), "parked in overflow")
	require.Equal(t, uint64(1), m.Stats().Overflow)

	advanceTo(m, l3Span)

	st := m.Stats()
	assert.Zero(t, st.Overflow, "the overflow rescan ran when L3 wrapped")
	assert.Equal(t, 3, m.levelOf(), "re-entered the wheel at L3")
	assert.Equal(t, uint64(1), st.Pending, "nothing was lost")
	assert.Zero(t, ks.expiredCount(), "and nothing expired early")
	t.Logf("a %v TTL sat in overflow until tick %d, then re-entered at L3",
		DefaultTick*time.Duration(l3Span+10), l3Span)
}

func TestLongTTLIsHeldInOverflow(t *testing.T) {
	m := newTestManager(newFakeKeyspace())
	m.Add(0, "k", time.Now().Add(40*24*time.Hour).UnixNano())
	assert.Equal(t, uint64(1), m.Stats().Overflow, "a 40 day TTL is beyond L3's ~38 day span")
}

// ---- stale hints: the load-bearing invariant ----

// TestStaleHintForDeletedKey covers the "key gone" row of ADR-0016.
func TestStaleHintForDeletedKey(t *testing.T) {
	ks := newFakeKeyspace()
	m := newTestManager(ks)

	expireAt := m.expireAtTick(3)
	ks.put(0, "k", expireAt)
	m.Add(0, "k", expireAt)
	ks.remove(0, "k") // deleted, and nothing told the wheel

	advanceTo(m, 3)

	st := m.Stats()
	assert.Zero(t, ks.expiredCount(), "a hint for a missing key must not expire anything")
	assert.Equal(t, uint64(1), st.Fired)
	assert.Equal(t, uint64(1), st.Dropped)
	assert.Zero(t, st.Expired)
	assert.Zero(t, st.Pending, "the hint was dropped, not re-scheduled")
	assert.Equal(t, 1, ks.lookupCount(), "a stale hint costs exactly one lookup")
}

// TestStaleHintForKeyOverwrittenWithoutTTL covers the "overwritten, no TTL"
// row: the key is live and must survive.
func TestStaleHintForKeyOverwrittenWithoutTTL(t *testing.T) {
	ks := newFakeKeyspace()
	m := newTestManager(ks)

	expireAt := m.expireAtTick(3)
	ks.put(0, "k", expireAt)
	m.Add(0, "k", expireAt)
	ks.put(0, "k", 0) // SET without EX: the entry is live and has no expiry

	advanceTo(m, 3)

	st := m.Stats()
	assert.Zero(t, ks.expiredCount(), "a key with no TTL must never be expired by a stale hint")
	assert.Equal(t, uint64(1), st.Dropped)
	assert.Zero(t, st.Pending)
}

// TestStaleHintForExtendedTTLReinserts covers the row that ADR-0016 calls the
// hardest bug in this design: the hint says now, the entry says later, and the
// entry wins.
func TestStaleHintForExtendedTTLReinserts(t *testing.T) {
	ks := newFakeKeyspace()
	m := newTestManager(ks)

	first := m.expireAtTick(3)
	ks.put(0, "k", first)
	m.Add(0, "k", first)

	extended := m.expireAtTick(400) // EXPIRE pushed it past the L0 span
	ks.put(0, "k", extended)

	advanceTo(m, 3)
	st := m.Stats()
	require.Zero(t, ks.expiredCount(), "an extended TTL must not expire on the old hint")
	require.Equal(t, uint64(1), st.Reinserted)
	require.Equal(t, uint64(1), st.Pending, "a fresh hint was scheduled")
	require.Equal(t, 1, m.levelOf(), "re-inserted at L1, matching the new deadline")

	advanceTo(m, 399)
	require.Zero(t, ks.expiredCount(), "the re-inserted hint fired early")

	advanceTo(m, 400)
	assert.Equal(t, []string{"k"}, ks.expiredKeys(), "and expired on the new deadline")
	assert.Zero(t, m.Stats().Pending)
}

// TestReinsertionAlwaysAdvances guards against the same-tick loop: a hint whose
// entry is due at this very instant must be re-scheduled forward, not
// re-examined forever.
func TestReinsertionAlwaysAdvances(t *testing.T) {
	ks := newFakeKeyspace()
	m := newTestManager(ks)

	expireAt := m.expireAtTick(3)
	ks.put(0, "k", expireAt)
	m.Add(0, "k", expireAt)

	// Drive with the clock pinned exactly on the deadline: IsExpired uses a
	// strict comparison, so the entry is not yet expired.
	m.catchUp(m.deadline(1))
	m.catchUp(m.deadline(2))
	m.catchUp(m.deadline(3))

	require.Zero(t, ks.expiredCount(), "not expired while now equals the deadline exactly")
	require.Equal(t, uint64(1), m.Stats().Reinserted)
	require.Equal(t, uint64(4), m.stripes[0].levels[0].slots[4][0].expireTick,
		"re-scheduled at least one tick forward")

	advanceTo(m, 4)
	assert.Equal(t, 1, ks.expiredCount())
}

// TestExpireRefusalIsCountedAsADrop covers the last race: FEAT-0012 re-checks
// under its write lock and finds a live value.
func TestExpireRefusalIsCountedAsADrop(t *testing.T) {
	ks := newFakeKeyspace()
	m := newTestManager(ks)

	expireAt := m.expireAtTick(2)
	ks.put(0, "k", expireAt)
	ks.refuseExpiry(0, "k")
	m.Add(0, "k", expireAt)

	advanceTo(m, 2)

	st := m.Stats()
	assert.Zero(t, st.Expired)
	assert.Equal(t, uint64(1), st.Dropped)
	assert.Zero(t, ks.expiredCount())
}

func TestNilKeyspaceDropsEveryHint(t *testing.T) {
	m := newManager(time.Now(), Config{Tick: DefaultTick, Stripes: 1}, nil)
	m.Add(0, "k", m.expireAtTick(2))

	advanceTo(m, 2)

	st := m.Stats()
	assert.Equal(t, uint64(1), st.Fired)
	assert.Equal(t, uint64(1), st.Dropped)
	assert.Zero(t, st.Expired)
	assert.False(t, nopKeyspace{}.Expire(0, "k"), "the placeholder never deletes anything")
}

// ---- catch-up ----

// TestCatchUpAdvancesThroughEverySkippedTick proves a missed tick does not
// strand keys. Jumping the clock instead of walking it would leave the key in
// its slot until the wheel wrapped, 25.6s later.
func TestCatchUpAdvancesThroughEverySkippedTick(t *testing.T) {
	ks := newFakeKeyspace()
	m := newTestManager(ks)

	for i := uint64(1); i <= 20; i++ {
		expireAt := m.expireAtTick(i)
		ks.put(0, key(i), expireAt)
		m.Add(0, key(i), expireAt)
	}

	// One catch-up pass covering twenty ticks, as if the process had been
	// descheduled for two seconds.
	processed := m.catchUp(m.deadline(20).Add(time.Microsecond))

	st := m.Stats()
	assert.Equal(t, 20, processed)
	assert.Equal(t, uint64(20), st.Ticks)
	assert.Equal(t, uint64(20), st.CurrentTick)
	assert.Equal(t, uint64(19), st.LateTicks, "nineteen of the twenty ran behind their deadline")
	assert.Equal(t, 20, ks.expiredCount(), "every key in the skipped window was reclaimed")
	assert.Zero(t, st.Pending)
	assert.Positive(t, st.MaxLag)
}

func TestCatchUpDoesNothingBeforeTheFirstDeadline(t *testing.T) {
	m := newTestManager(newFakeKeyspace())
	assert.Zero(t, m.catchUp(m.epoch))
	assert.Zero(t, m.catchUp(m.deadline(1).Add(-time.Nanosecond)))
	assert.Zero(t, m.Stats().Ticks)
}

// TestCatchUpStopsWhenAsked keeps a long catch-up from outliving a Stop.
func TestCatchUpStopsWhenAsked(t *testing.T) {
	m := newTestManager(newFakeKeyspace())
	close(m.quit)

	processed := m.catchUp(m.deadline(5_000))
	assert.Equal(t, 1, processed, "a closed quit channel breaks out after the first tick")
}

// ---- batch limit ----

func TestBatchLimitDefersHintsToTheNextTick(t *testing.T) {
	ks := newFakeKeyspace()
	m := newManager(time.Now(), Config{Tick: DefaultTick, Stripes: 1, MaxHintsPerTick: 3}, ks)

	expireAt := m.expireAtTick(1)
	for i := uint64(0); i < 10; i++ {
		ks.put(0, key(i), expireAt)
		m.Add(0, key(i), expireAt)
	}

	advanceTo(m, 1)
	st := m.Stats()
	assert.Equal(t, uint64(3), st.Fired, "the tick validated only its batch")
	assert.Equal(t, uint64(7), st.Deferred)
	assert.Equal(t, uint64(7), st.Pending, "the rest waits for the next tick")

	advanceTo(m, 4)
	assert.Equal(t, 10, ks.expiredCount(), "the backlog drained over the following ticks")
	assert.Zero(t, m.Stats().Pending)
}

func TestUnlimitedBatchProcessesEverythingInOneTick(t *testing.T) {
	ks := newFakeKeyspace()
	m := newManager(time.Now(), Config{Tick: DefaultTick, Stripes: 1, MaxHintsPerTick: -1}, ks)

	expireAt := m.expireAtTick(1)
	for i := uint64(0); i < 50; i++ {
		ks.put(0, key(i), expireAt)
		m.Add(0, key(i), expireAt)
	}

	advanceTo(m, 1)
	assert.Equal(t, 50, ks.expiredCount())
	assert.Zero(t, m.Stats().Deferred)
}

// ---- lifecycle ----

func TestStartStopLifecycle(t *testing.T) {
	m := newManager(time.Now(), Config{Tick: time.Millisecond, Stripes: 2}, newFakeKeyspace())

	require.NoError(t, m.Start())
	assert.True(t, m.Stats().Running)
	assert.ErrorIs(t, m.Start(), ErrAlreadyRunning)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, m.Stop(ctx))
	assert.False(t, m.Stats().Running)

	assert.NoError(t, m.Stop(ctx), "Stop is idempotent")
	assert.ErrorIs(t, m.Start(), ErrStopped, "a stopped manager is not restartable")
}

func TestStopWithoutStartIsANoop(t *testing.T) {
	m := newTestManager(newFakeKeyspace())
	require.NoError(t, m.Stop(context.Background()))
	assert.ErrorIs(t, m.Start(), ErrStopped)
}

// TestStopLeavesNoGoroutineBehind counts goroutines directly, on top of the
// package-wide goleak check.
func TestStopLeavesNoGoroutineBehind(t *testing.T) {
	runtime.GC()
	before := runtime.NumGoroutine()

	for i := 0; i < 10; i++ {
		m := newManager(time.Now(), Config{Tick: time.Millisecond, Stripes: 4}, newFakeKeyspace())
		require.NoError(t, m.Start())
		time.Sleep(3 * time.Millisecond)

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		require.NoError(t, m.Stop(ctx), "Stop did not drain within its deadline")
		cancel()
	}

	runtime.Gosched()
	assert.LessOrEqual(t, runtime.NumGoroutine(), before, "goroutines leaked across ten start/stop cycles")
}

// TestStopHonoursItsContextDeadline holds the tick loop inside hint validation
// and checks that Stop gives up rather than blocking forever.
func TestStopHonoursItsContextDeadline(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var once, enterOnce sync.Once

	ks := newFakeKeyspace()
	ks.beforeLookup = func() {
		enterOnce.Do(func() { close(entered) })
		<-release
	}

	past := time.Now().Add(-time.Second).UnixNano()
	m := newManager(time.Now(), Config{Tick: time.Millisecond, Stripes: 1}, ks)
	ks.put(0, "k", past)
	m.Add(0, "k", past)

	require.NoError(t, m.Start())
	defer func() {
		once.Do(func() { close(release) })
		<-m.done
	}()

	// Wait until the tick loop is actually inside hint validation, so Stop has
	// something to block on.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("tick loop never reached hint validation")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	assert.ErrorIs(t, m.Stop(ctx), context.DeadlineExceeded, "Stop must return on its deadline")

	once.Do(func() { close(release) })
	select {
	case <-m.done:
	case <-time.After(2 * time.Second):
		t.Fatal("tick loop did not exit once unblocked")
	}
}

// ---- cadence ----

// TestTickLoopHoldsCadenceUnderLoad is the no-drift criterion: after running
// under CPU contention, the wheel's logical clock still matches wall time.
func TestTickLoopHoldsCadenceUnderLoad(t *testing.T) {
	const (
		tick = 5 * time.Millisecond
		run  = 400 * time.Millisecond
	)

	m := newManager(time.Now(), Config{Tick: tick, Stripes: 4}, newFakeKeyspace())
	require.NoError(t, m.Start())

	// Artificial load: enough spinners to contend for every core, so the tick
	// loop has to be rescheduled repeatedly during the run.
	var stopped atomic.Bool
	var wg sync.WaitGroup
	for i := 0; i < 2*runtime.NumCPU(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			spun := uint64(0)
			for !stopped.Load() {
				spun++
			}
			_ = spun
		}()
	}

	time.Sleep(run)
	stopped.Store(true)
	wg.Wait()

	lower := uint64(time.Since(m.epoch) / tick)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, m.Stop(ctx))
	upper := uint64(time.Since(m.epoch)/tick) + 1

	st := m.Stats()
	t.Logf("elapsed %v at %v per tick: %d ticks processed, expected %d..%d (late %d, max lag %v)",
		run, tick, st.Ticks, lower, upper, st.LateTicks, st.MaxLag)

	assert.GreaterOrEqual(t, st.Ticks, lower,
		"the loop fell behind wall time: %d ticks short", lower-st.Ticks)
	assert.LessOrEqual(t, st.Ticks, upper, "the loop ran ahead of wall time")
	assert.Equal(t, st.Ticks, st.CurrentTick, "every processed tick advanced the wheel")
}

// TestConcurrentAddDuringTicking is a race-detector exercise over the Add/tick
// boundary.
func TestConcurrentAddDuringTicking(t *testing.T) {
	ks := newFakeKeyspace()
	m := newManager(time.Now(), Config{Tick: time.Millisecond, Stripes: 8}, ks)
	require.NoError(t, m.Start())

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(shard int) {
			defer wg.Done()
			for i := uint64(0); i < 200; i++ {
				expireAt := time.Now().Add(time.Duration(i%5) * time.Millisecond).UnixNano()
				ks.put(shard, key(i), expireAt)
				m.Add(shard, key(i), expireAt)
			}
		}(w)
	}
	wg.Wait()
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, m.Stop(ctx))

	st := m.Stats()
	assert.Equal(t, uint64(1600), st.Added)
	assert.Positive(t, st.Expired)
	t.Logf("added %d, fired %d, expired %d, dropped %d, pending %d",
		st.Added, st.Fired, st.Expired, st.Dropped, st.Pending)
}

// key builds a stable test key.
func key(i uint64) string {
	return "key-" + string(rune('a'+i%26)) + "-" + itoa(i)
}

func itoa(i uint64) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
