package scenarios_test

import (
	"strings"
	"testing"
)

// Each P1 scenario is run twice here: once against a model cache that behaves,
// where it must pass, and once against one carrying the exact defect the
// scenario was written for, where it must fail and say why. The second half is
// the part that matters — a scenario that cannot fail is worse than no scenario,
// because it reports green while the property it names goes unchecked.

// The eviction scenarios size their keyspace against storage.max_memory: 900
// with entries of 102 bytes, so eight of them are resident and the ninth write
// forces a choice of victim.
const modelBudget = 900

// ttlBudget holds roughly forty of the 1000-byte values the TTL scenarios use,
// which is enough for fill_until_full to measure a budget and small enough for
// the fill to be quick.
const ttlBudget = 44000

func TestStoredValuesSurviveBufferReuse(t *testing.T) {
	t.Parallel()

	const scenario = "stored_values_survive_buffer_reuse"

	t.Run("an engine that copies what it stores passes", func(t *testing.T) {
		t.Parallel()
		h := newModelHarness(t, modelOptions{policy: "none"})

		if failure := runScenario(t, scenario, h); failure != nil {
			t.Fatalf("a cache that copies its values failed the scenario: %s", failure.Message)
		}
	})

	t.Run("values that alias the connection's read buffer are caught", func(t *testing.T) {
		t.Parallel()
		// ISSUE-0009 exactly: the engine kept the caller's slice, so every read
		// hands back whatever arrived on that connection most recently. Nothing
		// in a server log would show this; only a read-back can.
		h := newModelHarness(t, modelOptions{policy: "none", aliasStoredValues: true})

		failure := runScenario(t, scenario, h)
		if failure == nil {
			t.Fatal("a cache whose values share one buffer passed; the scenario checks nothing")
		}
		if !strings.Contains(failure.Message, "GET buf:") {
			t.Errorf("message = %q, want it to name the read that came back wrong", failure.Message)
		}
	})
}

func TestEvictPoliciesBehaveAsSpecified(t *testing.T) {
	t.Parallel()

	const scenario = "evict_policies_behave_as_specified"

	t.Run("fifo, lfu and none each behave as named", func(t *testing.T) {
		t.Parallel()
		h := newModelHarness(t, modelOptions{policy: "fifo", maxMemory: modelBudget})

		if failure := runScenario(t, scenario, h); failure != nil {
			t.Fatalf("a cache honoring every policy failed the scenario: %s", failure.Message)
		}
		// The scenario is only meaningful if it really moved the server through
		// all three policies rather than asserting three times against fifo.
		logs := h.Logs()
		for _, policy := range []string{"lfu", "none"} {
			if !strings.Contains(logs, `"policy":"`+policy+`"`) {
				t.Errorf("the scenario never switched the server to %s:\n%s", policy, logs)
			}
		}
	})

	t.Run("a server that evicts by recency instead of by age is caught", func(t *testing.T) {
		t.Parallel()
		// The fifo phase reads the oldest key just before overflowing the
		// budget, so lru would spare it and fifo must not. A server that
		// silently ran lru while the config said fifo fails right there.
		h := newModelHarness(t, modelOptions{policy: "lru", maxMemory: modelBudget})

		failure := runScenario(t, scenario, h)
		if failure == nil {
			t.Fatal("a cache running lru passed a scenario that asked for fifo")
		}
		if !strings.Contains(failure.Message, "fifo:") {
			t.Errorf("message = %q, want it to name the phase that failed", failure.Message)
		}
		if !strings.Contains(failure.Message, "oldest key survived") {
			t.Errorf("message = %q, want it to say which key should have gone", failure.Message)
		}
	})
}

