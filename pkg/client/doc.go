// Package client is the Go SDK for AtlasCache.
//
// It is a separate Go module from the server (ADR-0015). Importing it pulls in
// nothing but the standard library: the server's configuration, logging and
// test dependencies are not part of what a consumer inherits, and the empty
// require block in its go.mod is the guarantee.
//
// # Getting started
//
//	c, err := client.New(
//		client.WithAddr("localhost:6379"),
//		client.WithPoolSize(10),
//	)
//	if err != nil {
//		return err
//	}
//	defer c.Close()
//
//	if err := c.Set(ctx, "greeting", []byte("hello"), time.Minute); err != nil {
//		return err
//	}
//	value, found, err := c.Get(ctx, "greeting")
//
// New opens no connections. The first call dials; Ping is how a caller asks
// whether the server is reachable before it depends on the answer.
//
// # Contexts
//
// Every method takes a context first, and every blocking step honors it: the
// dial, the wait for a pooled connection, the write, and the read. A call
// canceled mid-flight returns promptly and costs the connection it was using,
// which is discarded rather than handed to the next caller.
//
// # Values are bytes
//
// Values are []byte, and keys are strings that may hold arbitrary bytes. RESP
// is binary safe and so is this SDK; GetString and SetString are conveniences
// over the byte primitives, not replacements for them.
//
// # Errors
//
// Every failure wraps one of six category sentinels — [ErrNetwork],
// [ErrTimeout], [ErrProtocol], [ErrServer], [ErrAuth], [ErrClosed] — so a
// caller decides what to do with errors.Is rather than by matching strings:
//
//	if errors.Is(err, client.ErrNetwork) {
//		// the server was unreachable; the command may not have run
//	}
//
// [Retryable] is the same table in one call. A call canceled by its caller
// belongs to no category and wraps context.Canceled instead.
//
// # Retries
//
// Retries are opt-in per call site and apply only to commands whose reply is
// idempotent:
//
//	value, found, err := c.WithRetries(2).Get(ctx, "greeting")
//
// Get, Set, Exists, Expire, TTL, Keys, Ping, Echo, Info, DBSize and Stats may
// be retried. Del, SetNX, Scan and [Client.Do] never are — a retried DEL
// answers 0 for a key it deleted on the attempt whose reply was lost, a retried
// SETNX reports losing a race it won, a scan cursor belongs to the connection
// that issued it, and Do carries a command the SDK cannot classify at all.
//
// # Reconnection
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
package client
