package storage

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scanAll runs a scan to completion and returns every key it yielded, in order,
// duplicates included. Returning the raw sequence rather than a set is the
// point: the assertions below are about duplicates as much as about coverage.
func scanAll(t *testing.T, engine *ShardedEngine, owner ScanOwner, count int) []string {
	t.Helper()

	var yielded []string
	cursor := ScanCursorStart

	// The bound is a runaway guard, not a limit on the scan: a cursor that never
	// reports completion is the failure mode this whole feature exists to
	// remove, and a test that hangs reports it far less clearly than one that
	// fails.
	for pages := 0; ; pages++ {
		require.Less(t, pages, 10_000_000, "the scan did not terminate")

		keys, next, err := engine.Scan(owner, cursor, count)
		require.NoError(t, err)
		for _, key := range keys {
			yielded = append(yielded, string(key))
		}
		if next == ScanCursorStart {
			return yielded
		}
		cursor = next
	}
}

// assertExactlyOnce is the assertion FEAT-0016 turns on: the union of every page
// equals the true key set, and nothing was handed out twice.
func assertExactlyOnce(t *testing.T, want map[string]struct{}, got []string) {
	t.Helper()

	seen := make(map[string]int, len(got))
	for _, key := range got {
		seen[key]++
	}

	var duplicates, unexpected []string
	for key, times := range seen {
		if times > 1 {
			duplicates = append(duplicates, fmt.Sprintf("%s x%d", key, times))
		}
		if _, ok := want[key]; !ok {
			unexpected = append(unexpected, key)
		}
	}

	var missing []string
	for key := range want {
		if seen[key] == 0 {
			missing = append(missing, key)
		}
	}

	assert.Empty(t, duplicates, "%d keys were returned more than once", len(duplicates))
	assert.Empty(t, missing, "%d keys present at scan start were never returned", len(missing))
	assert.Empty(t, unexpected, "%d keys were returned that were never stored", len(unexpected))
	assert.Len(t, got, len(want), "the scan yielded a different number of keys than the keyspace holds")
}

func fillKeys(t *testing.T, engine *ShardedEngine, prefix string, n int) map[string]struct{} {
	t.Helper()

	want := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		key := prefix + strconv.Itoa(i)
		require.NoError(t, engine.Set([]byte(key), []byte("v"), 0))
		want[key] = struct{}{}
	}

	return want
}

// TestScanReturnsEveryKeyExactlyOnce is the regression test for ISSUE-0011.
//
// The positional cursor it replaces indexed into Shard.Keys(), which rebuilds a
// slice from a Go map on every call — and Go randomizes map iteration order per
// pass, so page two indexed into a different ordering than page one. Keys were
// missed and duplicated at any size, quite apart from the cursor overflowing
// above a million keys per shard.
//
// Page sizes that do and do not divide the keyspace are both covered: an
// off-by-one at a page boundary is exactly the kind of defect the old code hid.
func TestScanReturnsEveryKeyExactlyOnce(t *testing.T) {
	cases := []struct {
		name   string
		shards int
		keys   int
		count  int
	}{
		{"one shard, page divides the keyspace", 1, 1000, 10},
		{"one shard, page does not divide the keyspace", 1, 1000, 7},
		{"many shards, small pages", 64, 5000, 10},
		{"many shards, page larger than most shards", 64, 500, 100},
		{"page larger than the whole keyspace", 8, 50, 1000},
		{"single-key pages", 16, 200, 1},
		{"empty keyspace", 16, 0, 10},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			engine := NewShardedEngine(EngineConfig{ShardCount: tc.shards, MaxValueSize: 1024})
			defer engine.Close()

			want := fillKeys(t, engine, "scan:", tc.keys)
			got := scanAll(t, engine, 1, tc.count)

			assertExactlyOnce(t, want, got)
			t.Logf("shards=%d keys=%d count=%d -> %d yielded, %d unique",
				tc.shards, tc.keys, tc.count, len(got), len(want))
		})
	}
}

