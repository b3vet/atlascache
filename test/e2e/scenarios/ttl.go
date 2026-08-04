package scenarios

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/b3vet/atlascache/test/e2e/client"
	"github.com/b3vet/atlascache/test/e2e/runner"
)

func init() {
	runner.RegisterScenario("ttl_extension_outlives_its_hint", ttlExtensionOutlivesItsHint)
	runner.RegisterScenario("ttl_reclaims_memory", ttlReclaimsMemory)
	runner.RegisterScenario("ttl_reclaims_memory_through_higher_levels", ttlReclaimsMemoryThroughHigherLevels)
	runner.RegisterScenario("ttl_concurrent_expire_and_get", ttlConcurrentExpireAndGet)
	runner.RegisterScenario("ttl_churn_holds_steady_memory", ttlChurnHoldsSteadyMemory)
}

// ttlExtensionOutlivesItsHint is the stale-hint path of ADR-0016.
//
// A key written with a short TTL leaves a hint in the wheel. Extending the TTL
// does not remove that hint — by design, since there is no Remove — so the hint
// fires on the original schedule against a key that is now long-lived. The
// manager must re-read the entry and drop the hint. Acting on the hint instead
// would expire live data, which is the failure this scenario exists to catch.
//
// The control key is what keeps the test honest: it is written with the same
// short TTL and never extended, so if the wheel were not firing at all during
// the window, the extended key would survive for the wrong reason.
func ttlExtensionOutlivesItsHint(c *runner.Ctx) error {
	const extended = "hint:extended"
	const control = "hint:control"

	if err := expectOK(c, "SET %s survivor PX 400", extended); err != nil {
		return err
	}
	if err := expectOK(c, "SET %s doomed PX 400", control); err != nil {
		return err
	}

	if err := c.Sleep(150 * time.Millisecond); err != nil {
		return err
	}

	// The extension lands before the original deadline, so the wheel is now
	// holding a hint that says this key died at 400ms.
	if err := expectInteger(c, 1, "EXPIRE %s 30", extended); err != nil {
		return err
	}
	c.Logf("extended %s to 30s, 250ms before its original deadline", extended)

	// Well past the original deadline: the hint has fired by now.
	if err := c.Sleep(900 * time.Millisecond); err != nil {
		return err
	}

	if err := expectBulk(c, "survivor", "GET %s", extended); err != nil {
		return fmt.Errorf("the extended key was expired by its stale hint: %w", err)
	}
	if err := expectNil(c, "GET %s", control); err != nil {
		return fmt.Errorf("the control key outlived its TTL, so the wheel was not firing: %w", err)
	}

	remaining, err := ttlOf(c, extended)
	if err != nil {
		return err
	}
	if remaining < 25 || remaining > 30 {
		return fmt.Errorf("TTL %s = %d, want the extended expiry, between 25 and 30", extended, remaining)
	}
	c.Logf("%s survived with %ds left; %s expired on schedule", extended, remaining, control)

	// Shortening works the same way in reverse: the new deadline is the one
	// that counts, and it is the entry that says so rather than any hint.
	if err := expectInteger(c, 1, "EXPIRE %s 1", extended); err != nil {
		return err
	}
	if err := c.Sleep(1600 * time.Millisecond); err != nil {
		return err
	}
	return expectNil(c, "GET %s", extended)
}

// reclaimValue is the payload each key in the reclamation test carries. It is
// large enough that a few dozen keys fill the configured max_memory, and small
// enough that the fill is quick.
var reclaimValue = strings.Repeat("x", 1000)

// ttlReclaimsMemory is the ISSUE-0007 regression: expired keys must give their
// memory back, not merely start reporting misses.
//
// Memory has no command of its own before P2, so it is measured the way a
// client feels it. The server runs with eviction disabled and a small
// max_memory, so the keyspace is a fixed budget: the scenario fills it until a
// write is refused, waits for everything it wrote to expire without ever
// touching a key, and then spends the whole budget again. Under ISSUE-0007 the
// expired entries still hold their bytes, and the second fill is refused on its
// very first write.
//
// Nothing reads the doomed keys before the wait is over, so passive expiration
// cannot do the work: this is the active path or nothing. Turning
// ttl.active_expiration off makes the scenario fail, which is what gives it
// teeth.
func ttlReclaimsMemory(c *runner.Ctx) error {
	// 2.5s at the spec's 50ms tick is 50 ticks, well inside L0's 256-tick span.
	return reclaimAfterExpiry(c, "l0", "PX 2500", 3*time.Second)
}

