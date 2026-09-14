package client

import (
	"context"
	"strconv"
	"time"
)

// The commands this SDK wraps. Every one of them is implemented by a v0.1.0
// server (P2); anything the server grows later is reachable through Do without
// waiting for an SDK release (ADR-0022).
const (
	cmdGet    = "GET"
	cmdSet    = "SET"
	cmdSetNX  = "SETNX"
	cmdDel    = "DEL"
	cmdExists = "EXISTS"
	cmdExpire = "EXPIRE"
	cmdTTL    = "TTL"
	cmdKeys   = "KEYS"
	cmdScan   = "SCAN"
	cmdPing   = "PING"
	cmdEcho   = "ECHO"
	cmdInfo   = "INFO"
	cmdDBSize = "DBSIZE"
	cmdStats  = "STATS"
	cmdDo     = "DO"
)

// ScanStart is the cursor a scan begins at, and the cursor returned once one
// has finished. A scan is over when, and only when, the cursor comes back equal
// to this — an empty page says nothing, because COUNT bounds the work a page
// does rather than the keys it finds.
const ScanStart = "0"

// TTL sentinels, matching what the server answers and what every Redis client
// reports for the same two cases.
const (
	// TTLNoExpiry is returned for a key that exists and will not expire.
	TTLNoExpiry = time.Duration(-1)

	// TTLNoKey is returned when there is no such key. It is not an error: a key
	// that is not there is the ordinary answer for a cache.
	TTLNoKey = time.Duration(-2)
)

