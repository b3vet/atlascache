package scenarios

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/b3vet/atlascache/pkg/client"
	"github.com/b3vet/atlascache/test/e2e/runner"
)

func init() {
	runner.RegisterScenario("sdk_error_categories", sdkErrorCategories)
	runner.RegisterScenario("sdk_honors_cancellation", sdkHonorsCancellation)
}

// How long a failure that should be immediate may take. Each is a bound on
// something the SDK promises to do promptly, so a regression that turns a
// refusal into a hang fails here rather than in somebody's request path.
const (
	failFastBudget = 5 * time.Second

	// cancelBudget is how long a canceled call may take to return. It has to be
	// comfortably *under* the delay the relay holds replies for, or a client
	// that ignored its context entirely would still come in under budget when
	// the reply finally arrived — and the assertion would pass for the one
	// behavior it exists to catch.
	cancelBudget = time.Second
	// cancelDelay is how long the relay holds a reply. Twice the budget above.
	cancelDelay = 2 * time.Second

	// probeTimeout is the read timeout given to the client that talks to a
	// listener which never answers. It has to be short enough that the scenario
	// is quick and long enough that a loaded machine does not trip it early.
	probeTimeout = 500 * time.Millisecond
)

// categories is the table from P3 §4.2, as a list, so that each error can be
// checked against the ones it must *not* be as well as the one it must.
//
// That is the assertion with teeth. A client whose every failure wrapped all
// six sentinels would pass any check that only looked for the right one, and
// would make errors.Is useless to the caller it was built for.
var categories = []struct {
	name     string
	sentinel error
}{
	{"ErrNetwork", client.ErrNetwork},
	{"ErrTimeout", client.ErrTimeout},
	{"ErrProtocol", client.ErrProtocol},
	{"ErrServer", client.ErrServer},
	{"ErrAuth", client.ErrAuth},
	{"ErrClosed", client.ErrClosed},
}

// assertCategory checks that err belongs to exactly one category, and that it
// is the expected one.
func assertCategory(c *runner.Ctx, what string, err error, want error, retryable bool) error {
	if err == nil {
		return fmt.Errorf("%s did not fail at all", what)
	}
	if !errors.Is(err, want) {
		return fmt.Errorf("%s answered %v, which is not the category it should be", what, err)
	}
	for _, category := range categories {
		if errors.Is(category.sentinel, want) {
			continue
		}
		if errors.Is(err, category.sentinel) {
			return fmt.Errorf("%s is also a %s; the categories must be distinguishable, or errors.Is answers nothing",
				what, category.name)
		}
	}
	if client.Retryable(err) != retryable {
		return fmt.Errorf("%s reports Retryable=%v, want %v", what, client.Retryable(err), retryable)
	}
	c.Logf("%s: %v", what, err)
	return nil
}

// sdkErrorCategories produces each of the six failures against the real server
// and checks that the caller can tell them apart with errors.Is.
//
// The categories exist so that a caller decides what to do without matching on
// strings (FEAT-0028). That promise is only worth something if every path
// really does carry one — including the ones that surface from the pool rather
// than from a socket.
func sdkErrorCategories(c *runner.Ctx) error {
	if err := sdkNetworkAndTimeout(c); err != nil {
		return err
	}
	if err := sdkProtocolError(c); err != nil {
		return err
	}
	return sdkServerAuthAndClosed(c)
}

