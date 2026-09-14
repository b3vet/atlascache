package server

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestServer starts a server over an empty keyspace.
func newTestServer(t *testing.T) (*Server, chan error) {
	t.Helper()

	return newTestServerWith(t, newFakeStore())
}

// newTestServerWith starts a server over the given keyspace, for the tests that
// need one primed or made to fail.
func newTestServerWith(t *testing.T, store Store) (*Server, chan error) {
	t.Helper()

	srv, err := New(context.Background(), "127.0.0.1:0", zerolog.Nop(), store)
	require.NoError(t, err)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()

	return srv, serveErr
}

func dial(t *testing.T, srv *Server) (net.Conn, *bufio.Reader) {
	t.Helper()

	conn, err := dialContext(t, srv.Addr())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return conn, bufio.NewReader(conn)
}

func send(t *testing.T, conn net.Conn, payload string) {
	t.Helper()

	_, err := conn.Write([]byte(payload))
	require.NoError(t, err)
}

func readReply(t *testing.T, conn net.Conn, r *bufio.Reader) string {
	t.Helper()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	line, err := r.ReadString('\n')
	require.NoError(t, err)

	return line
}

func shutdownServer(t *testing.T, srv *Server, serveErr chan error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, srv.Shutdown(ctx))
	require.NoError(t, <-serveErr)
}

func TestPing(t *testing.T) {
	srv, serveErr := newTestServer(t)
	defer shutdownServer(t, srv, serveErr)

	conn, r := dial(t, srv)

	send(t, conn, "*1\r\n$4\r\nPING\r\n")
	assert.Equal(t, "+PONG\r\n", readReply(t, conn, r))

	// Inline form, as sent by netcat or telnet
	send(t, conn, "ping\r\n")
	assert.Equal(t, "+PONG\r\n", readReply(t, conn, r))

	// PING with a message echoes it back
	send(t, conn, "*2\r\n$4\r\nPING\r\n$5\r\nhello\r\n")
	assert.Equal(t, "$5\r\n", readReply(t, conn, r))
	assert.Equal(t, "hello\r\n", readReply(t, conn, r))
}

func TestUnknownCommandKeepsConnectionUsable(t *testing.T) {
	srv, serveErr := newTestServer(t)
	defer shutdownServer(t, srv, serveErr)

	conn, r := dial(t, srv)

	send(t, conn, "*1\r\n$3\r\nFOO\r\n")
	assert.Equal(t, "-ERR unknown command 'FOO'\r\n", readReply(t, conn, r))

	send(t, conn, "*1\r\n$4\r\nPING\r\n")
	assert.Equal(t, "+PONG\r\n", readReply(t, conn, r))
}

func TestPingWrongArity(t *testing.T) {
	srv, serveErr := newTestServer(t)
	defer shutdownServer(t, srv, serveErr)

	conn, r := dial(t, srv)

	send(t, conn, "*3\r\n$4\r\nPING\r\n$1\r\na\r\n$1\r\nb\r\n")
	assert.Equal(t, "-ERR wrong number of arguments for 'ping' command\r\n", readReply(t, conn, r))
}

func TestQuitClosesConnection(t *testing.T) {
	srv, serveErr := newTestServer(t)
	defer shutdownServer(t, srv, serveErr)

	conn, r := dial(t, srv)

	send(t, conn, "*1\r\n$4\r\nQUIT\r\n")
	assert.Equal(t, "+OK\r\n", readReply(t, conn, r))

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, err := r.ReadString('\n')
	assert.ErrorIs(t, err, io.EOF)
}

func TestProtocolErrorClosesConnection(t *testing.T) {
	srv, serveErr := newTestServer(t)
	defer shutdownServer(t, srv, serveErr)

	conn, r := dial(t, srv)

	send(t, conn, "*abc\r\n")
	assert.Equal(t, "-ERR Protocol error: invalid multibulk length\r\n", readReply(t, conn, r))

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, err := r.ReadString('\n')
	assert.ErrorIs(t, err, io.EOF)
}

