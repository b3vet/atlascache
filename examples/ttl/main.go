//go:build ignore

// This file carries the `ignore` build tag, and it is not a mistake.
//
// These programs live in the root module, so without the tag they would be
// swept up by `go test ./...` — as packages with no test files, contributing
// several hundred uncovered statements each and pulling the module below the
// coverage floor the phase gate enforces. The tag keeps them out of `./...`
// while leaving them buildable, runnable and vettable by name:
//
//	go run  ./examples/ttl/main.go -addr 127.0.0.1:6379
//	go vet  ./examples/ttl/main.go
//
// examples/run.sh builds and runs every one of them against a real server on
// every CI run, which is the check that actually matters here.

// Command ttl shows how expiry works from the SDK: setting one with the write,
// adding one afterwards, reading the two sentinels back, and watching a key go
// away.
//
//	go run ./examples/ttl/main.go -addr 127.0.0.1:6379
//
// It exits 0 when every step behaved as this program expected and 1 otherwise.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/b3vet/atlascache/pkg/client"
)

// The keys this program owns. It deletes them on the way out.
const (
	keyExpiring = "example:ttl:session"
	keyForever  = "example:ttl:config"
	keyMissing  = "example:ttl:no-such-key"
	keyShort    = "example:ttl:short"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:6379", "AtlasCache server, as host:port")
	flag.Parse()

	if err := run(*addr); err != nil {
		fmt.Fprintln(os.Stderr, "ttl:", err)
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

	defer func() { _, _ = c.Del(context.Background(), keyExpiring, keyForever, keyShort) }()

	if err := expiryOnTheWrite(ctx, c); err != nil {
		return err
	}
	if err := sentinels(ctx, c); err != nil {
		return err
	}
	if err := expiryAfterTheFact(ctx, c); err != nil {
		return err
	}
	return watchOneExpire(ctx, c)
}

// expiryOnTheWrite: a positive ttl on Set attaches an expiry, and zero does not.
func expiryOnTheWrite(ctx context.Context, c client.Client) error {
	if err := c.Set(ctx, keyExpiring, []byte("payload"), 10*time.Minute); err != nil {
		return fmt.Errorf("writing %s: %w", keyExpiring, err)
	}
	// Zero means no expiry — not "expire immediately". A negative duration is
	// rejected rather than guessed at.
	if err := c.Set(ctx, keyForever, []byte("eu-west-1"), 0); err != nil {
		return fmt.Errorf("writing %s: %w", keyForever, err)
	}

	remaining, err := c.TTL(ctx, keyExpiring)
	if err != nil {
		return fmt.Errorf("reading the TTL of %s: %w", keyExpiring, err)
	}
	if remaining <= 0 || remaining > 10*time.Minute {
		return fmt.Errorf("%s has %s left, want something under ten minutes", keyExpiring, remaining)
	}
	fmt.Printf("%s expires in %s\n", keyExpiring, remaining)
	return nil
}

// sentinels: TTL reports two answers that are not durations and not errors.
func sentinels(ctx context.Context, c client.Client) error {
	forever, err := c.TTL(ctx, keyForever)
	if err != nil {
		return fmt.Errorf("reading the TTL of %s: %w", keyForever, err)
	}
	if forever != client.TTLNoExpiry {
		return fmt.Errorf("%s reported %s, want TTLNoExpiry", keyForever, forever)
	}
	fmt.Printf("%s: TTLNoExpiry — stored without an expiry\n", keyForever)

	absent, err := c.TTL(ctx, keyMissing)
	if err != nil {
		return fmt.Errorf("reading the TTL of %s: %w", keyMissing, err)
	}
	if absent != client.TTLNoKey {
		return fmt.Errorf("%s reported %s, want TTLNoKey", keyMissing, absent)
	}
	fmt.Printf("%s: TTLNoKey — no such key, which is not an error\n", keyMissing)
	return nil
}

// expiryAfterTheFact: Expire attaches an expiry to a key that already exists,
// and reports whether it found one to attach it to.
func expiryAfterTheFact(ctx context.Context, c client.Client) error {
	applied, err := c.Expire(ctx, keyForever, time.Hour)
	if err != nil {
		return fmt.Errorf("expiring %s: %w", keyForever, err)
	}
	if !applied {
		return fmt.Errorf("%s was written and then not found by Expire", keyForever)
	}

	applied, err = c.Expire(ctx, keyMissing, time.Hour)
	if err != nil {
		return fmt.Errorf("expiring %s: %w", keyMissing, err)
	}
	if applied {
		return fmt.Errorf("%s exists, which this example assumed it would not", keyMissing)
	}
	fmt.Printf("Expire: applied to %s, found nothing at %s\n", keyForever, keyMissing)
	return nil
}

// watchOneExpire: a sub-second TTL is sent as milliseconds rather than rounded
// to a whole second, so a short-lived entry really is short-lived.
func watchOneExpire(ctx context.Context, c client.Client) error {
	const life = 300 * time.Millisecond

	if err := c.Set(ctx, keyShort, []byte("temporary"), life); err != nil {
		return fmt.Errorf("writing %s: %w", keyShort, err)
	}
	if _, found, err := c.Get(ctx, keyShort); err != nil || !found {
		return fmt.Errorf("%s was written and is already gone (found=%v, err=%v)", keyShort, found, err)
	}

	// Slack over the TTL so a slow machine does not make this a flake. The
	// server expires lazily on read as well as actively in the background, so
	// the Get below is what reclaims it if the sweeper has not yet.
	timer := time.NewTimer(life * 4)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return ctx.Err()
	}

	_, found, err := c.Get(ctx, keyShort)
	if err != nil {
		return fmt.Errorf("reading %s: %w", keyShort, err)
	}
	if found {
		return fmt.Errorf("%s outlived its %s TTL", keyShort, life)
	}
	fmt.Printf("%s: gone after %s — an expired key reads as a miss\n", keyShort, life)
	return nil
}
