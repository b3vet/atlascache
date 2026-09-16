package client

import (
	"context"
	"errors"
	"strconv"
	"sync"
)

// Walking the keyspace is the one operation in this SDK whose correctness
// depends on which connection carries it.
//
// A scan cursor names a snapshot the server is holding for the connection that
// asked for it (ADR-0017), and no other connection can present it: the second
// page of a walk that moved to another pooled connection comes back "invalid
// cursor", or — on a server that had handed that connection a cursor of its
// own — comes back as somebody else's page. A pool exists precisely to stop a
// caller from knowing which connection they are on, so the SDK has to carry
// this itself rather than leave every caller to discover it (ISSUE-0023).
//
// [Client.ScanAll] and [Client.ScanIter] both do that by pinning one connection
// for the length of the walk. [Client.Scan] remains the single-page primitive
// underneath them.

// ScanIterator walks the keyspace one key at a time, over one pinned connection.
//
// The zero value is not usable; [Client.ScanIter] is how one is made. It is not
// safe for concurrent use: one goroutine runs the loop.
//
//	it := c.ScanIter(ctx, "session:*", 100)
//	defer it.Close()
//	for it.Next() {
//		process(it.Key())
//	}
//	if err := it.Err(); err != nil {
//		return err
//	}
//
// # Closing it
//
// The connection is returned to the pool as soon as the walk finishes or fails,
// so a loop that runs to completion needs no Close. A loop left early — a
// break, a return, an error in the body — must Close, or the connection is held
// until the context given to ScanIter is done, which is the backstop rather
// than the plan. `defer it.Close()` after the call is the habit worth having:
// it is free when the walk completed and it is the whole of the cleanup when it
// did not.
//
// # The pool
//
// A walk in progress is holding one of the client's connections. On the default
// pool of ten that is unremarkable; on a pool of one it means a call made from
// inside the loop body has nothing to run on and waits for its own deadline,
// which is a deadlock a scan-and-delete loop reaches immediately. Either size
// the pool above one, or collect the keys with [Client.ScanAll] first and act on
// them afterwards.
//
// # When the connection fails
//
// A walk cannot move to another connection, so a network failure mid-scan ends
// it: [ScanIterator.Next] returns false and [ScanIterator.Err] explains that the
// walk has to be started again. It is never restarted silently — a scan that
// began again from the start would hand the caller duplicates it cannot
// detect, and one that stopped quietly would hide the keys it never reached.
type ScanIterator struct {
	c *client
	// ctx bounds the whole walk rather than one page of it. A context in a
	// struct is usually a smell; here the walk is one operation that spans
	// several round trips and pins a connection across them, and that context
	// is what the pinning is bounded by. See ScanIter.
	ctx   context.Context
	match string
	count int

	// mu guards everything below. The iterator itself is single-caller, but the
	// context watcher started in acquire is not: it releases the connection
	// from its own goroutine when the caller's context ends.
	mu sync.Mutex

	// conn is the pinned connection: nil before the first page, and again once
	// the walk has ended by any route.
	conn *conn

	// released is closed to stop the context watcher. It is nil whenever conn
	// is, so the two are always given up together.
	released chan struct{}

	cursor  string
	page    []string
	next    int
	key     string
	started bool
	drained bool
	err     error
	closed  bool
}