// sdkNetworkAndTimeout covers the two retryable categories.
func sdkNetworkAndTimeout(c *runner.Ctx) error {
	dead, err := deadPort()
	if err != nil {
		return err
	}
	refused, err := client.New(client.WithAddr(dead), client.WithReconnectWindow(0))
	if err != nil {
		return fmt.Errorf("building a client for a dead port: %w", err)
	}
	defer func() { _ = refused.Close() }()

	started := time.Now()
	err = refused.Ping(c.Context())
	if elapsed := time.Since(started); elapsed > failFastBudget {
		return fmt.Errorf("a refused dial took %s to surface", elapsed)
	}
	if categoryErr := assertCategory(c, "a dial to a port nothing is listening on",
		err, client.ErrNetwork, true); categoryErr != nil {
		return categoryErr
	}

	// A listener that accepts and then says nothing. The SDK's own read
	// deadline has to be what ends this, because the far end never will.
	hole, err := newBlackhole()
	if err != nil {
		return err
	}
	defer hole.close()

	silent, err := client.New(
		client.WithAddr(hole.addr()),
		client.WithDialTimeout(probeTimeout),
		client.WithReadTimeout(probeTimeout),
		client.WithReconnectWindow(0),
	)
	if err != nil {
		return fmt.Errorf("building a client for a silent listener: %w", err)
	}
	defer func() { _ = silent.Close() }()

	started = time.Now()
	err = silent.Ping(c.Context())
	if elapsed := time.Since(started); elapsed > failFastBudget {
		return fmt.Errorf("a read against a listener that never answers took %s to time out", elapsed)
	}
	return assertCategory(c, "a server that accepts and never answers", err, client.ErrTimeout, true)
}

// sdkProtocolError covers the category a correct server never produces.
//
// It is injected by a relay in front of the real server, which is the only
// honest way to produce one: the assertion is about what the SDK does with a
// stream it cannot parse, and a mock server would be asserting against the
// mock.
func sdkProtocolError(c *runner.Ctx) error {
	relay, err := newProxy(func() string { return c.Info().ClientAddr })
	if err != nil {
		return err
	}
	defer relay.close()

	sdk, err := sdkClient(c, client.WithAddr(relay.addr()), client.WithPoolSize(1))
	if err != nil {
		return err
	}
	defer func() { _ = sdk.Close() }()

	if setErr := sdk.Set(c.Context(), "sdk:protocol", []byte("v"), 0); setErr != nil {
		return fmt.Errorf("SET through the relay: %w", setErr)
	}

	relay.corruptNextReply()
	_, _, err = sdk.Get(c.Context(), "sdk:protocol")
	if categoryErr := assertCategory(c, "a reply the SDK cannot decode",
		err, client.ErrProtocol, false); categoryErr != nil {
		return categoryErr
	}

	// And the client still works: a protocol error costs the connection, not
	// the client.
	value, found, err := sdk.Get(c.Context(), "sdk:protocol")
	if err != nil {
		return fmt.Errorf("after a protocol error the client stopped working: %w", err)
	}
	if !found || string(value) != "v" {
		return fmt.Errorf("after a protocol error a GET answered (%q, %v)", value, found)
	}
	return nil
}

// sdkServerAuthAndClosed covers the three categories the server and the client's
// own lifecycle produce.
func sdkServerAuthAndClosed(c *runner.Ctx) error {
	token, err := specToken(c)
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("this scenario needs a spec with auth.token set, for the ErrAuth case")
	}

	sdk, err := sdkClient(c)
	if err != nil {
		return err
	}
	defer func() { _ = sdk.Close() }()

	_, err = sdk.Do(c.Context(), "NOSUCHCOMMAND")
	if categoryErr := assertCategory(c, "a command the server refuses",
		err, client.ErrServer, false); categoryErr != nil {
		return categoryErr
	}

	wrong, err := client.New(client.WithAddr(c.Info().ClientAddr), client.WithAuth("not-the-token"))
	if err != nil {
		return fmt.Errorf("building a client with a wrong token: %w", err)
	}
	defer func() { _ = wrong.Close() }()

	err = wrong.Ping(c.Context())
	if categoryErr := assertCategory(c, "a refused token", err, client.ErrAuth, false); categoryErr != nil {
		return categoryErr
	}

	closed, err := sdkClient(c)
	if err != nil {
		return err
	}
	if pingErr := closed.Ping(c.Context()); pingErr != nil {
		return fmt.Errorf("a client that is about to be closed could not ping: %w", pingErr)
	}
	if closeErr := closed.Close(); closeErr != nil {
		return fmt.Errorf("closing the client: %w", closeErr)
	}
	_, _, err = closed.Get(c.Context(), "anything")
	return assertCategory(c, "a call on a closed client", err, client.ErrClosed, false)
}