// TestScanAboveOneMillionKeysInOneShard is the other half of ISSUE-0011: the old
// cursor packed shard index and position with a decimal constant, so a shard
// past 1,000,000 live keys overflowed its position into the shard field and the
// cursor silently pointed at the wrong shard. There is no positional encoding
// left to overflow, and this asserts the size it used to break at.
func TestScanAboveOneMillionKeysInOneShard(t *testing.T) {
	if testing.Short() {
		t.Skip("holds more than a million entries in memory")
	}

	const keys = 1_000_001

	engine := NewShardedEngine(EngineConfig{ShardCount: 1, MaxValueSize: 1024})
	defer engine.Close()

	want := make(map[string]struct{}, keys)
	for i := 0; i < keys; i++ {
		key := "k:" + strconv.Itoa(i)
		require.NoError(t, engine.Set([]byte(key), nil, 0))
		want[key] = struct{}{}
	}
	require.Equal(t, 1, len(engine.GetAllShards()), "every key must land in one shard for this to test anything")
	require.Equal(t, uint64(keys), engine.Stats().Keys)

	got := scanAll(t, engine, 1, maxScanCount)

	assertExactlyOnce(t, want, got)
	t.Logf("scanned %d keys in one shard: %d yielded, %d unique, 0 duplicates", keys, len(got), len(want))
}

// TestScanWhileWritingAndDeleting asserts the guarantee's exact shape under
// concurrent mutation: keys untouched throughout appear exactly once, keys
// deleted mid-scan never reappear after their deletion, and keys created
// mid-scan are allowed to appear or not — but not twice either way.
func TestScanWhileWritingAndDeleting(t *testing.T) {
	const stable = 2000

	engine := NewShardedEngine(EngineConfig{ShardCount: 16, MaxValueSize: 1024})
	defer engine.Close()

	untouched := fillKeys(t, engine, "stable:", stable)

	// A separate population the writer churns through, so the scan sees real
	// insertions and deletions rather than a static map.
	for i := 0; i < 500; i++ {
		require.NoError(t, engine.Set([]byte("churn:"+strconv.Itoa(i)), []byte("v"), 0))
	}

	stop := make(chan struct{})
	failures := make(chan error, 1)
	var writers sync.WaitGroup
	writers.Add(2)

	go func() {
		defer writers.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := engine.Set([]byte("fresh:"+strconv.Itoa(i)), []byte("v"), 0); err != nil {
				failures <- err
				return
			}
		}
	}()
	go func() {
		defer writers.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			engine.Delete([]byte("churn:" + strconv.Itoa(i%500)))
		}
	}()

	got := scanAll(t, engine, 1, 10)

	close(stop)
	writers.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err, "the background writer failed")
	}

	seen := make(map[string]int, len(got))
	for _, key := range got {
		seen[key]++
	}

	var duplicates []string
	for key, times := range seen {
		if times > 1 {
			duplicates = append(duplicates, key)
		}
	}
	assert.Empty(t, duplicates, "no key may be returned twice, however the keyspace churned")

	var missing []string
	for key := range untouched {
		if seen[key] != 1 {
			missing = append(missing, key)
		}
	}
	assert.Empty(t, missing, "%d keys that were present throughout were missed", len(missing))

	t.Logf("%d keys yielded during concurrent writes and deletes, %d of them the untouched set, 0 duplicates",
		len(got), stable-len(missing))
}

