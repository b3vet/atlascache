package scenarios_test

import (
	"strings"
	"testing"

	"github.com/b3vet/atlascache/test/e2e/runner"
)

// Each SDK scenario runs twice here: once against a model server that behaves,
// where it must pass, and once against one carrying a defect it was written to
// catch, where it must fail and say why.
//
// The second half is the half that matters. A scenario that cannot fail is
// worse than no scenario, because it reports green while the property it names
// goes unchecked — and these particular scenarios are the only ones in the
// suite that run through the SDK rather than around it.

const modelToken = "model-token-0e4a7c31"

// sdkPasses runs a scenario against a model with the given configuration and
// fails the test if the scenario is unhappy with it.
func sdkPasses(t *testing.T, scenario string, defects sdkDefects) {
	t.Helper()

	if failure := runScenario(t, scenario, newSDKHarness(t, defects)); failure != nil {
		t.Fatalf("%s failed against a correct server: %s\n%s",
			scenario, failure.Message, strings.Join(failure.Notes, "\n"))
	}
}

// sdkCatches runs a scenario against a defective model and fails the test if
// the scenario is happy with it.
func sdkCatches(t *testing.T, scenario string, defects sdkDefects, wants string) {
	t.Helper()

	failure := runScenario(t, scenario, newSDKHarness(t, defects))
	if failure == nil {
		t.Fatalf("%s passed against a server with %+v", scenario, defects)
	}
	if !strings.Contains(failure.Message, wants) {
		t.Errorf("%s reported %q, which does not mention %q", scenario, failure.Message, wants)
	}
}

func TestSDKScenariosAreRegistered(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"sdk_typed_methods_round_trip",
		"sdk_do_sends_arbitrary_commands",
		"sdk_error_categories",
		"sdk_honors_cancellation",
		"sdk_tls_and_auth",
		"sdk_pool_reuses_and_bounds_connections",
		"sdk_pool_discards_after_a_protocol_error",
		"sdk_survives_a_server_restart",
		"sdk_pool_churn_leaks_nothing",
	} {
		if _, ok := runner.LookupScenario(name); !ok {
			t.Errorf("%s did not register itself", name)
		}
	}
}

func TestSDKTypedMethodsRoundTrip(t *testing.T) {
	t.Parallel()

	const scenario = "sdk_typed_methods_round_trip"

	t.Run("a server that behaves passes", func(t *testing.T) {
		t.Parallel()
		sdkPasses(t, scenario, sdkDefects{})
	})

	t.Run("a miss that reads as an empty value is caught", func(t *testing.T) {
		t.Parallel()
		// ADR-0022's amendment, undone: a two-value Get would have conflated
		// these, and a server that answers a miss with an empty bulk conflates
		// them on the wire. Either way the caller cannot tell an absent key
		// from a key holding nothing.
		sdkCatches(t, scenario, sdkDefects{missAsEmpty: true}, "reads as present")
	})

	t.Run("a value cut at its first null byte is caught", func(t *testing.T) {
		t.Parallel()
		// The C-string bug, which survives every test written with ASCII.
		sdkCatches(t, scenario, sdkDefects{truncateAtNull: true}, "binary value came back")
	})

	t.Run("EXISTS that de-duplicates its arguments is caught", func(t *testing.T) {
		t.Parallel()
		sdkCatches(t, scenario, sdkDefects{dedupeExists: true}, "EXISTS of one present key named twice")
	})

	t.Run("a scan that claims to be finished is caught", func(t *testing.T) {
		t.Parallel()
		// The quiet one: a scan that stops early returns a fraction of the
		// keyspace and reports success, so whatever was iterating simply
		// misses keys.
		sdkCatches(t, scenario, sdkDefects{scanStopsEarly: true}, "a full scan saw")
	})
}

func TestSDKDoSendsArbitraryCommands(t *testing.T) {
	t.Parallel()

	const scenario = "sdk_do_sends_arbitrary_commands"

	t.Run("a server that behaves passes", func(t *testing.T) {
		t.Parallel()
		sdkPasses(t, scenario, sdkDefects{})
	})

	t.Run("a server that accepts anything is caught", func(t *testing.T) {
		t.Parallel()
		// A command nobody implements answered with +OK. Do would report
		// success for a write that never happened.
		sdkCatches(t, scenario, sdkDefects{acceptsAnything: true}, "want an ErrServer")
	})
}

func TestSDKErrorCategories(t *testing.T) {
	t.Parallel()

	const scenario = "sdk_error_categories"

	t.Run("a server that behaves passes", func(t *testing.T) {
		t.Parallel()
		sdkPasses(t, scenario, sdkDefects{token: modelToken})
	})

	t.Run("a server that lets any token through is caught", func(t *testing.T) {
		t.Parallel()
		// The category is only useful if the failure it names actually
		// happens: a server that accepts anything produces no ErrAuth at all,
		// and the scenario says so rather than reporting five of six.
		sdkCatches(t, scenario, sdkDefects{token: modelToken, acceptsAnyToken: true},
			"a refused token")
	})

	t.Run("a server that refuses nothing is caught", func(t *testing.T) {
		t.Parallel()
		sdkCatches(t, scenario, sdkDefects{token: modelToken, acceptsAnything: true},
			"a command the server refuses")
	})
}

