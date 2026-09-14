package client

import (
	"bufio"
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Every category in the P3 §4.2 table, produced by a real failure rather than
// constructed, because the claim being tested is that the classification
// survives the path the error actually takes.
func TestErrNetworkWhenNothingIsListening(t *testing.T) {
	noLeaks(t)
	addr := freePort(t)

	c, err := New(WithAddr(addr), WithReconnectWindow(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close() }()

	err = c.Ping(t.Context())
	if !errors.Is(err, ErrNetwork) {
		t.Fatalf("dialing a dead address gave %v, want ErrNetwork", err)
	}
	if !Retryable(err) {
		t.Fatal("a network failure was reported as not retryable")
	}

	var typed *Error
	if !errors.As(err, &typed) {
		t.Fatalf("the error is %#v, want an *Error", err)
	}
	if typed.Op != opDial || typed.Addr != addr {
		t.Fatalf("the error names %q at %q, want a dial at %q", typed.Op, typed.Addr, addr)
	}
}

func TestErrTimeoutWhenTheServerDoesNotAnswer(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	s.setHook(func(_ *bufio.Writer, _ net.Conn, name string, _ [][]byte) bool {
		return name == "GET"
	})

	c := newTestClient(t, s, WithReadTimeout(50*time.Millisecond))
	_, _, err := c.Get(t.Context(), "k")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v, want ErrTimeout", err)
	}
	if !Retryable(err) {
		t.Fatal("a timeout was reported as not retryable")
	}
}

func TestErrProtocolOnAMalformedReply(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	s.setHook(func(w *bufio.Writer, _ net.Conn, name string, _ [][]byte) bool {
		if name != "GET" {
			return false
		}
		writeRaw(w, "@nonsense\r\n")
		return true
	})

	c := newTestClient(t, s)
	_, _, err := c.Get(t.Context(), "k")
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("got %v, want ErrProtocol", err)
	}
	if Retryable(err) {
		t.Fatal("a protocol error was reported as retryable")
	}
}

func TestErrServerOnAnErrorReply(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s)

	_, err := c.Do(t.Context(), "NOSUCHCOMMAND")
	if !errors.Is(err, ErrServer) {
		t.Fatalf("got %v, want ErrServer", err)
	}

	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != "ERR" {
		t.Fatalf("the error is %#v, want one carrying the server's kind", err)
	}
	if typed.Message == "" {
		t.Fatal("the error dropped the server's own text")
	}
}

func TestErrAuthOnABadToken(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	s.requireAuth("right")

	c := newTestClient(t, s, WithAuth("wrong"))
	err := c.Ping(t.Context())
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("got %v, want ErrAuth", err)
	}

	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != kindWrongPass {
		t.Fatalf("the error is %#v, want one carrying WRONGPASS", err)
	}
}