func TestShutdownClosesIdleConnections(t *testing.T) {
	srv, serveErr := newTestServer(t)

	conn, r := dial(t, srv)
	send(t, conn, "*1\r\n$4\r\nPING\r\n")
	require.Equal(t, "+PONG\r\n", readReply(t, conn, r))

	// The connection is idle in Read; shutdown must not wait for the client
	start := time.Now()
	shutdownServer(t, srv, serveErr)
	assert.Less(t, time.Since(start), 2*time.Second)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, err := r.ReadString('\n')
	assert.ErrorIs(t, err, io.EOF)
}

func TestShutdownStopsAccepting(t *testing.T) {
	srv, serveErr := newTestServer(t)
	addr := srv.Addr()
	shutdownServer(t, srv, serveErr)

	conn, err := dialContext(t, addr)
	if err == nil {
		_ = conn.Close()
		t.Fatal("expected the listener to be closed")
	}
}

func TestShutdownIsIdempotent(t *testing.T) {
	srv, serveErr := newTestServer(t)
	shutdownServer(t, srv, serveErr)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	assert.NoError(t, srv.Shutdown(ctx))
}

func TestNewFailsOnBusyPort(t *testing.T) {
	srv, serveErr := newTestServer(t)
	defer shutdownServer(t, srv, serveErr)

	_, err := New(context.Background(), srv.Addr(), zerolog.Nop(), newFakeStore())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listen on")
}

func TestTransportInterfaceSatisfied(t *testing.T) {
	transport, err := newNetTransport(
		context.Background(), "127.0.0.1:0", zerolog.Nop(), nil, DefaultConnLimits(), &connCounters{})
	require.NoError(t, err)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, transport.Shutdown(ctx))
	}()

	var iface Transport = transport
	assert.NotNil(t, iface)
}

// dialContext dials with a context so tests do not hang if the listener is gone.
func dialContext(t *testing.T, addr string) (net.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// TestUnterminatedLineIsRefusedAtTheLimit is ISSUE-0016 at the level a client
// meets it. A connection that streams bytes and never sends a newline used to
// grow the server's buffer until the process died; auth is off by default, so
// anyone who can open a socket could do it.
//
// The assertion is on what the server spent, not only on the error it sent: an
// error returned after reading 64MB is still 64MB read.
func TestUnterminatedLineIsRefusedAtTheLimit(t *testing.T) {
	const (
		flood = 32 << 20
		chunk = 64 << 10
	)

	srv, serveErr := newTestServer(t)
	defer shutdownServer(t, srv, serveErr)

	conn, r := dial(t, srv)

	// The reply arrives while the flood is still being written, so it is read
	// here rather than after: the server closes the connection behind the
	// error, and a client still writing into a closed socket may never get to
	// read what was already sent to it.
	replies := make(chan string, 1)
	go func() {
		line, err := r.ReadString('\n')
		if err != nil {
			line = "read failed: " + err.Error()
		}
		replies <- line
	}()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	payload := bytes.Repeat([]byte("x"), chunk)
	require.NoError(t, conn.SetWriteDeadline(time.Now().Add(10*time.Second)))
	sent := 0
	for sent < flood {
		n, err := conn.Write(payload)
		sent += n
		if err != nil {
			// The server closed on us, which is the expected end of this loop.
			break
		}
	}

	select {
	case reply := <-replies:
		assert.Equal(t, "-ERR Protocol error: too big inline request\r\n", reply)
	case <-time.After(5 * time.Second):
		t.Fatalf("no reply after writing %d bytes with no newline in them", sent)
	}

	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("wrote %d bytes; the process allocated %d serving them", sent, allocated)

	// The server reads at most the inline limit plus its connection buffer, so
	// what it allocates is bounded by those and not by what the client sends.
	// Before the fix this grew with the flood.
	assert.Lessf(t, allocated, uint64(4<<20),
		"serving a %d byte unterminated line allocated %d bytes", sent, allocated)

	// The connection is gone: a protocol error is not resynchronizable.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, err := r.ReadByte()
	assert.Error(t, err, "the connection must be closed behind the protocol error")
}
