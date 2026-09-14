//go:build ignore

// This file carries the `ignore` build tag, and it is not a mistake.
//
// These programs live in the root module, so without the tag they would be
// swept up by `go test ./...` — as packages with no test files, contributing
// several hundred uncovered statements each and pulling the module below the
// coverage floor the phase gate enforces. The tag keeps them out of `./...`
// while leaving them buildable, runnable and vettable by name:
//
//	go run  ./examples/pool/main.go -addr 127.0.0.1:6379
//	go vet  ./examples/pool/main.go
//
// examples/run.sh builds and runs every one of them against a real server on
// every CI run, which is the check that actually matters here.

// Command pool shows what the connection pool does under pressure: it blocks,
// it never grows past its size, and a caller that waited too long is told so
// by its own deadline.
//
//	go run ./examples/pool/main.go -addr 127.0.0.1:6379
//
// The point is that blocking is the design. A pool that grew under pressure
// would turn one leaked connection into exhausted memory; one that blocks turns
// the same leak into timeouts naming the callers that waited.
//
// It exits 0 when every claim below held and 1 otherwise.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/b3vet/atlascache/pkg/client"
)

const (
	key      = "example:pool:key"
	requests = 200
)

func main() {
	addr := flag.String("addr", "127.0.0.1:6379", "AtlasCache server, as host:port")
	flag.Parse()

	if err := run(*addr); err != nil {
		fmt.Fprintln(os.Stderr, "pool:", err)
		os.Exit(1)
	}
}

func run(addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	seed, err := client.New(client.WithAddr(addr))
	if err != nil {
		return fmt.Errorf("configuring the client: %w", err)
	}
	defer func() { _ = seed.Close() }()

	if err := seed.Set(ctx, key, []byte("value"), time.Minute); err != nil {
		return fmt.Errorf("writing %s: %w", key, err)
	}
	defer func() { _, _ = seed.Del(context.Background(), key) }()

	if err := sizeChangesThroughput(ctx, addr); err != nil {
		return err
	}
	if err := theCeilingHolds(ctx, addr, seed); err != nil {
		return err
	}
	return exhaustionIsTheCallersDeadline(ctx, addr)
}

// sizeChangesThroughput: the same work through one connection and through
// eight. A pool of one serializes every call, which is correct and slow; the
// numbers are what a reader should expect to see move when they change the
// size, not a benchmark.
func sizeChangesThroughput(ctx context.Context, addr string) error {
	for _, size := range []int{1, 8} {
		c, err := client.New(client.WithAddr(addr), client.WithPoolSize(size))
		if err != nil {
			return fmt.Errorf("configuring a pool of %d: %w", size, err)
		}

		started := time.Now()
		err = concurrently(ctx, c, requests)
		elapsed := time.Since(started)
		_ = c.Close()

		if err != nil {
			return err
		}
		fmt.Printf("pool of %d: %d concurrent Gets in %s\n", size, requests, elapsed.Round(time.Millisecond))
	}
	return nil
}

// theCeilingHolds: the pool is a ceiling and not a target. Thirty-two callers
// against a pool of four cost the server four connections, which the server's
// own counters confirm — this is not the client marking its own homework.
func theCeilingHolds(ctx context.Context, addr string, observer client.Client) error {
	const size = 4

	before, err := connectedClients(ctx, observer)
	if err != nil {
		return err
	}

	c, err := client.New(client.WithAddr(addr), client.WithPoolSize(size))
	if err != nil {
		return fmt.Errorf("configuring a pool of %d: %w", size, err)
	}
	defer func() { _ = c.Close() }()

	if err := concurrently(ctx, c, requests); err != nil {
		return err
	}

	after, err := connectedClients(ctx, observer)
	if err != nil {
		return err
	}

	opened := after - before
	if opened > size {
		return fmt.Errorf("%d callers against a pool of %d opened %d connections", requests, size, opened)
	}
	fmt.Printf("pool of %d: %d callers opened %d connections, never more than %d\n",
		size, requests, opened, size)
	return nil
}

// exhaustionIsTheCallersDeadline: a call that cannot get a connection in time
// fails as a timeout at Op "pool". That is the signature to look for when a
// service looks like it has hung on its cache.
func exhaustionIsTheCallersDeadline(ctx context.Context, addr string) error {
	c, err := client.New(client.WithAddr(addr), client.WithPoolSize(1))
	if err != nil {
		return fmt.Errorf("configuring the client: %w", err)
	}
	defer func() { _ = c.Close() }()

	// A deadline that has already passed is the same situation as a pool that
	// stays busy for longer than the caller can wait, without needing a slow
	// command to manufacture one.
	expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel()

	_, _, err = c.Get(expired, key)
	if !errors.Is(err, client.ErrTimeout) {
		return fmt.Errorf("a caller that could not be served gave %v, want ErrTimeout", err)
	}

	var atlasErr *client.Error
	if !errors.As(err, &atlasErr) || atlasErr.Op != "pool" {
		return fmt.Errorf("the failure did not name the pool: %v", err)
	}
	fmt.Printf("\nsaturated: %v\n", err)
	fmt.Println("  op=\"pool\" is the signature. Raise the pool size, or find what holds connections.")
	fmt.Println("  Always give a call a deadline: without one, an exhausted pool is an unbounded wait.")
	return nil
}

// concurrently runs n Gets at once and reports the first failure.
func concurrently(ctx context.Context, c client.Client, n int) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		fail error
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := c.Get(ctx, key); err != nil {
				mu.Lock()
				if fail == nil {
					fail = err
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return fail
}

// connectedClients asks the server how many connections it is holding, which
// is the only honest way to check a client's own accounting.
func connectedClients(ctx context.Context, c client.Client) (int64, error) {
	stats, err := c.Stats(ctx)
	if err != nil {
		return 0, fmt.Errorf("reading STATS: %w", err)
	}
	count, ok := stats.Value("connected_clients")
	if !ok {
		return 0, errors.New("this server does not publish connected_clients")
	}
	return count, nil
}