func TestErrClosedAfterClose(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)

	c, err := New(WithAddr(s.address()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Ping(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v, want ErrClosed", err)
	}
}

// The categories have to be mutually exclusive, or errors.Is answers yes to
// two questions with opposite consequences.
func TestErrorCategoriesDoNotOverlap(t *testing.T) {
	categories := []error{ErrNetwork, ErrTimeout, ErrProtocol, ErrServer, ErrAuth, ErrClosed}

	for _, category := range categories {
		err := error(newError(category, "GET", "127.0.0.1:6379", "detail", nil))
		matched := 0
		for _, other := range categories {
			if errors.Is(err, other) {
				matched++
			}
		}
		if matched != 1 {
			t.Errorf("an error in category %v matched %d categories", category, matched)
		}
	}
}

func TestErrorMessagesNameWhatFailed(t *testing.T) {
	cases := map[string]struct {
		err  *Error
		want string
	}{
		"a server reply": {
			newError(ErrServer, "GET", "127.0.0.1:6379", "ERR unknown command 'X'", nil),
			"atlascache: GET 127.0.0.1:6379: ERR unknown command 'X'",
		},
		"a wrapped cause": {
			newError(ErrNetwork, opDial, "127.0.0.1:6379", "", errors.New("connection refused")),
			"atlascache: dial 127.0.0.1:6379: connection refused",
		},
		"a bare category": {
			newError(ErrClosed, opPool, "", "", nil),
			"atlascache: pool: client is closed",
		},
		"nothing at all": {
			&Error{},
			"atlascache: unknown failure",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRetryableFollowsTheTable(t *testing.T) {
	retryable := map[error]bool{
		ErrNetwork:  true,
		ErrTimeout:  true,
		ErrProtocol: false,
		ErrServer:   false,
		ErrAuth:     false,
		ErrClosed:   false,
	}

	for category, want := range retryable {
		err := error(newError(category, "GET", "", "", nil))
		if got := Retryable(err); got != want {
			t.Errorf("Retryable(%v) = %v, want %v", category, got, want)
		}
	}

	if Retryable(nil) {
		t.Error("Retryable(nil) is true")
	}
	if Retryable(errors.New("something else")) {
		t.Error("an unclassified error was reported as retryable")
	}
}

// A canceled call belongs to no category: the caller did it, and reporting it
// as a network failure would send them looking for a server problem.
func TestCancellationIsNotACategory(t *testing.T) {
	err := error(contextError("GET", "127.0.0.1:6379", context.Canceled))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("%v does not unwrap to context.Canceled", err)
	}
	for _, category := range []error{ErrNetwork, ErrTimeout, ErrProtocol, ErrServer, ErrAuth, ErrClosed} {
		if errors.Is(err, category) {
			t.Fatalf("a canceled call was classified as %v", category)
		}
	}
	if Retryable(err) {
		t.Fatal("a canceled call was reported as retryable")
	}
}

// A deadline that expires is a timeout, wherever the clock came from.
func TestDeadlinesAreTimeouts(t *testing.T) {
	err := error(contextError("GET", "", context.DeadlineExceeded))
	if !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v is not both an ErrTimeout and a DeadlineExceeded", err)
	}
}

// failOnce makes a named command fail exactly the first n times it arrives, by
// dropping the connection under the client — which is what a server restart or
// a middlebox reset looks like from here.
func failOnce(s *fakeServer, command string, times int32) *atomic.Int32 {
	var arrivals atomic.Int32
	s.setHook(func(_ *bufio.Writer, nc net.Conn, name string, _ [][]byte) bool {
		if name != command {
			return false
		}
		if arrivals.Add(1) <= times {
			_ = nc.Close()
			return true
		}
		return false
	})
	return &arrivals
}

func TestRetryAppliesOnlyToIdempotentCommands(t *testing.T) {
	t.Run("GET is retried", func(t *testing.T) {
		noLeaks(t)
		s := newFakeServer(t)
		s.set("k", []byte("value"))
		arrivals := failOnce(s, "GET", 1)

		c := newTestClient(t, s, WithPoolSize(1))
		value, found, err := c.WithRetries(2).Get(t.Context(), "k")
		if err != nil {
			t.Fatalf("a retried GET failed: %v", err)
		}
		if !found || string(value) != "value" {
			t.Fatalf("the retried GET gave (%q, %v)", value, found)
		}
		if got := arrivals.Load(); got != 2 {
			t.Fatalf("GET reached the server %d times, want 2", got)
		}
	})

	t.Run("GET is not retried unless asked", func(t *testing.T) {
		noLeaks(t)
		s := newFakeServer(t)
		arrivals := failOnce(s, "GET", 1)

		c := newTestClient(t, s, WithPoolSize(1))
		if _, _, err := c.Get(t.Context(), "k"); !errors.Is(err, ErrNetwork) {
			t.Fatalf("got %v, want the failure surfaced", err)
		}
		if got := arrivals.Load(); got != 1 {
			t.Fatalf("GET reached the server %d times, want 1: retry is opt-in", got)
		}
	})

	// A retried DEL answers 0 for a key it deleted on the attempt whose reply
	// was lost, so the caller is told it removed nothing when it removed the
	// key. The reply is not idempotent even though the effect is.
	t.Run("DEL is not retried", func(t *testing.T) {
		noLeaks(t)
		s := newFakeServer(t)
		arrivals := failOnce(s, "DEL", 1)

		c := newTestClient(t, s, WithPoolSize(1))
		if _, err := c.WithRetries(3).Del(t.Context(), "k"); !errors.Is(err, ErrNetwork) {
			t.Fatalf("got %v, want the failure surfaced", err)
		}
		if got := arrivals.Load(); got != 1 {
			t.Fatalf("DEL reached the server %d times, want 1", got)
		}
	})

	// A retried SETNX that answers 0 cannot be told from a key that was already
	// there, so the caller is told it lost a race it won.
	t.Run("SETNX is not retried", func(t *testing.T) {
		noLeaks(t)
		s := newFakeServer(t)
		arrivals := failOnce(s, "SETNX", 1)

		c := newTestClient(t, s, WithPoolSize(1))
		if _, err := c.WithRetries(3).SetNX(t.Context(), "k", []byte("v")); !errors.Is(err, ErrNetwork) {
			t.Fatalf("got %v, want the failure surfaced", err)
		}
		if got := arrivals.Load(); got != 1 {
			t.Fatalf("SETNX reached the server %d times, want 1", got)
		}
	})

	// A scan cursor belongs to the connection that issued it (ADR-0017), so a
	// retry landing on another pooled connection would be told its cursor is
	// invalid — an error that reads like the caller's mistake and is not.
	t.Run("SCAN is not retried", func(t *testing.T) {
		noLeaks(t)
		s := newFakeServer(t)
		arrivals := failOnce(s, "SCAN", 1)

		c := newTestClient(t, s, WithPoolSize(1))
		if _, err := c.WithRetries(3).Scan(t.Context(), ScanStart, "", 0); !errors.Is(err, ErrNetwork) {
			t.Fatalf("got %v, want the failure surfaced", err)
		}
		if got := arrivals.Load(); got != 1 {
			t.Fatalf("SCAN reached the server %d times, want 1", got)
		}
	})

	t.Run("SET is retried", func(t *testing.T) {
		noLeaks(t)
		s := newFakeServer(t)
		arrivals := failOnce(s, "SET", 1)

		c := newTestClient(t, s, WithPoolSize(1))
		if err := c.WithRetries(2).Set(t.Context(), "k", []byte("v"), 0); err != nil {
			t.Fatalf("a retried SET failed: %v", err)
		}
		if got := arrivals.Load(); got != 2 {
			t.Fatalf("SET reached the server %d times, want 2", got)
		}
	})
}

// The one the SDK must never get wrong: Do carries a command the SDK cannot
// classify, so it is never sent twice on its own initiative — whatever the
// caller asked WithRetries for.
func TestDoIsNeverRetried(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	arrivals := failOnce(s, "INCRBY", 5)

	c := newTestClient(t, s, WithPoolSize(1))

	_, err := c.WithRetries(4).Do(t.Context(), "INCRBY", "counter", 1)
	if !errors.Is(err, ErrNetwork) {
		t.Fatalf("got %v, want the failure surfaced", err)
	}
	if got := arrivals.Load(); got != 1 {
		t.Fatalf("Do sent the command %d times; it must never be retried", got)
	}
}

// A retry exhausted by repeated failures surfaces the last error rather than a
// synthetic one, so the caller still learns what went wrong.
func TestRetriesSurfaceTheLastFailure(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	arrivals := failOnce(s, "GET", 10)

	c := newTestClient(t, s, WithPoolSize(1))
	_, _, err := c.WithRetries(2).Get(t.Context(), "k")
	if !errors.Is(err, ErrNetwork) {
		t.Fatalf("got %v, want ErrNetwork", err)
	}
	if got := arrivals.Load(); got != 3 {
		t.Fatalf("GET was attempted %d times, want 3 (the first plus two retries)", got)
	}
}

// The acceptance criterion from FEAT-0028: the server goes away mid-workload
// and the client carries on, surfacing at most one error.
func TestClientRecoversAcrossAServerRestart(t *testing.T) {
	noLeaks(t)
	addr := freePort(t)
	s := newFakeServerAt(t, addr)

	c := newTestClient(t, s, WithAddr(addr), WithPoolSize(1))
	ctx := t.Context()

	if err := c.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		failures []error
		calls    int
	)

	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			_, _, err := c.Get(ctx, "k")
			mu.Lock()
			calls++
			if err != nil {
				failures = append(failures, err)
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
		}
	}()

	time.Sleep(50 * time.Millisecond)
	s.stop()
	time.Sleep(150 * time.Millisecond) // a real gap, so the client has to retry the dial
	s.start(t, addr)
	time.Sleep(200 * time.Millisecond)

	close(done)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()

	if calls < 10 {
		t.Fatalf("the workload only managed %d calls; the test proved nothing", calls)
	}
	if len(failures) > 1 {
		t.Fatalf("a restart surfaced %d errors, want at most 1: %v", len(failures), failures)
	}

	// And the client is usable afterwards, on a connection it replaced itself.
	if _, _, err := c.Get(ctx, "k"); err != nil {
		t.Fatalf("the client did not recover: %v", err)
	}
}