// sdkHonorsCancellation checks that a context is honored at every blocking
// point, and that honoring it costs nothing but the call.
//
// "Returns promptly" is the whole requirement. A call that ignores its context
// until the reply arrives makes every timeout in the calling service a lie, and
// it is always the one that hangs in production.
func sdkHonorsCancellation(c *runner.Ctx) error {
	relay, err := newProxy(func() string { return c.Info().ClientAddr })
	if err != nil {
		return err
	}
	defer relay.close()

	// One connection, so that a canceled call that leaked its connection would
	// leave the client with none — which is exactly the failure to catch.
	sdk, err := sdkClient(c, client.WithAddr(relay.addr()), client.WithPoolSize(1))
	if err != nil {
		return err
	}
	defer func() { _ = sdk.Close() }()

	if setErr := sdk.Set(c.Context(), "sdk:ctx", []byte("v"), 0); setErr != nil {
		return fmt.Errorf("SET through the relay: %w", setErr)
	}

	// A context that is already over. Nothing should reach the wire.
	canceled, cancel := context.WithCancel(c.Context())
	cancel()
	started := time.Now()
	_, _, err = sdk.Get(canceled, "sdk:ctx")
	if elapsed := time.Since(started); elapsed > cancelBudget {
		return fmt.Errorf("a call with an already-canceled context took %s to return", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		return fmt.Errorf("a call with an already-canceled context answered %v, want a context.Canceled", err)
	}

	// Canceled while the reply is in flight. The relay holds every reply for
	// longer than the cancellation, so the call is genuinely mid-flight.
	relay.setDelay(cancelDelay)
	midflight, cancelMidflight := context.WithCancel(c.Context())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancelMidflight()
	}()

	started = time.Now()
	_, _, err = sdk.Get(midflight, "sdk:ctx")
	elapsed := time.Since(started)
	cancelMidflight()
	if !errors.Is(err, context.Canceled) {
		return fmt.Errorf("a call canceled mid-flight answered %v, want a context.Canceled", err)
	}
	if elapsed > cancelBudget {
		return fmt.Errorf("a call canceled mid-flight took %s to return; cancellation must be honored at the read",
			elapsed)
	}
	c.Logf("a call canceled mid-flight returned in %s", elapsed.Round(time.Millisecond))

	// A deadline rather than a cancellation is a timeout, which is a different
	// answer to a different question: nobody asked for this call to stop, the
	// clock did.
	deadline, cancelDeadline := context.WithTimeout(c.Context(), 150*time.Millisecond)
	defer cancelDeadline()
	started = time.Now()
	_, _, err = sdk.Get(deadline, "sdk:ctx")
	if elapsed := time.Since(started); elapsed > cancelBudget {
		return fmt.Errorf("a call with a 150ms deadline took %s to return", elapsed)
	}
	if categoryErr := assertCategory(c, "a call whose deadline expired",
		err, client.ErrTimeout, true); categoryErr != nil {
		return categoryErr
	}

	// And the client is still usable on its single connection: the canceled
	// calls discarded theirs rather than leaving it half-read in the pool.
	relay.setDelay(0)
	value, found, err := sdk.Get(c.Context(), "sdk:ctx")
	if err != nil {
		return fmt.Errorf("after two canceled calls the client stopped working: %w", err)
	}
	if !found || string(value) != "v" {
		return fmt.Errorf("after two canceled calls a GET answered (%q, %v); a connection was reused mid-reply",
			value, found)
	}
	c.Logf("the pool recovered its only connection after both cancellations")
	return nil
}