// Client is the SDK's contract.
//
// It is an interface from v0.1.0 with one implementation behind it, which is
// speculative generality with a date on it: P7 adds a cluster-aware client that
// satisfies the same interface, so callers switch by changing construction and
// not call sites (ADR-0022).
//
// Every method takes a context first, including the ones where cancellation
// looks pointless today. Adding it later would break every signature in every
// language binding modeled on this one.
//
// Values are []byte throughout. RESP is binary safe, a cached value is
// frequently a serialized blob, and a []byte primitive with a string
// convenience on top is the only arrangement that does not quietly corrupt one.
//
// # Retries
//
// A client built by [New] never retries. [Client.WithRetries] returns a view
// that retries the commands whose reply is idempotent — Get, GetString, Set,
// SetString, Exists, Expire, TTL, Keys, Ping, Echo, Info, DBSize and Stats — on
// a network or timeout failure and on nothing else.
//
// SetNX, Del, Scan and [Client.Do] are never retried whatever WithRetries was
// asked for, and each method below says why. Retryability is a property of the
// reply rather than of the effect: deleting twice is harmless, but the second
// DEL answers 0 for a key the first one removed.
type Client interface {
	// Get returns the value held at key. found distinguishes a missing key from
	// a key holding an empty value: both are legal, the server answers them
	// differently, and a client that conflates them is wrong in a way its
	// caller cannot detect.
	//
	// The returned slice is read-only: do not modify it, and copy it before
	// keeping it past the call that produced it. Nothing enforces that, and the
	// current implementation allocates a fresh slice per reply, so breaking it
	// costs nothing today. It is stated as a contract because it is the one the
	// storage engine on the other end of the wire already keeps — handing a
	// reader an internal view rather than a copy is what makes GET
	// allocation-free there (ADR-0013) — and because a caller who honors it is
	// a caller this SDK can later stop copying for.
	//
	// Retried when the client was built with WithRetries.
	Get(ctx context.Context, key string) (value []byte, found bool, err error)

	// GetString is Get for callers whose values are text. The string is a copy,
	// so the read-only contract on Get's slice does not apply to it.
	GetString(ctx context.Context, key string) (value string, found bool, err error)

	// Set stores a value, with an expiry when ttl is positive and none when it
	// is zero. A negative ttl is an error rather than an immediate expiry.
	//
	// A ttl that is a whole number of seconds goes out as SET ... EX, and
	// anything finer as SET ... PX, so a 500ms entry is a 500ms entry rather
	// than a rounded one.
	//
	// The value is copied onto the wire before Set returns, so the caller may
	// reuse its buffer immediately.
	//
	// Retried when the client was built with WithRetries: repeating SET with a
	// fixed value leaves the same key holding the same bytes. Only the expiry
	// drifts, by however long the retry took.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error

	// SetString is Set for callers whose values are text.
	SetString(ctx context.Context, key, value string, ttl time.Duration) error

	// SetNX stores a value only if the key is free, reporting whether it did.
	//
	// Never retried. A retry that answers 0 cannot be told apart from a key
	// that was already there, so a caller who won the race would be told it
	// lost — which is the one answer SETNX exists to give.
	SetNX(ctx context.Context, key string, value []byte) (stored bool, err error)

	// Del removes keys and reports how many it removed. Removing a key that is
	// not there is not an error; it is a zero in the count.
	//
	// Never retried. Deleting twice is harmless, but the second reply is 0, so
	// a retried DEL reports that it removed nothing when it removed the key on
	// the attempt whose reply was lost.
	Del(ctx context.Context, keys ...string) (removed int64, err error)

	// Exists counts how many of the given keys are present, counting a repeated
	// key once per mention — which is what the server does, and what callers
	// written against Redis expect.
	Exists(ctx context.Context, keys ...string) (count int64, err error)

	// Expire attaches an expiry to an existing key, reporting whether it found
	// one. A ttl that is zero or negative deletes the key, which is what the
	// server does with EXPIRE and what a caller written against Redis expects;
	// a positive ttl under a second is rounded up to one second rather than
	// down to zero, so it expires shortly rather than immediately.
	Expire(ctx context.Context, key string, ttl time.Duration) (applied bool, err error)

	// TTL reports the remaining life of a key, rounded to whole seconds by the
	// server, or [TTLNoExpiry] for a key that will not expire, or [TTLNoKey]
	// for a key that is not there. Neither sentinel is an error.
	TTL(ctx context.Context, key string) (time.Duration, error)

	// Keys returns every key matching a glob. It walks the whole keyspace and
	// holds a connection for the length of the walk, blocking the server for
	// the same period; Scan is what production code should use.
	Keys(ctx context.Context, pattern string) ([]string, error)

	// Scan returns one page of the keyspace and the cursor for the next.
	// An empty match means every key; a count of zero leaves the page size to
	// the server. Start at ScanStart and stop when [ScanResult.Done] is true:
	// an empty page means nothing on its own, because count bounds the work a
	// page does rather than the keys it finds.
	//
	// Never retried, and the reason is also a constraint on the caller: a scan
	// cursor is scoped to the connection that issued it (ADR-0017), so a page
	// fetched over a different connection is answered with an invalid-cursor
	// error. The SDK does not yet pin a connection for the length of a walk,
	// so a multi-page scan over a pool larger than one connection is correct
	// only by luck. ISSUE-0023 tracks the iterator that will carry the
	// constraint for the caller; until it lands, walk the keyspace on a client
	// built with WithPoolSize(1), or use Keys on a keyspace small enough to
	// block on.
	Scan(ctx context.Context, cursor, match string, count int) (ScanResult, error)

	// Ping checks that the server is answering.
	Ping(ctx context.Context) error

	// Echo returns its argument, unchanged and byte for byte.
	Echo(ctx context.Context, message []byte) ([]byte, error)

	// Info returns the server's INFO text, optionally narrowed to sections.
	Info(ctx context.Context, sections ...string) (string, error)

	// DBSize returns the number of keys in the keyspace.
	DBSize(ctx context.Context) (int64, error)

	// Stats returns the server's counters.
	Stats(ctx context.Context) (Stats, error)

	// Do sends an arbitrary command. It is the escape hatch that keeps SDK
	// releases decoupled from server releases; typed methods are preferred
	// wherever one exists.
	//
	// Arguments are rendered as wire bytes: []byte and string are passed
	// through byte for byte, the integer and float types are formatted, bool
	// becomes 1 or 0, and time.Duration becomes whole seconds. Anything else is
	// refused rather than formatted with %v, because a struct that reached the
	// wire as its Go formatting would be stored and only noticed much later.
	//
	// Do is never retried, whatever WithRetries was asked for. The SDK does not
	// know what command it is carrying — INCRBY and SET look alike from here —
	// and a silently repeated write is worse than a surfaced error (FEAT-0028).
	// A caller who knows their command is safe to repeat can retry it in their
	// own loop, which is the difference between a decision made and one guessed
	// at.
	Do(ctx context.Context, args ...any) (Reply, error)

	// WithRetries returns a view of this client that retries idempotent calls
	// up to n additional times on a network or timeout failure — the two
	// categories [Retryable] reports true for. A protocol error, a server
	// error, an authentication failure and a canceled context are surfaced on
	// the first attempt. A negative n is read as zero.
	//
	// Attempts are spaced by the same full-jitter backoff a reconnect uses, so
	// the delay before retry n is drawn from [0, min(base·2ⁿ, max)); see
	// [WithBackoff]. When every attempt fails the caller hears the last
	// failure, with its category intact, rather than a synthetic "gave up".
	//
	// The returned client shares this one's pool, so closing either closes
	// both, and a single client can be used both ways at once. Retry is opt-in
	// per call site rather than global because only the caller knows whether
	// repeating a command is safe — and the SDK will not guess on their behalf.
	WithRetries(n int) Client

	// Close releases every pooled connection and makes every later call return
	// [ErrClosed]. It is idempotent, and it always reports nil. Connections
	// checked out when it runs are closed as they are returned.
	Close() error
}

