package scenarios

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/b3vet/atlascache/pkg/client"
	"github.com/b3vet/atlascache/test/e2e/runner"
)

func init() {
	runner.RegisterScenario("sdk_pool_reuses_and_bounds_connections", sdkPoolReusesAndBoundsConnections)
	runner.RegisterScenario("sdk_pool_discards_after_a_protocol_error", sdkPoolDiscardsAfterAProtocolError)
	runner.RegisterScenario("sdk_survives_a_server_restart", sdkSurvivesAServerRestart)
	runner.RegisterScenario("sdk_pool_churn_leaks_nothing", sdkPoolChurnLeaksNothing)
}

// Pool sizes and workloads. Small numbers, because what is being asserted is a
// ceiling rather than a throughput: a pool that opened one connection too many
// is as broken at three as it would be at three hundred, and a scenario that
// took a minute to say so would be run less often.
const (
	poolSize        = 3
	poolCalls       = 60
	poolWorkers     = 8
	reconnectWindow = 20 * time.Second

	// recoveryBudget is how long the client has to be working again once the
	// server is. The reconnect window above is deliberately longer than any
	// restart, so this is what turns "never recovers" into a failure rather
	// than into a scenario that waits out the whole window.
	recoveryBudget = 5 * time.Second

	// outage is how long the server is down for, and afterOutage is how long
	// the workload keeps going once it is back. The outage is many times the
	// 20ms gap between calls, so calls certainly arrive while there is nothing
	// listening.
	outage      = 400 * time.Millisecond
	afterOutage = 300 * time.Millisecond

	// closeBudget is how long a closed client has to have released everything
	// it held. Closing is asynchronous at both ends, so this polls rather than
	// asserting the instant Close returns.
	closeBudget = 3 * time.Second
)

// sdkPoolReusesAndBoundsConnections checks the two halves of a fixed-size pool:
// it reuses what it has, and it never opens more than it was given.
//
// Both are counted at the relay rather than inferred from the server's
// counters, so the number is the SDK's own dials and nothing else — the runner
// holds a connection of its own, and any other spec running in parallel holds
// more.
func sdkPoolReusesAndBoundsConnections(c *runner.Ctx) error {
	relay, err := newProxy(func() string { return c.Info().ClientAddr })
	if err != nil {
		return err
	}
	defer relay.close()

	sdk, err := sdkClient(c, client.WithAddr(relay.addr()), client.WithPoolSize(poolSize))
	if err != nil {
		return err
	}
	defer func() { _ = sdk.Close() }()

	// Sequential calls reuse one connection: a pool that dialed per call would
	// show sixty here, and would be a connection storm in front of any server.
	for i := range poolCalls {
		key := fmt.Sprintf("sdk:pool:%d", i)
		if err := sdk.Set(c.Context(), key, []byte("v"), 0); err != nil {
			return fmt.Errorf("SET %s: %w", key, err)
		}
	}
	if dials := relay.connections(); dials != 1 {
		return fmt.Errorf("%d sequential calls opened %d connections; a pool reuses one", poolCalls, dials)
	}

	// Concurrent callers may open up to the pool size, and must not open a
	// connection past it however many of them there are.
	var group sync.WaitGroup
	failures := make(chan error, poolWorkers)
	for worker := range poolWorkers {
		group.Add(1)
		go func() {
			defer group.Done()
			for i := range 10 {
				key := fmt.Sprintf("sdk:pool:%d:%d", worker, i)
				if err := sdk.Set(c.Context(), key, []byte("v"), 0); err != nil {
					failures <- fmt.Errorf("worker %d: %w", worker, err)
					return
				}
				if _, _, err := sdk.Get(c.Context(), key); err != nil {
					failures <- fmt.Errorf("worker %d: %w", worker, err)
					return
				}
			}
		}()
	}
	group.Wait()
	close(failures)
	if err := <-failures; err != nil {
		return err
	}

	dials := relay.connections()
	if dials > poolSize {
		return fmt.Errorf("%d concurrent workers opened %d connections against a pool of %d",
			poolWorkers, dials, poolSize)
	}
	c.Logf("%d workers and %d calls ran on %d connections", poolWorkers, poolCalls+poolWorkers*20, dials)

	return sdkPoolBlocksWhenExhausted(c, relay)
}

