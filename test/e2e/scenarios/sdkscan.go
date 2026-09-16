package scenarios

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/b3vet/atlascache/pkg/client"
	"github.com/b3vet/atlascache/test/e2e/runner"
)

// Walking the keyspace through the SDK, against the server whose cursors make
// it delicate.
//
// A scan cursor names a snapshot the server holds for the connection that asked
// for it (ADR-0017). The SDK hides connections behind a pool, so a walk that did
// not hold one would fetch its second page over whichever connection happened to
// be free — and be answered "invalid cursor", or silently handed a fraction of
// the keyspace. Before ISSUE-0023 the SDK left that to the caller, and every
// caller "solved" it the same way: a pool of one connection.
//
// These run on the default pool, with other callers using it at the same time,
// because that is the arrangement the workaround was hiding.

func init() {
	runner.RegisterScenario("sdk_scan_walks_a_shared_pool", sdkScanWalksASharedPool)
	runner.RegisterScenario("sdk_scan_reports_a_broken_walk", sdkScanReportsABrokenWalk)
}

const (
	// scanKeys is how many keys a walk covers, and scanPage how much work one
	// page may do. Their ratio is what matters: several pages, so that a client
	// which loses the cursor after the first one is visibly wrong rather than
	// accidentally complete.
	scanKeys = 60
	scanPage = 4

	// scanPrefix keeps this scenario's keys out of the way of every other spec
	// running against the same server.
	scanPrefix = "sdk:scan:"

	// scanTraffic is how many other callers use the same client during the
	// walk. They exist to keep the pool's connections moving, which is what
	// makes "the walk held its own" mean something.
	scanTraffic = 4
)

// sdkScanWalksASharedPool is the case ISSUE-0023 exists for: a multi-page scan
// that is correct without the caller pinning the pool to one connection.
func sdkScanWalksASharedPool(c *runner.Ctx) error {
	sdk, err := sdkClient(c)
	if err != nil {
		return err
	}
	defer func() { _ = sdk.Close() }()

	want, err := seedScanKeys(c, sdk)
	if err != nil {
		return err
	}

	// Other callers on the same client, for the length of the walk. Each takes
	// a connection and gives it back, so the pool is churning underneath the
	// iterator rather than sitting idle with one warm connection to hand back.
	stop := make(chan struct{})
	var traffic sync.WaitGroup
	failures := make(chan error, scanTraffic)
	for worker := range scanTraffic {
		traffic.Add(1)
		go func() {
			defer traffic.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				key := fmt.Sprintf("%sbusy:%d", scanPrefix, worker)
				if setErr := sdk.Set(c.Context(), key, []byte("v"), time.Minute); setErr != nil {
					failures <- setErr
					return
				}
			}
		}()
	}

	seen := map[string]int{}
	it := sdk.ScanIter(c.Context(), scanPrefix+"walk:*", scanPage)
	pages := 0
	for it.Next() {
		seen[it.Key()]++
		pages++
	}
	it.Close()
	close(stop)
	traffic.Wait()
	close(failures)

	if err := <-failures; err != nil {
		return fmt.Errorf("a caller sharing the pool with the walk failed: %w", err)
	}
	if err := it.Err(); err != nil {
		return fmt.Errorf("a walk over a pool shared with %d other callers failed: %w", scanTraffic, err)
	}
	if err := checkSeen(seen, want); err != nil {
		return err
	}
	c.Logf("a %d-key walk over a shared default-sized pool returned every key exactly once", len(want))

	return sdkScanHoldsItsConnection(c, sdk, want)
}

// sdkScanHoldsItsConnection is the same claim made deterministically.
//
// A pool hands back the connection it was most recently given, so a walk that
// released one between pages usually gets the same one and looks correct — which
// is how this defect survived long enough to be written down. An idle timeout
// of a nanosecond retires every connection the moment it is returned, so a page
// fetched on a connection the walk did not hold is refused every run.
func sdkScanHoldsItsConnection(c *runner.Ctx, seeded client.Client, want map[string]bool) error {
	churning, err := sdkClient(c, client.WithMaxIdleTime(time.Nanosecond))
	if err != nil {
		return err
	}
	defer func() { _ = churning.Close() }()

	keys, err := churning.ScanAll(c.Context(), scanPrefix+"walk:*", scanPage)
	if err != nil {
		return fmt.Errorf("a walk on a client that retires every idle connection: %w", err)
	}

	seen := map[string]int{}
	for _, key := range keys {
		seen[key]++
	}
	if err := checkSeen(seen, want); err != nil {
		return err
	}
	c.Logf("the walk kept its connection through a pool that retires idle ones")

	return sdkScanReleasesAnAbandonedWalk(c, seeded)
}