// ScanResult is one page of a scan, as returned by [Client.Scan].
type ScanResult struct {
	// Cursor is what to pass to the next Scan. The scan is finished when it is
	// ScanStart, and only then.
	Cursor string

	// Keys is this page. It may be empty on a page that is not the last.
	Keys []string
}

// Done reports whether the scan has finished, which is true when and only when
// the cursor has come back to [ScanStart]. A page with no keys in it is not a
// finished scan: count bounds the work the server does for one page, not the
// keys that page finds, so an empty page in the middle of a sparse keyspace is
// ordinary.
func (r ScanResult) Done() bool { return r.Cursor == ScanStart }

// Stats is the server's counters, keyed as STATS names them.
//
// It is a map rather than a struct so that a counter added by a later server
// reaches callers without an SDK release — the same reasoning that gives the
// SDK a Do at all.
type Stats map[string]int64

// Value returns one counter, reporting whether the server sent it.
//
// The second result is what distinguishes a counter this server does not
// publish from one that is genuinely zero — a distinction an older server
// talking to a newer caller will produce, and one that plain map indexing
// loses.
func (s Stats) Value(name string) (int64, bool) {
	v, ok := s[name]
	return v, ok
}

// client is the single-node implementation.
type client struct {
	opts *options
	pool *pool

	// retries is how many additional attempts an idempotent call gets. It is
	// per-view, not per-pool: WithRetries copies the struct and shares the
	// pool, so one client can be used both ways without two sets of
	// connections.
	retries int
}

// New builds a client. It validates its options and opens nothing: the first
// call dials, and Ping is how a caller asks whether the server is reachable.
//
// Deferring the dial means New cannot fail because a server is briefly down,
// which is what a process starting up alongside its cache needs. It also means
// the only error New can report is a bad option — a nil option is skipped, and
// with no options at all the defaults apply: 127.0.0.1:6379, ten connections,
// a five second dial timeout and three second read and write timeouts.
//
// An option that does not validate comes back as an [*Error] with Op "New" and
// no Category, so errors.Is against the category sentinels does not match it.
// That is a gap rather than a design: ISSUE-0023 tracks the ErrConfig sentinel
// that will classify it.
//
// The returned client is safe for concurrent use, and one per server address
// per process is the intended arrangement — it owns a connection pool, so a
// client per call site multiplies connections without multiplying throughput.
// Close it when the process is done with it.
func New(opts ...Option) (Client, error) {
	resolved := defaultOptions()
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(resolved); err != nil {
			return nil, err
		}
	}
	return &client{opts: resolved, pool: newPool(resolved)}, nil
}

func (c *client) Get(ctx context.Context, key string) ([]byte, bool, error) {
	reply, err := c.call(ctx, idempotent, cmdGet, []byte(key))
	if err != nil {
		return nil, false, err
	}
	if reply.IsNil() {
		return nil, false, nil
	}
	value, err := reply.Bytes()
	if err != nil {
		return nil, false, err
	}
	return value, true, nil
}

func (c *client) GetString(ctx context.Context, key string) (string, bool, error) {
	value, found, err := c.Get(ctx, key)
	return string(value), found, err
}

