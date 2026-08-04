package scenarios

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/b3vet/atlascache/test/e2e/runner"
)

func init() {
	runner.RegisterScenario("evict_policies_behave_as_specified", evictPoliciesBehaveAsSpecified)
	runner.RegisterScenario("evict_holds_the_memory_limit", evictHoldsTheMemoryLimit)
	runner.RegisterScenario("evict_sustained_writes_stay_bounded", evictSustainedWritesStayBounded)
}

// The keyspace every eviction scenario works in. Keys are two bytes and values
// twenty, so an entry costs a known 102 bytes with the fixed overhead, and a
// spec can size max_memory to hold an exact number of them.
const (
	evictValueSize = 20
	evictEntrySize = 2 + evictValueSize + 80
	evictFill      = 8 // Entries that fit under the specs' max_memory of 900

	// evictBudget is the storage.max_memory the eviction specs configure. The
	// scenarios need it to turn a count of surviving keys into bytes.
	evictBudget = 900
)

// evictValue is what every eviction scenario stores, sized so entries are all
// the same and the victim is decided by the policy rather than by size.
var evictValue = strings.Repeat("v", evictValueSize)

// evictPoliciesBehaveAsSpecified drives lfu, fifo and none against the same
// keyspace shape, switching policy through the hot-reload path the server
// already has (P1 section 5.2) because a spec runs one server.
//
// Each phase is arranged so that the three policies would disagree: the key the
// active policy must drop is not the one the other two would pick. A phase that
// passed under the wrong policy would prove only that something was evicted.
func evictPoliciesBehaveAsSpecified(c *runner.Ctx) error {
	// fifo first, since it is what the spec's config asks for.
	if err := evictFIFOPhase(c); err != nil {
		return fmt.Errorf("fifo: %w", err)
	}
	if err := switchPolicy(c, "lfu"); err != nil {
		return err
	}
	if err := evictLFUPhase(c); err != nil {
		return fmt.Errorf("lfu: %w", err)
	}
	if err := switchPolicy(c, "none"); err != nil {
		return err
	}
	if err := evictNonePhase(c); err != nil {
		return fmt.Errorf("none: %w", err)
	}
	return nil
}

// evictFIFOPhase checks that fifo drops by insertion order however hot the key
// is. The oldest key is read after the fill, so it is the most recently used
// one in the cache: lru would spare it and fifo must not.
func evictFIFOPhase(c *runner.Ctx) error {
	keys, err := fillKeyspace(c, "f")
	if err != nil {
		return err
	}

	// Reading the oldest key makes it the most recently used, so lru would now
	// spare it and take keys[1] instead. fifo must take it anyway.
	if err := expectBulk(c, evictValue, "GET %s", keys[0]); err != nil {
		return err
	}

	if err := setValue(c, "fz"); err != nil {
		return err
	}
	if err := expectNil(c, "GET %s", keys[0]); err != nil {
		return fmt.Errorf("the oldest key survived: %w", err)
	}
	if err := expectBulk(c, evictValue, "GET %s", keys[1]); err != nil {
		return fmt.Errorf("the least recently used key was taken instead of the oldest: %w", err)
	}
	return dropKeys(c, append(keys, "fz"))
}

