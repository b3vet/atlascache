package client

import (
	"bufio"
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// The test this file exists for.
//
// A connection that has hit a protocol error is at an unknown position in its
// stream: the bytes the decoder could not read are still there, and the next
// caller's first read would take them as its own reply. That is one caller
// receiving another caller's data, which is a disclosure bug and not a
// performance one — so the connection is discarded, never returned.
//
// The pool is sized to one so that reuse is the only thing that could happen if
// the discard were missing: there is no second connection to hide behind.
func TestPoolDiscardsAConnectionAfterAProtocolError(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	s.set("b", []byte("b-value"))

	// The poisoned reply is a frame the decoder cannot read, followed by a
	// perfectly valid one. The second is the reply a careless pool would hand
	// to whoever came next.
	s.setHook(func(w *bufio.Writer, _ net.Conn, name string, _ [][]byte) bool {
		if name != "POISON" {
			return false
		}
		writeRaw(w, "@not-a-resp-type\r\n+STALE-REPLY\r\n")
		return true
	})

	c := newTestClient(t, s, WithPoolSize(1))
	ctx := t.Context()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	opened := s.connectionsOpened()

	_, err := c.Do(ctx, "POISON")
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("a malformed reply gave %v, want ErrProtocol", err)
	}

	// The next caller must get its own reply. If the poisoned connection had
	// been reused, this reads "STALE-REPLY" — the previous caller's data.
	value, found, err := c.Get(ctx, "b")
	if err != nil {
		t.Fatalf("the call after a protocol error failed: %v", err)
	}
	if !found || string(value) != "b-value" {
		t.Fatalf("GET b gave %q, want its own reply and not the previous caller's", value)
	}

	if s.connectionsOpened() <= opened {
		t.Fatalf("the poisoned connection was reused: the server has still seen only %d connections", s.connectionsOpened())
	}
}

// The quieter half of the same bug. Here the reply decodes cleanly and the
// connection looks healthy — but the server sent two replies for one command,
// so a second reply is sitting in the buffer with the next caller's name on it.
func TestPoolDiscardsADesynchronizedConnection(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	s.set("b", []byte("b-value"))

	s.setHook(func(w *bufio.Writer, _ net.Conn, name string, _ [][]byte) bool {
		if name != "DOUBLE" {
			return false
		}
		writeRaw(w, "+OK\r\n+LEFTOVER\r\n")
		return true
	})

	c := newTestClient(t, s, WithPoolSize(1))
	ctx := t.Context()

	reply, err := c.Do(ctx, "DOUBLE")
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	text, err := reply.Text()
	if err != nil || text != statusOK {
		t.Fatalf("the first reply was (%q, %v), want OK", text, err)
	}

	value, found, err := c.Get(ctx, "b")
	if err != nil {
		t.Fatalf("the call after a doubled reply failed: %v", err)
	}
	if !found || string(value) != "b-value" {
		t.Fatalf("GET b gave %q, want its own reply and not the leftover", value)
	}
}

func TestPoolReusesHealthyConnections(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s, WithPoolSize(4))

	for range 20 {
		if err := c.Ping(t.Context()); err != nil {
			t.Fatalf("Ping: %v", err)
		}
	}

	if opened := s.connectionsOpened(); opened != 1 {
		t.Fatalf("20 sequential calls opened %d connections, want 1", opened)
	}
}

// Exhaustion is a wait, not a new connection. A pool that grew under pressure
// would turn a leaked connection into memory exhaustion; one that blocks turns
// the same leak into a timeout naming the caller that waited.
func TestPoolExhaustionBlocksUntilTheContextExpires(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)

	release := make(chan struct{})
	var once sync.Once
	closeRelease := func() { once.Do(func() { close(release) }) }
	t.Cleanup(closeRelease)

	s.setHook(func(w *bufio.Writer, _ net.Conn, name string, _ [][]byte) bool {
		if name != "SLOW" {
			return false
		}
		<-release
		writeRaw(w, "+OK\r\n")
		return true
	})

	c := newTestClient(t, s, WithPoolSize(1))

	held := make(chan error, 1)
	go func() {
		_, err := c.Do(context.Background(), "SLOW")
		held <- err
	}()

	// Wait until the slow call really is holding the only connection.
	waitFor(t, 2*time.Second, "the only connection to be checked out", func() bool {
		return s.liveConnections() == 1
	})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := c.Ping(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("a call against an exhausted pool gave %v, want ErrTimeout", err)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("the call gave up after %v without waiting for its deadline", elapsed)
	}
	if elapsed > time.Second {
		t.Fatalf("the call took %v to honor a 150ms deadline", elapsed)
	}
	if opened := s.connectionsOpened(); opened != 1 {
		t.Fatalf("the pool grew past its size: %d connections opened", opened)
	}

	closeRelease()
	if err := <-held; err != nil {
		t.Fatalf("the call holding the connection failed: %v", err)
	}
}

