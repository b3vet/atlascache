package scenarios

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// proxy is a TCP relay the SDK dials instead of the server, so that a scenario
// can do to the connection what no server would: hold a reply back, corrupt
// one, or move the server out from under it.
//
// Two of the SDK's requirements cannot be driven any other way.
//
// A restart changes the server's ports, because the harness hands out fresh
// ones per launch — while an SDK client holds the address it was built with.
// The proxy's address does not change, and it re-reads the server's on every
// connection, so "the client survives a restart" can be asserted the way a
// caller would experience it rather than by rebuilding the client, which would
// assert nothing.
//
// And a protocol error has no other source. A correct server never emits one,
// yet FEAT-0027 turns on what the SDK does when it sees one: the connection is
// at an unknown stream position, and reusing it hands the next caller somebody
// else's reply. Injecting the corruption here exercises that against the real
// server rather than against a mock of it.
type proxy struct {
	listener net.Listener
	upstream func() string

	// delay is how long a reply is held before it reaches the client. It makes
	// a connection slow on demand, which is what "the pool blocks rather than
	// growing" and "a canceled call returns promptly" both need.
	delay atomic.Int64

	// corrupt, when armed, prefixes the next reply with a byte no RESP type
	// starts with. The real reply follows it, still in the stream: a client
	// that returned this connection to its pool would hand the next caller the
	// previous caller's answer, which is the failure worth catching.
	corrupt atomic.Bool

	accepted atomic.Int64

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool

	wg sync.WaitGroup
}

// upstreamDialTimeout bounds one dial to the server behind the relay. A dial
// that cannot complete is what a restart looks like from here, and the client
// should hear about it rather than wait.
const upstreamDialTimeout = 5 * time.Second

// junkPrefix is not a valid RESP type byte, so a client reading it reports a
// protocol error rather than quietly decoding something.
var junkPrefix = []byte("\x01not a reply\r\n")

// newProxy starts a relay in front of whatever upstream returns when a
// connection arrives.
func newProxy(upstream func() string) (*proxy, error) {
	var config net.ListenConfig
	listener, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listening for the proxy: %w", err)
	}

	p := &proxy{listener: listener, upstream: upstream, conns: map[net.Conn]struct{}{}}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.serve()
	}()
	return p, nil
}

// addr is what a client should be pointed at.
func (p *proxy) addr() string { return p.listener.Addr().String() }

// connections counts how many times a client has connected through the proxy,
// which is how a pool's reuse is measured from outside it.
func (p *proxy) connections() int64 { return p.accepted.Load() }

// open counts the connections the proxy is currently holding, client and
// server side together. It is how a soak asserts that a closed client really
// released what it held: a leak here is a leak in the SDK, whatever else is
// running alongside it.
func (p *proxy) open() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}

// setDelay holds every reply for d before passing it on.
func (p *proxy) setDelay(d time.Duration) { p.delay.Store(int64(d)) }

// corruptNextReply arms one injection, consumed by the next reply that passes.
func (p *proxy) corruptNextReply() { p.corrupt.Store(true) }

func (p *proxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.accepted.Add(1)
		p.track(client)

		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.relay(client)
		}()
	}
}

// relay joins one client connection to a freshly dialed server connection.
func (p *proxy) relay(client net.Conn) {
	defer p.forget(client)

	dialer := net.Dialer{Timeout: upstreamDialTimeout}
	server, err := dialer.DialContext(context.Background(), "tcp", p.upstream())
	if err != nil {
		// The server is down — during a restart, for instance. Closing the
		// client side is exactly what the server would have done.
		return
	}
	p.track(server)
	defer p.forget(server)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.pump(server, client, false) }()
	go func() { defer wg.Done(); p.pump(client, server, true) }()
	wg.Wait()
}

// pump copies one direction. fromServer marks the direction the delay and the
// corruption apply to, since both are things a reply does.
func (p *proxy) pump(dst, src net.Conn, fromServer bool) {
	defer func() { _ = dst.Close() }()

	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if fromServer {
				if delay := time.Duration(p.delay.Load()); delay > 0 {
					time.Sleep(delay)
				}
				if p.corrupt.CompareAndSwap(true, false) {
					if _, writeErr := dst.Write(junkPrefix); writeErr != nil {
						return
					}
				}
			}
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *proxy) track(conn net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = conn.Close()
		return
	}
	p.conns[conn] = struct{}{}
}

func (p *proxy) forget(conn net.Conn) {
	p.mu.Lock()
	delete(p.conns, conn)
	p.mu.Unlock()
	_ = conn.Close()
}

// close stops the proxy and every connection through it, and waits for the
// goroutines to go: a scenario asserting that nothing leaked must not be
// leaking the rig it asserted with.
func (p *proxy) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	conns := make([]net.Conn, 0, len(p.conns))
	for conn := range p.conns {
		conns = append(conns, conn)
	}
	p.conns = map[net.Conn]struct{}{}
	p.mu.Unlock()

	_ = p.listener.Close()
	for _, conn := range conns {
		_ = conn.Close()
	}
	p.wg.Wait()
}

// blackhole accepts connections and answers nothing, which is how a scenario
// produces a read timeout without waiting for one to happen by chance.
type blackhole struct {
	listener net.Listener

	mu     sync.Mutex
	conns  []net.Conn
	closed bool
	wg     sync.WaitGroup
}

func newBlackhole() (*blackhole, error) {
	var config net.ListenConfig
	listener, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listening for the blackhole: %w", err)
	}

	b := &blackhole{listener: listener}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			b.mu.Lock()
			if b.closed {
				b.mu.Unlock()
				_ = conn.Close()
				return
			}
			b.conns = append(b.conns, conn)
			b.mu.Unlock()
		}
	}()
	return b, nil
}

func (b *blackhole) addr() string { return b.listener.Addr().String() }

func (b *blackhole) close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	conns := b.conns
	b.conns = nil
	b.mu.Unlock()

	_ = b.listener.Close()
	for _, conn := range conns {
		_ = conn.Close()
	}
	b.wg.Wait()
}

// deadPort returns an address nothing is listening on: a port is bound, its
// address read, and the listener closed. A dial against it is refused rather
// than left hanging, which is the failure ErrNetwork names.
func deadPort() (string, error) {
	var config net.ListenConfig
	listener, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("allocating a dead port: %w", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return "", fmt.Errorf("closing the dead port's listener: %w", err)
	}
	return addr, nil
}