// TestScanFiltersKeysRemovedAfterTheSnapshot covers the read-time filter. Both
// halves matter and they take different paths: a delete removes the map entry,
// while an expiry may leave it resident until something reclaims it, and a scan
// must treat the second as gone just as firmly as the first.
func TestScanFiltersKeysRemovedAfterTheSnapshot(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{ShardCount: 1, MaxValueSize: 1024, DisableLazyExpiration: true})
	defer engine.Close()

	for i := 0; i < 30; i++ {
		require.NoError(t, engine.Set([]byte("live:"+strconv.Itoa(i)), []byte("v"), 0))
	}
	for i := 0; i < 30; i++ {
		require.NoError(t, engine.Set([]byte("doomed:"+strconv.Itoa(i)), []byte("v"), 0))
	}
	for i := 0; i < 30; i++ {
		require.NoError(t, engine.Set([]byte("dying:"+strconv.Itoa(i)), []byte("v"), 50*time.Millisecond))
	}

	// One page fixes the snapshot; everything else is removed after it is taken.
	first, cursor, err := engine.Scan(1, ScanCursorStart, 5)
	require.NoError(t, err)
	require.NotEqual(t, ScanCursorStart, cursor)

	for i := 0; i < 30; i++ {
		engine.Delete([]byte("doomed:" + strconv.Itoa(i)))
	}
	time.Sleep(80 * time.Millisecond)

	// Pages after the removals are held to the strict rule: nothing removed may
	// appear in them. The first page is excluded because it was handed out
	// before anything was removed, so a doomed key in it is correct, not a leak.
	var laterPages []string
	for {
		keys, next, scanErr := engine.Scan(1, cursor, 5)
		require.NoError(t, scanErr)
		for _, key := range keys {
			laterPages = append(laterPages, string(key))
		}
		if next == ScanCursorStart {
			break
		}
		cursor = next
	}

	var leakedDeleted, leakedExpired []string
	for _, key := range laterPages {
		switch {
		case strings.HasPrefix(key, "doomed:"):
			leakedDeleted = append(leakedDeleted, key)
		case strings.HasPrefix(key, "dying:"):
			leakedExpired = append(leakedExpired, key)
		}
	}
	assert.Empty(t, leakedDeleted, "keys deleted after the snapshot must not appear in later pages")
	assert.Empty(t, leakedExpired, "keys expired after the snapshot must not appear in later pages")

	// And the filter must not have thrown out the live keys with them.
	live := make(map[string]struct{}, 30)
	for _, key := range first {
		live[string(key)] = struct{}{}
	}
	for _, key := range laterPages {
		live[key] = struct{}{}
	}
	for i := 0; i < 30; i++ {
		assert.Contains(t, live, "live:"+strconv.Itoa(i), "a key that was never removed must still be returned")
	}
}

// TestScanCursorExpiresWhenIdle covers the first of the three bounds. The clock
// is injected rather than slept through: a 60 second sleep in a unit test is not
// a test, and the behavior under test is a comparison against a deadline.
func TestScanCursorExpiresWhenIdle(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{ShardCount: 4, MaxValueSize: 1024, ScanIdleTimeout: time.Minute})
	defer engine.Close()

	fillKeys(t, engine, "k:", 100)

	now := time.Now()
	engine.scans.now = func() time.Time { return now }

	_, cursor, err := engine.Scan(1, ScanCursorStart, 5)
	require.NoError(t, err)
	require.NotEqual(t, ScanCursorStart, cursor)
	require.Equal(t, uint64(1), engine.Stats().ScanCursors)

	t.Run("a cursor still within the window survives", func(t *testing.T) {
		now = now.Add(59 * time.Second)

		_, next, pageErr := engine.Scan(1, cursor, 5)
		require.NoError(t, pageErr)
		cursor = next
	})

	t.Run("an idle cursor is dropped and then errors clearly", func(t *testing.T) {
		now = now.Add(2 * time.Minute)

		keys, next, pageErr := engine.Scan(1, cursor, 5)
		assert.ErrorIs(t, pageErr, ErrScanCursorUnknown)
		assert.Nil(t, keys, "an expired cursor must not return partial results")
		assert.Equal(t, ScanCursorStart, next)
	})

	t.Run("the dropped snapshot's memory is given back", func(t *testing.T) {
		stats := engine.Stats()
		assert.Equal(t, uint64(0), stats.ScanCursors)
		assert.Equal(t, uint64(0), stats.ScanSnapshotBytes)
	})
}

// TestScanPerConnectionCursorCap covers the second bound. Without it a single
// connection can open scans until the process dies.
func TestScanPerConnectionCursorCap(t *testing.T) {
	const cap = 3

	engine := NewShardedEngine(EngineConfig{ShardCount: 4, MaxValueSize: 1024, MaxScanCursorsPerConn: cap})
	defer engine.Close()

	fillKeys(t, engine, "k:", 200)

	open := make([]string, 0, cap)
	for i := 0; i < cap; i++ {
		_, cursor, err := engine.Scan(1, ScanCursorStart, 1)
		require.NoError(t, err)
		require.NotEqual(t, ScanCursorStart, cursor)
		open = append(open, cursor)
	}

	_, _, err := engine.Scan(1, ScanCursorStart, 1)
	assert.ErrorIs(t, err, ErrTooManyScanCursors)

	t.Run("the cap is per connection, not global", func(t *testing.T) {
		_, cursor, otherErr := engine.Scan(2, ScanCursorStart, 1)
		require.NoError(t, otherErr)
		assert.NotEqual(t, ScanCursorStart, cursor)
	})

	t.Run("finishing a scan frees its slot", func(t *testing.T) {
		// Drain the first cursor to completion, which drops it.
		cursor := open[0]
		for cursor != ScanCursorStart {
			_, next, pageErr := engine.Scan(1, cursor, maxScanCount)
			require.NoError(t, pageErr)
			cursor = next
		}

		_, fresh, freshErr := engine.Scan(1, ScanCursorStart, 1)
		require.NoError(t, freshErr)
		assert.NotEqual(t, ScanCursorStart, fresh)
	})
}