// ttlReclaimsMemoryThroughHigherLevels is the same measurement one wheel level
// up. Its spec runs a 10ms tick, which puts L0's span at 2.56s, so a 5s TTL
// cannot be scheduled in L0 at all: the hint sits in L1 and is only reclaimed if
// the cascade moves it down as its slot comes into range. Memory that comes back
// is the proof that it did.
func ttlReclaimsMemoryThroughHigherLevels(c *runner.Ctx) error {
	return reclaimAfterExpiry(c, "l1", "PX 5000", 6*time.Second)
}

// reclaimAfterExpiry fills the memory budget with keys carrying the given
// expiry, waits without touching any of them, and requires the whole budget to
// be spendable again.
func reclaimAfterExpiry(c *runner.Ctx, prefix, expiry string, wait time.Duration) error {
	const fillCap = 400
	const minimum = 20

	written, err := fillUntilFull(c, prefix+":doomed", fillCap, expiry)
	if err != nil {
		return err
	}
	if written < minimum {
		return fmt.Errorf("only %d keys fit before the cache was full; the spec needs a budget it can measure", written)
	}
	c.Logf("filled the cache with %d keys of %d bytes at %s, then hit the memory limit",
		written, len(reclaimValue), expiry)

	// Long enough for every key to be due and for the wheel to have swept it.
	if err := c.Sleep(wait); err != nil {
		return err
	}

	// The budget must be back in full: every key of the second fill has to fit
	// where the first one did, without a single refusal.
	for i := 0; i < written; i++ {
		reply, err := send(c, "SET %s:fresh:%04d %s", prefix, i, reclaimValue)
		if err != nil {
			return err
		}
		if reply.Kind == runner.KindError {
			return fmt.Errorf(
				"the cache was still full %d keys into the second fill (%s): the %d expired keys never gave their memory back",
				i, reply.Text, written)
		}
	}
	c.Logf("wrote %d fresh keys into the space the expired ones returned", written)

	// And the keyspace itself came back to baseline, not just the memory.
	for _, i := range []int{0, written / 2, written - 1} {
		if err := expectNil(c, "GET %s:doomed:%04d", prefix, i); err != nil {
			return err
		}
	}
	return nil
}

// ttlConcurrentExpireAndGet is ISSUE-0008 end to end: EXPIRE and GET racing on
// one key, from separate connections, for as long as the scenario runs.
//
// Every extension is to a deadline far in the future, so a correct server never
// misses. A torn read of an expiry — the mixed atomic and plain access the
// issue is about — would produce a deadline in the past, and the very next read
// would reclaim a key that is alive. That shows up here as a nil reply.
func ttlConcurrentExpireAndGet(c *runner.Ctx) error {
	const key = "race:key"
	const value = "racing"
	const writers, readers = 2, 3
	const duration = 2 * time.Second

	if err := expectOK(c, "SET %s %s EX 60", key, value); err != nil {
		return err
	}

	race := &race{ctx: c, until: time.Now().Add(duration)}
	for i := 0; i < writers; i++ {
		seconds := 40 + i*20 // Two different extensions, both far in the future
		race.start(&race.expires, func(conn *client.Client) error {
			return expireOnce(c, conn, key, seconds)
		})
	}
	for i := 0; i < readers; i++ {
		race.start(&race.gets, func(conn *client.Client) error {
			return readOnce(c, conn, key, value)
		})
	}

	if err := race.wait(); err != nil {
		return err
	}
	c.Logf("%d reads and %d expiry updates raced on one key for %s", race.gets, race.expires, duration)

	if race.gets == 0 || race.expires == 0 {
		return fmt.Errorf("the race did not run: %d reads, %d expiry updates", race.gets, race.expires)
	}

	remaining, err := ttlOf(c, key)
	if err != nil {
		return err
	}
	if remaining < 30 || remaining > 60 {
		return fmt.Errorf("TTL %s = %d after the race, want one of the extensions that were applied", key, remaining)
	}
	return nil
}

// race runs several clients against one server until a deadline, each on its
// own connection, and collects what they did and what went wrong.
type race struct {
	ctx   *runner.Ctx
	until time.Time

	wg       sync.WaitGroup
	mu       sync.Mutex
	problems []error

	// Counts of what each side completed, addressed by the caller so a worker
	// reports into the tally it belongs to.
	gets, expires int64
}

