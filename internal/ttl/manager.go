package ttl

import (
	"context"
	"sync/atomic"
	"time"
)

// Manager lifecycle states.
const (
	stateNew int32 = iota
	stateRunning
	stateStopped
)

// manager is the Manager implementation. It owns a set of striped wheels that
// share one logical clock, a tick loop that advances them, and the validation
// step that turns a fired hint into either an expiry, a drop, or a
// re-insertion.
type manager struct {
	tick       time.Duration
	epoch      time.Time
	epochNano  int64
	maxPerTick int

	keyspace Keyspace
	stripes  []*wheel
	mask     int

	state atomic.Int32
	quit  chan struct{}
	done  chan struct{}

	current    atomic.Uint64
	ticks      atomic.Uint64
	added      atomic.Uint64
	ignored    atomic.Uint64
	fired      atomic.Uint64
	expired    atomic.Uint64
	dropped    atomic.Uint64
	reinserted atomic.Uint64
	deferred   atomic.Uint64
	lateTicks  atomic.Uint64
	maxLag     atomic.Int64
}

// New returns a Manager that schedules expiries against ks. A nil ks is
// replaced by one that reports every key as gone, so every hint is dropped;
// that is useful for exercising the wheel in isolation but expires nothing.
func New(cfg Config, ks Keyspace) Manager {
	return newManager(time.Now(), cfg, ks)
}

// newManager is New with an explicit epoch, so tests can pin the wheel's
// logical clock to a known instant.
func newManager(epoch time.Time, cfg Config, ks Keyspace) *manager {
	cfg = cfg.normalize()
	if ks == nil {
		ks = nopKeyspace{}
	}

	stripes := make([]*wheel, cfg.Stripes)
	for i := range stripes {
		stripes[i] = newWheel()
	}

	return &manager{
		tick:       cfg.Tick,
		epoch:      epoch,
		epochNano:  epoch.UnixNano(),
		maxPerTick: cfg.MaxHintsPerTick,
		keyspace:   ks,
		stripes:    stripes,
		mask:       cfg.Stripes - 1,
		quit:       make(chan struct{}),
		done:       make(chan struct{}),
	}
}

// stripeFor maps a shard onto its wheel. The mask keeps the result in range
// for any int, including a negative shard index.
func (m *manager) stripeFor(shard int) *wheel {
	return m.stripes[shard&m.mask]
}

// tickFor maps an absolute expiry in Unix nanoseconds onto the first tick at
// or after it. Rounding up rather than down is what keeps a hint from firing
// before its deadline.
func (m *manager) tickFor(expireAt int64) uint64 {
	delta := expireAt - m.epochNano
	if delta <= 0 {
		return 0
	}
	step := int64(m.tick)
	t := delta / step
	if delta%step != 0 {
		t++
	}
	return uint64(t) //nolint:gosec // delta and step are both positive, so t is
}

// deadline is the wall-clock instant at which the given tick falls due. It is
// computed from the epoch rather than accumulated, so the loop cannot drift.
func (m *manager) deadline(tick uint64) time.Time {
	return m.epoch.Add(time.Duration(tick) * m.tick) //nolint:gosec // tick counts 100ms steps; it cannot approach the int64 range
}

// Add registers an expiry hint. See Manager.Add.
func (m *manager) Add(shard int, key string, expireAt int64) {
	if expireAt <= 0 {
		m.ignored.Add(1)
		return
	}
	if m.state.Load() == stateStopped {
		return
	}
	m.stripeFor(shard).add(hint{
		key:        key,
		expireTick: m.tickFor(expireAt),
		shard:      shard,
	})
	m.added.Add(1)
}

// Start launches the tick loop. See Manager.Start.
func (m *manager) Start() error {
	if m.state.CompareAndSwap(stateNew, stateRunning) {
		go m.run()
		return nil
	}
	if m.state.Load() == stateRunning {
		return ErrAlreadyRunning
	}
	return ErrStopped
}