func TestSDKHonorsCancellation(t *testing.T) {
	t.Parallel()

	const scenario = "sdk_honors_cancellation"

	t.Run("a client that returns promptly passes", func(t *testing.T) {
		t.Parallel()
		sdkPasses(t, scenario, sdkDefects{})
	})

	t.Run("a read that comes back with the wrong value is caught", func(t *testing.T) {
		t.Parallel()
		// The state the scenario checks after the cancellations: a client whose
		// canceled call left its connection half-read would come back with
		// somebody else's reply, and this is what that looks like from outside.
		sdkCatches(t, scenario, sdkDefects{wrongValues: true}, "after two canceled calls")
	})
}

func TestSDKTLSAndAuth(t *testing.T) {
	t.Parallel()

	const scenario = "sdk_tls_and_auth"

	t.Run("a server with TLS and a token passes", func(t *testing.T) {
		t.Parallel()
		sdkPasses(t, scenario, sdkDefects{serveTLS: true, token: modelToken})
	})

	t.Run("a server that accepts any token is caught", func(t *testing.T) {
		t.Parallel()
		sdkCatches(t, scenario, sdkDefects{serveTLS: true, token: modelToken, acceptsAnyToken: true},
			"a wrong token")
	})

	t.Run("a refused token without its kind is caught", func(t *testing.T) {
		t.Parallel()
		sdkCatches(t, scenario, sdkDefects{serveTLS: true, token: modelToken, genericAuthError: true},
			"WRONGPASS")
	})
}

func TestSDKPoolReusesAndBoundsConnections(t *testing.T) {
	t.Parallel()

	const scenario = "sdk_pool_reuses_and_bounds_connections"

	t.Run("a pool that reuses its connections passes", func(t *testing.T) {
		t.Parallel()
		sdkPasses(t, scenario, sdkDefects{})
	})

	t.Run("a connection that cannot be reused is caught", func(t *testing.T) {
		t.Parallel()
		// A server that hangs up after every reply, so there is nothing for a
		// pool to reuse. The scenario goes red the moment a call meets a
		// connection that cannot be reused — which is what it is for, and what
		// no other spec in the suite would notice.
		sdkCatches(t, scenario, sdkDefects{hangUpEachTime: true}, "sdk:pool")
	})
}

func TestSDKPoolDiscardsAfterAProtocolError(t *testing.T) {
	t.Parallel()

	const scenario = "sdk_pool_discards_after_a_protocol_error"

	t.Run("a client that discards the poisoned connection passes", func(t *testing.T) {
		t.Parallel()
		sdkPasses(t, scenario, sdkDefects{})
	})

	t.Run("a read answered with the previous caller's value is caught", func(t *testing.T) {
		t.Parallel()
		// ISSUE-0009's shape, which is also exactly what reusing a connection
		// at an unknown stream position produces: one caller receiving another
		// caller's data, with nothing in any log to say so.
		sdkCatches(t, scenario, sdkDefects{aliasReads: true}, "previous caller's reply")
	})
}

func TestSDKSurvivesAServerRestart(t *testing.T) {
	t.Parallel()

	const scenario = "sdk_survives_a_server_restart"

	t.Run("a server that comes back passes", func(t *testing.T) {
		t.Parallel()
		sdkPasses(t, scenario, sdkDefects{})
	})

	t.Run("a server that does not come back is caught", func(t *testing.T) {
		t.Parallel()
		harness := newSDKHarness(t, sdkDefects{})
		harness.staysDead = true

		failure := runScenario(t, scenario, harness)
		if failure == nil {
			t.Fatal("the scenario passed against a server that never came back")
		}
		if !strings.Contains(failure.Message, "restart") {
			t.Errorf("message = %q, want it to name the restart", failure.Message)
		}
	})
}

func TestSDKPoolChurnLeaksNothing(t *testing.T) {
	t.Parallel()

	const scenario = "sdk_pool_churn_leaks_nothing"

	t.Run("a client that returns what it borrowed passes", func(t *testing.T) {
		t.Parallel()
		sdkPasses(t, scenario, sdkDefects{})
	})

	t.Run("a connection per call is caught", func(t *testing.T) {
		t.Parallel()
		// A connection that cannot survive one command turns every round into a
		// re-dial, and the churn notices inside the first one. The other half
		// of this scenario — that a closed client leaves nothing open — is
		// proven directly in the proxy's own test.
		sdkCatches(t, scenario, sdkDefects{hangUpEachTime: true}, "round 0")
	})
}
