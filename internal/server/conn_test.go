package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// What one client may cost (FEAT-0024, ISSUE-0018).
//
// Every test here asserts on a resource rather than on a reply: connections
// held, goroutines held, bytes allocated, time spent. A server that answers
// correctly while holding a goroutine for a client that went away in 2019 is
// answering the wrong question.

// newLimitedServer starts a real server — listener and all — under the given
// limits.
func newLimitedServer(t *testing.T, store Store, limits ConnLimits) (*Server, chan error) {
	t.Helper()

	srv, err := New(context.Background(), "127.0.0.1:0", zerolog.Nop(), store, WithConnLimits(limits))
	require.NoError(t, err)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()

	return srv, serveErr
}

// TestConnectionsPastTheLimitAreToldWhy. Refusing to accept would reach the
// client as a bare connection-refused, which is indistinguishable from the
// server being down and sends an operator looking at the network (ADR-0021).
func TestConnectionsPastTheLimitAreToldWhy(t *testing.T) {
	srv, serveErr := newLimitedServer(t, newFakeStore(), ConnLimits{MaxConnections: 2})
	defer shutdownServer(t, srv, serveErr)

	// Fill the server.
	for i := range 2 {
		conn, r := dial(t, srv)
		send(t, conn, "*1\r\n$4\r\nPING\r\n")
		require.Equalf(t, "+PONG\r\n", readReply(t, conn, r), "connection %d", i)
	}

	over, err := dialContext(t, srv.Addr())
	require.NoError(t, err, "a connection over the limit must still be accepted, so it can be told why")
	defer func() { _ = over.Close() }()

	r := bufio.NewReader(over)
	require.NoError(t, over.SetReadDeadline(time.Now().Add(5*time.Second)))
	line, err := r.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "-ERR max number of clients reached\r\n", line)

	// And then it is closed, rather than left open holding the slot it was
	// refused for.
	_, err = r.ReadByte()
	assert.Error(t, err, "the refused connection must be closed")

	assert.Equal(t, uint64(1), srv.conns.rejected.Load())
	assert.Equal(t, int64(2), srv.conns.connected.Load(), "a refused connection is not a client")
}

// TestAFreedSlotIsReusable. The limit has to be a live count and not a
// high-water mark, or a server that has once been busy never accepts again.
func TestAFreedSlotIsReusable(t *testing.T) {
	srv, serveErr := newLimitedServer(t, newFakeStore(), ConnLimits{MaxConnections: 1})
	defer shutdownServer(t, srv, serveErr)

	first, r := dial(t, srv)
	send(t, first, "*1\r\n$4\r\nPING\r\n")
	require.Equal(t, "+PONG\r\n", readReply(t, first, r))
	require.NoError(t, first.Close())

	// The slot comes back when the handler notices the close, which is not
	// instant; the point is that it comes back at all.
	require.Eventually(t, func() bool {
		conn, err := dialContext(t, srv.Addr())
		if err != nil {
			return false
		}
		defer func() { _ = conn.Close() }()

		reader := bufio.NewReader(conn)
		if _, writeErr := conn.Write([]byte("*1\r\n$4\r\nPING\r\n")); writeErr != nil {
			return false
		}
		if deadlineErr := conn.SetReadDeadline(time.Now().Add(time.Second)); deadlineErr != nil {
			return false
		}
		line, readErr := reader.ReadString('\n')
		return readErr == nil && line == "+PONG\r\n"
	}, 5*time.Second, 20*time.Millisecond, "the slot a closed connection held was never given back")
}

// idleLimits is a short timeout so the idle tests finish in a test's time
// rather than in a client's. The configuration floor is a second; nothing in
// the server enforces that, because it is a usability bound rather than a
// correctness one.
func idleLimits(d time.Duration) ConnLimits {
	return ConnLimits{IdleTimeout: d}
}

