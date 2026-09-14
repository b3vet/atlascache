package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

const connBufferSize = 16 * 1024

// maxClientsReply is what a connection past max_connections is told before it
// is closed.
//
// It is Redis's wording, because clients match on it: a pool that grows past
// the server's limit needs to know it hit a limit rather than a network fault,
// and the alternative — refusing to accept — reaches the client as a bare
// connection-refused with nothing in it to act on (ADR-0021).
const maxClientsReply = "-ERR max number of clients reached\r\n"

// refuseTimeout bounds how long the server will spend telling a client it is
// over the limit. The message is 35 bytes and fits in any socket that has just
// been accepted; the deadline is there so a client that refuses to read one
// cannot hold a goroutine, which is the resource being defended.
const refuseTimeout = time.Second

// drainWriteTimeout bounds a flush on a connection whose idle timeout is
// disabled. Without it a client that stops reading holds its goroutine and its
// buffers forever — the same exposure client_idle_timeout closes, reached
// through the write side instead of the read side.
const drainWriteTimeout = 30 * time.Second

// errRequestTooLarge is what a connection's read side reports once one request
// has drawn its whole byte budget from the socket. It surfaces through the
// decoder as a read failure, which is what it is.
var errRequestTooLarge = errors.New("request exceeds the per-request byte budget")

// netTransport is the stdlib net implementation of Transport, one goroutine
// per connection (ADR-0007)
type netTransport struct {
	ln     net.Listener
	log    zerolog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	limits   ConnLimits
	counters *connCounters

	mu    sync.Mutex
	conns map[*netConn]struct{}
	wg    sync.WaitGroup
}

// newNetTransport binds addr immediately so bind failures surface at startup.
//
// A non-nil tlsConfig wraps the listener, and that is the whole of TLS as far as
// the rest of the server is concerned: the Conn a handler receives is the same
// type either way, and nothing above this file can tell the difference
// (ADR-0007, FEAT-0023).
func newNetTransport(
	parent context.Context,
	addr string,
	log zerolog.Logger,
	tlsConfig *tls.Config,
	limits ConnLimits,
	counters *connCounters,
) (*netTransport, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(parent, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	if tlsConfig != nil {
		ln = tls.NewListener(ln, tlsConfig)
	}

	ctx, cancel := context.WithCancel(parent)

	if counters == nil {
		// Never nil in practice — New passes the server's own. The guard is
		// here because the panic handler below touches it, and a nil
		// dereference inside a recover is a process the recover was there to
		// save.
		counters = &connCounters{}
	}

	return &netTransport{
		ln:       ln,
		log:      log,
		ctx:      ctx,
		cancel:   cancel,
		limits:   limits.normalize(),
		counters: counters,
		conns:    make(map[*netConn]struct{}),
	}, nil
}

// Addr returns the bound address, which is resolved when the port is 0
func (t *netTransport) Addr() string {
	return t.ln.Addr().String()
}

// Serve accepts connections until Shutdown closes the listener
func (t *netTransport) Serve(handler ConnHandler) error {
	for {
		raw, err := t.ln.Accept()
		if err != nil {
			if t.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}

		c := newNetConn(raw, t.limits)
		switch t.admit(c) {
		case admitted:
			t.serve(handler, c)
		case overLimit:
			t.refuse(c)
		case shuttingDown:
			_ = c.Close()
		}
	}
}

// serve runs one connection on its own goroutine.
func (t *netTransport) serve(handler ConnHandler, c *netConn) {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer t.untrack(c)
		defer t.close(c)

		// Panic isolation. A handler that panics takes its connection down and
		// nothing else: without this, the first input FEAT-0025's fuzzing finds
		// a panic on ends the run by killing the server, and in production one
		// malformed request would take every other client with it.
		defer func() {
			if recovered := recover(); recovered != nil {
				t.counters.panics.Add(1)
				t.log.Error().
					Str("remote_addr", c.RemoteAddr()).
					Interface("panic", recovered).
					Str("stack", string(debug.Stack())).
					Msg("connection handler panicked; closing the connection")
			}
		}()

		// The handshake runs here rather than in the accept loop: it talks
		// to the peer, and a peer that stalls mid-handshake would otherwise
		// stall every other client waiting to be accepted.
		if err := c.handshake(t.ctx); err != nil {
			t.log.Debug().Err(err).Str("remote_addr", c.RemoteAddr()).Msg("TLS handshake failed")
			return
		}

		// The idle clock starts when the connection starts being served, so a
		// client that opens a socket and says nothing is reaped on the same
		// budget as one that stops mid-conversation (ISSUE-0018).
		c.armRead()

		handler.Handle(t.ctx, c)
	}()
}