// TestScanRejectsCursorsItDidNotIssue is the "never silently restart" rule. A
// scan that quietly resets on an unknown cursor hands the caller duplicates it
// cannot detect, so every one of these must be an error instead.
func TestScanRejectsCursorsItDidNotIssue(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{ShardCount: 4, MaxValueSize: 1024})
	defer engine.Close()

	fillKeys(t, engine, "k:", 100)

	t.Run("an id that was never issued", func(t *testing.T) {
		keys, next, err := engine.Scan(1, "deadbeefdeadbeefdeadbeefdeadbeef", 10)
		assert.ErrorIs(t, err, ErrScanCursorUnknown)
		assert.Nil(t, keys)
		assert.Equal(t, ScanCursorStart, next)
	})

	t.Run("a malformed id", func(t *testing.T) {
		_, _, err := engine.Scan(1, "not-a-cursor", 10)
		assert.ErrorIs(t, err, ErrScanCursorUnknown)
	})

	t.Run("another connection's cursor", func(t *testing.T) {
		_, cursor, err := engine.Scan(1, ScanCursorStart, 5)
		require.NoError(t, err)

		keys, next, err := engine.Scan(2, cursor, 5)
		assert.ErrorIs(t, err, ErrScanCursorUnknown,
			"a cursor is scoped to the connection that created it")
		assert.Nil(t, keys)
		assert.Equal(t, ScanCursorStart, next)

		// And the owner still holds it: rejecting the impostor must not have
		// dropped the real scan.
		_, next, err = engine.Scan(1, cursor, 5)
		require.NoError(t, err)
		assert.NotEqual(t, ScanCursorStart, next)
	})

	t.Run("a cursor whose scan has already finished", func(t *testing.T) {
		cursor := ScanCursorStart
		var last string
		for {
			_, next, err := engine.Scan(3, cursor, 10)
			require.NoError(t, err)
			if next == ScanCursorStart {
				break
			}
			last = next
			cursor = next
		}
		require.NotEmpty(t, last)

		_, _, err := engine.Scan(3, last, 10)
		assert.ErrorIs(t, err, ErrScanCursorUnknown,
			"an exhausted cursor must error rather than start the scan over")
	})
}

// TestScanCursorIDsAreUnguessable checks the property, not the implementation:
// ids must not be sequential, repeated, or derived from anything a client can
// see. A predictable id plus the connection check would still let a client walk
// its own finished cursors back into existence.
func TestScanCursorIDsAreUnguessable(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{ShardCount: 4, MaxValueSize: 1024, MaxScanCursorsPerConn: 64})
	defer engine.Close()

	fillKeys(t, engine, "k:", 50)

	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		_, cursor, err := engine.Scan(ScanOwner(i%4), ScanCursorStart, 1)
		require.NoError(t, err)

		assert.Len(t, cursor, 2*cursorIDBytes, "an id carries %d bytes of entropy", cursorIDBytes)
		assert.NotEqual(t, ScanCursorStart, cursor)
		assert.NotContains(t, seen, cursor, "cursor ids must not repeat")
		seen[cursor] = struct{}{}
	}
}