// TestAnIdleConnectionIsClosedAfterTheTimeout. A connection that opens and says
// nothing held a goroutine and its buffers forever — the gap ISSUE-0018 found
// alongside the request budget.
func TestAnIdleConnectionIsClosedAfterTheTimeout(t *testing.T) {
	const idle = 300 * time.Millisecond

	srv, serveErr := newLimitedServer(t, newFakeStore(), idleLimits(idle))
	defer shutdownServer(t, srv, serveErr)

	conn, r := dial(t, srv)

	started := time.Now()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err := r.ReadByte()
	elapsed := time.Since(started)

	require.Error(t, err, "an idle connection must be closed, not held")
	assert.GreaterOrEqual(t, elapsed, idle, "closed before the timeout was up")
	assert.Less(t, elapsed, 3*idle, "the timeout took %s to fire", elapsed)
	assert.Equal(t, uint64(1), srv.conns.idleClosed.Load(), "the disconnect must be attributable")
}

// TestAnActiveConnectionOutlivesTheIdleTimeout. The clock has to restart, or
// the timeout is a connection lifetime.
func TestAnActiveConnectionOutlivesTheIdleTimeout(t *testing.T) {
	const idle = 300 * time.Millisecond

	srv, serveErr := newLimitedServer(t, newFakeStore(), idleLimits(idle))
	defer shutdownServer(t, srv, serveErr)

	conn, r := dial(t, srv)

	deadline := time.Now().Add(4 * idle)
	for time.Now().Before(deadline) {
		send(t, conn, "*1\r\n$4\r\nPING\r\n")
		require.Equal(t, "+PONG\r\n", readReply(t, conn, r))
		time.Sleep(idle / 3)
	}

	assert.Zero(t, srv.conns.idleClosed.Load(), "a connection issuing commands is not idle")
}

// TestByteDribblingDoesNotDefeatTheIdleTimeout is the reason the clock is
// refreshed per command and not per byte.
//
// A client sending one byte at a time forever never completes a request, so
// there is nothing it is waiting for and nothing the server owes it — but a
// timeout armed on read activity would be pushed out by every byte and never
// fire. This client is the one the timeout exists for.
func TestByteDribblingDoesNotDefeatTheIdleTimeout(t *testing.T) {
	const idle = 300 * time.Millisecond

	srv, serveErr := newLimitedServer(t, newFakeStore(), idleLimits(idle))
	defer shutdownServer(t, srv, serveErr)

	conn, r := dial(t, srv)

	// A command that is never terminated, one byte at a time, for well past the
	// timeout.
	dribbling := make(chan struct{})
	go func() {
		defer close(dribbling)
		for range 200 {
			if _, err := conn.Write([]byte("P")); err != nil {
				return
			}
			time.Sleep(idle / 10)
		}
	}()

	started := time.Now()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err := r.ReadByte()
	elapsed := time.Since(started)
	<-dribbling

	require.Error(t, err, "a client dribbling bytes without issuing a command must still be reaped")
	assert.Less(t, elapsed, 10*idle, "the dribble pushed the idle timeout out by %s", elapsed)
	assert.Equal(t, uint64(1), srv.conns.idleClosed.Load())
}

// TestAZeroIdleTimeoutDisablesIt. Zero is an operator's decision and not an
// unset field, so normalize must leave it alone.
func TestAZeroIdleTimeoutDisablesIt(t *testing.T) {
	assert.Zero(t, ConnLimits{IdleTimeout: 0}.normalize().IdleTimeout)
	assert.Equal(t, defaultIdleTimeout, ConnLimits{IdleTimeout: -1}.normalize().IdleTimeout)

	srv, serveErr := newLimitedServer(t, newFakeStore(), idleLimits(0))
	defer shutdownServer(t, srv, serveErr)

	conn, r := dial(t, srv)
	send(t, conn, "*1\r\n$4\r\nPING\r\n")
	require.Equal(t, "+PONG\r\n", readReply(t, conn, r))

	// Idle for longer than any default would allow, then still usable.
	time.Sleep(300 * time.Millisecond)
	send(t, conn, "*1\r\n$4\r\nPING\r\n")
	assert.Equal(t, "+PONG\r\n", readReply(t, conn, r))
	assert.Zero(t, srv.conns.idleClosed.Load())
}