func TestEvictHoldsTheMemoryLimit(t *testing.T) {
	t.Parallel()

	const scenario = "evict_holds_the_memory_limit"

	t.Run("a cache that makes room rather than refusing passes", func(t *testing.T) {
		t.Parallel()
		h := newModelHarness(t, modelOptions{policy: "lru", maxMemory: modelBudget})

		if failure := runScenario(t, scenario, h); failure != nil {
			t.Fatalf("a cache that evicts on the write path failed the scenario: %s", failure.Message)
		}
	})

	t.Run("a cache that refuses writes instead of evicting is caught", func(t *testing.T) {
		t.Parallel()
		// Half of ADR-0018: with a policy configured, a write past max_memory
		// must make room, not report OOM. A server that answered OOM anyway
		// would look healthy to anything that only checked resident bytes.
		h := newModelHarness(t, modelOptions{policy: "none", maxMemory: modelBudget})

		failure := runScenario(t, scenario, h)
		if failure == nil {
			t.Fatal("a cache that refused every write past the limit passed")
		}
		if !strings.Contains(failure.Message, "was refused") {
			t.Errorf("message = %q, want it to say the write was refused", failure.Message)
		}
	})

	t.Run("a cache that keeps everything it was given is caught", func(t *testing.T) {
		t.Parallel()
		// The other half: a server that accepted every write but never actually
		// freed anything would pass a check that only looked for refusals. The
		// resident count is what notices, so the budget here is effectively
		// unbounded while the scenario still measures against 900.
		h := newModelHarness(t, modelOptions{policy: "lru", maxMemory: 1 << 20})

		failure := runScenario(t, scenario, h)
		if failure == nil {
			t.Fatal("a cache that never evicted anything passed a memory-limit scenario")
		}
		if !strings.Contains(failure.Message, "keys are resident") {
			t.Errorf("message = %q, want it to report the resident set it measured", failure.Message)
		}
	})
}

func TestEvictSustainedWritesStayBounded(t *testing.T) {
	t.Parallel()

	const scenario = "evict_sustained_writes_stay_bounded"

	t.Run("a cache that holds its budget over many rounds passes", func(t *testing.T) {
		t.Parallel()
		h := newModelHarness(t, modelOptions{policy: "lru", maxMemory: modelBudget})

		if failure := runScenario(t, scenario, h); failure != nil {
			t.Fatalf("a cache that stayed bounded failed the soak scenario: %s", failure.Message)
		}
	})

	t.Run("a resident set that keeps growing is caught", func(t *testing.T) {
		t.Parallel()
		// A soak exists to catch what a burst does not: accounting that drifts
		// slowly. A budget that never bites stands in for accounting that has
		// lost track of what it is holding.
		h := newModelHarness(t, modelOptions{policy: "lru", maxMemory: 1 << 20})

		failure := runScenario(t, scenario, h)
		if failure == nil {
			t.Fatal("a cache whose resident set grew without bound passed the soak scenario")
		}
		if !strings.Contains(failure.Message, "keys are resident") {
			t.Errorf("message = %q, want it to report the resident set", failure.Message)
		}
	})
}

func TestTTLExtensionOutlivesItsHint(t *testing.T) {
	t.Parallel()

	const scenario = "ttl_extension_outlives_its_hint"

	t.Run("a superseded expiry hint is dropped rather than acted on", func(t *testing.T) {
		t.Parallel()
		h := newModelHarness(t, modelOptions{policy: "none", maxMemory: ttlBudget})

		if failure := runScenario(t, scenario, h); failure != nil {
			t.Fatalf("a cache that re-reads the entry failed the scenario: %s", failure.Message)
		}
	})

	t.Run("a stale hint that expires a live key is caught", func(t *testing.T) {
		t.Parallel()
		// The ADR-0016 failure: the wheel has no Remove, so an extended key
		// still has a hint pointing at its old deadline. Acting on the hint
		// instead of re-reading the entry deletes data that is alive — and
		// nothing but a read-back after the old deadline would notice.
		h := newModelHarness(t, modelOptions{policy: "none", maxMemory: ttlBudget, honorStaleHints: true})

		failure := runScenario(t, scenario, h)
		if failure == nil {
			t.Fatal("a cache that acted on a stale hint passed; the scenario checks nothing")
		}
		if !strings.Contains(failure.Message, "expired by its stale hint") {
			t.Errorf("message = %q, want it to name the stale hint", failure.Message)
		}
	})
}