// TestScanSnapshotMemoryIsNotChargedToMaxMemory is the accounting rule from
// ADR-0017. Charging snapshots to the data budget would let a large scan evict
// the very keys it is scanning — the failure would look like data loss under
// read load, which is about the worst shape a bug can take.
func TestScanSnapshotMemoryIsNotChargedToMaxMemory(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{
		ShardCount:   1,
		MaxValueSize: 1024,
		MaxMemory:    1 << 20,
	})
	defer engine.Close()

	engine.SetEvictionController(evictEverything{})

	fillKeys(t, engine, "key-with-a-reasonable-length:", 2000)

	before := engine.Stats()
	require.Equal(t, uint64(2000), before.Keys)

	_, cursor, err := engine.Scan(1, ScanCursorStart, 1)
	require.NoError(t, err)
	require.NotEqual(t, ScanCursorStart, cursor)

	after := engine.Stats()

	assert.Equal(t, before.MemoryUsed, after.MemoryUsed,
		"a snapshot must not move the data memory counter")
	assert.Equal(t, before.Evictions, after.Evictions,
		"a scan must not evict the keys it is scanning")
	assert.Equal(t, before.Keys, after.Keys)
	assert.Positive(t, after.ScanSnapshotBytes, "snapshot memory is tracked, just separately")
	assert.Equal(t, uint64(1), after.ScanCursors)

	assertAccounting(t, engine)
}

// evictEverything is a controller that will take any key offered to it. It makes
// the test above fail loudly if snapshot bytes ever reach the memory tracker:
// with a real policy a marginal overshoot might evict nothing.
type evictEverything struct{}

func (evictEverything) SelectVictims(shard *Shard, _ uint64) []string {
	var victims []string
	shard.ForEach(func(key string, _ *Entry) bool {
		victims = append(victims, key)
		return len(victims) < 8
	})
	return victims
}

// TestScanGlobalSnapshotMemoryCap covers the third bound, and the part of it
// that is easy to get wrong: the evicted cursor must fail, not quietly resume.
func TestScanGlobalSnapshotMemoryCap(t *testing.T) {
	// 200 keys of 12 bytes account for 128 + 200*(16+12) = 5728 bytes, so the
	// cap below admits exactly one snapshot at a time.
	const keys = 200

	engine := NewShardedEngine(EngineConfig{
		ShardCount:           1,
		MaxValueSize:         1024,
		MaxScanSnapshotBytes: 8000,
	})
	defer engine.Close()

	for i := 0; i < keys; i++ {
		require.NoError(t, engine.Set([]byte(fmt.Sprintf("key:%08d", i)), []byte("v"), 0))
	}

	_, first, err := engine.Scan(1, ScanCursorStart, 1)
	require.NoError(t, err)
	firstBytes := engine.Stats().ScanSnapshotBytes
	require.Positive(t, firstBytes)

	_, second, err := engine.Scan(2, ScanCursorStart, 1)
	require.NoError(t, err)
	require.NotEqual(t, ScanCursorStart, second)

	t.Run("the oldest cursor was evicted to make room", func(t *testing.T) {
		stats := engine.Stats()
		assert.Equal(t, uint64(1), stats.ScanCursors)
		assert.LessOrEqual(t, stats.ScanSnapshotBytes, uint64(8000), "the cap is a hard bound")
	})

	t.Run("the evicted cursor errors rather than restarting", func(t *testing.T) {
		pageKeys, next, pageErr := engine.Scan(1, first, 10)
		assert.ErrorIs(t, pageErr, ErrScanCursorUnknown)
		assert.Nil(t, pageKeys)
		assert.Equal(t, ScanCursorStart, next)
	})

	t.Run("the surviving cursor still works", func(t *testing.T) {
		_, next, pageErr := engine.Scan(2, second, 10)
		require.NoError(t, pageErr)
		assert.NotEqual(t, ScanCursorStart, next)
	})
}

