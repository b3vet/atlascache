package client

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"sync/atomic"
	"time"
)

// conn is one connection to one server: the socket, the buffered reader over
// it, and the one fact that decides whether it may ever be reused.
//
// A conn is used by one caller at a time. The pool owns that rule; nothing here
// locks, because two callers sharing a connection is the bug the pool exists to
// prevent, not a case to be tolerated.
type conn struct {
	nc   net.Conn
	r    *bufio.Reader
	opts *options

	// wbuf is this connection's request buffer, reused by every command it
	// sends. A command is rendered into it whole and written in one call, so a
	// partial write is a failed connection rather than a half-sent command.
	wbuf []byte

	// proto is the protocol version the handshake settled on: 3 when the server
	// accepted HELLO 3, 2 when it answered -NOPROTO and the SDK fell back.
	proto int

	// broken means this connection may never be handed to another caller.
	//
	// It is set by every failure that leaves the stream at an unknown position:
	// a protocol error, a socket error, and a call abandoned mid-flight. The
	// consequence of getting this wrong is not a slow client — it is one
	// caller reading another caller's reply, which is data disclosure between
	// callers (FEAT-0027).
	broken bool

	// usedAt is when this connection last completed a command, which is what
	// the pool's idle check measures against.
	usedAt time.Time

	// probe is the one byte scratch the liveness check reads into. It is a
	// field so the check allocates nothing on the hot path.
	probe [1]byte
}

// interruptDeadline is a time far enough in the past that setting it aborts any
// read or write already blocked on the socket. It is how a context cancellation
// reaches a goroutine parked in the kernel.
var interruptDeadline = time.Unix(1, 0)

// newConn dials, negotiates and authenticates one connection.
//
// Everything it does is bounded by the caller's context and, independently, by
// the dial timeout: a server that accepts the TCP connection and then never
// answers HELLO is indistinguishable from a slow one until a clock says
// otherwise.
func newConn(ctx context.Context, opts *options) (*conn, error) {
	dialCtx := ctx
	if opts.dialTimeout > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, opts.dialTimeout)
		defer cancel()
	}

	nc, err := dial(dialCtx, opts)
	if err != nil {
		return nil, wireError(opDial, opts.addr, err)
	}

	c := &conn{
		nc:     nc,
		r:      bufio.NewReaderSize(nc, readBufferSize),
		opts:   opts,
		wbuf:   make([]byte, 0, 256),
		proto:  2,
		usedAt: time.Now(),
	}

	if err := c.handshake(dialCtx); err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

// dial opens the socket and, when TLS is configured, completes the handshake on
// it. Both halves take the context: a TLS handshake against an unresponsive
// peer blocks exactly as long as a dial does.
func dial(ctx context.Context, opts *options) (net.Conn, error) {
	var (
		nc  net.Conn
		err error
	)
	if opts.dialer != nil {
		nc, err = opts.dialer(ctx, "tcp", opts.addr)
	} else {
		var dialer net.Dialer
		nc, err = dialer.DialContext(ctx, "tcp", opts.addr)
	}
	if err != nil {
		return nil, err
	}

	if opts.tlsConfig == nil {
		return nc, nil
	}

	config := opts.tlsConfig
	if config.ServerName == "" && !config.InsecureSkipVerify {
		if host, _, splitErr := net.SplitHostPort(opts.addr); splitErr == nil {
			config = config.Clone()
			config.ServerName = host
		}
	}

	tlsConn := tls.Client(nc, config)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = nc.Close()
		return nil, err
	}
	return tlsConn, nil
}