// sdkScanReleasesAnAbandonedWalk checks the other half of pinning: a connection
// held for a walk has to come back, including from a walk nobody finished.
//
// The pool here holds one connection, so a leak is not a statistic — the next
// call has nothing to run on and waits out its own deadline.
func sdkScanReleasesAnAbandonedWalk(c *runner.Ctx, seeded client.Client) error {
	single, err := sdkClient(c, client.WithPoolSize(1))
	if err != nil {
		return err
	}
	defer func() { _ = single.Close() }()

	it := single.ScanIter(c.Context(), scanPrefix+"walk:*", scanPage)
	if !it.Next() {
		return fmt.Errorf("a walk over a seeded keyspace produced no keys: %w", it.Err())
	}
	it.Close()

	if pingErr := single.Ping(c.Context()); pingErr != nil {
		return fmt.Errorf("a call after an abandoned walk was closed: %w", pingErr)
	}
	c.Logf("an abandoned walk gave its connection back on Close")

	// And the keys are still there afterwards: an abandoned walk leaves its
	// snapshot to expire server-side (ADR-0017) and changes nothing else.
	size, err := seeded.DBSize(c.Context())
	if err != nil {
		return fmt.Errorf("DBSIZE after an abandoned walk: %w", err)
	}
	if size < scanKeys {
		return fmt.Errorf("DBSIZE answered %d after a walk over %d keys", size, scanKeys)
	}
	return nil
}

// sdkScanReportsABrokenWalk checks that a walk whose connection dies says so.
//
// Silent truncation is the failure worth designing against: a caller that asked
// for every key and was handed some of them, with no error, cannot tell. The
// relay is how the connection is taken away mid-walk — a correct server never
// does it, and the SDK's answer to it is the whole point.
func sdkScanReportsABrokenWalk(c *runner.Ctx) error {
	relay, err := newProxy(func() string { return c.Info().ClientAddr })
	if err != nil {
		return err
	}
	defer relay.close()

	sdk, err := sdkClient(c, client.WithAddr(relay.addr()), client.WithReconnectWindow(0))
	if err != nil {
		return err
	}
	defer func() { _ = sdk.Close() }()

	seeder, err := sdkClient(c)
	if err != nil {
		return err
	}
	defer func() { _ = seeder.Close() }()
	if _, seedErr := seedScanKeys(c, seeder); seedErr != nil {
		return seedErr
	}

	it := sdk.ScanIter(c.Context(), scanPrefix+"walk:*", scanPage)
	defer it.Close()

	if !it.Next() {
		return fmt.Errorf("the first page produced no keys: %w", it.Err())
	}

	// Everything the client had, gone: what a server restart, a middlebox or a
	// killed connection does in the middle of a walk.
	relay.close()

	seen := 1
	for it.Next() {
		seen++
	}

	err = it.Err()
	if err == nil {
		return fmt.Errorf("a walk that lost its connection after %d of %d keys reported no error at all",
			seen, scanKeys)
	}
	if !errors.Is(err, client.ErrNetwork) && !errors.Is(err, client.ErrTimeout) {
		return fmt.Errorf("a walk that lost its connection answered %v, want a network failure", err)
	}
	if seen >= scanKeys {
		return fmt.Errorf("a walk that lost its connection returned all %d keys; it cannot have", seen)
	}
	c.Logf("a walk interrupted after %d of %d keys failed rather than reporting a short keyspace", seen, scanKeys)
	return nil
}

// seedScanKeys writes the keys a walk is measured against.
func seedScanKeys(c *runner.Ctx, sdk client.Client) (map[string]bool, error) {
	want := make(map[string]bool, scanKeys)
	for i := range scanKeys {
		key := fmt.Sprintf("%swalk:%03d", scanPrefix, i)
		if err := sdk.Set(c.Context(), key, []byte("v"), time.Hour); err != nil {
			return nil, fmt.Errorf("seeding %s: %w", key, err)
		}
		want[key] = true
	}
	return want, nil
}

// checkSeen is the guarantee ADR-0017 offers, as an assertion: every key that
// was there when the scan began, returned exactly once.
func checkSeen(seen map[string]int, want map[string]bool) error {
	for key, count := range seen {
		if !want[key] {
			return fmt.Errorf("the walk returned %q, which it was not asked for", key)
		}
		if count != 1 {
			return fmt.Errorf("the walk returned %q %d times; a scan returns each key once", key, count)
		}
	}
	for key := range want {
		if seen[key] == 0 {
			return fmt.Errorf("the walk saw %d of %d keys; %q is missing", len(seen), len(want), key)
		}
	}
	return nil
}