func (c *client) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	expiry, err := expiryArgs(ttl)
	if err != nil {
		return err
	}

	args := make([][]byte, 0, 2+len(expiry))
	args = append(args, []byte(key), value)
	args = append(args, expiry...)

	// SET with a fixed value is idempotent: repeating it leaves the same key
	// holding the same bytes. Only the TTL drifts, by however long the retry
	// took, which is the same drift a slow network already causes.
	_, err = c.call(ctx, idempotent, cmdSet, args...)
	return err
}

func (c *client) SetString(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.Set(ctx, key, []byte(value), ttl)
}

// SetNX is not retried. A retry that answers 0 cannot be told apart from a key
// that was already there, so the caller would be told it lost a race it won.
func (c *client) SetNX(ctx context.Context, key string, value []byte) (bool, error) {
	reply, err := c.call(ctx, unsafeToRetry, cmdSetNX, []byte(key), value)
	if err != nil {
		return false, err
	}
	return reply.Bool()
}

// Del is not retried. Deleting twice is harmless, but the second reply is 0 —
// so a retried DEL reports that it removed nothing when it removed the key on
// the attempt whose reply was lost (FEAT-0028).
func (c *client) Del(ctx context.Context, keys ...string) (int64, error) {
	if len(keys) == 0 {
		return 0, configError("DEL needs at least one key")
	}
	reply, err := c.call(ctx, unsafeToRetry, cmdDel, keyArgs(keys)...)
	if err != nil {
		return 0, err
	}
	return reply.Int64()
}

func (c *client) Exists(ctx context.Context, keys ...string) (int64, error) {
	if len(keys) == 0 {
		return 0, configError("EXISTS needs at least one key")
	}
	reply, err := c.call(ctx, idempotent, cmdExists, keyArgs(keys)...)
	if err != nil {
		return 0, err
	}
	return reply.Int64()
}

func (c *client) Expire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	seconds := int64(ttl / time.Second)
	if ttl > 0 && seconds == 0 {
		// EXPIRE's unit is whole seconds, and rounding a sub-second TTL down to
		// zero would delete the key instead of expiring it shortly.
		seconds = 1
	}
	reply, err := c.call(ctx, idempotent, cmdExpire, []byte(key), []byte(strconv.FormatInt(seconds, 10)))
	if err != nil {
		return false, err
	}
	return reply.Bool()
}

func (c *client) TTL(ctx context.Context, key string) (time.Duration, error) {
	reply, err := c.call(ctx, idempotent, cmdTTL, []byte(key))
	if err != nil {
		return 0, err
	}
	seconds, err := reply.Int64()
	if err != nil {
		return 0, err
	}
	switch seconds {
	case -1:
		return TTLNoExpiry, nil
	case -2:
		return TTLNoKey, nil
	default:
		return time.Duration(seconds) * time.Second, nil
	}
}

func (c *client) Keys(ctx context.Context, pattern string) ([]string, error) {
	reply, err := c.call(ctx, idempotent, cmdKeys, []byte(pattern))
	if err != nil {
		return nil, err
	}
	return reply.Strings()
}

