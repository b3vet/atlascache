package scenarios_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/b3vet/atlascache/test/e2e/runner"
	"github.com/b3vet/atlascache/test/e2e/runner/fakeharness"
)

// graceful_shutdown_closes_connections is the one scenario that needs a real
// socket: its whole subject is what happens to a connection the server was
// serving when SIGTERM arrived, and a harness that answers commands from a map
// has no connection to close. So it is driven here against a listener whose
// shutdown behavior each test chooses — including the wrong behaviors, which a
// working server cannot be asked to produce.

// respServer answers every command with one canned reply and, when shut down,
// does whatever the test asked of the connections it was holding.
type respServer struct {
	reply  string // empty means hang up instead of answering
	onStop func(net.Conn)

	ln net.Listener

	mu      sync.Mutex
	conns   []net.Conn
	stopped bool
}

// newRESPServer starts a server on loopback. onStop is called for each open
// connection when the server is shut down; nil closes them, which is what a
// correct server does.
func newRESPServer(t *testing.T, reply string, onStop func(net.Conn)) *respServer {
	t.Helper()

	var config net.ListenConfig
	ln, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	if onStop == nil {
		onStop = func(conn net.Conn) { _ = conn.Close() }
	}
	server := &respServer{reply: reply, onStop: onStop, ln: ln}
	go server.accept()

	// Whatever the test asked the shutdown to do, nothing may outlive the test.
	t.Cleanup(func() {
		server.shutdown()
		server.mu.Lock()
		defer server.mu.Unlock()
		for _, conn := range server.conns {
			_ = conn.Close()
		}
	})
	return server
}

func (s *respServer) addr() string { return s.ln.Addr().String() }

func (s *respServer) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, conn)
		s.mu.Unlock()
		go s.serve(conn)
	}
}

func (s *respServer) serve(conn net.Conn) {
	reader := bufio.NewReader(conn)
	for {
		if _, err := readArgs(reader); err != nil {
			return
		}
		if s.reply == "" {
			_ = conn.Close()
			return
		}
		if _, err := io.WriteString(conn, s.reply); err != nil {
			return
		}
	}
}

// shutdown stops listening and hands every open connection to onStop. It is
// idempotent, so a test may shut the server down and then let cleanup run.
func (s *respServer) shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.stopped = true
	_ = s.ln.Close()
	for _, conn := range s.conns {
		s.onStop(conn)
	}
}

// writes returns a shutdown behavior that sends bytes down the connection
// instead of closing it — what a server still serving, or one corrupting its
// stream on the way out, would do.
func writes(t *testing.T, payload string) func(net.Conn) {
	t.Helper()
	return func(conn net.Conn) {
		if _, err := io.WriteString(conn, payload); err != nil {
			t.Errorf("writing to the idle connection: %v", err)
		}
	}
}

// shutdownStub is a harness whose Stop really stops something. The scenario
// opens its own connection to Info().ClientAddr, so pointing that at a real
// listener is what lets the scenario observe a shutdown.
type shutdownStub struct {
	*fakeharness.Harness

	server *respServer
	// stopDelay stands in for a server that takes its time exiting.
	stopDelay time.Duration
	// keepPID leaves a pid reported after the stop, as a server that was
	// signaled but never exited would.
	keepPID bool
	// keepServing leaves the harness answering commands after the stop, as a
	// server that ignored SIGTERM would.
	keepServing bool

	mu  sync.Mutex
	pid int
}

func newShutdownStub(t *testing.T, reply string, onStop func(net.Conn)) *shutdownStub {
	t.Helper()
	return &shutdownStub{
		Harness: fakeharness.New(map[string]runner.Reply{"PING": runner.StatusReply("PONG")}),
		server:  newRESPServer(t, reply, onStop),
		pid:     4242,
	}
}

func (s *shutdownStub) Info() runner.ServerInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return runner.ServerInfo{ClientAddr: s.server.addr(), AdminAddr: s.server.addr(), PID: s.pid}
}

func (s *shutdownStub) Stop(ctx context.Context) error {
	time.Sleep(s.stopDelay)
	s.server.shutdown()

	if !s.keepPID {
		s.mu.Lock()
		s.pid = 0
		s.mu.Unlock()
	}
	if s.keepServing {
		return nil
	}
	return s.Harness.Stop(ctx)
}

