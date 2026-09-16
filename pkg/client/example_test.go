package client_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/b3vet/atlascache/pkg/client"
)

// The examples are compiled by `go test`, so a signature that changes breaks
// them exactly as it breaks a caller. That is the point of writing them here
// rather than in a markdown file, which nothing compiles.
//
// The ones that need no server also run, and their printed output is checked.
// The ones that connect are compiled and not run: a unit test suite that opened
// sockets would be a different kind of test. Complete programs that do run,
// against a real server started in CI, live in the repository's examples/
// directory — an example that has never been executed is a guess.

// ---- Connecting --------------------------------------------------------------

func ExampleNew() {
	c, err := client.New(
		client.WithAddr("localhost:6379"),
		client.WithPoolSize(10),
		client.WithDialTimeout(5*time.Second),
	)
	if err != nil {
		// The only failure New can report is a bad option.
		fmt.Println("misconfigured:", err)
		return
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// New opens nothing. Ping is how you ask whether the server is there.
	if err := c.Ping(ctx); err != nil {
		fmt.Println("unreachable:", err)
		return
	}
	fmt.Println("connected")
}

// A token belongs in the environment rather than in the source or on a command
// line: an argument list is visible in `ps` to every other user on the host.
func ExampleWithAuth() {
	c, err := client.New(
		client.WithAddr("cache.internal:6379"),
		client.WithAuth(os.Getenv("ATLASCACHE_AUTH")),
	)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	if err := c.Ping(context.Background()); errors.Is(err, client.ErrAuth) {
		// Not retryable, and not transient: the token or the server is wrong.
		fmt.Println("check ATLASCACHE_AUTH:", err)
	}
}

// TLS is a tls.Config like any other. A certificate from a public CA needs
// nothing but a minimum version; a private CA is [client.WithTLSCAFile] below.
func ExampleWithTLS() {
	c, err := client.New(
		client.WithAddr("cache.example.com:6379"),
		client.WithTLS(&tls.Config{MinVersion: tls.VersionTLS13}),
		client.WithAuth(os.Getenv("ATLASCACHE_AUTH")),
	)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	fmt.Println(c.Ping(context.Background()))
}

// A private certificate authority is a file path, not fifteen lines of x509.
// The file is read by New, so a path that is wrong is a startup failure rather
// than a handshake failure on the first call.
func ExampleWithTLSCAFile() {
	c, err := client.New(
		client.WithAddr("cache.internal:6379"),
		client.WithTLSCAFile("/etc/atlascache/ca.crt"),
		client.WithAuth(os.Getenv("ATLASCACHE_AUTH")),
	)
	if errors.Is(err, client.ErrConfig) {
		// The CA file is missing, unreadable, or holds no certificate. Nothing
		// was dialed, and retrying will not help.
		fmt.Println("misconfigured:", err)
		return
	}
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	fmt.Println(c.Ping(context.Background()))
}

// The pool is a ceiling, and a call that arrives when every connection is busy
// waits for one rather than opening another. Give every call a deadline, or
// that wait has no bound.
func ExampleWithPoolSize() {
	c, err := client.New(
		client.WithAddr("localhost:6379"),
		// Sized for concurrent in-flight calls, not for goroutines: a
		// connection is held for one round trip, not for the request that
		// borrowed it.
		client.WithPoolSize(32),
	)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, err = c.Get(ctx, "session:abc")
	var atlasErr *client.Error
	if errors.As(err, &atlasErr) && atlasErr.Op == "pool" {
		// Every connection was busy for the whole 50ms. Raise the pool size,
		// or find what is holding connections that long.
		fmt.Println("the pool is saturated")
	}
}

// A long reconnect window rides out a server restart inside one call. Zero
// surfaces the first dial failure at once, which is what a caller who would
// rather serve a miss than wait wants.
func ExampleWithReconnectWindow() {
	c, err := client.New(
		client.WithAddr("localhost:6379"),
		client.WithReconnectWindow(30*time.Second),
		client.WithBackoff(50*time.Millisecond, 2*time.Second),
	)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	// The caller's own deadline still wins: the window bounds the wait, it
	// never extends one.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fmt.Println(c.Ping(ctx))
}

// ---- Values ------------------------------------------------------------------

func ExampleClient_Get() {
	c, err := client.New(client.WithAddr("localhost:6379"))
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	if setErr := c.Set(ctx, "session:abc", []byte("payload"), 5*time.Minute); setErr != nil {
		fmt.Println(setErr)
		return
	}

	// found tells a missing key from a key holding an empty value. Both are
	// legal, and the server answers them differently.
	value, found, err := c.Get(ctx, "session:abc")
	switch {
	case err != nil:
		fmt.Println(err)
	case !found:
		fmt.Println("cache miss")
	default:
		// The slice is read-only. Copy it to keep it past this call.
		kept := make([]byte, len(value))
		copy(kept, value)
		fmt.Printf("%d bytes\n", len(kept))
	}
}

// Errors carry a category, so a caller decides what to do with errors.Is rather
// than by matching on strings.
func ExampleClient_Get_errors() {
	c, err := client.New(client.WithAddr("localhost:6379"))
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	_, _, err = c.Get(context.Background(), "key")
	switch {
	case err == nil:
		fmt.Println("served from the cache")
	case errors.Is(err, client.ErrAuth):
		// Configuration, not weather. Retrying makes it worse.
		fmt.Println("the client is misconfigured:", err)
	case client.Retryable(err):
		// The cache is unreachable; serve from the source instead.
		fmt.Println("degraded:", err)
	default:
		fmt.Println(err)
	}
}

// A TTL is part of the write. Zero stores the key without an expiry, and a
// duration finer than a second is sent as milliseconds rather than rounded.
func ExampleClient_Set() {
	c, err := client.New(client.WithAddr("localhost:6379"))
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	ctx := context.Background()

	if err := c.Set(ctx, "session:abc", []byte("payload"), 15*time.Minute); err != nil {
		fmt.Println(err)
		return
	}
	if err := c.SetString(ctx, "config:region", "eu-west-1", 0); err != nil { // no expiry
		fmt.Println(err)
		return
	}
	fmt.Println("stored")
}

// TTL answers with two sentinels that are not errors: one for a key with no
// expiry, one for a key that is not there.
func ExampleClient_TTL() {
	c, err := client.New(client.WithAddr("localhost:6379"))
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	ttl, err := c.TTL(context.Background(), "session:abc")
	switch {
	case err != nil:
		fmt.Println(err)
	case ttl == client.TTLNoKey:
		fmt.Println("no such key")
	case ttl == client.TTLNoExpiry:
		fmt.Println("stored forever")
	default:
		fmt.Printf("expires in %s\n", ttl)
	}
}

// SetNX is the lock primitive: it reports whether this caller was the one that
// created the key. It is never retried, because a retry that answers "already
// there" cannot be told from losing the race.
func ExampleClient_SetNX() {
	c, err := client.New(client.WithAddr("localhost:6379"))
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	stored, err := c.SetNX(context.Background(), "lock:report", []byte("worker-3"))
	switch {
	case err != nil:
		fmt.Println(err)
	case stored:
		fmt.Println("this worker holds the lock")
	default:
		fmt.Println("another worker got there first")
	}
}

// ---- Retries -----------------------------------------------------------------

// Retries are opt-in per call site, and only for commands whose reply survives
// being asked for twice. SetNX, Del, Scan and Do are never retried.
func ExampleClient_WithRetries() {
	c, err := client.New(client.WithAddr("localhost:6379"))
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	// The view shares the pool, so this costs no extra connections and the
	// plain client keeps its no-retry behavior.
	reliable := c.WithRetries(2)

	value, found, err := reliable.Get(context.Background(), "key")
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(found, len(value))
}

// ---- Iterating ---------------------------------------------------------------

// ScanAll walks the whole keyspace and hands back the keys. It holds one
// connection for the walk, because the server's cursor belongs to the
// connection that issued it (ADR-0017) — which is the whole reason to prefer it
// over a loop of your own.
func ExampleClient_ScanAll() {
	c, err := client.New(client.WithAddr("localhost:6379"))
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	keys, err := c.ScanAll(ctx, "session:*", 100)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(len(keys))
}

// ScanIter is ScanAll for a keyspace too large to hold: one key at a time, one
// page in memory. Close it if the loop is left early, and check Err afterwards
// — a walk that failed and one that finished both stop the loop, and only Err
// tells them apart.
func ExampleClient_ScanIter() {
	c, err := client.New(client.WithAddr("localhost:6379"))
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	it := c.ScanIter(ctx, "session:*", 500)
	defer it.Close()

	var expired int
	for it.Next() {
		// The walk is holding one of the pool's connections while this runs, so
		// anything in here that calls the same client needs a pool of more than
		// one. Collecting the keys with ScanAll first is the other way.
		ttl, ttlErr := c.TTL(ctx, it.Key())
		if ttlErr != nil {
			fmt.Println(ttlErr)
			return
		}
		if ttl == client.TTLNoExpiry {
			expired++
		}
	}
	if err := it.Err(); err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(expired)
}

// Scan is the single-page primitive under the two above. The scan is over when
// the cursor comes back as ScanStart, and only then — an empty page says
// nothing. A walk built this way is correct only on a client whose pool holds
// one connection, since every page has to go over the connection that issued
// the cursor.
func ExampleClient_Scan() {
	c, err := client.New(client.WithAddr("localhost:6379"), client.WithPoolSize(1))
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	var total int
	for cursor := client.ScanStart; ; {
		page, pageErr := c.Scan(ctx, cursor, "session:*", 100)
		if pageErr != nil {
			fmt.Println(pageErr)
			return
		}
		total += len(page.Keys)
		if page.Done() {
			break
		}
		cursor = page.Cursor
	}
	fmt.Println(total)
}

func ExampleScanResult_Done() {
	// A page in the middle of a sparse keyspace can be empty and still not be
	// the end: count bounds the work the server does, not the keys it finds.
	middle := client.ScanResult{Cursor: "4096", Keys: nil}
	last := client.ScanResult{Cursor: client.ScanStart, Keys: []string{"session:abc"}}

	fmt.Println(middle.Done(), last.Done())
	// Output: false true
}

// ---- The escape hatch ---------------------------------------------------------

// Do reaches commands the SDK does not wrap, so an SDK release is never what
// stands between a caller and a new server command.
func ExampleClient_Do() {
	c, err := client.New(client.WithAddr("localhost:6379"))
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	// Do is never retried, whatever WithRetries was asked for: the SDK cannot
	// tell a read from a write here.
	reply, err := c.Do(context.Background(), "DBSIZE")
	if err != nil {
		fmt.Println(err)
		return
	}

	keys, err := reply.Int64()
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("%d keys\n", keys)
}

func ExampleReply_Int64() {
	// The server answers some numeric questions with an integer and some with
	// a bulk string. Int64 takes either, so a caller need not know which.
	integer := client.Reply{Type: client.TypeInteger, Int: 42}
	bulk := client.Reply{Type: client.TypeBulk, Str: []byte("42")}

	a, err := integer.Int64()
	b, err2 := bulk.Int64()
	if err != nil || err2 != nil {
		fmt.Println(err, err2)
		return
	}
	fmt.Println(a, b)
	// Output: 42 42
}

func ExampleReply_Bytes() {
	value := client.Reply{Type: client.TypeBulk, Str: []byte("payload")}
	missing := client.Reply{Type: client.TypeNil}

	// A nil reply is not an error — it is a cache miss — so the length of the
	// result cannot tell a missing key from an empty one. IsNil can.
	got, err := value.Bytes()
	absent, err2 := missing.Bytes()

	fmt.Printf("%q %v | %q %v %v\n", got, err, absent, err2, missing.IsNil())
	// Output: "payload" <nil> | "" <nil> true
}

func ExampleStats_Value() {
	stats := client.Stats{"hits": 1024, "misses": 12}

	// The second result separates a counter this server does not publish from
	// one that is genuinely zero.
	hits, ok := stats.Value("hits")
	evictions, published := stats.Value("evictions")

	fmt.Println(hits, ok, evictions, published)
	// Output: 1024 true 0 false
}

// ---- Errors -------------------------------------------------------------------

// Retryable is the category table in one call: a network failure and a timeout
// may be tried again, and nothing else may. It says nothing about whether the
// command is safe to repeat — that is the caller's call, and the reason
// WithRetries is opt-in.
func ExampleRetryable() {
	categories := []error{
		client.ErrNetwork,
		client.ErrTimeout,
		client.ErrProtocol,
		client.ErrServer,
		client.ErrAuth,
		client.ErrClosed,
		client.ErrConfig,
	}

	for _, category := range categories {
		err := &client.Error{Category: category, Op: "GET", Addr: "localhost:6379"}
		fmt.Printf("%-22s %v\n", category, client.Retryable(err))
	}

	// Output:
	// network failure        true
	// deadline exceeded      true
	// protocol error         false
	// server error           false
	// authentication failed  false
	// client is closed       false
	// configuration error    false
}

// ErrConfig is what New returns for an option it will not accept. It is the
// category a program acts on differently from every other: a bad address or an
// unreadable CA file is a reason to stop, not to retry.
func ExampleErrConfig() {
	_, err := client.New(client.WithPoolSize(0))

	fmt.Println(errors.Is(err, client.ErrConfig), client.Retryable(err))
	fmt.Println(err)

	// Output:
	// true false
	// atlascache: New: the pool size must be at least 1
}

// ErrAuthDisabled is the one authentication failure the caller's token did not
// cause: the server has no password set, so it refuses every AUTH. Telling it
// apart is what keeps an operator from rotating a credential that is fine.
func ExampleErrAuthDisabled() {
	// What such a server produces, built here so the example runs.
	var err error = &client.Error{
		Category: client.ErrAuth,
		Op:       "AUTH",
		Addr:     "cache.internal:6379",
		Kind:     "ERR",
		Message:  "this server has authentication disabled and refuses every AUTH",
		Err:      client.ErrAuthDisabled,
	}

	if errors.Is(err, client.ErrAuthDisabled) {
		fmt.Println("the server is not asking for a token; unset ATLASCACHE_AUTH or turn auth on")
	}
	fmt.Println(errors.Is(err, client.ErrAuth))

	// Output:
	// the server is not asking for a token; unset ATLASCACHE_AUTH or turn auth on
	// true
}

// Error carries the detail the category leaves out: which command, which
// server, and which error kind the server used. Reach for it with errors.As
// when the category is not enough — never by matching on the message.
func ExampleError() {
	// What a server out of memory produces, built here so the example runs.
	var err error = &client.Error{
		Category: client.ErrServer,
		Op:       "SET",
		Addr:     "cache.internal:6379",
		Kind:     "OOM",
		Message:  "OOM command not allowed when used memory > 'maxmemory'",
	}

	fmt.Println(err)

	var atlasErr *client.Error
	if errors.As(err, &atlasErr) && atlasErr.Kind == "OOM" {
		fmt.Printf("%s on %s is out of memory; shed load rather than retrying\n",
			atlasErr.Op, atlasErr.Addr)
	}
	fmt.Println(errors.Is(err, client.ErrServer), client.Retryable(err))

	// Output:
	// atlascache: SET cache.internal:6379: OOM command not allowed when used memory > 'maxmemory'
	// SET on cache.internal:6379 is out of memory; shed load rather than retrying
	// true false
}