// start runs work in a loop on its own connection until the deadline, adding
// what it completed to tally.
func (r *race) start(tally *int64, work func(*client.Client) error) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()

		conn, err := client.Dial(r.ctx.Context(), r.ctx.Info().ClientAddr)
		if err != nil {
			r.fail(fmt.Errorf("connecting a racing client: %w", err))
			return
		}
		defer func() { _ = conn.Close() }()

		var done int64
		for time.Now().Before(r.until) && r.ctx.Context().Err() == nil {
			if err := work(conn); err != nil {
				r.fail(err)
				break
			}
			done++
		}

		r.mu.Lock()
		defer r.mu.Unlock()
		*tally += done
	}()
}

func (r *race) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.problems = append(r.problems, err)
}

// wait blocks until every client has stopped and returns everything that went
// wrong along the way.
func (r *race) wait() error {
	r.wg.Wait()

	r.mu.Lock()
	defer r.mu.Unlock()
	return errors.Join(r.problems...)
}

// expireOnce extends the key's expiry, requiring the key to still be there.
func expireOnce(c *runner.Ctx, conn *client.Client, key string, seconds int) error {
	reply, err := conn.Call(c.Context(), "EXPIRE", key, strconv.Itoa(seconds))
	if err != nil {
		return err
	}
	if reply.Kind != runner.KindInteger || reply.Integer != 1 {
		return fmt.Errorf("EXPIRE answered %s %q, want 1 — the key went missing under concurrent access",
			reply.Kind, reply.String())
	}
	return nil
}

// readOnce reads the key, requiring the value it was written with.
func readOnce(c *runner.Ctx, conn *client.Client, key, value string) error {
	reply, err := conn.Call(c.Context(), "GET", key)
	if err != nil {
		return err
	}
	if reply.Kind != runner.KindBulk || reply.Text != value {
		return fmt.Errorf(
			"GET answered %s %q while its TTL was being extended, want %q — a live key was expired under it",
			reply.Kind, truncate(reply.String()), value)
	}
	return nil
}

// ttlChurnHoldsSteadyMemory is the soak form of the same property: TTL traffic
// that never stops must not accumulate. Rounds of short-lived keys are written
// against a fixed budget with eviction off, so an engine that leaked even a
// fraction of each round would run out of room and start refusing writes.
func ttlChurnHoldsSteadyMemory(c *runner.Ctx) error {
	const rounds = 12
	const perRound = 25
	const pause = 1200 * time.Millisecond

	for round := 0; round < rounds; round++ {
		for i := 0; i < perRound; i++ {
			reply, err := send(c, "SET churn:%02d:%03d %s PX 700", round, i, reclaimValue)
			if err != nil {
				return err
			}
			if reply.Kind == runner.KindError {
				return fmt.Errorf(
					"round %d, key %d was refused (%s): memory is not returning to steady state under TTL churn",
					round, i, reply.Text)
			}
		}

		// Long enough for the round to expire and be swept before the next one
		// starts, so a leak shows up as the budget shrinking round by round.
		if err := c.Sleep(pause); err != nil {
			return err
		}
	}
	c.Logf("%d rounds of %d expiring keys held steady", rounds, perRound)

	// The budget is intact at the end: a full round still fits.
	for i := 0; i < perRound; i++ {
		reply, err := send(c, "SET after:%03d %s", i, reclaimValue)
		if err != nil {
			return err
		}
		if reply.Kind == runner.KindError {
			return fmt.Errorf("after the churn, key %d of a final round was refused (%s)", i, reply.Text)
		}
	}
	return nil
}

// fillUntilFull writes keys under prefix until one is refused, returning how
// many were accepted. It is how a spec measures the memory budget without a
// command that reports it.
func fillUntilFull(c *runner.Ctx, prefix string, limit int, options string) (int, error) {
	for i := 0; i < limit; i++ {
		reply, err := send(c, "SET %s:%04d %s %s", prefix, i, reclaimValue, options)
		if err != nil {
			return 0, err
		}
		if reply.Kind == runner.KindError {
			if !strings.HasPrefix(reply.Text, "OOM") {
				return 0, fmt.Errorf("SET %s:%04d failed with %q, want either success or an OOM refusal", prefix, i, reply.Text)
			}
			return i, nil
		}
	}
	return 0, fmt.Errorf("wrote %d keys without filling the cache; max_memory is too large for this spec to measure", limit)
}

// ttlOf reads a key's TTL as an integer.
func ttlOf(c *runner.Ctx, key string) (int64, error) {
	reply, err := send(c, "TTL %s", key)
	if err != nil {
		return 0, err
	}
	if reply.Kind != runner.KindInteger {
		return 0, fmt.Errorf("TTL %s answered %s %q, want an integer", key, reply.Kind, reply.String())
	}
	return reply.Integer, nil
}