// TestARequestOverTheBudgetIsRefusedWhileItArrives is ISSUE-0018.
//
// The largest request every per-field limit accepts is a million one-byte
// elements: legal, cheap to send, and expensive to decode. The budget refuses
// it part-way through rather than after, so what the server spends is bounded
// by the budget and not by what the client declared.
func TestARequestOverTheBudgetIsRefusedWhileItArrives(t *testing.T) {
	const budget = minRequestBytes

	srv, serveErr := newLimitedServer(t, newFakeStore(), ConnLimits{MaxRequestBytes: budget})
	defer shutdownServer(t, srv, serveErr)

	conn, r := dial(t, srv)

	// A DEL whose keys are individually small, few enough to be inside the
	// element cap, and together far past the byte budget. Every field is inside
	// its own limit; the request as a whole is not, which is the gap the
	// per-field limits leave (ISSUE-0018).
	const (
		elements = 8000
		key      = "0123456789abcdef0123456789abcdef"
	)
	require.Less(t, elements, budget/minElementWire, "the element cap must not be what refuses this")

	var request strings.Builder
	fmt.Fprintf(&request, "*%d\r\n$3\r\nDEL\r\n", elements)
	for range elements - 1 {
		fmt.Fprintf(&request, "$%d\r\n%s\r\n", len(key), key)
	}
	require.Greater(t, request.Len(), 4*budget, "the request must be well past the budget")

	replies := make(chan string, 1)
	go func() {
		line, err := r.ReadString('\n')
		if err != nil {
			line = "read failed: " + err.Error()
		}
		replies <- line
	}()

	require.NoError(t, conn.SetWriteDeadline(time.Now().Add(10*time.Second)))
	sent, err := conn.Write([]byte(request.String()))
	if err != nil {
		t.Logf("the server closed after %d of %d bytes", sent, request.Len())
	}

	select {
	case reply := <-replies:
		assert.Equal(t, "-ERR Protocol error: request is too large\r\n", reply)
	case <-time.After(5 * time.Second):
		t.Fatalf("no reply after sending a %d byte request against a %d byte budget", request.Len(), budget)
	}

	assert.Equal(t, uint64(1), srv.conns.requestClosed.Load())

	// A protocol error leaves the stream unsynchronized, so the connection goes.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, err = r.ReadByte()
	assert.Error(t, err, "the connection must be closed behind the refusal")
}

// TestARequestInsideTheBudgetIsStillServed. A budget that refused ordinary
// requests would be a bug dressed as a limit, so the boundary is checked from
// the accepting side too.
func TestARequestInsideTheBudgetIsStillServed(t *testing.T) {
	srv, serveErr := newLimitedServer(t, newFakeStore(), ConnLimits{MaxRequestBytes: minRequestBytes})
	defer shutdownServer(t, srv, serveErr)

	conn, r := dial(t, srv)

	// A value a few kilobytes under the budget, written and read back whole.
	value := strings.Repeat("v", minRequestBytes-4096)
	send(t, conn, fmt.Sprintf("*3\r\n$3\r\nSET\r\n$5\r\nbig:k\r\n$%d\r\n%s\r\n", len(value), value))
	require.Equal(t, "+OK\r\n", readReply(t, conn, r))

	send(t, conn, "*2\r\n$3\r\nGET\r\n$5\r\nbig:k\r\n")
	require.Equal(t, fmt.Sprintf("$%d\r\n", len(value)), readReply(t, conn, r))

	// And the budget was returned: a second request of the same size works, so
	// the budget is per request rather than per connection.
	send(t, conn, fmt.Sprintf("*3\r\n$3\r\nSET\r\n$5\r\nbig:2\r\n$%d\r\n%s\r\n", len(value), value))
	body := make([]byte, len(value)+2)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err := io.ReadFull(r, body)
	require.NoError(t, err)
	assert.Equal(t, "+OK\r\n", readReply(t, conn, r))
}