// unreachableAddr is an address on loopback with nothing listening on it.
func unreachableAddr(t *testing.T) string {
	t.Helper()

	var config net.ListenConfig
	ln, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("closing the listener: %v", err)
	}
	return addr
}

// deadHarness reports an address nothing is listening on, for the case where
// the scenario cannot even open the connection it is about to hold.
type deadHarness struct {
	*fakeharness.Harness
	addr string
}

func (h *deadHarness) Info() runner.ServerInfo {
	return runner.ServerInfo{ClientAddr: h.addr, AdminAddr: h.addr}
}

func TestGracefulShutdownClosesConnections(t *testing.T) {
	t.Parallel()

	const scenario = "graceful_shutdown_closes_connections"

	t.Run("a server that closes the connection it was serving passes", func(t *testing.T) {
		t.Parallel()
		stub := newShutdownStub(t, "+PONG\r\n", nil)

		if failure := runScenario(t, scenario, stub); failure != nil {
			t.Fatalf("a correct shutdown failed the scenario: %s", failure.Message)
		}
	})

	t.Run("a server that exits without closing its connections fails", func(t *testing.T) {
		t.Parallel()
		// The whole point of the scenario: a client left holding a socket
		// nobody is reading cannot tell a shutdown from a hang, and would sit
		// there until its own timeout.
		stub := newShutdownStub(t, "+PONG\r\n", func(net.Conn) {})

		requireFailure(t, runScenario(t, scenario, stub), "without closing the connection")
	})

	t.Run("a server that answers on the idle connection fails", func(t *testing.T) {
		t.Parallel()
		// A reply on a connection nothing asked on means the server is still
		// serving, not shutting down.
		stub := newShutdownStub(t, "+PONG\r\n", writes(t, "+OK\r\n"))

		requireFailure(t, runScenario(t, scenario, stub), "received a reply during shutdown")
	})

	t.Run("a connection that dies of a protocol error is not a close", func(t *testing.T) {
		t.Parallel()
		// Garbage on the wire is a bug in the server, not a graceful close, and
		// counting it as one would let a shutdown that corrupts the stream pass.
		stub := newShutdownStub(t, "+PONG\r\n", writes(t, "@not-a-resp-type\r\n"))

		requireFailure(t, runScenario(t, scenario, stub), "protocol error rather than a close")
	})

	t.Run("a server still holding a pid after the stop fails", func(t *testing.T) {
		t.Parallel()
		stub := newShutdownStub(t, "+PONG\r\n", nil)
		stub.keepPID = true

		requireFailure(t, runScenario(t, scenario, stub), "still running after shutdown")
	})

	t.Run("a server still answering commands after the stop fails", func(t *testing.T) {
		t.Parallel()
		stub := newShutdownStub(t, "+PONG\r\n", nil)
		stub.keepServing = true

		requireFailure(t, runScenario(t, scenario, stub), "answered PING after it was shut down")
	})

	t.Run("a server that overruns the shutdown budget fails", func(t *testing.T) {
		t.Parallel()
		// The budget is the promise the product makes, and it is deliberately
		// tighter than the harness's own SIGKILL window: a server creeping
		// towards that limit has to be caught while it is still merely slow.
		stub := newShutdownStub(t, "+PONG\r\n", nil)
		stub.stopDelay = 5100 * time.Millisecond

		requireFailure(t, runScenario(t, scenario, stub), "over the 5s window")
	})

	t.Run("a connection that cannot be opened at all fails", func(t *testing.T) {
		t.Parallel()
		h := &deadHarness{
			Harness: fakeharness.New(map[string]runner.Reply{"PING": runner.StatusReply("PONG")}),
			addr:    unreachableAddr(t),
		}

		requireFailure(t, runScenario(t, scenario, h), "opening a connection to")
	})

	t.Run("a connection that is not usable before the shutdown fails", func(t *testing.T) {
		t.Parallel()
		// A server that hangs up on the first command was already broken; the
		// scenario must say so rather than blame the shutdown that follows.
		stub := newShutdownStub(t, "", nil)

		requireFailure(t, runScenario(t, scenario, stub), "not usable before shutdown")
	})

	t.Run("a server that does not answer PONG fails", func(t *testing.T) {
		t.Parallel()
		stub := newShutdownStub(t, "+BUSY loading\r\n", nil)

		requireFailure(t, runScenario(t, scenario, stub), "before shutdown, want PONG")
	})
}