// refuse tells a connection over the limit why it is being closed.
//
// It happens on its own goroutine because it writes to the peer, and the accept
// loop must not be held up by a client that is slow to read — the loop is what
// every other client is waiting in.
func (t *netTransport) refuse(c *netConn) {
	t.counters.rejected.Add(1)
	t.log.Warn().
		Str("remote_addr", c.RemoteAddr()).
		Int("max_connections", t.limits.MaxConnections).
		Msg("refused a connection: at max_connections")

	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer t.close(c)

		if err := c.raw.SetWriteDeadline(time.Now().Add(refuseTimeout)); err != nil {
			return
		}
		if _, err := c.raw.Write([]byte(maxClientsReply)); err != nil {
			t.log.Debug().Err(err).Str("remote_addr", c.RemoteAddr()).Msg("could not report the connection limit")
		}
	}()
}

func (t *netTransport) close(c *netConn) {
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.log.Debug().Err(err).Str("remote_addr", c.RemoteAddr()).Msg("connection close failed")
	}
}

// Shutdown stops accepting, unblocks idle readers, and waits for in-flight
// commands to finish. Connections still busy when ctx expires are closed.
func (t *netTransport) Shutdown(ctx context.Context) error {
	t.cancel()

	closeErr := t.ln.Close()
	if errors.Is(closeErr, net.ErrClosed) {
		closeErr = nil
	}

	// An idle connection is blocked in Read; expiring the read deadline lets
	// its handler observe the canceled context and return. A connection part
	// way through a command is not reading, so it is unaffected and finishes
	// what it was doing — which is the difference between draining and
	// hanging up.
	t.mu.Lock()
	for c := range t.conns {
		if err := c.drain(); err != nil {
			t.log.Debug().Err(err).Str("remote_addr", c.RemoteAddr()).Msg("failed to unblock reader")
		}
	}
	t.mu.Unlock()

	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return closeErr
	case <-ctx.Done():
		t.closeAll()
		return ctx.Err()
	}
}

// admission is what the accept loop decided about a connection.
type admission int

const (
	admitted admission = iota
	overLimit
	shuttingDown
)

// admit registers a connection if the server has room for it.
//
// The live connection map is the count: a separate counter would be a second
// source of truth for the same fact, and the two would drift the first time a
// connection was closed on a path that forgot to decrement.
func (t *netTransport) admit(c *netConn) admission {
	if t.ctx.Err() != nil {
		return shuttingDown
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.conns) >= t.limits.MaxConnections {
		return overLimit
	}
	t.conns[c] = struct{}{}
	return admitted
}

func (t *netTransport) untrack(c *netConn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.conns, c)
}

func (t *netTransport) closeAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for c := range t.conns {
		_ = c.Close()
	}
}

// netConn adapts net.Conn to the Conn interface, and owns the two deadlines and
// the byte budget that bound what one client costs.
type netConn struct {
	raw    net.Conn
	budget *budgetReader
	r      *bufio.Reader
	w      *bufio.Writer

	idleTimeout time.Duration

	// draining is set when the transport is shutting the connection down. It
	// stops Touch from pushing the read deadline back out again, which would
	// otherwise let a connection that keeps completing commands outlive the
	// drain it was told to stop for.
	draining atomic.Bool

	closeOnce sync.Once
	closeErr  error
}

func newNetConn(raw net.Conn, limits ConnLimits) *netConn {
	budget := &budgetReader{src: raw, limit: limits.MaxRequestBytes}
	return &netConn{
		raw:         raw,
		budget:      budget,
		r:           bufio.NewReaderSize(budget, connBufferSize),
		w:           bufio.NewWriterSize(raw, connBufferSize),
		idleTimeout: limits.IdleTimeout,
	}
}

func (c *netConn) Reader() *bufio.Reader { return c.r }