// TestTheIssue0018RequestIsNoLongerAccepted. The exact shape the fuzzing found:
// a million one-byte elements, inside every per-field limit, 7MB to send and
// 154MB to decode. It is now refused on the header, before an element arrives.
func TestTheIssue0018RequestIsNoLongerAccepted(t *testing.T) {
	const rounds = 50

	srv, serveErr := newLimitedServer(t, newFakeStore(), ConnLimits{})
	defer shutdownServer(t, srv, serveErr)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	for range rounds {
		conn, err := dialContext(t, srv.Addr())
		require.NoError(t, err)
		require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))

		_, err = conn.Write([]byte("*1048576\r\n$3\r\nDEL\r\n"))
		require.NoError(t, err)

		reply, err := bufio.NewReader(conn).ReadString('\n')
		require.NoError(t, err)
		assert.Equal(t, "-ERR Protocol error: invalid multibulk length\r\n", reply)
		require.NoError(t, conn.Close())
	}

	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("refusing %d million-element headers allocated %d bytes in total", rounds, allocated)

	// Before the element cap, each of these bought a decode; 24MB of slice
	// headers even with ISSUE-0016's clamped reservation, and 154MB if the
	// elements were sent.
	assert.Lessf(t, allocated, uint64(rounds*256*1024),
		"refusing %d million-element headers allocated %d bytes", rounds, allocated)
}

// TestTheLargestAcceptedRequestHasABoundedCost is the other side of the same
// question, and the one TestOneAcceptedRequestHasABoundedCost in
// internal/protocol leaves open: what does the biggest request this server will
// now accept actually cost it?
//
// The protocol test measures the decoder against the protocol's own ceiling.
// This one measures the server against the ceiling it runs with, which is the
// number that decides whether a handful of connections can exhaust the process.
func TestTheLargestAcceptedRequestHasABoundedCost(t *testing.T) {
	srv, serveErr := newLimitedServer(t, newFakeStore(), ConnLimits{})
	defer shutdownServer(t, srv, serveErr)

	elements := srv.codecLimits.MaxMultiBulkLength
	require.Equal(t, maxRequestElements, elements, "the standing element cap is what binds under the defaults")

	var request strings.Builder
	fmt.Fprintf(&request, "*%d\r\n$3\r\nDEL\r\n", elements)
	for range elements - 1 {
		request.WriteString("$1\r\nk\r\n")
	}
	payload := request.String()
	require.Less(t, len(payload), srv.limits.MaxRequestBytes, "the byte budget must not be what refuses this")

	conn, err := dialContext(t, srv.Addr())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(20*time.Second)))

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	_, err = conn.Write([]byte(payload))
	require.NoError(t, err)
	reply, err := bufio.NewReader(conn).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, ":0\r\n", reply, "the request is legal and is served")

	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("the largest request this server accepts is %d bytes of %d elements, and serving it "+
		"allocated %d bytes (%.1fx)", len(payload), elements, allocated, float64(allocated)/float64(len(payload)))

	// The ratio is the decoder's and is pinned in internal/protocol; what is
	// bounded here is the absolute figure, which is what a connection can hold
	// at once. ISSUE-0018 measured 154MB for the same shape at the protocol
	// ceiling.
	assert.Lessf(t, allocated, uint64(48<<20),
		"serving the largest accepted request allocated %d bytes", allocated)
}

// TestTheElementCapIsDerivedFromTheBudget. The budget bounds the bytes and the
// element cap bounds what those bytes can be spent on; a request that declares
// more elements than the budget could hold is refused on the header, before a
// single element has arrived.
func TestTheElementCapIsDerivedFromTheBudget(t *testing.T) {
	srv, serveErr := newLimitedServer(t, newFakeStore(), ConnLimits{MaxRequestBytes: minRequestBytes})
	defer shutdownServer(t, srv, serveErr)

	conn, r := dial(t, srv)
	send(t, conn, "*1048576\r\n")
	assert.Equal(t, "-ERR Protocol error: invalid multibulk length\r\n", readReply(t, conn, r))
}

// panicStore is a keyspace that fails the way a bug does.
type panicStore struct{ *fakeStore }

func (panicStore) Get([]byte) ([]byte, bool) { panic("a handler bug, as found by a fuzzer") }

