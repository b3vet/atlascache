package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fake server scopes its scan cursors to the connection that issued them,
// exactly as the real one does (ADR-0017). That is what gives these tests their
// teeth: an SDK that let a walk wander across a pool fails them rather than
// passing by luck, which is how the defect ISSUE-0023 records survived in the
// first place.

// seedKeys writes n keys under a prefix and returns the set of their names.
func seedKeys(t *testing.T, c Client, prefix string, n int) map[string]bool {
	t.Helper()

	want := make(map[string]bool, n)
	for i := range n {
		key := fmt.Sprintf("%s%03d", prefix, i)
		if err := c.Set(t.Context(), key, []byte("v"), 0); err != nil {
			t.Fatalf("seeding %s: %v", key, err)
		}
		want[key] = true
	}
	return want
}

func TestScanIterWalksEveryKeyWhileThePoolIsBusy(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	// A pool of more than one, and busy: the walk has to hold its own
	// connection while other callers are handed the rest. A client that fetched
	// each page from whichever connection was free would be told its cursor is
	// invalid here — and was correct only by luck before.
	c := newTestClient(t, s, WithPoolSize(4))
	want := seedKeys(t, c, "walk:", 25)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	stop := make(chan struct{})
	var traffic sync.WaitGroup
	for range 3 {
		traffic.Add(1)
		go func() {
			defer traffic.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, _, err := c.Get(ctx, "walk:000"); err != nil {
					return
				}
			}
		}()
	}

	seen := map[string]int{}
	it := c.ScanIter(ctx, "walk:*", 2)
	defer it.Close()
	for it.Next() {
		seen[it.Key()]++
	}
	close(stop)
	traffic.Wait()

	if err := it.Err(); err != nil {
		t.Fatalf("the walk failed: %v", err)
	}
	if len(seen) != len(want) {
		t.Fatalf("the walk saw %d keys, want %d", len(seen), len(want))
	}
	for key, count := range seen {
		if !want[key] {
			t.Errorf("the walk returned %q, which was never written", key)
		}
		if count != 1 {
			t.Errorf("the walk returned %q %d times; a scan returns each key once", key, count)
		}
	}
}

// The walk holds its connection even when the pool would otherwise have
// replaced it between pages.
//
// This is the deterministic form of the test above. A pool hands back the
// connection it was most recently given, so a walk that released one between
// pages usually gets the same one and looks correct — which is exactly how the
// defect survived long enough to be written down. An idle timeout this short
// retires every connection the moment it is returned, so a page fetched on a
// connection the walk did not own is answered "invalid cursor" every time
// rather than one run in ten.
func TestAWalkKeepsItsConnectionThroughAnIdleTimeout(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s, WithPoolSize(4), WithMaxIdleTime(time.Nanosecond))
	want := seedKeys(t, c, "idle:", 12)

	keys, err := c.ScanAll(t.Context(), "idle:*", 2)
	if err != nil {
		t.Fatalf("ScanAll while the pool retires every idle connection: %v", err)
	}
	if len(keys) != len(want) {
		t.Fatalf("the walk returned %d keys, want %d", len(keys), len(want))
	}
}

func TestScanAllCollectsTheWholeKeyspace(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s, WithPoolSize(4))
	want := seedKeys(t, c, "all:", 17)
	if err := c.Set(t.Context(), "other:key", []byte("v"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	keys, err := c.ScanAll(t.Context(), "all:*", 3)
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if len(keys) != len(want) {
		t.Fatalf("ScanAll returned %d keys, want %d: %v", len(keys), len(want), keys)
	}
	for _, key := range keys {
		if !want[key] {
			t.Errorf("ScanAll returned %q, which does not match the pattern it was given", key)
		}
	}

	// An empty match is every key, and the one outside the prefix proves the
	// filter above was doing something.
	everything, err := c.ScanAll(t.Context(), "", 3)
	if err != nil {
		t.Fatalf("ScanAll over everything: %v", err)
	}
	if len(everything) != len(want)+1 {
		t.Fatalf("an unfiltered ScanAll returned %d keys, want %d", len(everything), len(want)+1)
	}
}

// A cursor presented on a connection that did not issue it is refused. This is
// the server behavior the iterator exists to carry — asserted here so that the
// tests above are known to be testing something.
func TestAScanCursorIsRefusedOnAnotherConnection(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	first := newTestClient(t, s, WithPoolSize(1))
	seedKeys(t, first, "scoped:", 6)

	page, err := first.Scan(t.Context(), ScanStart, "scoped:*", 2)
	if err != nil {
		t.Fatalf("the first page: %v", err)
	}
	if page.Done() {
		t.Fatalf("the first page of six keys came back finished: %+v", page)
	}

	second := newTestClient(t, s, WithPoolSize(1))
	_, err = second.Scan(t.Context(), page.Cursor, "scoped:*", 2)
	if !errors.Is(err, ErrServer) {
		t.Fatalf("another connection presenting the cursor got %v, want an ErrServer", err)
	}
	if !strings.Contains(err.Error(), "invalid cursor") {
		t.Errorf("the refusal reads %q, which does not name the cursor", err)
	}
}

// An iterator left half-walked gives its connection back when it is closed. The
// pool here holds one connection, so a leaked one is not a statistic: the next
// call blocks until its own deadline.
func TestClosingAnIteratorEarlyReleasesItsConnection(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s, WithPoolSize(1))
	seedKeys(t, c, "early:", 10)

	it := c.ScanIter(t.Context(), "early:*", 2)
	if !it.Next() {
		t.Fatalf("the walk produced no keys: %v", it.Err())
	}
	it.Close()

	if _, _, err := c.Get(mustFinishIn(t, time.Second), "early:000"); err != nil {
		t.Fatalf("a call after an abandoned walk was closed: %v", err)
	}

	// Close is idempotent, and so is everything after it: a closed iterator is
	// finished rather than broken.
	it.Close()
	if it.Next() {
		t.Error("a closed iterator produced another key")
	}
	if err := it.Err(); err != nil {
		t.Errorf("closing a walk early reported an error: %v", err)
	}
}

