package scenarios

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// The relay and the leak detector are test rigging, and rigging that is wrong
// makes a scenario lie in whichever direction the bug leans. So they are tested
// here directly: the corruption really corrupts, the delay really delays, and
// waitForNoOpenConnections really fails when something is still open.

// echoServer answers every line with the same line, which is enough to see
// whether bytes crossed the relay and in what state.
func echoServer(t *testing.T) string {
	t.Helper()

	var config net.ListenConfig
	listener, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				reader := bufio.NewReader(conn)
				for {
					line, readErr := reader.ReadString('\n')
					if readErr != nil {
						return
					}
					if _, writeErr := conn.Write([]byte(line)); writeErr != nil {
						return
					}
				}
			}()
		}
	}()
	return listener.Addr().String()
}

func dialProxy(t *testing.T, relay *proxy) (net.Conn, *bufio.Reader) {
	t.Helper()

	var dialer net.Dialer
	conn, err := dialer.DialContext(context.Background(), "tcp", relay.addr())
	if err != nil {
		t.Fatalf("dialing the relay: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, bufio.NewReader(conn)
}

func TestProxyRelaysAndCounts(t *testing.T) {
	t.Parallel()

	upstream := echoServer(t)
	relay, err := newProxy(func() string { return upstream })
	if err != nil {
		t.Fatalf("starting the relay: %v", err)
	}
	defer relay.close()

	conn, reader := dialProxy(t, relay)
	if _, writeErr := conn.Write([]byte("hello\n")); writeErr != nil {
		t.Fatalf("write: %v", writeErr)
	}
	line, err := reader.ReadString('\n')
	if err != nil || line != "hello\n" {
		t.Fatalf("read %q (%v), want the line back unchanged", line, err)
	}
	if relay.connections() != 1 {
		t.Errorf("the relay counted %d connections, want 1", relay.connections())
	}
	if relay.open() == 0 {
		t.Error("the relay reports nothing open while a connection is in use")
	}
}

func TestProxyCorruptsExactlyOneReply(t *testing.T) {
	t.Parallel()

	upstream := echoServer(t)
	relay, err := newProxy(func() string { return upstream })
	if err != nil {
		t.Fatalf("starting the relay: %v", err)
	}
	defer relay.close()

	conn, reader := dialProxy(t, relay)

	relay.corruptNextReply()
	if _, writeErr := conn.Write([]byte("first\n")); writeErr != nil {
		t.Fatalf("write: %v", writeErr)
	}

	// The junk arrives first, and the real reply is still behind it — which is
	// what makes a client that reuses the connection hand the next caller this
	// reply instead of its own.
	junk, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading the injected bytes: %v", err)
	}
	if !strings.HasPrefix(junk, string(junkPrefix[:1])) {
		t.Errorf("first line was %q, want the injected prefix", junk)
	}
	real, err := reader.ReadString('\n')
	if err != nil || real != "first\n" {
		t.Fatalf("the real reply did not follow the corruption: %q (%v)", real, err)
	}

	// And only one reply is corrupted: the arming is consumed.
	if _, writeErr := conn.Write([]byte("second\n")); writeErr != nil {
		t.Fatalf("write: %v", writeErr)
	}
	line, err := reader.ReadString('\n')
	if err != nil || line != "second\n" {
		t.Errorf("the next reply was %q (%v), want it untouched", line, err)
	}
}

func TestProxyDelaysReplies(t *testing.T) {
	t.Parallel()

	upstream := echoServer(t)
	relay, err := newProxy(func() string { return upstream })
	if err != nil {
		t.Fatalf("starting the relay: %v", err)
	}
	defer relay.close()

	conn, reader := dialProxy(t, relay)
	relay.setDelay(200 * time.Millisecond)

	started := time.Now()
	if _, err := conn.Write([]byte("slow\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("read: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
		t.Errorf("a reply held for 200ms arrived in %s", elapsed)
	}
}

// TestWaitForNoOpenConnections is the soak's leak detector, checked in both
// directions: it has to pass when everything was closed and fail when one
// connection was left open. A detector that only ever passes is how a leak test
// stays green through a leak.
func TestWaitForNoOpenConnections(t *testing.T) {
	t.Parallel()

	upstream := echoServer(t)
	relay, err := newProxy(func() string { return upstream })
	if err != nil {
		t.Fatalf("starting the relay: %v", err)
	}
	defer relay.close()

	var dialer net.Dialer
	conn, err := dialer.DialContext(context.Background(), "tcp", relay.addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := conn.Write([]byte("open\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The relay registers a connection on its own goroutine, so wait for it to
	// be seen before asserting anything about what is open.
	waitFor(t, func() bool { return relay.open() > 0 })

	// Still open: the detector must say so rather than waiting forever or
	// reporting success. The short context is what keeps this test quick; the
	// message has to name what was open either way.
	brief, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := waitForNoOpenConnections(brief, relay); err == nil {
		t.Fatal("the detector reported no open connections while one was open")
	} else if !strings.Contains(err.Error(), "still open") {
		t.Errorf("error = %v, want it to name what was still open", err)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}
	if err := waitForNoOpenConnections(context.Background(), relay); err != nil {
		t.Errorf("the detector still reports an open connection after everything closed: %v", err)
	}
}

// waitFor polls until the condition holds, or fails the test.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("the condition never held")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDeadPortIsRefused(t *testing.T) {
	t.Parallel()

	addr, err := deadPort()
	if err != nil {
		t.Fatalf("deadPort: %v", err)
	}
	dialer := net.Dialer{Timeout: time.Second}
	conn, err := dialer.DialContext(context.Background(), "tcp", addr)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("something is listening on %s", addr)
	}
}

func TestBlackholeAcceptsAndSaysNothing(t *testing.T) {
	t.Parallel()

	hole, err := newBlackhole()
	if err != nil {
		t.Fatalf("newBlackhole: %v", err)
	}
	defer hole.close()

	var dialer net.Dialer
	conn, err := dialer.DialContext(context.Background(), "tcp", hole.addr())
	if err != nil {
		t.Fatalf("dialing the blackhole: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, writeErr := conn.Write([]byte("anybody there?\n")); writeErr != nil {
		t.Fatalf("write: %v", writeErr)
	}
	if deadlineErr := conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); deadlineErr != nil {
		t.Fatalf("setting a deadline: %v", deadlineErr)
	}

	buf := make([]byte, 1)
	_, err = conn.Read(buf)
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Errorf("read returned %v, want a timeout: the blackhole must answer nothing", err)
	}

	// Closing twice is a no-op, which is what a deferred close needs.
	hole.close()
	hole.close()
}