// sdkPoolBlocksWhenExhausted checks what an exhausted pool does to the caller
// that arrives next.
//
// It must wait, bounded by its own context, rather than causing another
// connection to be opened. A pool that grew under pressure would turn a leaked
// connection into memory exhaustion; one that blocks turns the same leak into
// timeouts, which name the caller that is holding them (FEAT-0027).
func sdkPoolBlocksWhenExhausted(c *runner.Ctx, relay *proxy) error {
	single, err := sdkClient(c, client.WithAddr(relay.addr()), client.WithPoolSize(1))
	if err != nil {
		return err
	}
	defer func() { _ = single.Close() }()

	if setErr := single.Set(c.Context(), "sdk:exhaust", []byte("v"), 0); setErr != nil {
		return fmt.Errorf("SET before exhausting the pool: %w", setErr)
	}
	before := relay.connections()

	// The relay holds every reply, so the one connection this client has is
	// genuinely busy for the length of the call rather than briefly.
	relay.setDelay(time.Second)
	defer relay.setDelay(0)

	busy := make(chan error, 1)
	go func() {
		_, _, getErr := single.Get(c.Context(), "sdk:exhaust")
		busy <- getErr
	}()

	// Long enough that the call above has certainly taken the connection.
	if sleepErr := c.Sleep(150 * time.Millisecond); sleepErr != nil {
		return sleepErr
	}

	waiter, cancel := context.WithTimeout(c.Context(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _, err = single.Get(waiter, "sdk:exhaust")
	waited := time.Since(started)

	if !errors.Is(err, client.ErrTimeout) {
		return fmt.Errorf("a caller arriving at an exhausted pool answered %v, want an ErrTimeout", err)
	}
	if waited > time.Second {
		return fmt.Errorf("a caller with a 200ms deadline waited %s for a connection", waited)
	}
	if dials := relay.connections(); dials != before {
		return fmt.Errorf("an exhausted pool opened %d more connections; it must block, not grow", dials-before)
	}

	if err := <-busy; err != nil {
		return fmt.Errorf("the call holding the connection failed: %w", err)
	}
	c.Logf("a caller at an exhausted pool waited %s for its own deadline and no connection was opened",
		waited.Round(time.Millisecond))
	return nil
}

// sdkPoolDiscardsAfterAProtocolError is the case FEAT-0027 names as the one
// with real consequences.
//
// A connection that has hit a protocol error is at an unknown stream position.
// Returning it to the pool means the next caller reads what was left behind —
// one caller receiving another caller's data, which is a disclosure bug rather
// than a performance one. The relay leaves the real reply in the stream behind
// the corruption, so a client that reused the connection would answer this
// scenario's second GET with the first GET's value, and say nothing.
func sdkPoolDiscardsAfterAProtocolError(c *runner.Ctx) error {
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

	const (
		firstKey    = "sdk:discard:first"
		secondKey   = "sdk:discard:second"
		firstValue  = "the first caller's value"
		secondValue = "the second caller's value"
	)
	for key, value := range map[string]string{firstKey: firstValue, secondKey: secondValue} {
		if setErr := sdk.SetString(c.Context(), key, value, 0); setErr != nil {
			return fmt.Errorf("seeding %s: %w", key, setErr)
		}
	}
	before := relay.connections()

	relay.corruptNextReply()
	_, _, err = sdk.Get(c.Context(), firstKey)
	if !errors.Is(err, client.ErrProtocol) {
		return fmt.Errorf("a corrupted reply answered %v, want an ErrProtocol", err)
	}

	value, found, err := sdk.Get(c.Context(), secondKey)
	if err != nil {
		return fmt.Errorf("the call after a protocol error failed: %w", err)
	}
	if !found {
		return fmt.Errorf("the call after a protocol error found no %s", secondKey)
	}
	if bytes.Equal(value, []byte(firstValue)) {
		return errors.New("a caller was handed the previous caller's reply; the connection was reused after a protocol error")
	}
	if string(value) != secondValue {
		return fmt.Errorf("the call after a protocol error answered %q, want %q", value, secondValue)
	}

	if dials := relay.connections(); dials <= before {
		return errors.New("the connection that saw a protocol error was reused rather than discarded")
	}
	c.Logf("the poisoned connection was discarded and the next caller got its own reply")
	return nil
}

// sdkSurvivesAServerRestart checks that a restart is a pause inside one call
// rather than a run of failures across many.
//
// The client is built once, against an address that does not change, and is
// never rebuilt — which is the point. The server's ports do change across a
// restart, so the relay in front of it is what keeps the client's address
// stable, exactly as a load balancer or a DNS name would in the deployment this
// models.
func sdkSurvivesAServerRestart(c *runner.Ctx) error {
	relay, err := newProxy(func() string { return c.Info().ClientAddr })
	if err != nil {
		return err
	}
	defer relay.close()

	sdk, err := sdkClient(c,
		client.WithAddr(relay.addr()),
		client.WithPoolSize(1),
		client.WithReconnectWindow(reconnectWindow),
	)
	if err != nil {
		return err
	}
	defer func() { _ = sdk.Close() }()

	if setErr := sdk.SetString(c.Context(), "sdk:restart", "before", 0); setErr != nil {
		return fmt.Errorf("SET before the restart: %w", setErr)
	}
	before := relay.connections()

	load := startWorkload(c, sdk)
	restartErr := holdTheServerDown(c)

	// The workload keeps running for a moment after the server is back, so
	// that calls land on the far side of the outage as well as inside it.
	// Without this the scenario would be timing the restart rather than
	// watching a client recover from one.
	if restartErr == nil {
		restartErr = c.Sleep(afterOutage)
	}
	calls, surfaced, lastErr := load.stop()

	if restartErr != nil {
		return fmt.Errorf("restarting the server: %w", restartErr)
	}

	// The client really did re-dial. Without this the scenario would pass
	// against a restart so quick that no call ever met it, which would be a
	// green light for a client that cannot reconnect at all.
	if dials := relay.connections(); dials <= before {
		return fmt.Errorf("the client made no new connection across the restart (%d before, %d after); nothing was reconnected",
			before, dials)
	}

	if surfaced > 1 {
		return fmt.Errorf("a restart surfaced %d errors over %d calls; a replaced connection should cost at most one (last: %v)",
			surfaced, calls, lastErr)
	}
	c.Logf("%d calls spanned the restart, %d of them surfaced an error", calls, surfaced)

	// The client that was built before the restart still works, without having
	// been told anything about it.
	recovery, cancelRecovery := context.WithTimeout(c.Context(), recoveryBudget)
	defer cancelRecovery()

	if setErr := sdk.SetString(recovery, "sdk:restart", "after", 0); setErr != nil {
		return fmt.Errorf("the client did not recover from the restart within %s: %w", recoveryBudget, setErr)
	}
	value, found, err := sdk.GetString(recovery, "sdk:restart")
	if err != nil {
		return fmt.Errorf("GET after the restart: %w", err)
	}
	if !found || value != "after" {
		return fmt.Errorf("GET after the restart answered (%q, %v)", value, found)
	}
	return nil
}

// holdTheServerDown stops the server, leaves it down long enough that calls
// genuinely arrive while there is nothing to answer them, and starts it again.
//
// A restart done as one step would be over in milliseconds, and a client that
// could not reconnect at all would pass by never meeting the outage. The
// outage has to outlast the gap between calls for the scenario to be about
// reconnection rather than about timing.
func holdTheServerDown(c *runner.Ctx) error {
	if err := c.Stop(); err != nil {
		return fmt.Errorf("stopping the server: %w", err)
	}
	c.Logf("server stopped; holding it down for %s", outage)

	if err := c.Sleep(outage); err != nil {
		return err
	}
	if err := c.Start(); err != nil {
		return fmt.Errorf("starting the server again: %w", err)
	}
	c.Logf("server started again")
	return nil
}

// workload is a stream of calls running while something is done to the server
// under it, and the tally of what they saw.
type workload struct {
	calls    atomic.Int64
	failures atomic.Int64
	lastErr  atomic.Value

	cancel context.CancelFunc
	done   chan struct{}
}

// startWorkload begins writing to the server every 20ms until it is stopped.
//
// A call canceled by the stop is not a failure — the caller went away, which is
// not the server's doing — so only errors that arrive while the workload is
// still meant to be running are counted.
func startWorkload(c *runner.Ctx, sdk client.Client) *workload {
	ctx, cancel := context.WithCancel(c.Context())
	load := &workload{cancel: cancel, done: make(chan struct{})}

	go func() {
		defer close(load.done)
		for ctx.Err() == nil {
			load.calls.Add(1)
			if err := sdk.SetString(ctx, "sdk:restart", "during", 0); err != nil {
				if ctx.Err() != nil {
					return
				}
				load.failures.Add(1)
				load.lastErr.Store(err)
			}
			if sleepErr := sleepCtx(ctx, 20*time.Millisecond); sleepErr != nil {
				return
			}
		}
	}()
	return load
}

// stop ends the workload and reports what it saw.
func (w *workload) stop() (calls, failures int64, lastErr any) {
	w.cancel()
	<-w.done
	return w.calls.Load(), w.failures.Load(), w.lastErr.Load()
}

// sdkPoolChurnLeaksNothing is the soak: clients built and closed, and callers
// coming and going, for long enough that a connection leaked once per round
// would be unmistakable.
//
// The count is taken at the relay, which sees every connection the SDK opens
// and every one it closes. A leak there is a leak in the SDK regardless of what
// any other spec running alongside this one is doing — and a client that leaked
// one connection per round would hold the server's connection limit long before
// anything else noticed.
func sdkPoolChurnLeaksNothing(c *runner.Ctx) error {
	relay, err := newProxy(func() string { return c.Info().ClientAddr })
	if err != nil {
		return err
	}
	defer relay.close()

	const rounds = 15

	for round := range rounds {
		sdk, clientErr := sdkClient(c, client.WithAddr(relay.addr()), client.WithPoolSize(poolSize))
		if clientErr != nil {
			return clientErr
		}

		var group sync.WaitGroup
		failures := make(chan error, poolWorkers)
		for worker := range poolWorkers {
			group.Add(1)
			go func() {
				defer group.Done()
				key := fmt.Sprintf("sdk:soak:%d:%d", round, worker)
				for range 25 {
					if err := sdk.Set(c.Context(), key, binaryValue, time.Minute); err != nil {
						failures <- err
						return
					}
					if _, _, err := sdk.Get(c.Context(), key); err != nil {
						failures <- err
						return
					}
					if _, err := sdk.Del(c.Context(), key); err != nil {
						failures <- err
						return
					}
				}
			}()
		}
		group.Wait()
		close(failures)
		if err := <-failures; err != nil {
			_ = sdk.Close()
			return fmt.Errorf("round %d: %w", round, err)
		}

		if err := sdk.Close(); err != nil {
			return fmt.Errorf("round %d: Close: %w", round, err)
		}

		// Close returns every connection the client held. Anything still open a
		// moment later was leaked, not lingering.
		if err := waitForNoOpenConnections(c.Context(), relay); err != nil {
			return fmt.Errorf("round %d: %w", round, err)
		}
		c.Logf("round %d: %d connections opened in total, none still open", round, relay.connections())
	}

	// Total dials are bounded by the pool size per round. A client that dialed
	// per call would be in the thousands by now.
	if dials := relay.connections(); dials > rounds*poolSize {
		return fmt.Errorf("%d rounds of a pool of %d opened %d connections", rounds, poolSize, dials)
	}
	return nil
}

// waitForNoOpenConnections waits for the relay to see every connection closed.
// Closing is asynchronous at both ends, so this polls for the condition rather
// than asserting it the instant Close returns.
// It takes a plain context rather than the scenario's own so that it can be
// tested directly: a leak detector that has never been seen to fire is not a
// leak detector.
func waitForNoOpenConnections(ctx context.Context, relay *proxy) error {
	deadline := time.Now().Add(closeBudget)
	for {
		open := relay.open()
		if open == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d connections were still open %s after the client was closed", open, closeBudget)
		}
		if err := sleepCtx(ctx, 20*time.Millisecond); err != nil {
			return fmt.Errorf("%d connections were still open when the run ended: %w", open, err)
		}
	}
}

// sleepCtx waits for d unless the context ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
