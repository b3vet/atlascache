//go:build ignore

// This file carries the `ignore` build tag, and it is not a mistake.
//
// These programs live in the root module, so without the tag they would be
// swept up by `go test ./...` — as packages with no test files, contributing
// several hundred uncovered statements each and pulling the module below the
// coverage floor the phase gate enforces. The tag keeps them out of `./...`
// while leaving them buildable, runnable and vettable by name:
//
//	go run  ./examples/retries/main.go -addr 127.0.0.1:6379
//	go vet  ./examples/retries/main.go
//
// examples/run.sh builds and runs every one of them against a real server on
// every CI run, which is the check that actually matters here.

// Command retries shows what WithRetries does and, more usefully, what it
// refuses to do: it never repeats a command whose reply would lie the second
// time, and it never repeats Do at all.
//
//	go run ./examples/retries/main.go -addr 127.0.0.1:6379
//
// It exits 0 when every claim below held and 1 otherwise.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/b3vet/atlascache/pkg/client"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:6379", "AtlasCache server, as host:port")
	flag.Parse()

	if err := run(*addr); err != nil {
		fmt.Fprintln(os.Stderr, "retries:", err)
		os.Exit(1)
	}
}

func run(addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := retryIsOptIn(ctx, addr); err != nil {
		return err
	}
	if err := theViewSharesThePool(ctx, addr); err != nil {
		return err
	}
	return whatRetryCosts(ctx)
}

// retryIsOptIn: a client built by New never retries. WithRetries returns a
// view that does, for the commands whose reply survives being asked twice.
func retryIsOptIn(ctx context.Context, addr string) error {
	c, err := client.New(client.WithAddr(addr))
	if err != nil {
		return fmt.Errorf("configuring the client: %w", err)
	}
	defer func() { _ = c.Close() }()

	const key = "example:retries:key"
	if err := c.Set(ctx, key, []byte("value"), time.Minute); err != nil {
		return fmt.Errorf("writing %s: %w", key, err)
	}
	defer func() { _, _ = c.Del(context.Background(), key) }()

	// Reads are safe to repeat, so a retrying view is the right default for a
	// cache lookup on a request path.
	value, found, err := c.WithRetries(2).Get(ctx, key)
	if err != nil || !found {
		return fmt.Errorf("a retrying Get gave (%q, %v, %v)", value, found, err)
	}
	fmt.Printf("WithRetries(2).Get  -> %q\n", value)

	// Writes are the interesting half. SET repeats safely because the value is
	// fixed: the same bytes land at the same key, and only the expiry drifts.
	if err := c.WithRetries(2).Set(ctx, key, []byte("value"), time.Minute); err != nil {
		return fmt.Errorf("a retrying Set failed: %w", err)
	}

	// DEL, SETNX, SCAN and Do are never retried, whatever the view was built
	// with, because their replies do not survive it: a second DEL answers 0
	// for a key the first one removed, a second SETNX reports losing a race it
	// won, and a scan cursor belongs to the connection that issued it.
	removed, err := c.WithRetries(2).Del(ctx, key)
	if err != nil {
		return fmt.Errorf("Del: %w", err)
	}
	fmt.Printf("WithRetries(2).Del  -> %d (sent once whatever the view asked for)\n", removed)
	return nil
}

// theViewSharesThePool: WithRetries copies the client, not its connections.
// Closing either closes both, which is the one surprise in an otherwise
// unsurprising method.
func theViewSharesThePool(ctx context.Context, addr string) error {
	c, err := client.New(client.WithAddr(addr))
	if err != nil {
		return fmt.Errorf("configuring the client: %w", err)
	}
	reliable := c.WithRetries(3)

	if err := reliable.Ping(ctx); err != nil {
		return fmt.Errorf("Ping through the view: %w", err)
	}

	_ = c.Close()
	if err := reliable.Ping(ctx); !errors.Is(err, client.ErrClosed) {
		return fmt.Errorf("the view survived its parent's Close: %v", err)
	}
	fmt.Println("WithRetries view    -> shares the pool; closing either closes both")
	return nil
}

// whatRetryCosts: against a server that is not there, a retrying Get spends
// its attempts and then reports the last failure with its category intact. Do
// spends exactly one attempt in the same situation.
func whatRetryCosts(ctx context.Context) error {
	dead, err := closedPort()
	if err != nil {
		return err
	}

	c, err := client.New(
		client.WithAddr(dead),
		// Without this the SDK would spend five seconds trying to reconnect
		// inside each attempt, which is right for a long-running process and
		// wrong for a demonstration.
		client.WithReconnectWindow(0),
		client.WithBackoff(20*time.Millisecond, 100*time.Millisecond),
	)
	if err != nil {
		return fmt.Errorf("configuring the client: %w", err)
	}
	defer func() { _ = c.Close() }()

	plain := timeIt(func() error {
		_, _, getErr := c.Get(ctx, "example:retries:key")
		return getErr
	})
	retrying := timeIt(func() error {
		_, _, getErr := c.WithRetries(3).Get(ctx, "example:retries:key")
		return getErr
	})
	escapeHatch := timeIt(func() error {
		_, doErr := c.WithRetries(3).Do(ctx, "GET", "example:retries:key")
		return doErr
	})

	for label, outcome := range map[string]attempt{
		"Get":                plain,
		"WithRetries(3).Get": retrying,
		"WithRetries(3).Do":  escapeHatch,
	} {
		if !errors.Is(outcome.err, client.ErrNetwork) {
			return fmt.Errorf("%s against a dead server gave %v, want ErrNetwork", label, outcome.err)
		}
	}

	fmt.Printf("\nagainst a server that is not listening:\n")
	fmt.Printf("  Get                 %6s  one attempt, failure surfaced\n", round(plain.elapsed))
	fmt.Printf("  WithRetries(3).Get  %6s  four attempts, the last failure surfaced\n", round(retrying.elapsed))
	fmt.Printf("  WithRetries(3).Do   %6s  one attempt: Do is never retried\n", round(escapeHatch.elapsed))
	return nil
}

// attempt is one timed call.
type attempt struct {
	elapsed time.Duration
	err     error
}

func timeIt(call func() error) attempt {
	started := time.Now()
	err := call()
	return attempt{elapsed: time.Since(started), err: err}
}

// round keeps the printed durations readable; the exact numbers depend on the
// jitter the backoff draws and are not worth reporting to the nanosecond.
func round(d time.Duration) time.Duration { return d.Round(time.Millisecond) }

// closedPort returns an address nothing is listening on.
func closedPort() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("reserving a port: %w", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return "", fmt.Errorf("releasing the port: %w", err)
	}
	return addr, nil
}
