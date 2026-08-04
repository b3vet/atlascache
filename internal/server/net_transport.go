package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

const connBufferSize = 16 * 1024

// netTransport is the stdlib net implementation of Transport, one goroutine
// per connection (ADR-0007)
type netTransport struct {
	ln     net.Listener
	log    zerolog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	conns map[*netConn]struct{}
	wg    sync.WaitGroup
}

// newNetTransport binds addr immediately so bind failures surface at startup
func newNetTransport(parent context.Context, addr string, log zerolog.Logger) (*netTransport, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(parent, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}

	ctx, cancel := context.WithCancel(parent)

	return &netTransport{
		ln:     ln,
		log:    log,
		ctx:    ctx,
		cancel: cancel,
		conns:  make(map[*netConn]struct{}),
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

		c := newNetConn(raw)
		if !t.track(c) {
			_ = c.Close()
			continue
		}

		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			defer t.untrack(c)
			defer func() {
				if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
					t.log.Debug().Err(err).Str("remote_addr", c.RemoteAddr()).Msg("connection close failed")
				}
			}()
			handler.Handle(t.ctx, c)
		}()
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
	// its handler observe the canceled context and return.
	t.mu.Lock()
	for c := range t.conns {
		if err := c.unblockReads(); err != nil {
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

func (t *netTransport) track(c *netConn) bool {
	if t.ctx.Err() != nil {
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.conns[c] = struct{}{}
	return true
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

// netConn adapts net.Conn to the Conn interface
type netConn struct {
	raw       net.Conn
	r         *bufio.Reader
	w         *bufio.Writer
	closeOnce sync.Once
	closeErr  error
}

func newNetConn(raw net.Conn) *netConn {
	return &netConn{
		raw: raw,
		r:   bufio.NewReaderSize(raw, connBufferSize),
		w:   bufio.NewWriterSize(raw, connBufferSize),
	}
}

func (c *netConn) Reader() *bufio.Reader { return c.r }

func (c *netConn) Writer() io.Writer { return c.w }

func (c *netConn) Flush() error { return c.w.Flush() }

func (c *netConn) RemoteAddr() string { return c.raw.RemoteAddr().String() }

func (c *netConn) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.raw.Close()
	})
	return c.closeErr
}

// unblockReads expires the read deadline so a blocked Read returns at once
func (c *netConn) unblockReads() error {
	return c.raw.SetReadDeadline(time.Now())
}