// evictLFUPhase checks that lfu drops by frequency rather than by age or
// recency. The rarely-read key is written last and read most recently, so
// neither fifo nor lru would choose it; the often-read key is the oldest and
// the least recently touched, so both of them would.
func evictLFUPhase(c *runner.Ctx) error {
	const hot = "la"
	const cold = "lb"

	if err := setValue(c, hot); err != nil {
		return err
	}
	for i := 0; i < 10; i++ {
		if err := expectBulk(c, evictValue, "GET %s", hot); err != nil {
			return err
		}
	}

	middle := make([]string, 0, evictFill-2)
	for i := 0; i < evictFill-2; i++ {
		key := fmt.Sprintf("l%d", i)
		if err := setValue(c, key); err != nil {
			return err
		}
		for read := 0; read < 3; read++ {
			if err := expectBulk(c, evictValue, "GET %s", key); err != nil {
				return err
			}
		}
		middle = append(middle, key)
	}

	// Written last and never read again: the newest key in the cache, and the
	// one nothing has asked for.
	if err := setValue(c, cold); err != nil {
		return err
	}

	if err := setValue(c, "lz"); err != nil {
		return err
	}
	if err := expectNil(c, "GET %s", cold); err != nil {
		return fmt.Errorf("the least frequently used key survived: %w", err)
	}
	if err := expectBulk(c, evictValue, "GET %s", hot); err != nil {
		return fmt.Errorf("the most frequently used key was evicted, which is what lru or fifo would have done: %w", err)
	}

	return dropKeys(c, append(append(middle, hot, cold), "lz"))
}

// evictNonePhase checks that none evicts nothing: the write is refused with
// Redis's OOM error and the resident keys are all still there afterwards.
func evictNonePhase(c *runner.Ctx) error {
	keys, err := fillKeyspace(c, "n")
	if err != nil {
		return err
	}

	reply, err := send(c, "SET nz %s", evictValue)
	if err != nil {
		return err
	}
	if reply.Kind != runner.KindError || !strings.HasPrefix(reply.Text, "OOM") {
		return fmt.Errorf("SET past max_memory under policy none answered %s %q, want an OOM error",
			reply.Kind, reply.String())
	}

	for _, key := range keys {
		if err := expectBulk(c, evictValue, "GET %s", key); err != nil {
			return fmt.Errorf("policy none dropped a key rather than refusing the write: %w", err)
		}
	}
	return expectNil(c, "GET nz")
}

// evictHoldsTheMemoryLimit writes far more than max_memory and checks the two
// halves of ADR-0018: no write is ever refused, because eviction runs on the
// write path and makes room; and what is resident afterwards still fits in the
// limit, because the room it made was real.
func evictHoldsTheMemoryLimit(c *runner.Ctx) error {
	const writes = 600

	keyAt := func(i int) string { return fmt.Sprintf("h%04d", i) }

	for i := 0; i < writes; i++ {
		reply, err := send(c, "SET %s %s", keyAt(i), evictValue)
		if err != nil {
			return err
		}
		if reply.Kind == runner.KindError {
			return fmt.Errorf("write %d was refused (%s); eviction is meant to make room, not report failure", i, reply.Text)
		}

		// Sampled while the writes are still coming rather than only at the
		// end, so a limit that is honored just in time to be measured would
		// still fail here.
		if i == writes/3 || i == 2*writes/3 {
			if err := checkResident(c, keyAt, i); err != nil {
				return err
			}
		}
	}
	return checkResident(c, keyAt, writes)
}

// evictSustainedWritesStayBounded is the soak form: the same pressure, held for
// long enough that a slow leak in the accounting would show up as a keyspace
// that keeps growing.
func evictSustainedWritesStayBounded(c *runner.Ctx) error {
	const rounds = 40
	const perRound = 50
	const pause = 400 * time.Millisecond

	keyAt := func(i int) string { return fmt.Sprintf("s%05d", i) }

	for round := 0; round < rounds; round++ {
		for i := 0; i < perRound; i++ {
			written := round*perRound + i
			reply, err := send(c, "SET %s %s", keyAt(written), evictValue)
			if err != nil {
				return err
			}
			if reply.Kind == runner.KindError {
				return fmt.Errorf("write %d of a sustained run was refused (%s)", written, reply.Text)
			}
		}

		// Spread over time rather than run flat out: a leak that only shows up
		// once the wheel and the eviction path have both been busy for a while
		// is exactly what a soak is for.
		if err := c.Sleep(pause); err != nil {
			return err
		}
		if round%8 == 7 {
			if err := checkResident(c, keyAt, (round+1)*perRound); err != nil {
				return err
			}
		}
	}

	c.Logf("%d writes over %s held the memory limit", rounds*perRound, time.Duration(rounds)*pause)
	return checkResident(c, keyAt, rounds*perRound)
}