// ScanAll walks the keyspace and returns every matching key.
func (c *client) ScanAll(ctx context.Context, match string, count int) ([]string, error) {
	it := c.ScanIter(ctx, match, count)
	defer it.Close()

	var keys []string
	for it.Next() {
		keys = append(keys, it.Key())
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	return keys, nil
}

// ScanIter returns an iterator over the matching keys.
//
// The context bounds the whole walk rather than one page of it, which is the
// one place in this SDK a context outlives the call it was passed to: the walk
// is a single operation that takes several round trips, and the connection it
// pins is held across them. When that context is done, the connection goes back
// to the pool whether or not the caller closed the iterator.
func (c *client) ScanIter(ctx context.Context, match string, count int) *ScanIterator {
	return &ScanIterator{c: c, ctx: ctx, match: match, count: count, cursor: ScanStart}
}

// Next advances to the next key, reporting whether there is one.
//
// It returns false at the end of the walk and on the first failure, which are
// told apart by [ScanIterator.Err]. It fetches a page whenever the current one
// is spent, so most calls are free and some are a round trip.
func (it *ScanIterator) Next() bool {
	it.mu.Lock()
	defer it.mu.Unlock()

	if it.err != nil || it.closed {
		return false
	}

	// A loop, not an if: a page may be empty without the walk being over, since
	// COUNT bounds the work the server does rather than the keys it finds.
	for it.next >= len(it.page) {
		if it.drained {
			it.release()
			return false
		}
		if !it.fetch() {
			return false
		}
	}

	it.key = it.page[it.next]
	it.next++
	return true
}

// Key is the key [ScanIterator.Next] last returned true for. It is undefined
// before the first Next and after one that returned false.
func (it *ScanIterator) Key() string {
	it.mu.Lock()
	defer it.mu.Unlock()
	return it.key
}

// Err is the error that ended the walk, or nil if the walk finished.
//
// Check it after the loop, always. A walk that ended on a failure and one that
// ran out of keys both leave Next returning false, and the difference between
// them is the difference between "these are all the keys" and "these are some
// of them".
func (it *ScanIterator) Err() error {
	it.mu.Lock()
	defer it.mu.Unlock()
	return it.err
}

// Close ends the walk and returns the pinned connection to the pool.
//
// It is idempotent, safe to call on a finished walk, and returns nothing to
// check: giving a connection back cannot fail, and a Close that returned an
// error would only invite callers to handle one that is not there. Whatever
// ended the walk is on [ScanIterator.Err] and stays there.
//
// The server keeps the abandoned snapshot until it times out, sixty seconds
// after the last page (ADR-0017). Nothing is sent to drop it, because v0.1.0
// has no command that does.
func (it *ScanIterator) Close() {
	it.mu.Lock()
	defer it.mu.Unlock()
	it.closed = true
	it.release()
}

// fetch reads one page onto the iterator. The caller holds mu.
func (it *ScanIterator) fetch() bool {
	if err := it.ctx.Err(); err != nil {
		it.fail(contextError(cmdScan, it.c.opts.addr, err))
		return false
	}
	if it.conn == nil {
		if it.started {
			// The watcher gave the connection up while the caller was between
			// keys, which happens only when the context is done — and the check
			// above would have caught that. Treat it as the cancellation it is
			// rather than dialing a connection the cursor is meaningless on.
			it.fail(contextError(cmdScan, it.c.opts.addr, context.Canceled))
			return false
		}
		if err := it.acquire(); err != nil {
			it.fail(err)
			return false
		}
	}

	reply, err := it.c.exchange(it.ctx, it.conn, cmdScan, scanCommand(it.cursor, it.match, it.count))
	if err != nil {
		it.fail(it.interrupted(err))
		return false
	}

	result, err := it.c.scanResult(reply)
	if err != nil {
		it.fail(err)
		return false
	}

	it.cursor = result.Cursor
	it.page = result.Keys
	it.next = 0
	it.drained = result.Done()
	return true
}

// acquire takes the connection this walk will run on and starts the watcher
// that gives it back if the caller's context ends first.
//
// The watcher is what keeps an abandoned iterator from costing a connection
// forever. It is a backstop and not a substitute for Close: it fires whenever
// the context does, which for a caller who gave their scan no deadline and no
// cancel is never.
func (it *ScanIterator) acquire() error {
	conn, err := it.c.pool.get(it.ctx)
	if err != nil {
		return err
	}
	it.conn = conn
	it.started = true

	done := it.ctx.Done()
	if done == nil {
		return nil
	}

	released := make(chan struct{})
	it.released = released
	go func() {
		select {
		case <-done:
			it.mu.Lock()
			it.release()
			it.mu.Unlock()
		case <-released:
		}
	}()
	return nil
}

// release gives the pinned connection back, at most once. The caller holds mu.
//
// It runs from the caller's goroutine and from the watcher's, which is why the
// connection is cleared before it is handed over: whichever arrives second must
// find nothing to return.
func (it *ScanIterator) release() {
	conn := it.conn
	it.conn = nil

	if it.released != nil {
		close(it.released)
		it.released = nil
	}
	if conn != nil {
		it.c.pool.put(conn)
	}
}

// fail records the error that ended the walk and gives up the connection. The
// caller holds mu.
func (it *ScanIterator) fail(err error) {
	it.err = err
	it.release()
}

// interrupted restates a failed page as the end of the walk.
//
// The category is kept, so a caller classifying with errors.Is still sees the
// network failure underneath. What changes is what the message tells them to
// do: a cursor dies with the connection that issued it, so there is no resuming
// this walk on the next one — and a caller who read "connection reset" without
// that would reasonably retry the page and be told their cursor is invalid.
func (it *ScanIterator) interrupted(err error) error {
	var typed *Error
	if !errors.As(err, &typed) || !Retryable(err) {
		return err
	}

	restated := *typed
	restated.Message = "the connection carrying this scan failed at cursor " + it.cursor +
		" (" + detailOf(typed) + "); a cursor is only valid on the connection that issued it, " +
		"so the walk cannot be resumed and has to be started again"
	return &restated
}

// detailOf is whatever an *Error had to say for itself, for quoting inside a
// message that adds to it.
func detailOf(err *Error) string {
	switch {
	case err.Message != "":
		return err.Message
	case err.Err != nil:
		return err.Err.Error()
	case err.Category != nil:
		return err.Category.Error()
	default:
		return "no detail"
	}
}

// scanArgs renders SCAN's arguments after the command name.
func scanArgs(cursor, match string, count int) [][]byte {
	if cursor == "" {
		cursor = ScanStart
	}
	args := [][]byte{[]byte(cursor)}
	if match != "" {
		args = append(args, []byte("MATCH"), []byte(match))
	}
	if count > 0 {
		args = append(args, []byte("COUNT"), []byte(strconv.Itoa(count)))
	}
	return args
}

// scanCommand renders a whole SCAN, command name included, which is what a call
// on a pinned connection sends — client.call builds that prefix itself, and the
// iterator does not go through it.
func scanCommand(cursor, match string, count int) [][]byte {
	args := scanArgs(cursor, match, count)
	return append([][]byte{[]byte(cmdScan)}, args...)
}

// scanResult reads the two-element reply SCAN answers with: the next cursor,
// then the page.
func (c *client) scanResult(reply Reply) (ScanResult, error) {
	page, err := reply.Slice()
	if err != nil {
		return ScanResult{}, err
	}
	if len(page) != 2 {
		return ScanResult{}, protocolError(cmdScan, c.opts.addr,
			"SCAN answered with "+strconv.Itoa(len(page))+" elements, want a cursor and a page")
	}

	next, err := page[0].Text()
	if err != nil {
		return ScanResult{}, err
	}
	keys, err := page[1].Strings()
	if err != nil {
		return ScanResult{}, err
	}
	return ScanResult{Cursor: next, Keys: keys}, nil
}