// An iterator abandoned without Close gives its connection back when the
// context it was given ends. It is the backstop, not the plan — but a forgotten
// Close must not cost a connection for the life of the process.
func TestAnAbandonedIteratorReleasesItsConnectionWithTheContext(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s, WithPoolSize(1))
	seedKeys(t, c, "abandoned:", 10)

	ctx, cancel := context.WithCancel(t.Context())
	it := c.ScanIter(ctx, "abandoned:*", 2)
	if !it.Next() {
		t.Fatalf("the walk produced no keys: %v", it.Err())
	}
	cancel() // and nothing else: no Close, the iterator is simply dropped

	// The connection comes back on the watcher's goroutine, so this polls
	// rather than asserting on the instant after cancel.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, _, err := c.Get(mustFinishIn(t, 200*time.Millisecond), "abandoned:000"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the connection an abandoned walk held was never returned to the pool")
		}
	}
}

// A walk whose connection dies does not quietly stop. Silent truncation is the
// failure worth designing against: the caller gets a list that looks complete
// and is not.
func TestAWalkThatLosesItsConnectionSaysSo(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s, WithPoolSize(2), WithReconnectWindow(0))
	seedKeys(t, c, "broken:", 20)

	// Kill the connection under the second page, leaving the first one
	// delivered: a client that treated the failure as the end of the walk would
	// report two keys out of twenty and no error at all.
	s.setHook(func(_ *bufio.Writer, nc net.Conn, name string, args [][]byte) bool {
		if name == "SCAN" && len(args) > 0 && string(args[0]) != ScanStart {
			_ = nc.Close()
			return true
		}
		return false
	})

	it := c.ScanIter(t.Context(), "broken:*", 2)
	defer it.Close()

	seen := 0
	for it.Next() {
		seen++
	}

	err := it.Err()
	if err == nil {
		t.Fatalf("a walk that lost its connection after %d of 20 keys reported no error", seen)
	}
	if !errors.Is(err, ErrNetwork) {
		t.Fatalf("the failure is %v, want an ErrNetwork", err)
	}
	if !strings.Contains(err.Error(), "started again") {
		t.Errorf("the failure reads %q; it does not tell the caller the walk cannot be resumed", err)
	}
	if !strings.Contains(err.Error(), s.address()) {
		t.Errorf("the failure reads %q; it does not name the server", err)
	}
}

func TestScanIterOnAClosedClientIsErrClosed(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c, err := New(WithAddr(s.address()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	it := c.ScanIter(t.Context(), "", 0)
	defer it.Close()
	if it.Next() {
		t.Fatal("a closed client produced a key")
	}
	if !errors.Is(it.Err(), ErrClosed) {
		t.Fatalf("a walk on a closed client gave %v, want ErrClosed", it.Err())
	}
	if _, err := c.ScanAll(t.Context(), "", 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("ScanAll on a closed client gave %v, want ErrClosed", err)
	}
}

func TestScanIterHonorsACanceledContext(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s)
	seedKeys(t, c, "canceled:", 4)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	it := c.ScanIter(ctx, "canceled:*", 2)
	defer it.Close()
	if it.Next() {
		t.Fatal("a walk on a canceled context produced a key")
	}
	if !errors.Is(it.Err(), context.Canceled) {
		t.Fatalf("a walk on a canceled context gave %v, want a context.Canceled", it.Err())
	}
}

// An empty page in the middle of a walk is ordinary: COUNT bounds the work the
// server does for one page, not the keys that page finds. An iterator that
// stopped at one would silently return a fraction of the keyspace.
func TestAnEmptyPageDoesNotEndAWalk(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s)

	pages := 0
	s.setHook(func(w *bufio.Writer, _ net.Conn, name string, _ [][]byte) bool {
		if name != "SCAN" {
			return false
		}
		pages++
		switch pages {
		case 1:
			// A page with nothing on it, and a cursor that says the walk goes on.
			writeArrayHeader(w, 2)
			writeBulk(w, []byte("7"))
			writeArrayHeader(w, 0)
		default:
			writeArrayHeader(w, 2)
			writeBulk(w, []byte(ScanStart))
			writeArrayHeader(w, 1)
			writeBulk(w, []byte("found-after-an-empty-page"))
		}
		return true
	})

	keys, err := c.ScanAll(t.Context(), "", 0)
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if len(keys) != 1 || keys[0] != "found-after-an-empty-page" {
		t.Fatalf("ScanAll returned %v; an empty page ended the walk", keys)
	}
}

// mustFinishIn is a context that bounds one call, so that a connection the pool
// never got back surfaces as a failed test rather than as one that hangs.
func mustFinishIn(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	t.Cleanup(cancel)
	return ctx
}