// TestScanSnapshotLargerThanTheCapIsRejected is the case the cap cannot resolve
// by evicting: one shard alone needs more than the whole budget. It is reported
// rather than admitted, because exceeding the cap is how a scan becomes an
// outage — and rather than truncated, because a truncated snapshot silently
// breaks exactly-once.
func TestScanSnapshotLargerThanTheCapIsRejected(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{
		ShardCount:           1,
		MaxValueSize:         1024,
		MaxScanSnapshotBytes: 1024,
	})
	defer engine.Close()

	fillKeys(t, engine, "key:", 500)

	keys, next, err := engine.Scan(1, ScanCursorStart, 10)
	assert.ErrorIs(t, err, ErrScanMemoryExhausted)
	assert.Nil(t, keys)
	assert.Equal(t, ScanCursorStart, next)

	stats := engine.Stats()
	assert.Equal(t, uint64(0), stats.ScanCursors, "a cursor that cannot proceed must not hold a slot")
	assert.Equal(t, uint64(0), stats.ScanSnapshotBytes)

	// A cap below the per-cursor overhead refuses the scan at the door rather
	// than admitting a cursor it can never feed. The bound has to hold at both
	// ends or it is not a bound.
	t.Run("a cap smaller than one cursor refuses to open one", func(t *testing.T) {
		tiny := NewShardedEngine(EngineConfig{
			ShardCount:           1,
			MaxValueSize:         1024,
			MaxScanSnapshotBytes: scanCursorOverhead - 1,
		})
		defer tiny.Close()

		fillKeys(t, tiny, "k:", 10)

		_, cursor, openErr := tiny.Scan(1, ScanCursorStart, 10)
		assert.ErrorIs(t, openErr, ErrScanMemoryExhausted)
		assert.Equal(t, ScanCursorStart, cursor)
		assert.Equal(t, uint64(0), tiny.Stats().ScanCursors)
	})
}

// TestConcurrentScansDoNotInterfere runs several scans over one shard at once.
// Each holds its own snapshot, so the pages one takes must not move another's
// position — the defect a shared or positional cursor would have.
func TestConcurrentScansDoNotInterfere(t *testing.T) {
	const (
		scanners = 8
		keys     = 2000
	)

	engine := NewShardedEngine(EngineConfig{ShardCount: 1, MaxValueSize: 1024})
	defer engine.Close()

	want := fillKeys(t, engine, "shared:", keys)

	results := make([][]string, scanners)
	var wg sync.WaitGroup
	wg.Add(scanners)
	for i := 0; i < scanners; i++ {
		go func(id int) {
			defer wg.Done()
			results[id] = scanAll(t, engine, ScanOwner(id), 13)
		}(i)
	}
	wg.Wait()

	for i, got := range results {
		t.Run(fmt.Sprintf("scanner-%d", i), func(t *testing.T) {
			assertExactlyOnce(t, want, got)
		})
	}

	stats := engine.Stats()
	assert.Equal(t, uint64(0), stats.ScanCursors, "every completed scan releases its cursor")
	assert.Equal(t, uint64(0), stats.ScanSnapshotBytes)
}

// TestReleaseScansDropsAConnectionsCursors covers connection teardown. An
// abandoned scan has nobody left to finish it, and holding its snapshot until
// the idle timeout is memory spent on a connection that cannot come back.
func TestReleaseScansDropsAConnectionsCursors(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{ShardCount: 4, MaxValueSize: 1024})
	defer engine.Close()

	fillKeys(t, engine, "k:", 300)

	var mine []string
	for i := 0; i < 3; i++ {
		_, cursor, err := engine.Scan(1, ScanCursorStart, 1)
		require.NoError(t, err)
		mine = append(mine, cursor)
	}
	_, theirs, err := engine.Scan(2, ScanCursorStart, 1)
	require.NoError(t, err)

	require.Equal(t, uint64(4), engine.Stats().ScanCursors)

	engine.ReleaseScans(1)

	stats := engine.Stats()
	assert.Equal(t, uint64(1), stats.ScanCursors, "only the released connection's cursors go")

	for _, cursor := range mine {
		_, _, pageErr := engine.Scan(1, cursor, 10)
		assert.ErrorIs(t, pageErr, ErrScanCursorUnknown)
	}

	_, _, err = engine.Scan(2, theirs, 10)
	assert.NoError(t, err, "another connection's scan is untouched")

	t.Run("releasing an unknown connection is a no-op", func(t *testing.T) {
		engine.ReleaseScans(99)
		assert.Equal(t, uint64(1), engine.Stats().ScanCursors)
	})
}

