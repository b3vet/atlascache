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
	// A pool of one, because the SDK does not yet pin a connection for the
	// length of a walk and a cursor is only valid on the connection that
	// issued it. ISSUE-0023 tracks the iterator that will make this
	// unnecessary; until it lands, this is the setting that makes a multi-page
	// scan correct rather than lucky.
	c, err := client.New(client.WithAddr(addr), client.WithPoolSize(1))
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

	seen, pages, empties, err := walk(ctx, c, pageSize)
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

	return compareWithKeys(ctx, c, len(written))
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

// walk is the whole of the SCAN contract: start at ScanStart, stop on Done,
// carry the cursor forward, and treat an empty page as nothing more than a page
// that found nothing.
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