// Writer returns the connection itself, so that every write carries a deadline.
func (c *netConn) Writer() io.Writer { return c }

// Write puts reply bytes into the buffer under a write deadline.
//
// The deadline is set here rather than only in Flush because a large reply
// makes the buffer flush itself mid-write, and a client that has stopped
// reading would otherwise block the connection's goroutine there indefinitely.
func (c *netConn) Write(p []byte) (int, error) {
	if err := c.armWrite(); err != nil {
		return 0, err
	}
	return c.w.Write(p)
}

func (c *netConn) Flush() error {
	if err := c.armWrite(); err != nil {
		return err
	}
	return c.w.Flush()
}

func (c *netConn) RemoteAddr() string { return c.raw.RemoteAddr().String() }

// Touch records that a complete command was served on this connection.
//
// It restarts the idle clock and returns the request budget, which is why it is
// one call and not two: both measure the same thing, the boundary between one
// request and the next. Crucially it is not called per byte — a client that
// dribbles bytes without ever completing a request never reaches here, and is
// reaped by the idle timeout it was trying to defeat.
func (c *netConn) Touch() {
	c.budget.reset()
	c.armRead()
}

func (c *netConn) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.raw.Close()
	})
	return c.closeErr
}

// armRead puts the idle deadline on the read side.
//
// A failure to set it means the connection is already closed, which the next
// read reports properly; returning it from here would make every caller handle
// a close twice.
func (c *netConn) armRead() {
	if c.idleTimeout <= 0 || c.draining.Load() {
		return
	}
	if err := c.raw.SetReadDeadline(time.Now().Add(c.idleTimeout)); err != nil {
		return
	}
}

// armWrite puts a deadline on the write side, so that a client which stops
// reading cannot pin a goroutine and its buffers.
func (c *netConn) armWrite() error {
	timeout := c.idleTimeout
	if timeout <= 0 {
		timeout = drainWriteTimeout
	}
	return c.raw.SetWriteDeadline(time.Now().Add(timeout))
}

// drain expires the read deadline so a blocked Read returns at once, and stops
// the idle clock from being rearmed.
func (c *netConn) drain() error {
	c.draining.Store(true)
	return c.raw.SetReadDeadline(time.Now())
}

// handshake completes the TLS handshake under a deadline, and does nothing at
// all on a plaintext connection.
//
// Doing it here rather than letting the first Read trigger it buys two things: a
// peer that opens a connection and never speaks is dropped instead of holding a
// goroutine forever, and a plaintext client that has dialed a TLS port fails
// immediately with a handshake error rather than hanging until something times
// out. The failure is the transport's; no handler is ever started for it, so
// none of this is visible above this file.
func (c *netConn) handshake(ctx context.Context) error {
	conn, ok := c.raw.(*tls.Conn)
	if !ok {
		return nil
	}

	if err := c.raw.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return err
	}
	if err := conn.HandshakeContext(ctx); err != nil {
		return err
	}
	// Back to no deadline; the idle clock is armed separately once the
	// connection starts being served.
	return c.raw.SetDeadline(time.Time{})
}

// budgetReader bounds the bytes one request may draw from the socket.
//
// It sits under the read buffer rather than over it so the limit applies while
// the request is arriving, not once it has arrived — which is the whole lesson
// of ISSUE-0016, applied to the quantity ISSUE-0018 found: a request that is
// within every field limit and still costs the server twenty times what it
// costs the client to send.
//
// The budget is charged against bytes read from the socket, which over-counts
// slightly when one read carries several pipelined requests. That is why the
// budget has a floor well above the read buffer: the over-count is at most one
// buffer, and a request is never refused for arriving in company.
//
// No locking: one connection is read by one goroutine (ADR-0021).
type budgetReader struct {
	src   io.Reader
	limit int
	spent int
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.limit <= 0 {
		return b.src.Read(p)
	}
	remaining := b.limit - b.spent
	if remaining <= 0 {
		return 0, errRequestTooLarge
	}
	if len(p) > remaining {
		p = p[:remaining]
	}

	n, err := b.src.Read(p)
	b.spent += n
	return n, err
}

// reset returns the budget, which the connection does once per completed
// command.
func (b *budgetReader) reset() { b.spent = 0 }