// TestCloseDropsEveryCursor: shutdown must not leave snapshots behind, and a
// cursor must not outlive the engine that could answer it.
func TestCloseDropsEveryCursor(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{ShardCount: 4, MaxValueSize: 1024})

	fillKeys(t, engine, "k:", 100)
	_, cursor, err := engine.Scan(1, ScanCursorStart, 1)
	require.NoError(t, err)
	require.Equal(t, uint64(1), engine.Stats().ScanCursors)

	require.NoError(t, engine.Close())

	stats := engine.Stats()
	assert.Equal(t, uint64(0), stats.ScanCursors)
	assert.Equal(t, uint64(0), stats.ScanSnapshotBytes)

	_, _, err = engine.Scan(1, cursor, 10)
	assert.ErrorIs(t, err, ErrEngineClosed)
}

// TestScanCountBounds covers the two ends of the page-size argument. count is
// how much work a page does, and both a zero and an enormous one have to land
// somewhere sane rather than doing none or all of it.
func TestScanCountBounds(t *testing.T) {
	engine := NewShardedEngine(EngineConfig{ShardCount: 1, MaxValueSize: 1024})
	defer engine.Close()

	// More keys than maxScanCount, so a clamped page is visibly shorter than the
	// count asked for rather than merely shorter than the keyspace.
	fillKeys(t, engine, "k:", maxScanCount+5000)

	t.Run("a non-positive count falls back to the default", func(t *testing.T) {
		for _, count := range []int{0, -1} {
			keys, cursor, err := engine.Scan(ScanOwner(1), ScanCursorStart, count)
			require.NoError(t, err)
			assert.Len(t, keys, defaultScanCount)
			assert.NotEqual(t, ScanCursorStart, cursor)
			engine.ReleaseScans(1)
		}
	})

	t.Run("an enormous count is clamped rather than honored", func(t *testing.T) {
		keys, _, err := engine.Scan(2, ScanCursorStart, 1<<30)
		require.NoError(t, err)
		assert.Len(t, keys, maxScanCount, "one page may not walk an unbounded number of entries")
	})
}

// TestScanCursorIDGeneratorFailures covers the paths that only a broken random
// source reaches. They are worth holding: silently issuing a predictable id
// would be worse than refusing to scan.
func TestScanCursorIDGeneratorFailures(t *testing.T) {
	boom := errors.New("no entropy")

	t.Run("a failing generator is reported", func(t *testing.T) {
		engine := NewShardedEngine(EngineConfig{ShardCount: 1, MaxValueSize: 1024})
		defer engine.Close()

		engine.scans.newID = func() (string, error) { return "", boom }

		_, _, err := engine.Scan(1, ScanCursorStart, 10)
		assert.ErrorIs(t, err, boom)
	})

	t.Run("a generator that only ever returns one id gives up", func(t *testing.T) {
		engine := NewShardedEngine(EngineConfig{ShardCount: 1, MaxValueSize: 1024, MaxScanCursorsPerConn: 4})
		defer engine.Close()

		fillKeys(t, engine, "k:", 20)
		engine.scans.newID = func() (string, error) { return "always-the-same", nil }

		_, first, err := engine.Scan(1, ScanCursorStart, 1)
		require.NoError(t, err)
		assert.Equal(t, "always-the-same", first)

		_, _, err = engine.Scan(1, ScanCursorStart, 1)
		assert.Error(t, err, "a colliding id must not overwrite a live cursor")
		assert.NotErrorIs(t, err, ErrScanCursorUnknown)
	})
}

// TestRandomCursorIDShape guards the generator itself: 128 bits, hex, distinct.
func TestRandomCursorIDShape(t *testing.T) {
	seen := make(map[string]struct{}, 128)
	for i := 0; i < 128; i++ {
		id, err := randomCursorID()
		require.NoError(t, err)
		assert.Len(t, id, 2*cursorIDBytes)
		assert.NotContains(t, seen, id)
		seen[id] = struct{}{}
	}
}

// TestSnapshotBytesAccounting checks the arithmetic the cap depends on. An
// understated snapshot size makes the cap advisory, which is the same as not
// having one.
func TestSnapshotBytesAccounting(t *testing.T) {
	assert.Equal(t, uint64(0), snapshotBytes(nil))
	assert.Equal(t, uint64(scanKeyOverhead+3), snapshotBytes([]string{"abc"}))
	assert.Equal(t, uint64(2*scanKeyOverhead+5), snapshotBytes([]string{"abc", "de"}))
}