// handshake negotiates the protocol and authenticates.
//
// HELLO 3 is sent first and its failure is not one: a v0.1.0 server answers
// -NOPROTO and the connection carries on in RESP2 (ADR-0028). Sending it now
// rather than when P6 adds RESP3 means the SDK needs no change then, and it
// exercises the fallback path every other client library depends on.
func (c *conn) handshake(ctx context.Context) error {
	reply, err := c.exchange(ctx, opHello, bytesArgs("HELLO", "3"))
	if err != nil {
		return err
	}
	if reply.Type == TypeError {
		// -NOPROTO from a RESP2 server, or -ERR unknown command from one older
		// still. Either way the server has told us what it speaks, which is not
		// a failure — it is the negotiation working.
		c.proto = 2
	} else {
		c.proto = 3
	}

	if c.opts.token == "" {
		return nil
	}

	args := bytesArgs("AUTH", c.opts.token)
	if c.opts.username != "" {
		args = bytesArgs("AUTH", c.opts.username, c.opts.token)
	}

	reply, err = c.exchange(ctx, opAuth, args)
	if err != nil {
		return err
	}
	if reply.Type == TypeError {
		// Every failure here is an authentication failure, whatever kind the
		// server put on it. A server with no password set answers a plain -ERR
		// to AUTH, and a caller who configured a token against the wrong server
		// needs to hear "authentication", not "server error".
		return &Error{Category: ErrAuth, Op: opAuth, Addr: c.opts.addr, Kind: reply.Kind, Message: string(reply.Str)}
	}
	return nil
}

// exchange sends one command and reads its reply.
//
// Every blocking step is bounded twice: by a deadline derived from the context
// and the configured timeout, and by a watcher that unblocks the socket the
// moment the context is done. The deadline alone is not enough — a context
// canceled with no deadline set would otherwise wait out the full read
// timeout, and a context contract honored everywhere but one place is not a
// contract (FEAT-0028).
func (c *conn) exchange(ctx context.Context, op string, args [][]byte) (Reply, error) {
	if err := ctx.Err(); err != nil {
		return Reply{}, contextError(op, c.opts.addr, err)
	}

	release := c.watch(ctx)

	reply, err := c.roundTrip(ctx, args)

	if interrupted := release(); interrupted {
		// The context fired while the socket was in use. Whatever this call
		// read or wrote, the stream is no longer at a frame boundary.
		c.broken = true
		return Reply{}, contextError(op, c.opts.addr, ctx.Err())
	}
	if err != nil {
		c.broken = true
		return Reply{}, c.fail(op, err)
	}
	if reply.Type == TypePush {
		// Nothing in v0.1.0 sends one, so an unsolicited push means the stream
		// is not where this connection thinks it is. Reading past it would put
		// the next reply one frame out.
		c.broken = true
		return Reply{}, protocolError(op, c.opts.addr, "unsolicited push message")
	}

	c.usedAt = time.Now()
	return reply, nil
}

// roundTrip writes the request and reads the reply, with a deadline on each.
func (c *conn) roundTrip(ctx context.Context, args [][]byte) (Reply, error) {
	c.wbuf = appendCommand(c.wbuf[:0], args)

	if err := c.nc.SetWriteDeadline(c.deadline(ctx, c.opts.writeTimeout)); err != nil {
		return Reply{}, err
	}
	if _, err := c.nc.Write(c.wbuf); err != nil {
		return Reply{}, err
	}

	if err := c.nc.SetReadDeadline(c.deadline(ctx, c.opts.readTimeout)); err != nil {
		return Reply{}, err
	}
	reply, err := decodeReply(c.r, 0)
	if err != nil {
		return Reply{}, err
	}

	// Cleared so that a connection sitting in the pool carries no deadline from
	// the call that put it there.
	if err := c.nc.SetDeadline(time.Time{}); err != nil {
		return Reply{}, err
	}
	return reply, nil
}

// deadline combines the caller's deadline with the configured timeout, taking
// whichever expires first. A zero time means neither applies.
func (c *conn) deadline(ctx context.Context, timeout time.Duration) time.Time {
	var (
		fromTimeout time.Time
		fromContext time.Time
	)
	if timeout > 0 {
		fromTimeout = time.Now().Add(timeout)
	}
	if d, ok := ctx.Deadline(); ok {
		fromContext = d
	}

	switch {
	case fromTimeout.IsZero():
		return fromContext
	case fromContext.IsZero():
		return fromTimeout
	case fromContext.Before(fromTimeout):
		return fromContext
	default:
		return fromTimeout
	}
}