// checkResident counts how many of the keys written so far are still there and
// requires the total to fit under max_memory. Counting the survivors is how a
// client measures resident memory before there is a command that reports it:
// every entry costs the same known number of bytes.
func checkResident(c *runner.Ctx, keyAt func(int) string, written int) error {
	resident := evictBudget / evictEntrySize

	alive := 0
	for i := 0; i < written; i++ {
		reply, err := send(c, "GET %s", keyAt(i))
		if err != nil {
			return err
		}
		if reply.Kind == runner.KindBulk {
			alive++
		}
	}

	if alive > resident {
		return fmt.Errorf("%d keys are resident after %d writes, which is %dB against a %dB limit",
			alive, written, alive*evictEntrySize, resident*evictEntrySize)
	}
	if alive == 0 {
		return fmt.Errorf("nothing survived %d writes; eviction is taking more than it needs to", written)
	}
	c.Logf("after %d writes, %d of %d possible keys are resident", written, alive, resident)
	return nil
}

// fillKeyspace writes exactly as many keys as fit under max_memory, returning
// them in the order they were written.
func fillKeyspace(c *runner.Ctx, prefix string) ([]string, error) {
	keys := make([]string, 0, evictFill)
	for i := 0; i < evictFill; i++ {
		key := fmt.Sprintf("%s%d", prefix, i)
		if err := setValue(c, key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, nil
}

func setValue(c *runner.Ctx, key string) error {
	return expectOK(c, "SET %s %s", key, evictValue)
}

// dropKeys empties the keyspace between phases, so each one starts from a known
// budget.
func dropKeys(c *runner.Ctx, keys []string) error {
	for _, key := range keys {
		if _, err := send(c, "DEL %s", key); err != nil {
			return err
		}
	}
	return nil
}

// switchPolicy changes eviction.policy on the running server through config
// hot-reload, and waits for the server to say it applied it.
//
// The config file is the harness's, so this reaches into the directory the
// harness owns rather than through the protocol — there is no command for it,
// and inventing one would be P2 work smuggled into P1.
func switchPolicy(c *runner.Ctx, policy string) error {
	binary, err := binaryOf(c)
	if err != nil {
		return err
	}

	path := filepath.Join(binary.Root(), "config.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading the server's config at %s: %w", path, err)
	}

	var config map[string]any
	if err = yaml.Unmarshal(raw, &config); err != nil {
		return fmt.Errorf("parsing the server's config: %w", err)
	}
	eviction, ok := config["eviction"].(map[string]any)
	if !ok {
		return fmt.Errorf("the server's config has no eviction block: %s", raw)
	}
	eviction["policy"] = policy

	updated, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("rendering the updated config: %w", err)
	}
	if err = os.WriteFile(path, updated, 0o600); err != nil {
		return fmt.Errorf("writing the updated config: %w", err)
	}

	if err := waitForPolicy(c, policy); err != nil {
		return err
	}
	c.Logf("eviction policy is now %s", policy)
	return nil
}

// waitForPolicy blocks until the server logs that it applied the policy. The
// log is a positive signal; a sleep would be either slow or flaky.
func waitForPolicy(c *runner.Ctx, policy string) error {
	const timeout = 10 * time.Second
	const poll = 50 * time.Millisecond

	want := fmt.Sprintf("%q:%q", "policy", policy)
	deadline := time.Now().Add(timeout)
	for {
		logs := c.Harness.Logs()
		if strings.Contains(logs, want) && strings.Contains(logs, "eviction policy applied") {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the server did not apply eviction policy %s within %s of the config changing", policy, timeout)
		}
		if err := c.Sleep(poll); err != nil {
			return err
		}
	}
}