// Cancellation has to return promptly and cost the connection it was using —
// a call abandoned mid-flight leaves the stream at an unknown position, and a
// connection in that state must never reach another caller.
func TestCancellationReturnsPromptlyAndLeavesNothingBehind(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)

	blocked := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(blocked) }) }
	t.Cleanup(release)

	s.setHook(func(w *bufio.Writer, _ net.Conn, name string, _ [][]byte) bool {
		if name != "GET" {
			return false
		}
		<-blocked
		writeRaw(w, "$-1\r\n")
		return true
	})

	c := newTestClient(t, s, WithPoolSize(2), WithReadTimeout(30*time.Second))
	impl, ok := c.(*client)
	if !ok {
		t.Fatalf("New returned %T, want the single-node implementation", c)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, _, err := c.Get(ctx, "k")
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled call gave %v, want context.Canceled", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("the canceled call took %v to return, with a 30s read timeout configured", elapsed)
	}

	// The connection it was using is gone, not pooled: it was abandoned with a
	// reply still to come.
	waitFor(t, 2*time.Second, "the abandoned connection to be discarded", func() bool {
		stats := impl.pool.stats()
		return stats.Idle == 0 && stats.Live == 0
	})
	// Released before the server-side check: the fake server's handler is still
	// parked in the hook, so it cannot notice the closed socket until it tries
	// to answer.
	release()
	waitFor(t, 2*time.Second, "the server to see it close", func() bool {
		return s.liveConnections() == 0
	})

	// And the client still works, on a connection it opened for the purpose.
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("the client was unusable after a cancellation: %v", err)
	}
}