// Stop signals the loop and waits for it. See Manager.Stop.
func (m *manager) Stop(ctx context.Context) error {
	if !m.state.CompareAndSwap(stateRunning, stateStopped) {
		// Never started, or already stopped: nothing to drain.
		m.state.CompareAndSwap(stateNew, stateStopped)
		return nil
	}
	close(m.quit)

	// Prefer a completed drain over an already-expired context, so a Stop that
	// had nothing left to wait for does not report a deadline error.
	select {
	case <-m.done:
		return nil
	default:
	}

	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stats returns a snapshot. See Manager.Stats.
func (m *manager) Stats() Stats {
	var pending, overflow uint64
	for _, w := range m.stripes {
		p, o := w.counts()
		pending += p
		overflow += o
	}
	return Stats{
		Running:     m.state.Load() == stateRunning,
		Ticks:       m.ticks.Load(),
		CurrentTick: m.current.Load(),
		Added:       m.added.Load(),
		Ignored:     m.ignored.Load(),
		Fired:       m.fired.Load(),
		Expired:     m.expired.Load(),
		Dropped:     m.dropped.Load(),
		Reinserted:  m.reinserted.Load(),
		Deferred:    m.deferred.Load(),
		LateTicks:   m.lateTicks.Load(),
		MaxLag:      time.Duration(m.maxLag.Load()),
		Pending:     pending,
		Overflow:    overflow,
	}
}

// run is the tick loop. It waits on an absolute deadline for each tick rather
// than sleeping a fixed interval, so scheduling delay does not accumulate, and
// it hands every elapsed tick to catchUp rather than jumping the clock, so a
// missed tick cannot strand keys until the wheel wraps.
func (m *manager) run() {
	defer close(m.done)

	timer := time.NewTimer(time.Until(m.deadline(m.current.Load() + 1)))
	defer timer.Stop()

	for {
		select {
		case <-m.quit:
			return
		case <-timer.C:
		}

		m.catchUp(time.Now())

		select {
		case <-m.quit:
			return
		default:
		}

		timer.Reset(time.Until(m.deadline(m.current.Load() + 1)))
	}
}

// catchUp advances through every tick whose deadline has passed, one at a
// time, and reports how many it processed.
func (m *manager) catchUp(now time.Time) int {
	if m.deadline(m.current.Load() + 1).After(now) {
		return 0
	}
	if lag := now.Sub(m.deadline(m.current.Load() + 1)); lag > time.Duration(m.maxLag.Load()) {
		m.maxLag.Store(int64(lag))
	}

	nowNano := now.UnixNano()
	var buf []hint
	processed := 0

	for !m.deadline(m.current.Load() + 1).After(now) {
		buf = m.advanceOne(buf[:0], nowNano)
		processed++

		// A long catch-up must not outlive a Stop request.
		select {
		case <-m.quit:
			m.recordLate(processed)
			return processed
		default:
		}
	}

	m.recordLate(processed)
	return processed
}

// recordLate counts every tick beyond the first in a single catch-up pass as
// late: those are the ticks whose deadline had already gone by.
func (m *manager) recordLate(processed int) {
	if processed > 1 {
		m.lateTicks.Add(uint64(processed - 1))
	}
}

// advanceOne moves every stripe on by one tick and validates what fired.
func (m *manager) advanceOne(buf []hint, nowNano int64) []hint {
	m.current.Add(1)
	m.ticks.Add(1)
	for _, w := range m.stripes {
		buf = w.advance(buf)
	}
	m.process(buf, nowNano)
	return buf
}

// process is hint validation: the step that makes a stale hint harmless.
// storage.Entry.ExpireAt, read through the Keyspace seam, decides every case;
// the hint only decides which key to look at.
func (m *manager) process(fired []hint, nowNano int64) {
	limit := len(fired)
	if m.maxPerTick > 0 && limit > m.maxPerTick {
		for _, h := range fired[m.maxPerTick:] {
			m.stripeFor(h.shard).requeue(h)
		}
		m.deferred.Add(uint64(limit - m.maxPerTick)) //nolint:gosec // limit > maxPerTick
		limit = m.maxPerTick
	}

	for _, h := range fired[:limit] {
		m.fired.Add(1)
		expireAt, present := m.keyspace.ExpiryOf(h.shard, h.key)

		switch {
		case !present, expireAt == 0:
			// Key gone, or overwritten and no longer carrying a TTL.
			m.dropped.Add(1)
		case nowNano > expireAt:
			// Genuinely expired. Keyspace.Expire re-checks under its own write
			// lock, so it may still decline if the key came back to life.
			if m.keyspace.Expire(h.shard, h.key) {
				m.expired.Add(1)
			} else {
				m.dropped.Add(1)
			}
		default:
			// TTL extended, or the deadline is a hair away. Re-schedule; insert
			// clamps to at least the next tick, so this always makes progress.
			h.expireTick = m.tickFor(expireAt)
			m.stripeFor(h.shard).add(h)
			m.reinserted.Add(1)
		}
	}
}