// TestAHandlerPanicClosesOnlyThatConnection. Without the recovery, FEAT-0025's
// fuzzing ends on the first input that finds a panic, because the panic takes
// the server with it and there is nothing left to fuzz.
func TestAHandlerPanicClosesOnlyThatConnection(t *testing.T) {
	srv, serveErr := newLimitedServer(t, panicStore{newFakeStore()}, ConnLimits{})
	defer shutdownServer(t, srv, serveErr)

	victim, r := dial(t, srv)
	send(t, victim, "*2\r\n$3\r\nGET\r\n$1\r\nk\r\n")

	require.NoError(t, victim.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err := r.ReadByte()
	require.Error(t, err, "the panicking connection must be closed")

	// The server is still there, and so is everything else about it.
	survivor, survivorReader := dial(t, srv)
	send(t, survivor, "*1\r\n$4\r\nPING\r\n")
	assert.Equal(t, "+PONG\r\n", readReply(t, survivor, survivorReader))

	assert.Equal(t, uint64(1), srv.conns.panics.Load(), "a survived panic is still a bug and is counted")
}

// slowStore delays every read, standing in for a command that is genuinely
// expensive — a KEYS over a large keyspace, which ADR-0021 documents as
// blocking its connection.
type slowStore struct {
	*fakeStore
	delay time.Duration
}

func (s slowStore) Get(key []byte) ([]byte, bool) {
	time.Sleep(s.delay)
	return s.fakeStore.Get(key)
}

// TestDrainFinishesAnInFlightCommand. The promise of a graceful shutdown is
// that a command already running is answered; closing the socket under it would
// leave the client unable to tell a completed write from a lost one.
func TestDrainFinishesAnInFlightCommand(t *testing.T) {
	store := newFakeStore()
	require.NoError(t, store.Set([]byte("drain:k"), []byte("value"), 0))

	srv, serveErr := newLimitedServer(t, slowStore{fakeStore: store, delay: 400 * time.Millisecond}, ConnLimits{})

	conn, r := dial(t, srv)
	// Warm the connection so the command below is certainly in flight.
	send(t, conn, "*1\r\n$4\r\nPING\r\n")
	require.Equal(t, "+PONG\r\n", readReply(t, conn, r))

	send(t, conn, "*2\r\n$3\r\nGET\r\n$7\r\ndrain:k\r\n")
	time.Sleep(100 * time.Millisecond)

	started := time.Now()
	shutdownServer(t, srv, serveErr)
	elapsed := time.Since(started)

	assert.Equal(t, "$5\r\n", readReply(t, conn, r), "the in-flight command was not answered")
	assert.Equal(t, "value\r\n", readReply(t, conn, r))
	assert.Less(t, elapsed, 5*time.Second, "the drain took %s", elapsed)
	t.Logf("the drain waited %s for the in-flight command", elapsed.Round(time.Millisecond))
}

// TestDrainDoesNotWaitForIdleConnections. An idle client has nothing in flight
// and nothing owed, so it is closed at once rather than waited on — otherwise
// every shutdown costs the full timeout.
func TestDrainDoesNotWaitForIdleConnections(t *testing.T) {
	srv, serveErr := newLimitedServer(t, newFakeStore(), ConnLimits{})

	const idle = 20
	conns := make([]net.Conn, 0, idle)
	for range idle {
		conn, r := dial(t, srv)
		send(t, conn, "*1\r\n$4\r\nPING\r\n")
		require.Equal(t, "+PONG\r\n", readReply(t, conn, r))
		conns = append(conns, conn)
	}

	started := time.Now()
	shutdownServer(t, srv, serveErr)
	elapsed := time.Since(started)

	assert.Less(t, elapsed, time.Second, "draining 20 idle connections took %s", elapsed)
	for i, conn := range conns {
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
		_, err := bufio.NewReader(conn).ReadByte()
		assert.Errorf(t, err, "connection %d was left open by the drain", i)
	}
}

// TestConnectionChurnLeaksNothing is the soak property in miniature: connect,
// command, disconnect, repeatedly, and end with the goroutines and the
// connection accounting back where they started.
//
// A leak here is invisible in every other test in this file and fatal in a
// process that runs for a week.
func TestConnectionChurnLeaksNothing(t *testing.T) {
	defer goleak.VerifyNone(t,
		// The listener goroutine of the server under test, which shutdownServer
		// stops; goleak samples before the deferred stop has been observed.
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
	)

	srv, serveErr := newLimitedServer(t, newFakeStore(), ConnLimits{})

	const (
		workers = 8
		rounds  = 40
	)

	before := runtime.NumGoroutine()

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				conn, err := dialContext(t, srv.Addr())
				if !assert.NoError(t, err) {
					return
				}
				r := bufio.NewReader(conn)
				if !assert.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second))) {
					_ = conn.Close()
					return
				}
				_, err = conn.Write([]byte("*1\r\n$4\r\nPING\r\n"))
				assert.NoError(t, err)
				line, err := r.ReadString('\n')
				assert.NoError(t, err)
				assert.Equal(t, "+PONG\r\n", line)
				assert.NoError(t, conn.Close())
			}
		}()
	}
	wg.Wait()

	require.Eventually(t, func() bool { return srv.conns.connected.Load() == 0 },
		5*time.Second, 10*time.Millisecond, "connections are still counted after every client closed")

	shutdownServer(t, srv, serveErr)

	require.Eventually(t, func() bool { return runtime.NumGoroutine() <= before+2 },
		5*time.Second, 20*time.Millisecond,
		"goroutines went from %d to %d over %d connections", before, runtime.NumGoroutine(), workers*rounds)

	assert.EqualValues(t, workers*rounds, srv.conns.connectionsReceived.Load())
	assert.EqualValues(t, workers*rounds, srv.conns.commandsProcessed.Load())
}