// Sustained churn: cancel calls at random points and assert that neither
// connections nor goroutines accumulate. This is the cheap version of the
// sdk-pool-soak spec.
func TestChurnLeaksNothing(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s, WithPoolSize(4))
	impl, ok := c.(*client)
	if !ok {
		t.Fatalf("New returned %T", c)
	}

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 40 {
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration((i+j)%5+1)*time.Millisecond)
				//nolint:errcheck // the point is the cancellation, not the reply
				_, _, _ = c.Get(ctx, "k")
				cancel()
			}
		}()
	}
	wg.Wait()

	waitFor(t, 3*time.Second, "the pool to settle", func() bool {
		return impl.pool.stats().Live <= 4
	})
	if stats := impl.pool.stats(); stats.Live > 4 {
		t.Fatalf("the pool holds %+v after churn, more than its size", stats)
	}
	if len(impl.pool.slots) != 4 {
		t.Fatalf("the pool has %d of its 4 slots after churn", len(impl.pool.slots))
	}

	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("the client was unusable after churn: %v", err)
	}
}

func TestDialTimeoutIsEnforced(t *testing.T) {
	noLeaks(t)

	resolved := defaultOptions()
	resolved.addr = "192.0.2.1:6379"
	resolved.dialTimeout = 80 * time.Millisecond
	resolved.reconnectWindow = 0
	resolved.dialer = func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	p := newPool(resolved)
	t.Cleanup(p.close)

	start := time.Now()
	_, err := p.get(context.Background())
	elapsed := time.Since(start)

	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("a dial that never completes gave %v, want ErrTimeout", err)
	}
	if elapsed < 50*time.Millisecond || elapsed > time.Second {
		t.Fatalf("an 80ms dial timeout fired after %v", elapsed)
	}
}

// A write that cannot make progress is bounded by the write timeout. net.Pipe
// is unbuffered, so once the far end stops reading the very next write blocks —
// which is the condition a full socket buffer creates against a real server
// that has stopped draining.
func TestWriteTimeoutIsEnforced(t *testing.T) {
	noLeaks(t)

	serverEnd, clientEnd := net.Pipe()
	t.Cleanup(func() {
		_ = serverEnd.Close()
		_ = clientEnd.Close()
	})

	go func() {
		r := bufio.NewReader(serverEnd)
		// Answer the handshake, then stop reading entirely.
		if _, err := readRequest(r); err != nil {
			return
		}
		if _, err := serverEnd.Write([]byte("-NOPROTO unsupported protocol version\r\n")); err != nil {
			return
		}
	}()

	resolved := defaultOptions()
	resolved.addr = "pipe"
	resolved.writeTimeout = 80 * time.Millisecond
	resolved.reconnectWindow = 0
	resolved.dialer = func(context.Context, string, string) (net.Conn, error) { return clientEnd, nil }

	p := newPool(resolved)
	t.Cleanup(p.close)

	conn, err := p.get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	start := time.Now()
	_, err = conn.exchange(context.Background(), "SET", bytesArgs("SET", "k", "v"))
	elapsed := time.Since(start)

	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("a write that cannot proceed gave %v, want ErrTimeout", err)
	}
	if elapsed > time.Second {
		t.Fatalf("an 80ms write timeout fired after %v", elapsed)
	}
	if !conn.broken {
		t.Fatal("a connection whose write timed out was not marked broken")
	}
	p.put(conn)
}