// Scan is not retried. The server's cursors are scoped to the connection that
// issued them (ADR-0017), so a retry landing on a different pooled connection
// would be told its cursor is invalid — an error that reads like a caller
// mistake and is not.
func (c *client) Scan(ctx context.Context, cursor, match string, count int) (ScanResult, error) {
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

	reply, err := c.call(ctx, unsafeToRetry, cmdScan, args...)
	if err != nil {
		return ScanResult{}, err
	}

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

func (c *client) Ping(ctx context.Context) error {
	_, err := c.call(ctx, idempotent, cmdPing)
	return err
}

func (c *client) Echo(ctx context.Context, message []byte) ([]byte, error) {
	reply, err := c.call(ctx, idempotent, cmdEcho, message)
	if err != nil {
		return nil, err
	}
	return reply.Bytes()
}

func (c *client) Info(ctx context.Context, sections ...string) (string, error) {
	reply, err := c.call(ctx, idempotent, cmdInfo, keyArgs(sections)...)
	if err != nil {
		return "", err
	}
	return reply.Text()
}

func (c *client) DBSize(ctx context.Context) (int64, error) {
	reply, err := c.call(ctx, idempotent, cmdDBSize)
	if err != nil {
		return 0, err
	}
	return reply.Int64()
}

func (c *client) Stats(ctx context.Context) (Stats, error) {
	reply, err := c.call(ctx, idempotent, cmdStats)
	if err != nil {
		return nil, err
	}

	pairs, err := reply.Map()
	if err != nil {
		return nil, err
	}

	stats := make(Stats, len(pairs))
	for name, value := range pairs {
		n, err := value.Int64()
		if err != nil {
			return nil, err
		}
		stats[name] = n
	}
	return stats, nil
}

func (c *client) Do(ctx context.Context, args ...any) (Reply, error) {
	if len(args) == 0 {
		return Reply{}, configError("Do needs at least a command name")
	}

	encoded := make([][]byte, 0, len(args))
	for i, arg := range args {
		value, err := encodeArg(arg)
		if err != nil {
			return Reply{}, configError("Do argument " + strconv.Itoa(i) + ": " + err.Error())
		}
		encoded = append(encoded, value)
	}

	// unsafeToRetry, unconditionally and regardless of WithRetries: the SDK
	// cannot classify a command it does not recognize, and guessing wrong
	// duplicates a write.
	return c.call(ctx, unsafeToRetry, cmdDo, encoded...)
}

func (c *client) WithRetries(n int) Client {
	if n < 0 {
		n = 0
	}
	view := *c
	view.retries = n
	return &view
}

func (c *client) Close() error {
	c.pool.close()
	return nil
}

// retryPolicy says whether a call may be sent again after a network failure.
// It is a named type so that every call site reads as a claim about the
// command's semantics rather than as a bare boolean.
type retryPolicy bool

const (
	idempotent    retryPolicy = true
	unsafeToRetry retryPolicy = false
)

// call runs one command, retrying only where the command allows it and the
// client was asked for it.
func (c *client) call(ctx context.Context, policy retryPolicy, op string, args ...[]byte) (Reply, error) {
	command := args
	if op != cmdDo {
		command = append([][]byte{[]byte(op)}, args...)
	} else {
		op = string(args[0])
	}

	attempts := 1
	if policy == idempotent {
		attempts += c.retries
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, backoffDelay(c.opts.backoffBase, c.opts.backoffMax, attempt-1)); err != nil {
				return Reply{}, contextError(op, c.opts.addr, err)
			}
		}

		reply, err := c.attempt(ctx, op, command)
		if err == nil {
			return reply, nil
		}
		lastErr = err

		if !Retryable(err) || ctx.Err() != nil {
			return Reply{}, err
		}
	}
	return Reply{}, lastErr
}

// attempt is one round trip on one connection.
func (c *client) attempt(ctx context.Context, op string, command [][]byte) (Reply, error) {
	conn, err := c.pool.get(ctx)
	if err != nil {
		return Reply{}, err
	}

	reply, err := conn.exchange(ctx, op, command)

	// Returned before the reply is examined, and returned on every path: a
	// connection held past the call that borrowed it is a leak, and a leaked
	// connection in a fixed-size pool is an outage.
	c.pool.put(conn)

	if err != nil {
		return Reply{}, err
	}
	if reply.Type == TypeError {
		return Reply{}, replyError(op, c.opts.addr, reply)
	}
	return reply, nil
}

// keyArgs renders string arguments as the bytes the wire takes.
func keyArgs(keys []string) [][]byte {
	args := make([][]byte, 0, len(keys))
	for _, key := range keys {
		args = append(args, []byte(key))
	}
	return args
}

// expiryArgs renders SET's optional expiry clause.
//
// Sub-second TTLs go out as PX so that a 500ms cache entry is a 500ms cache
// entry: rounding it to EX 0 would be rejected, and rounding to EX 1 would
// double it.
func expiryArgs(ttl time.Duration) ([][]byte, error) {
	switch {
	case ttl == 0:
		return nil, nil
	case ttl < 0:
		return nil, configError("the TTL must not be negative")
	case ttl%time.Second == 0:
		return [][]byte{[]byte("EX"), []byte(strconv.FormatInt(int64(ttl/time.Second), 10))}, nil
	default:
		milliseconds := int64(ttl / time.Millisecond)
		if milliseconds == 0 {
			milliseconds = 1
		}
		return [][]byte{[]byte("PX"), []byte(strconv.FormatInt(milliseconds, 10))}, nil
	}
}
