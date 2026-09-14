package client

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// pool is a fixed-size set of connections, handed out one caller at a time.
//
// The size is a hard ceiling rather than a target. A caller who arrives when
// every connection is busy waits for one, bounded by its own context, and never
// causes a new connection to be opened beyond the limit: a pool that grew under
// pressure would turn a leaked connection into memory exhaustion, while one
// that blocks turns the same leak into timeouts that name the caller holding
// them (FEAT-0027).
type pool struct {
	opts *options

	// slots is the ceiling, expressed as tokens. Holding one is the right to
	// have a connection open; it is taken before a connection is fetched or
	// dialed, and given back when one is returned or closed.
	slots chan struct{}

	mu     sync.Mutex
	idle   []*conn
	closed bool

	// live counts connections that exist — idle or checked out. It is what the
	// leak tests assert on, and it is maintained under mu so it cannot drift
	// from the slots it shadows.
	live int
}

func newPool(opts *options) *pool {
	p := &pool{
		opts:  opts,
		slots: make(chan struct{}, opts.poolSize),
		idle:  make([]*conn, 0, opts.poolSize),
	}
	for range opts.poolSize {
		p.slots <- struct{}{}
	}
	return p
}

// get returns a connection the caller owns until it puts it back.
//
// Every wait here is on the caller's context: the wait for a slot, the backoff
// between connection attempts, and the dial itself. There is no internal
// timeout a caller cannot see past.
func (p *pool) get(ctx context.Context) (*conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextError(opPool, p.opts.addr, err)
	}
	if p.isClosed() {
		return nil, closedError(opPool)
	}

	select {
	case <-p.slots:
	case <-ctx.Done():
		// Pool exhaustion surfaces as the caller's own deadline, which is the
		// point: a leaked connection shows up as the callers who waited for it,
		// not as a process that grew until it died.
		return nil, contextError(opPool, p.opts.addr, ctx.Err())
	}

	c, err := p.acquire(ctx)
	if err != nil {
		p.releaseSlot()
		return nil, err
	}
	return c, nil
}

// acquire finds a healthy idle connection or opens one. The caller already
// holds a slot.
func (p *pool) acquire(ctx context.Context) (*conn, error) {
	for {
		c := p.takeIdle()
		if c == nil {
			break
		}
		if c.alive() {
			return c, nil
		}
		// Not an error and not worth reporting: an idle connection the server
		// or a middlebox closed is the ordinary case this check exists for.
		c.close()
		p.forget()
	}

	if p.isClosed() {
		return nil, closedError(opPool)
	}

	c, err := p.dial(ctx)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	p.live++
	p.mu.Unlock()
	return c, nil
}

// dial opens one connection, retrying with jittered backoff for as long as the
// reconnect window and the caller's context both allow.
//
// This is the whole of "automatic reconnection" (FEAT-0028): there is no
// background reconnect loop, because a connection nobody is waiting for does
// not need to exist. A server restart becomes a pause inside one call rather
// than a run of failures across many.
func (p *pool) dial(ctx context.Context) (*conn, error) {
	deadline := time.Now().Add(p.opts.reconnectWindow)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	for attempt := 0; ; attempt++ {
		c, err := newConn(ctx, p.opts)
		if err == nil {
			return c, nil
		}
		// An authentication failure or a closed client will fail identically
		// however many times it is tried, and retrying a bad token against a
		// server that logs failures is worse than useless.
		if !Retryable(err) {
			return nil, err
		}

		delay := backoffDelay(p.opts.backoffBase, p.opts.backoffMax, attempt)
		if time.Now().Add(delay).After(deadline) {
			// Out of window: the caller hears the last failure, with its own
			// category intact, rather than a synthetic "gave up".
			return nil, err
		}

		if waitErr := sleep(ctx, delay); waitErr != nil {
			return nil, contextError(opDial, p.opts.addr, waitErr)
		}
	}
}

// put returns a connection to the pool, or closes it.
//
// A connection that has seen any failure is closed rather than returned. That
// is the rule the pool exists to enforce: the cost of being wrong is one caller
// reading another caller's reply, which no amount of connection reuse pays for.
func (p *pool) put(c *conn) {
	if c == nil {
		return
	}

	p.mu.Lock()
	if p.closed || c.broken {
		p.live--
		p.mu.Unlock()
		c.close()
		p.releaseSlot()
		return
	}
	p.idle = append(p.idle, c)
	p.mu.Unlock()
	p.releaseSlot()
}

// close releases every connection the pool holds and refuses to hand out more.
// Connections checked out when it runs are closed as they come back.
func (p *pool) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	idle := p.idle
	p.idle = nil
	p.live -= len(idle)
	p.mu.Unlock()

	for _, c := range idle {
		c.close()
	}
}

func (p *pool) takeIdle() *conn {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.idle) == 0 {
		return nil
	}
	// Most recently returned first, so that a pool sized for a peak keeps the
	// connections it is actually using warm and lets the rest age out.
	last := len(p.idle) - 1
	c := p.idle[last]
	p.idle[last] = nil
	p.idle = p.idle[:last]
	return c
}

// forget drops the accounting for a connection that has been closed outside
// put — an idle one that failed its health check.
func (p *pool) forget() {
	p.mu.Lock()
	p.live--
	p.mu.Unlock()
}

func (p *pool) releaseSlot() {
	select {
	case p.slots <- struct{}{}:
	default:
		// Unreachable: slots is sized to the pool and every release matches an
		// acquire. The default arm is here so that a bug in that pairing
		// deadlocks nothing.
	}
}

func (p *pool) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// poolStats is what the tests assert on, and what a future Stats() on the
// client would report.
type poolStats struct {
	Idle int
	Live int
}

func (p *pool) stats() poolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return poolStats{Idle: len(p.idle), Live: p.live}
}

// backoffDelay is full jitter over an exponentially growing window: the delay
// before attempt n is drawn uniformly from [0, min(base·2ⁿ, max)).
//
// The jitter is the part that matters. Every client that lost its connection to
// one server lost it at the same instant, so an unjittered schedule has all of
// them retrying in the same millisecond — and the server they are waiting for
// spends its first seconds back serving a synchronized stampede instead of
// coming up. Spreading the attempts costs nothing and removes the failure mode.
func backoffDelay(base, maximum time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}

	window := base
	for range attempt {
		window *= 2
		if maximum > 0 && window >= maximum {
			window = maximum
			break
		}
		if window <= 0 { // overflow, for an absurd base
			window = maximum
			break
		}
	}
	if maximum > 0 && window > maximum {
		window = maximum
	}
	if window <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(window)))
}

// sleep waits for d, or until the context is done — whichever comes first.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