// Full jitter over an exponentially growing window: every delay inside the
// window, the window doubling, and the spread actually there — an unjittered
// schedule would return the same value every time and synchronize every client
// that lost the same server.
func TestBackoffIsExponentialWithFullJitter(t *testing.T) {
	const (
		base    = 10 * time.Millisecond
		maximum = 160 * time.Millisecond
	)

	t.Run("every delay is inside its window", func(t *testing.T) {
		for attempt := range 10 {
			window := min(base<<attempt, maximum)
			for range 200 {
				got := backoffDelay(base, maximum, attempt)
				if got < 0 || got >= window {
					t.Fatalf("attempt %d gave %v, want [0, %v)", attempt, got, window)
				}
			}
		}
	})

	t.Run("the window grows and then stops at the maximum", func(t *testing.T) {
		// The largest delay seen over many draws approximates the window, which
		// is what shows the growth without asserting on a single random value.
		var previous time.Duration
		for attempt := range 5 {
			var largest time.Duration
			for range 500 {
				if got := backoffDelay(base, maximum, attempt); got > largest {
					largest = got
				}
			}
			if attempt > 0 && largest <= previous {
				t.Fatalf("attempt %d reached %v, no further than attempt %d's %v", attempt, largest, attempt-1, previous)
			}
			previous = largest
		}

		for range 500 {
			if got := backoffDelay(base, maximum, 20); got >= maximum {
				t.Fatalf("a late attempt gave %v, past the %v maximum", got, maximum)
			}
		}
	})

	t.Run("the delays are actually spread", func(t *testing.T) {
		seen := make(map[time.Duration]bool)
		for range 100 {
			seen[backoffDelay(base, maximum, 4)] = true
		}
		if len(seen) < 50 {
			t.Fatalf("100 draws produced %d distinct delays; the jitter is not doing its job", len(seen))
		}
	})

	t.Run("a zero base means no delay", func(t *testing.T) {
		if got := backoffDelay(0, maximum, 3); got != 0 {
			t.Fatalf("a zero base gave %v", got)
		}
	})
}

func TestSleepHonoursItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if err := sleep(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleep gave %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("a canceled sleep took %v", elapsed)
	}

	if err := sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("a completed sleep gave %v", err)
	}
}

// The reconnect window bounds how long a failing dial is retried, so a caller
// with no deadline of its own still gets an answer. Without the bound, a client
// pointed at a server that is never coming back would block forever on its
// first call.
func TestReconnectWindowBoundsTheRetries(t *testing.T) {
	noLeaks(t)
	addr := freePort(t)

	c, err := New(
		WithAddr(addr),
		WithReconnectWindow(150*time.Millisecond),
		WithBackoff(5*time.Millisecond, 20*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close() }()

	start := time.Now()
	err = c.Ping(context.Background())
	elapsed := time.Since(start)

	if !errors.Is(err, ErrNetwork) {
		t.Fatalf("got %v, want ErrNetwork once the window ran out", err)
	}
	if elapsed < 50*time.Millisecond {
		t.Fatalf("the call gave up after %v without spending its window", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("a 150ms window took %v to expire", elapsed)
	}
}

// And the window never extends the caller's own deadline: whichever expires
// first wins, which is what makes the context contract worth anything.
func TestTheCallerDeadlineBeatsTheReconnectWindow(t *testing.T) {
	noLeaks(t)
	addr := freePort(t)

	c, err := New(WithAddr(addr), WithReconnectWindow(30*time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := c.Ping(ctx); err == nil {
		t.Fatal("a call against a dead address succeeded")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a 120ms deadline took %v against a 30s reconnect window", elapsed)
	}
}