// TestBudgetReaderStopsAtTheLimit pins the reader itself, because the property
// it has to hold is one about bytes taken rather than about errors returned: a
// reader that returned the error after reading everything would have done the
// damage already (ISSUE-0016's lesson, applied to ISSUE-0018's quantity).
func TestBudgetReaderStopsAtTheLimit(t *testing.T) {
	source := &countingReader{}
	reader := &budgetReader{src: source, limit: 1000}

	buf := make([]byte, 256)
	var err error
	for err == nil {
		_, err = reader.Read(buf)
	}

	require.ErrorIs(t, err, errRequestTooLarge)
	assert.Equal(t, 1000, source.read, "the reader took %d bytes against a 1000 byte budget", source.read)

	reader.reset()
	n, err := reader.Read(buf)
	require.NoError(t, err)
	assert.Positive(t, n, "the budget must come back for the next request")
}

// TestBudgetReaderWithNoLimitPassesThrough covers the disabled case, so that a
// zero budget is an absence of a limit rather than a limit of zero.
func TestBudgetReaderWithNoLimitPassesThrough(t *testing.T) {
	source := &countingReader{}
	reader := &budgetReader{src: source, limit: 0}

	buf := make([]byte, 4096)
	for range 10 {
		n, err := reader.Read(buf)
		require.NoError(t, err)
		require.Equal(t, len(buf), n)
	}
	assert.Equal(t, 10*len(buf), source.read)
}

type countingReader struct{ read int }

func (c *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	c.read += len(p)
	return len(p), nil
}

// TestStalledWriterIsDisconnected. A client that asks for replies and stops
// reading them pins a goroutine and a buffer; the write deadline is what turns
// that into a disconnect rather than a permanent resident.
func TestStalledWriterIsDisconnected(t *testing.T) {
	store := newFakeStore()
	for i := range 2000 {
		require.NoError(t, store.Set([]byte(fmt.Sprintf("stall:%s:%05d", strings.Repeat("k", 40), i)), []byte("v"), 0))
	}

	limits := ConnLimits{IdleTimeout: 300 * time.Millisecond, MaxOutputBytes: 0}
	srv, serveErr := newLimitedServer(t, store, limits)
	defer shutdownServer(t, srv, serveErr)

	conn, err := dialContext(t, srv.Addr())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	// Ask for far more than any socket buffer will hold, and never read it.
	require.NoError(t, conn.SetWriteDeadline(time.Now().Add(5*time.Second)))
	_, err = conn.Write([]byte(strings.Repeat("*2\r\n$4\r\nKEYS\r\n$1\r\n*\r\n", 200)))
	require.NoError(t, err)

	require.Eventually(t, func() bool { return srv.conns.stalledClosed.Load() == 1 },
		10*time.Second, 50*time.Millisecond,
		"a client that stopped reading was not disconnected")

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = io.Copy(io.Discard, conn)
	assert.True(t, err == nil || !errors.Is(err, context.DeadlineExceeded),
		"the connection must have been closed by the server")
}
