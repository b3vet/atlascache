//go:build ignore

// This file carries the `ignore` build tag, and it is not a mistake.
//
// These programs live in the root module, so without the tag they would be
// swept up by `go test ./...` — as packages with no test files, contributing
// several hundred uncovered statements each and pulling the module below the
// coverage floor the phase gate enforces. The tag keeps them out of `./...`
// while leaving them buildable, runnable and vettable by name:
//
//	go run  ./examples/scan/main.go -addr 127.0.0.1:6379
//	go vet  ./examples/scan/main.go
//
// examples/run.sh builds and runs every one of them against a real server on
// every CI run, which is the check that actually matters here.

// Command scan walks a keyspace a page at a time, which is what production
// code should do instead of calling KEYS.
//
//	go run ./examples/scan/main.go -addr 127.0.0.1:6379 -keys 500
//
// Two things about SCAN are easy to get wrong and both are load-bearing here.
// The walk is over when the cursor comes back to ScanStart and at no other
// time — an empty page in the middle of a sparse keyspace is ordinary, because
// COUNT bounds the work the server does for a page rather than the keys it
// finds. And a cursor belongs to the connection that issued it (ADR-0017), so
// every page of one walk must go over the same connection.
//
// The second one is the SDK's job now: ScanIter and ScanAll hold one connection
// for the length of a walk, so this runs on an ordinary pool. It used to need a
// client pinned to a single connection (ISSUE-0023), which worked and hid the
// constraint in every caller's setup code.
//
// It exits 0 when the walk returned every key exactly once and 1 otherwise.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/b3vet/atlascache/pkg/client"
)

const prefix = "example:scan:"

func main() {
	addr := flag.String("addr", "127.0.0.1:6379", "AtlasCache server, as host:port")
	count := flag.Int("keys", 500, "how many keys to write before walking them")
	page := flag.Int("count", 50, "SCAN's COUNT: how much work one page may do")
	flag.Parse()

	if err := run(*addr, *count, *page); err != nil {
		fmt.Fprintln(os.Stderr, "scan:", err)
		os.Exit(1)
	}
}