// watch unblocks the socket when the context is done, and reports on release
// whether it had to.
//
// The handshake between the watcher and the caller is deliberate. Once the
// watcher has decided to interrupt, the connection is unusable whatever the
// call did next, so release waits for the watcher to finish before answering —
// a connection returned to the pool with an interrupt deadline still landing on
// it would fail the next caller's command for a reason that has nothing to do
// with them.
func (c *conn) watch(ctx context.Context) func() bool {
	done := ctx.Done()
	if done == nil {
		return func() bool { return false }
	}

	var (
		finished    = make(chan struct{})
		released    = make(chan struct{})
		interrupted atomic.Bool
	)

	go func() {
		defer close(finished)
		select {
		case <-done:
			interrupted.Store(true)
			//nolint:errcheck // the connection is being abandoned either way: a
			// deadline that could not be set changes nothing about that.
			_ = c.nc.SetDeadline(interruptDeadline)
		case <-released:
		}
	}()

	return func() bool {
		close(released)
		<-finished
		return interrupted.Load()
	}
}

// How the liveness probe is bounded.
//
// probeDeadline has to be in the *future*, and that is not a detail: given a
// deadline already past, the runtime reports the timeout without looking at the
// socket at all, so every dead connection would read as a healthy one. Given a
// deadline just ahead, a socket the peer has closed answers EOF in microseconds
// and a healthy one costs the deadline.
//
// probeIdleAfter is what keeps that cost off the hot path. A connection handed
// back moments ago has not been closed by anything, so a busy pool never probes
// at all; a connection that has been sitting is exactly the one a restarted
// server or an idle-timing middlebox has dropped, and it is also the one whose
// caller is in no hurry.
const (
	probeDeadline  = 100 * time.Microsecond
	probeIdleAfter = 25 * time.Millisecond
)

// alive reports whether this connection is fit to be handed to another caller.
//
// The buffered check is the one that matters: bytes waiting to be read mean the
// last exchange left a reply behind, so the next caller would read it as their
// own. The socket probe catches the other common case — a server that restarted
// or a middlebox that timed the connection out — before a command is sent into
// a connection that is already gone.
//
// A probe that misses is safe. It costs one surfaced error, which is the budget
// a dropped connection has anyway; a probe that wrongly condemned a healthy
// connection would cost a dial on every checkout.
func (c *conn) alive() bool {
	if c.broken {
		return false
	}
	if c.r.Buffered() > 0 {
		return false
	}

	idle := time.Since(c.usedAt)
	if c.opts.maxIdleTime > 0 && idle > c.opts.maxIdleTime {
		return false
	}
	if idle < probeIdleAfter {
		return true
	}

	if err := c.nc.SetReadDeadline(time.Now().Add(probeDeadline)); err != nil {
		return false
	}
	_, err := c.nc.Read(c.probe[:])
	if resetErr := c.nc.SetReadDeadline(time.Time{}); resetErr != nil {
		return false
	}

	// A read that timed out is the healthy answer: the socket is open and had
	// nothing to say. Anything else — EOF, a reset, or a byte nobody asked
	// for — means this connection is not ours to reuse.
	return errors.Is(err, os.ErrDeadlineExceeded)
}

func (c *conn) close() {
	if c.nc != nil {
		_ = c.nc.Close()
	}
}

// fail attaches this connection's identity to an error from the codec or the
// socket. A protocol error arrives already categorized and only needs naming;
// anything else is the network, and wireError decides which kind.
func (c *conn) fail(op string, err error) error {
	var typed *Error
	if errors.As(err, &typed) {
		if typed.Op == "" {
			typed.Op = op
		}
		if typed.Addr == "" {
			typed.Addr = c.opts.addr
		}
		return typed
	}
	return wireError(op, c.opts.addr, err)
}

// The operation names that are not command names.
const (
	opDial  = "dial"
	opHello = "HELLO"
	opAuth  = "AUTH"
	opPool  = "pool"
)

// bytesArgs is the string-literal shorthand the handshake uses. Commands built
// from caller data never go through it: their arguments are already bytes, and
// routing them through strings would be the one place binary safety is lost.
func bytesArgs(parts ...string) [][]byte {
	args := make([][]byte, 0, len(parts))
	for _, part := range parts {
		args = append(args, []byte(part))
	}
	return args
}
