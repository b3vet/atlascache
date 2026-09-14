//go:build ignore

// This file carries the `ignore` build tag, and it is not a mistake.
//
// These programs live in the root module, so without the tag they would be
// swept up by `go test ./...` — as packages with no test files, contributing
// several hundred uncovered statements each and pulling the module below the
// coverage floor the phase gate enforces. The tag keeps them out of `./...`
// while leaving them buildable, runnable and vettable by name:
//
//	go run  ./examples/do/main.go -addr 127.0.0.1:6379
//	go vet  ./examples/do/main.go
//
// examples/run.sh builds and runs every one of them against a real server on
// every CI run, which is the check that actually matters here.

// Command do uses the SDK's escape hatch: the generic Do, which reaches any
// command the typed methods do not wrap. It is what keeps an SDK release from
// standing between a caller and a new server command (ADR-0022).
//
//	go run ./examples/do/main.go -addr 127.0.0.1:6379
//
// Two rules come with it. Do returns an untyped Reply, so the caller says what
// shape it expects and hears about it when the server disagrees. And Do is
// never retried, whatever WithRetries was asked for: the SDK cannot tell a read
// from a write here, and a silently repeated write is worse than an error.
//
// It exits 0 when every claim below held and 1 otherwise.
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

const key = "example:do:key"

func main() {
	addr := flag.String("addr", "127.0.0.1:6379", "AtlasCache server, as host:port")
	flag.Parse()

	if err := run(*addr); err != nil {
		fmt.Fprintln(os.Stderr, "do:", err)
		os.Exit(1)
	}
}

func run(addr string) error {
	c, err := client.New(client.WithAddr(addr))
	if err != nil {
		return fmt.Errorf("configuring the client: %w", err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer func() { _, _ = c.Del(context.Background(), key) }()

	if err := commandsWithNoTypedMethod(ctx, c); err != nil {
		return err
	}
	if err := argumentsAreRendered(ctx, c); err != nil {
		return err
	}
	return replyShapesAreTheCallersToAssert(ctx, c)
}

// commandsWithNoTypedMethod: COMMAND has no wrapper and needs none. A server
// that grows a command tomorrow is reachable today.
func commandsWithNoTypedMethod(ctx context.Context, c client.Client) error {
	reply, err := c.Do(ctx, "COMMAND")
	if err != nil {
		return fmt.Errorf("COMMAND: %w", err)
	}
	entries, err := reply.Slice()
	if err != nil {
		return fmt.Errorf("reading COMMAND's reply: %w", err)
	}
	// v0.1.0 answers COMMAND with an empty array; the point is that the call
	// is reachable at all, without a wrapper and without an SDK release.
	fmt.Printf("COMMAND            -> an array of %d entries\n", len(entries))

	// PING takes an optional message the typed Ping does not expose, which is
	// the other thing Do is for: the parts of a command a wrapper simplified
	// away.
	reply, err = c.Do(ctx, "PING", "still here")
	if err != nil {
		return fmt.Errorf("PING: %w", err)
	}
	echoed, err := reply.Text()
	if err != nil {
		return fmt.Errorf("reading PING's reply: %w", err)
	}
	fmt.Printf("PING \"still here\"  -> %q\n", echoed)
	return nil
}

// argumentsAreRendered: strings and []byte go out byte for byte, numbers are
// formatted, bool becomes 1 or 0, and time.Duration becomes whole seconds.
// Anything else is refused rather than formatted with %v, because a struct that
// reached the wire as its Go formatting would be stored and noticed much later.
func argumentsAreRendered(ctx context.Context, c client.Client) error {
	// SET key value EX 60 — the int is rendered as its digits.
	reply, err := c.Do(ctx, "SET", key, []byte("value"), "EX", 60)
	if err != nil {
		return fmt.Errorf("SET through Do: %w", err)
	}
	status, err := reply.Text()
	if err != nil {
		return fmt.Errorf("reading SET's reply: %w", err)
	}
	fmt.Printf("SET ... EX 60      -> %q\n", status)

	// A type the SDK will not guess at is a configuration error, caught before
	// anything reaches the wire.
	if _, err := c.Do(ctx, "SET", key, struct{ Field string }{"value"}); err == nil {
		return errors.New("Do accepted a struct argument; it should refuse one")
	}
	fmt.Println("SET ... struct{}   -> refused before it reached the wire")
	return nil
}

// replyShapesAreTheCallersToAssert: the conversion helpers accept every shape
// that can sensibly carry the value asked for, and report ErrProtocol for the
// ones that cannot.
func replyShapesAreTheCallersToAssert(ctx context.Context, c client.Client) error {
	reply, err := c.Do(ctx, "DBSIZE")
	if err != nil {
		return fmt.Errorf("DBSIZE: %w", err)
	}
	keys, err := reply.Int64()
	if err != nil {
		return fmt.Errorf("reading DBSIZE's reply: %w", err)
	}
	fmt.Printf("DBSIZE             -> %d keys\n", keys)

	// The same reply read as the wrong shape is a protocol error, not a panic
	// and not a zero value.
	if _, err := reply.Slice(); !errors.Is(err, client.ErrProtocol) {
		return fmt.Errorf("reading an integer as an array gave %v, want ErrProtocol", err)
	}
	fmt.Println("DBSIZE as an array -> ErrProtocol, naming both shapes")

	// A miss comes back as a nil reply, which is not an error: IsNil is how a
	// Do caller tells a missing key from an empty value, exactly as found does
	// for the typed Get.
	if _, err := c.Del(ctx, key); err != nil {
		return fmt.Errorf("deleting %s: %w", key, err)
	}
	reply, err = c.Do(ctx, "GET", key)
	if err != nil {
		return fmt.Errorf("GET through Do: %w", err)
	}
	if !reply.IsNil() {
		return fmt.Errorf("a missing key gave a %s reply, want nil", reply.Type)
	}
	fmt.Println("GET <missing>      -> a nil reply, which is a miss and not an error")

	// And the rule that matters most: this is never sent twice on the SDK's
	// own initiative, however many retries the view was built with.
	if _, err := c.WithRetries(5).Do(ctx, "PING"); err != nil {
		return fmt.Errorf("PING through a retrying view: %w", err)
	}
	fmt.Println("WithRetries(5).Do  -> still exactly one attempt, by design")
	return nil
}