func run(addr string, keyCount, pageSize int) error {
	// The default pool, deliberately: a walk that needed a pool of one would be
	// a walk whose correctness depended on the caller's configuration.
	c, err := client.New(client.WithAddr(addr))
	if err != nil {
		return fmt.Errorf("configuring the client: %w", err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	written, err := seed(ctx, c, keyCount)
	if err != nil {
		return err
	}
	defer cleanUp(c, written)

	// The by-hand page loop gets a client of its own, pinned to one connection.
	// That is not a formality: it carries the cursor between calls itself, and
	// a page that reached a different connection would be answered "invalid
	// cursor". The iterator below is the same walk without that requirement.
	paged, err := client.New(client.WithAddr(addr), client.WithPoolSize(1))
	if err != nil {
		return fmt.Errorf("configuring the paging client: %w", err)
	}
	defer func() { _ = paged.Close() }()

	seen, pages, empties, err := walk(ctx, paged, pageSize)
	if err != nil {
		return err
	}

	fmt.Printf("walked %d keys in %d pages (COUNT %d)", len(seen), pages, pageSize)
	if empties > 0 {
		fmt.Printf(", %d of them empty and none of them the end", empties)
	}
	fmt.Println()

	if err := checkComplete(written, seen); err != nil {
		return err
	}
	fmt.Printf("every one of the %d keys came back exactly once\n", len(written))

	if err := walkWithIterator(ctx, c, written, pageSize); err != nil {
		return err
	}
	if err := walkWithScanAll(ctx, c, written, pageSize); err != nil {
		return err
	}
	if err := abandonAWalk(ctx, c, pageSize); err != nil {
		return err
	}

	return compareWithKeys(ctx, c, len(written))
}

// walkWithIterator is the same walk on the default pool, which is the point:
// the iterator holds one connection for the length of it, so nothing about the
// client's configuration has to be arranged for a scan to be correct.
func walkWithIterator(ctx context.Context, c client.Client, written []string, pageSize int) error {
	it := c.ScanIter(ctx, prefix+"*", pageSize)
	// Free when the walk finishes on its own, and the whole of the cleanup when
	// it does not.
	defer it.Close()

	seen := make(map[string]int, len(written))
	for it.Next() {
		seen[it.Key()]++
	}
	// Checked after the loop, always: a walk that failed and one that finished
	// both stop it, and only this tells them apart.
	if err := it.Err(); err != nil {
		return fmt.Errorf("the walk: %w", err)
	}
	if err := checkComplete(written, seen); err != nil {
		return fmt.Errorf("ScanIter: %w", err)
	}
	fmt.Printf("ScanIter walked the same %d keys on an ordinary pool\n", len(seen))
	return nil
}

// walkWithScanAll is the one-line version of everything above, for a keyspace
// small enough to hold in memory — which is most of them.
func walkWithScanAll(ctx context.Context, c client.Client, written []string, pageSize int) error {
	keys, err := c.ScanAll(ctx, prefix+"*", pageSize)
	if err != nil {
		return fmt.Errorf("ScanAll: %w", err)
	}

	seen := make(map[string]int, len(keys))
	for _, key := range keys {
		seen[key]++
	}
	if err := checkComplete(written, seen); err != nil {
		return fmt.Errorf("ScanAll: %w", err)
	}
	fmt.Printf("ScanAll returned the same %d keys in one call\n", len(keys))
	return nil
}

// abandonAWalk leaves an iterator half-finished, which is the case worth
// showing: the connection it pinned has to come back, or a pool loses one
// connection per abandoned walk until it has none.
func abandonAWalk(ctx context.Context, c client.Client, pageSize int) error {
	it := c.ScanIter(ctx, prefix+"*", pageSize)
	// The whole cleanup, and the reason a break in this loop costs nothing.
	defer it.Close()

	if !it.Next() {
		return fmt.Errorf("an iterator over a seeded keyspace produced no keys: %w", it.Err())
	}
	first := it.Key()
	it.Close()

	// The client is immediately usable, which is what says the connection came
	// back rather than being held by an iterator nobody finished.
	if _, _, err := c.Get(ctx, first); err != nil {
		return fmt.Errorf("a call after an abandoned walk: %w", err)
	}
	fmt.Printf("abandoned a walk after %s and the connection went back to the pool\n", first)
	return nil
}

// seed writes the keys this program then walks.
func seed(ctx context.Context, c client.Client, n int) ([]string, error) {
	written := make([]string, 0, n)
	for i := range n {
		key := prefix + strconv.Itoa(i)
		if err := c.Set(ctx, key, []byte("value"), 10*time.Minute); err != nil {
			return nil, fmt.Errorf("writing %s: %w", key, err)
		}
		written = append(written, key)
	}
	fmt.Printf("wrote %d keys under %s\n", len(written), prefix)
	return written, nil
}

// walk is the whole of the SCAN contract, done by hand with the single-page
// primitive: start at ScanStart, stop on Done, carry the cursor forward, and
// treat an empty page as nothing more than a page that found nothing.
//
// Written out here because it is what ScanAll and ScanIter do for you, and
// because the page API is still the right tool when one page is what is wanted.
// It runs on a client pinned to one connection; on any other, nothing but luck
// keeps consecutive pages on the connection that owns the cursor.
func walk(ctx context.Context, c client.Client, pageSize int) (seen map[string]int, pages, empties int, err error) {
	seen = make(map[string]int)

	for cursor := client.ScanStart; ; {
		result, scanErr := c.Scan(ctx, cursor, prefix+"*", pageSize)
		if scanErr != nil {
			return nil, 0, 0, fmt.Errorf("scanning from cursor %s: %w", cursor, scanErr)
		}
		pages++
		if len(result.Keys) == 0 {
			empties++
		}
		for _, key := range result.Keys {
			seen[key]++
		}

		// Done, and only Done, ends the walk.
		if result.Done() {
			break
		}
		cursor = result.Cursor
	}
	return seen, pages, empties, nil
}

// checkComplete is the guarantee ADR-0017 offers: every key present when the
// scan began is returned exactly once.
func checkComplete(written []string, seen map[string]int) error {
	for _, key := range written {
		switch seen[key] {
		case 1:
		case 0:
			return fmt.Errorf("%s was never returned by the walk", key)
		default:
			return fmt.Errorf("%s was returned %d times", key, seen[key])
		}
	}
	if len(seen) != len(written) {
		return fmt.Errorf("the walk returned %d keys, want %d", len(seen), len(written))
	}
	return nil
}

// compareWithKeys: KEYS answers the same question in one reply, and holds a
// connection — and the server — for the length of the walk. It is a debugging
// tool, not a production one.
func compareWithKeys(ctx context.Context, c client.Client, want int) error {
	keys, err := c.Keys(ctx, prefix+"*")
	if err != nil {
		return fmt.Errorf("KEYS: %w", err)
	}
	if len(keys) != want {
		return fmt.Errorf("KEYS returned %d keys, SCAN returned %d", len(keys), want)
	}
	fmt.Printf("KEYS agrees (%d), in one reply and one long pause for the server\n", len(keys))
	return nil
}

// cleanUp removes what this program wrote, in batches, so a failure here does
// not leave a keyspace full of example data behind.
func cleanUp(c client.Client, keys []string) {
	const batch = 100
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for start := 0; start < len(keys); start += batch {
		end := min(start+batch, len(keys))
		if _, err := c.Del(ctx, keys[start:end]...); err != nil {
			fmt.Fprintln(os.Stderr, "scan: cleaning up:", err)
			return
		}
	}
}