func TestTTLReclaimsMemory(t *testing.T) {
	t.Parallel()

	const scenario = "ttl_reclaims_memory"

	t.Run("expired keys give their memory back", func(t *testing.T) {
		t.Parallel()
		h := newModelHarness(t, modelOptions{policy: "none", maxMemory: ttlBudget})

		if failure := runScenario(t, scenario, h); failure != nil {
			t.Fatalf("a cache that reclaims on expiry failed the scenario: %s", failure.Message)
		}
	})

	t.Run("expired keys that keep their bytes are caught", func(t *testing.T) {
		t.Parallel()
		// ISSUE-0007: the entry stops answering, so every miss-based check goes
		// green, while the bytes are never returned. The only way to see it
		// from outside is to try to spend the budget again.
		h := newModelHarness(t, modelOptions{policy: "none", maxMemory: ttlBudget, leakExpiredMemory: true})

		failure := runScenario(t, scenario, h)
		if failure == nil {
			t.Fatal("a cache that never returned the memory of expired keys passed")
		}
		if !strings.Contains(failure.Message, "never gave their memory back") {
			t.Errorf("message = %q, want it to say the memory was not returned", failure.Message)
		}
	})
}

func TestTTLChurnHoldsSteadyMemory(t *testing.T) {
	t.Parallel()

	const scenario = "ttl_churn_holds_steady_memory"

	t.Run("rounds of expiring keys hold steady", func(t *testing.T) {
		t.Parallel()
		h := newModelHarness(t, modelOptions{policy: "none", maxMemory: ttlBudget})

		if failure := runScenario(t, scenario, h); failure != nil {
			t.Fatalf("a cache that returns to steady state failed the churn scenario: %s", failure.Message)
		}
	})

	t.Run("a budget that shrinks round by round is caught", func(t *testing.T) {
		t.Parallel()
		// The soak form of ISSUE-0007: a leak too small to see in one wave, but
		// which runs the cache out of room once the traffic never stops.
		h := newModelHarness(t, modelOptions{policy: "none", maxMemory: ttlBudget, leakExpiredMemory: true})

		failure := runScenario(t, scenario, h)
		if failure == nil {
			t.Fatal("a cache leaking a whole round of keys per round passed the churn scenario")
		}
		if !strings.Contains(failure.Message, "was refused") {
			t.Errorf("message = %q, want it to name the write that was refused", failure.Message)
		}
	})
}

func TestTTLConcurrentExpireAndGet(t *testing.T) {
	t.Parallel()

	const scenario = "ttl_concurrent_expire_and_get"

	t.Run("readers and writers racing on one key see it alive throughout", func(t *testing.T) {
		t.Parallel()
		// This one opens its own connections rather than going through the
		// harness, so it runs against the model's real listener.
		h := newModelHarness(t, modelOptions{policy: "none", maxMemory: ttlBudget})

		if failure := runScenario(t, scenario, h); failure != nil {
			t.Fatalf("a cache that reads expiries consistently failed the race scenario: %s", failure.Message)
		}
	})

	t.Run("a key that goes missing under concurrent access is caught", func(t *testing.T) {
		t.Parallel()
		// Every extension in the scenario is to a deadline far in the future,
		// so a correct server never misses. A torn read of an expiry — the
		// mixed atomic and plain access ISSUE-0008 is about — lands a deadline
		// in the past, and the next reader sees a live key reported gone.
		h := newModelHarness(t, modelOptions{policy: "none", maxMemory: ttlBudget, tornExpiry: true})

		failure := runScenario(t, scenario, h)
		if failure == nil {
			t.Fatal("a cache that dropped the key mid-race passed the concurrency scenario")
		}
		if !strings.Contains(failure.Message, "under concurrent access") &&
			!strings.Contains(failure.Message, "a live key was expired under it") {
			t.Errorf("message = %q, want it to name what the race exposed", failure.Message)
		}
	})
}