// A canceled wait for the pool is cancellation, not a timeout, and it returns
// at once rather than at the deadline.
func TestPoolWaitHonoursCancellation(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)

	release := make(chan struct{})
	var once sync.Once
	closeRelease := func() { once.Do(func() { close(release) }) }
	t.Cleanup(closeRelease)

	s.setHook(func(w *bufio.Writer, _ net.Conn, name string, _ [][]byte) bool {
		if name != "SLOW" {
			return false
		}
		<-release
		writeRaw(w, "+OK\r\n")
		return true
	})

	c := newTestClient(t, s, WithPoolSize(1))

	held := make(chan error, 1)
	go func() {
		_, err := c.Do(context.Background(), "SLOW")
		held <- err
	}()
	waitFor(t, 2*time.Second, "the only connection to be checked out", func() bool {
		return s.liveConnections() == 1
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := c.Ping(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled pool wait gave %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the canceled call took %v to return", elapsed)
	}

	closeRelease()
	<-held
}

// The size is a ceiling on connections that exist, not a target — and it holds
// under concurrency, which is the only condition under which it could fail.
func TestPoolNeverExceedsItsSize(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)

	const size = 3
	c := newTestClient(t, s, WithPoolSize(size))
	ctx := t.Context()

	var wg sync.WaitGroup
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				if err := c.Ping(ctx); err != nil {
					t.Errorf("Ping: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if opened := s.connectionsOpened(); opened > size {
		t.Fatalf("the pool opened %d connections for a size of %d", opened, size)
	}
}

func TestPoolDiscardsConnectionsThatHaveSatTooLong(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s, WithPoolSize(1), WithMaxIdleTime(20*time.Millisecond))
	ctx := t.Context()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	if opened := s.connectionsOpened(); opened != 2 {
		t.Fatalf("an idle connection was reused past its limit: %d connections opened", opened)
	}
}

func TestPoolRefusesToHandOutConnectionsOnceClosed(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)

	opts := defaultOptions()
	opts.addr = s.address()
	p := newPool(opts)

	conn, err := p.get(t.Context())
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	p.close()
	p.close() // idempotent

	if _, err := p.get(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("get on a closed pool gave %v, want ErrClosed", err)
	}

	// A connection checked out when the pool closed is closed when it comes
	// back, not stored for a client that no longer exists.
	p.put(conn)
	if stats := p.stats(); stats.Idle != 0 || stats.Live != 0 {
		t.Fatalf("a closed pool is holding %+v", stats)
	}
	waitFor(t, 2*time.Second, "the server to see the connection close", func() bool {
		return s.liveConnections() == 0
	})
}

// Accounting has to survive the paths that do not go through put: a connection
// that failed its health check, and one that never dialed at all.
func TestPoolAccountingSurvivesFailures(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)

	opts := defaultOptions()
	opts.addr = s.address()
	opts.poolSize = 2
	opts.reconnectWindow = 0
	p := newPool(opts)
	t.Cleanup(p.close)

	conn, err := p.get(t.Context())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	p.put(conn)
	if stats := p.stats(); stats.Idle != 1 || stats.Live != 1 {
		t.Fatalf("after one round trip the pool holds %+v", stats)
	}

	// Kill the connection behind the pool's back, the way a server restart
	// does. The health check has to notice and the accounting has to follow.
	s.stop()
	waitFor(t, 2*time.Second, "the idle connection to be dropped", func() bool {
		conn, err := p.get(t.Context())
		if err != nil {
			return true
		}
		p.put(conn)
		return false
	})

	if _, err := p.get(t.Context()); !errors.Is(err, ErrNetwork) {
		t.Fatalf("dialing a stopped server gave %v, want ErrNetwork", err)
	}
	if stats := p.stats(); stats.Live != 0 {
		t.Fatalf("a failed dial left %+v behind", stats)
	}

	// Every slot must have come back, or the pool is now smaller than it was
	// configured to be and the next caller waits forever.
	if len(p.slots) != opts.poolSize {
		t.Fatalf("the pool has %d of its %d slots", len(p.slots), opts.poolSize)
	}
}

func TestPoolGetRefusesAnAlreadyDoneContext(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)

	opts := defaultOptions()
	opts.addr = s.address()
	p := newPool(opts)
	t.Cleanup(p.close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := p.get(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("get with a canceled context gave %v", err)
	}
	if len(p.slots) != opts.poolSize {
		t.Fatalf("a refused get kept a slot: %d of %d", len(p.slots), opts.poolSize)
	}
}
