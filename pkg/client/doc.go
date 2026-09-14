// Package client is the Go SDK for AtlasCache.
//
// It is a separate Go module from the server (ADR-0015). Importing it pulls in
// nothing but the standard library: the server's configuration, logging and
// test dependencies are not part of what a consumer inherits, and the empty
// require block in its go.mod is the guarantee.
//
// # Getting started
//
// Build a client with [New], use it, and close it when the process is done.
// The runnable examples on this page are the shortest path: one under New for
// connecting, one under Get for a round trip, one under each of the options
// that need explaining. The examples/ directory in the repository holds
// complete programs, run against a live server in CI, and docs/sdk-go.md is
// the task-oriented guide — how to connect over TLS, what happens when the
// server restarts, how to size the pool. This page is the reference.
//
// [New] opens no connections. It validates its options and returns; the first
// call dials. [Client.Ping] is how a caller asks whether the server is
// reachable before it depends on the answer.
//
// One client per server address per process is the intended arrangement. It
// owns a connection pool and is safe for concurrent use, so a second client to
// the same server multiplies connections without multiplying throughput.
//
// # Contexts
//
// Every method takes a context first, and every blocking step honors it: the
// dial, the wait for a pooled connection, the write, and the read. A call
// canceled mid-flight returns promptly and costs the connection it was using,
// which is discarded rather than handed to the next caller.
//
// Give every call a deadline. A cache call that has not answered in a few
// milliseconds has already failed at its job, and a context with no deadline
// turns a busy pool into an unbounded wait.
//
// # Values are bytes
//
// Values are []byte, and keys are strings that may hold arbitrary bytes. RESP
// is binary safe and so is this SDK; [Client.GetString] and [Client.SetString]
// are conveniences over the byte primitives, not replacements for them.
//
// A found value and an empty value are different answers, which is why
// [Client.Get] returns three results rather than two. A cache miss is not an
// error — it is the most ordinary outcome there is — and an empty string is a
// value the server stores and returns like any other (ADR-0022).
//
// The slice [Client.Get] returns is read-only: do not modify it, and copy it
// before keeping it past the call. The method documents why.
//
// # Errors
//
// Every failure wraps one of six category sentinels — [ErrNetwork],
// [ErrTimeout], [ErrProtocol], [ErrServer], [ErrAuth], [ErrClosed] — so a
// caller decides what to do with errors.Is rather than by matching strings.
// [Retryable] is the network/timeout half of that table in one call, and
// [Error] carries the detail — which command, which server, which error kind
// the server used — for errors.As. A call canceled by its caller belongs to no
// category and wraps context.Canceled instead.
//
// The example under Retryable is that table as code, and the one under Get
// shows a caller treating a degraded cache differently from a broken one.
//
// # Retries
//
// Retries are opt-in per call site, because only the caller knows whether
// repeating a command is safe:
//
//	value, found, err := c.WithRetries(2).Get(ctx, "greeting")
//
// Get, GetString, Set, SetString, Exists, Expire, TTL, Keys, Ping, Echo, Info,
// DBSize and Stats may be retried. SetNX, Del, Scan and [Client.Do] never are —
// a retried DEL answers 0 for a key it deleted on the attempt whose reply was
// lost, a retried SETNX reports losing a race it won, a scan cursor belongs to
// the connection that issued it, and Do carries a command the SDK cannot
// classify at all. What makes a command unsafe to retry is its reply, not its
// effect.
//
// # Connections
//
// The client holds a fixed-size pool, ten connections by default. A call that
// arrives when all of them are busy waits for one rather than opening an
// eleventh, and an expired context surfaces as [ErrTimeout] with Op "pool".
// Blocking there is the design and not a hang; [WithPoolSize] explains the
// trade and how to size it.
//
// A connection dropped by a server restart is replaced inside the pool, not by
// a background loop. A call that arrives while the server is coming back
// retries the dial with full-jitter exponential backoff for as long as
// [WithReconnectWindow] and the caller's context both allow, so a restart is a
// pause inside one call rather than a run of failures across many.
//
// # Protocol
//
// The SDK sends HELLO 3 on every new connection and falls back silently when
// the server answers -NOPROTO, which is what a v0.1.0 server does (ADR-0028).
// RESP3 reply types are decoded already, so the SDK needs no change when the
// server starts accepting the negotiation.
//
// [Client.Do] reaches any command the typed methods do not wrap, so a server
// release is never what stands between a caller and a new command (ADR-0022).
package client
