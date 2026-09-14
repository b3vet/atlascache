//go:build ignore

// This file carries the `ignore` build tag, and it is not a mistake.
//
// These programs live in the root module, so without the tag they would be
// swept up by `go test ./...` — as packages with no test files, contributing
// several hundred uncovered statements each and pulling the module below the
// coverage floor the phase gate enforces. The tag keeps them out of `./...`
// while leaving them buildable, runnable and vettable by name:
//
//	go run  ./examples/quickstart/main.go -addr 127.0.0.1:6379
//	go vet  ./examples/quickstart/main.go
//
// examples/run.sh builds and runs every one of them against a real server on
// every CI run, which is the check that actually matters here.

// Command quickstart is the shortest complete AtlasCache program: connect,
// write, read, and tell a cache miss from an empty value.
//
// Run it against a server:
//
//	go run ./examples/quickstart/main.go -addr 127.0.0.1:6379
//
// It exits 0 when every step behaved as this program expected and 1 otherwise,
// which is what lets CI run it as a test of the documentation it illustrates.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/b3vet/atlascache/pkg/client"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:6379", "AtlasCache server, as host:port")
	flag.Parse()

	if err := run(*addr); err != nil {
		fmt.Fprintln(os.Stderr, "quickstart:", err)
		os.Exit(1)
	}
}

func run(addr string) error {
	// New validates its options and opens nothing. The first call dials.
	c, err := client.New(
		client.WithAddr(addr),
		client.WithPoolSize(4),
		client.WithDialTimeout(5*time.Second),
	)
	if err != nil {
		return fmt.Errorf("configuring the client: %w", err)
	}
	// Close releases every pooled connection, and is idempotent.
	defer func() { _ = c.Close() }()

	// Every call takes a context, and every blocking step inside it honors
	// one: the dial, the wait for a connection, the write and the read.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Ping is how you find out whether the server is there, because New does
	// not dial and so cannot tell you.
	if err := c.Ping(ctx); err != nil {
		return fmt.Errorf("the server at %s did not answer: %w", addr, err)
	}
	fmt.Println("connected to", addr)

	const key = "example:quickstart:greeting"

	// Values are bytes. SetString is a convenience over the byte primitive,
	// not a replacement for it.
	if err := c.Set(ctx, key, []byte("hello"), time.Minute); err != nil {
		return fmt.Errorf("writing %s: %w", key, err)
	}

	// Get returns three values. found separates a key that is not there from a
	// key holding an empty value; both are legal and the server answers them
	// differently, so a two-value Get would have to conflate one of them.
	value, found, err := c.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("reading %s: %w", key, err)
	}
	if !found {
		return fmt.Errorf("%s was written and then missing", key)
	}
	// The slice is read-only and belongs to the SDK. Copy it to keep it.
	fmt.Printf("%s = %q\n", key, string(value))

	if err := demonstrateMissVersusEmpty(ctx, c); err != nil {
		return err
	}

	removed, err := c.Del(ctx, key)
	if err != nil {
		return fmt.Errorf("deleting %s: %w", key, err)
	}
	fmt.Printf("deleted %d key\n", removed)

	// Every call after Close is ErrClosed. It is a lifecycle bug rather than a
	// transient failure, which is why it is not retryable.
	_ = c.Close()
	if _, _, err := c.Get(ctx, key); !errors.Is(err, client.ErrClosed) {
		return fmt.Errorf("a call after Close gave %v, want ErrClosed", err)
	}
	fmt.Println("closed")
	return nil
}

// demonstrateMissVersusEmpty is the distinction the three-value Get exists for.
// An empty value round-trips as found; a key nobody wrote does not.
func demonstrateMissVersusEmpty(ctx context.Context, c client.Client) error {
	const (
		empty   = "example:quickstart:empty"
		missing = "example:quickstart:no-such-key"
	)

	if err := c.Set(ctx, empty, []byte{}, time.Minute); err != nil {
		return fmt.Errorf("writing %s: %w", empty, err)
	}
	value, found, err := c.Get(ctx, empty)
	if err != nil {
		return fmt.Errorf("reading %s: %w", empty, err)
	}
	if !found || len(value) != 0 {
		return fmt.Errorf("an empty value came back as (%q, found=%v), want (\"\", found=true)", value, found)
	}
	fmt.Printf("%s: found, %d bytes — an empty value is a value\n", empty, len(value))

	value, found, err = c.Get(ctx, missing)
	if err != nil {
		return fmt.Errorf("reading %s: %w", missing, err)
	}
	if found {
		return fmt.Errorf("%s exists, which this example assumed it would not", missing)
	}
	fmt.Printf("%s: not found, %d bytes — a miss is not an error\n", missing, len(value))

	if _, err := c.Del(ctx, empty); err != nil {
		return fmt.Errorf("cleaning up %s: %w", empty, err)
	}
	return nil
}
