package scenarios

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/b3vet/atlascache/test/e2e/client"
	"github.com/b3vet/atlascache/test/e2e/runner"
)

func init() {
	runner.RegisterScenario("conn_pipeline_returns_in_order", connPipelineReturnsInOrder)
	runner.RegisterScenario("conn_pipeline_retains_a_partial_frame", connPipelineRetainsAPartialFrame)
	runner.RegisterScenario("conn_limit_reports_then_closes", connLimitReportsThenCloses)
	runner.RegisterScenario("conn_idle_timeout_reaps_the_silent", connIdleTimeoutReapsTheSilent)
	runner.RegisterScenario("conn_idle_timeout_spares_the_busy", connIdleTimeoutSparesTheBusy)
	runner.RegisterScenario("conn_dribbling_does_not_defeat_the_timeout", connDribblingDoesNotDefeatTheTimeout)
	runner.RegisterScenario("conn_request_budget_costs_nothing", connRequestBudgetCostsNothing)
	runner.RegisterScenario("conn_output_cap_disconnects_a_silent_reader", connOutputCapDisconnectsASilentReader)
	runner.RegisterScenario("conn_drain_under_load_exits_in_time", connDrainUnderLoadExitsInTime)
	runner.RegisterScenario("conn_churn_leaks_nothing", connChurnLeaksNothing)
	runner.RegisterScenario("compat_inline_lf_bulk_load", compatInlineLFBulkLoad)
}

// FEAT-0024 at the level a client meets it: what the server does with many
// connections, pipelined commands, silent clients, clients that stop reading,
// and a shutdown arriving in the middle of all of it.
//
// Everything here uses a raw socket rather than the E2E client, for the same
// reason the protocol scenarios do: the client sends one command and reads one
// reply, and every property below is about what happens when a client does
// something else.

// pipelineDepth is how many commands go out in one write. A thousand is the
// figure FEAT-0024 names, and it is also enough that a server answering one
// syscall at a time is visibly slower than one batching.
const pipelineDepth = 1000

// connPipelineReturnsInOrder is the acceptance criterion: a thousand commands
// written without waiting come back in the order they were asked for.
//
// Ordering is the property a client cannot check for itself — it matches
// replies to requests positionally, so a reordered reply is not an error, it is
// a wrong answer to a different question. Each command therefore carries its
// own index and the check is on the payload rather than on the count.
func connPipelineReturnsInOrder(c *runner.Ctx) error {
	conn, err := dialRaw(c)
	if err != nil {
		return err
	}
	defer conn.close()

	var batch strings.Builder
	for i := range pipelineDepth {
		fmt.Fprintf(&batch, "*2\r\n$4\r\nECHO\r\n$%d\r\n%d\r\n", len(strconv.Itoa(i)), i)
	}

	started := time.Now()
	if _, writeErr := conn.write([]byte(batch.String())); writeErr != nil {
		return fmt.Errorf("writing %d pipelined commands: %w", pipelineDepth, writeErr)
	}

	// Two lines per bulk reply: the length header, then the payload.
	lines, err := conn.readLines(2 * pipelineDepth)
	if err != nil {
		return fmt.Errorf("reading the replies to %d pipelined commands: %w", pipelineDepth, err)
	}
	elapsed := time.Since(started)

	for i := range pipelineDepth {
		if got := lines[2*i+1]; got != strconv.Itoa(i) {
			return fmt.Errorf("reply %d is %q; the replies are out of order", i, got)
		}
	}
	c.Logf("%d pipelined commands answered in order in %s", pipelineDepth, elapsed.Round(time.Millisecond))

	// The connection is still good afterwards: a batch is a batch, not the end
	// of the conversation.
	if err := conn.pingOver("\r\n"); err != nil {
		return fmt.Errorf("after the batch: %w", err)
	}
	return nil
}

// connPipelineRetainsAPartialFrame is the subtlety FEAT-0024 names: a client
// that pipelines and then waits.
//
// Two complete commands and the first two bytes of a third is what a TCP
// segment boundary looks like from the server's side. A server that kept
// decoding while anything was buffered would block inside the third command
// holding the replies to the first two, and the client — which is waiting for
// exactly those — would never send the rest. Both sides would wait forever.
func connPipelineRetainsAPartialFrame(c *runner.Ctx) error {
	conn, err := dialRaw(c)
	if err != nil {
		return err
	}
	defer conn.close()

	if _, writeErr := conn.write([]byte("*1\r\n$4\r\nPING\r\n*1\r\n$4\r\nPING\r\n*1\r\n$4\r\nPI")); writeErr != nil {
		return fmt.Errorf("writing two commands and a partial third: %w", writeErr)
	}

	lines, err := conn.readLines(2)
	if err != nil {
		return fmt.Errorf("the two complete commands were not answered while a third was partial: %w", err)
	}
	if lines[0] != "+"+pong || lines[1] != "+"+pong {
		return fmt.Errorf("the two complete commands answered %q", lines)
	}
	c.Logf("both complete commands were answered with a partial frame still in the buffer")

	// The retained frame is completed rather than resynchronized: the tail
	// finishes the PING it began, and is not read as a fresh request.
	if _, writeErr := conn.write([]byte("NG\r\n")); writeErr != nil {
		return fmt.Errorf("completing the partial frame: %w", writeErr)
	}
	completed, err := conn.readLine()
	if err != nil {
		return fmt.Errorf("the completed frame was not answered: %w", err)
	}
	if completed != "+"+pong {
		return fmt.Errorf("the completed frame answered %q, want +%s", completed, pong)
	}
	return nil
}

// maxClientsError is what a connection over the limit must be told. Redis's
// wording, because clients match on it.
const maxClientsError = "-ERR max number of clients reached"

// maxReachableLimit is the largest max_connections this scenario will try to
// fill. Beyond it the test machine runs out of ephemeral ports long before the
// server runs out of slots, and the failure says nothing about the server.
const maxReachableLimit = 500

// connLimitReportsThenCloses checks that the limit is enforced by telling the
// client rather than by refusing to accept.
//
// The difference matters to whoever is paged. A connection refused at accept
// reaches the client as ECONNREFUSED, which is what a server that is down looks
// like; this way the client is told it hit a limit, and the operator is told
// which limit.
func connLimitReportsThenCloses(c *runner.Ctx) error {
	// The spec sets the limit; the scenario finds it rather than repeating it,
	// so the two cannot disagree.
	limit, err := infoInt(c, "maxclients")
	if err != nil {
		return err
	}
	c.Logf("the server reports maxclients=%d", limit)
	if limit > maxReachableLimit {
		return fmt.Errorf(
			"the server reports maxclients=%d; a limit that high cannot be reached from one machine, "+
				"so this spec must configure a smaller one", limit)
	}

	before, err := infoInt(c, "rejected_connections")
	if err != nil {
		return err
	}

	held, refused, err := openUntilRefused(c, limit+4)
	// Every held connection goes before anything else is asked of the server:
	// the limit is full while they are open, so a command sent now would be
	// refused by the very limit under test.
	closeAll(held)
	if err != nil {
		return err
	}

	if refused == 0 {
		return fmt.Errorf("opened %d connections against a limit of %d and none was refused", limit+4, limit)
	}
	if len(held) > limit {
		return fmt.Errorf("%d connections were served against a limit of %d", len(held), limit)
	}
	c.Logf("%d connections served, %d refused with %q", len(held), refused, maxClientsError)

	// Closing them gives the slots back: the limit is a live count, not a
	// high-water mark.
	if slotErr := waitForASlot(c); slotErr != nil {
		return slotErr
	}

	rejected, err := infoInt(c, "rejected_connections")
	if err != nil {
		return err
	}
	if rejected-before < refused {
		return fmt.Errorf("INFO reports %d rejected connections after %d refusals", rejected-before, refused)
	}
	return nil
}

// openUntilRefused opens connections until it has tried attempts of them,
// keeping the ones that were served and checking that every refusal says why
// and then closes.
func openUntilRefused(c *runner.Ctx, attempts int) (held []*rawConn, refused int, err error) {
	for range attempts {
		conn, dialErr := dialRaw(c)
		if dialErr != nil {
			return held, refused, fmt.Errorf(
				"a connection over the limit must still be accepted, so it can be told why: %w", dialErr)
		}

		served, line, probeErr := conn.probe()
		switch {
		case probeErr != nil:
			conn.close()
			return held, refused, probeErr

		case served:
			held = append(held, conn)

		case line == maxClientsError:
			refused++
			closeErr := conn.expectClosed()
			conn.close()
			if closeErr != nil {
				return held, refused, fmt.Errorf("a refused connection was left open: %w", closeErr)
			}

		default:
			conn.close()
			return held, refused, fmt.Errorf("a connection over the limit answered %q, want %q",
				line, maxClientsError)
		}
	}
	return held, refused, nil
}

// waitForASlot waits for the server to serve a new connection again, which it
// can only do once the handlers for the closed ones have noticed.
func waitForASlot(c *runner.Ctx) error {
	deadline := time.Now().Add(rawTimeout)
	for time.Now().Before(deadline) {
		if err := freshConnectionServes(c); err == nil {
			c.Logf("the slots came back once the connections closed")
			return nil
		}
		if err := c.Sleep(50 * time.Millisecond); err != nil {
			return err
		}
	}
	return errors.New("the connection slots were never given back after every client closed")
}

func closeAll(conns []*rawConn) {
	for _, conn := range conns {
		conn.close()
	}
}

// idleArmingSkew is how much earlier than this scenario's clock the server's
// own may have started. It arms the deadline when it accepts the connection,
// which is before the dial returns here; a few milliseconds of that is not a
// server closing early, and the assertion below is about a server that closes
// at once rather than one that closes a moment sooner than expected.
const idleArmingSkew = 250 * time.Millisecond

// connIdleTimeoutReapsTheSilent is the gap ISSUE-0018 found alongside the
// request budget: a connection that opens and says nothing held a goroutine and
// two buffers for as long as the process lived.
func connIdleTimeoutReapsTheSilent(c *runner.Ctx) error {
	timeout, err := idleTimeout(c)
	if err != nil {
		return err
	}

	conn, err := dialRaw(c)
	if err != nil {
		return err
	}
	defer conn.close()

	started := time.Now()
	if closeErr := conn.waitForClose(3 * timeout); closeErr != nil {
		return fmt.Errorf("a connection that sent nothing was not reaped: %w", closeErr)
	}
	elapsed := time.Since(started)

	if elapsed+idleArmingSkew < timeout {
		return fmt.Errorf("the connection was closed after %s, inside the %s idle timeout", elapsed, timeout)
	}
	c.Logf("a silent connection was closed %s after opening, against a %s timeout", elapsed.Round(time.Millisecond), timeout)

	closed, err := infoInt(c, "atlascache_idle_closed")
	if err != nil {
		return err
	}
	if closed < 1 {
		return errors.New("the idle disconnect was not recorded, so it is not attributable")
	}
	return nil
}

// connIdleTimeoutSparesTheBusy. The clock restarts per command; without that
// the timeout is a connection lifetime and every long-lived client is dropped
// mid-conversation.
func connIdleTimeoutSparesTheBusy(c *runner.Ctx) error {
	timeout, err := idleTimeout(c)
	if err != nil {
		return err
	}

	conn, err := dialRaw(c)
	if err != nil {
		return err
	}
	defer conn.close()

	deadline := time.Now().Add(3 * timeout)
	commands := 0
	for time.Now().Before(deadline) {
		if _, err := conn.write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
			return fmt.Errorf("after %s of commands every %s, the connection was gone: %w",
				timeout, timeout/3, err)
		}
		line, readErr := conn.readLine()
		if readErr != nil {
			return fmt.Errorf("after %d commands the connection was closed: %w", commands, readErr)
		}
		if line != "+"+pong {
			return fmt.Errorf("PING answered %q", line)
		}
		commands++

		if err := c.Sleep(timeout / 3); err != nil {
			return err
		}
	}

	c.Logf("%d commands over %s kept a connection alive through a %s idle timeout",
		commands, (3 * timeout).Round(time.Second), timeout)
	return nil
}

// connDribblingDoesNotDefeatTheTimeout is why the clock is refreshed per
// command and not per byte.
//
// A client sending one byte at a time never completes a request, so there is
// nothing the server owes it — but a timeout armed on read activity would be
// pushed out by every byte and would never fire. This client is precisely the
// one the timeout exists for.
func connDribblingDoesNotDefeatTheTimeout(c *runner.Ctx) error {
	timeout, err := idleTimeout(c)
	if err != nil {
		return err
	}

	conn, err := dialRaw(c)
	if err != nil {
		return err
	}
	defer conn.close()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(timeout / 10)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				// One byte of a command that is never terminated.
				if _, err := conn.write([]byte("P")); err != nil {
					return
				}
			}
		}
	}()

	started := time.Now()
	if err := conn.waitForClose(4 * timeout); err != nil {
		return fmt.Errorf("a client dribbling bytes without issuing a command was not reaped: %w", err)
	}
	c.Logf("a byte-dribbling connection was closed %s after opening, against a %s timeout",
		time.Since(started).Round(time.Millisecond), timeout)
	return nil
}

// budgetProbeRounds is how many maximal requests the flood sends. Enough that
// an unbounded cost is unmistakable and few enough that a bounded one is quick.
const budgetProbeRounds = 60

// connRequestBudgetCostsNothing is ISSUE-0018, measured from outside the
// process.
//
// The instrument is the point. Every per-field limit returned the right answer
// for the request that found this — 7MB sent, 154MB allocated — so reading the
// reply proves nothing; that is ISSUE-0016's lesson and this issue was found by
// measuring rather than by reading. So the server's resident set is sampled
// with ps while the flood is in flight.
//
// Two shapes are flooded. The first is the request the fuzzing found, which the
// element cap now refuses on its header. The second is the largest request this
// configuration still accepts, which is the one that has to be bounded rather
// than refused: a limit that only rejects proves nothing about what accepting
// costs.
func connRequestBudgetCostsNothing(c *runner.Ctx) error {
	baseline, err := residentBytes(c)
	if err != nil {
		return err
	}
	c.Logf("the server holds %d bytes resident before the flood", baseline)

	// The shape from the issue: a million one-byte elements, inside every
	// per-field limit.
	refusedPeak, err := floodWith(c, "*1048576\r\n$3\r\nDEL\r\n", budgetProbeRounds, baseline)
	if err != nil {
		return fmt.Errorf("flooding with million-element headers: %w", err)
	}
	c.Logf("%d million-element requests refused: resident set peaked at %d bytes, %+d on the baseline",
		budgetProbeRounds, refusedPeak, refusedPeak-baseline)

	// A request past the byte budget, which is refused while it is still
	// arriving rather than after it has all been read.
	overBudget, err := oversizedRequest(c)
	if err != nil {
		return err
	}
	acceptedPeak, err := floodWith(c, overBudget, budgetProbeRounds, baseline)
	if err != nil {
		return fmt.Errorf("flooding with over-budget requests: %w", err)
	}
	c.Logf("%d over-budget requests refused: resident set peaked at %d bytes, %+d on the baseline",
		budgetProbeRounds, acceptedPeak, acceptedPeak-baseline)

	peak := max(refusedPeak, acceptedPeak)
	if peak-baseline > allocationHeadroom {
		return fmt.Errorf(
			"flooding with maximal requests grew the resident set by %d bytes, past the %d byte headroom: "+
				"the cost of one request is not bounded by what it took to send it",
			peak-baseline, allocationHeadroom)
	}

	return pingThroughHarness(c)
}

// oversizedRequest builds a request whose elements are each well inside the
// bulk limit, whose count is well inside the element limit, and which together
// is past the byte budget. That combination is ISSUE-0018 in one request: every
// field legal, the product not.
func oversizedRequest(c *runner.Ctx) (string, error) {
	budget, err := infoInt(c, "atlascache_max_request_size")
	if err != nil {
		return "", err
	}
	if budget <= 0 {
		return "", fmt.Errorf(
			"the server reports a request budget of %d; nothing bounds what one request costs it "+
				"and this scenario needs a budget to measure against (ISSUE-0018)", budget)
	}

	const key = "0123456789abcdef0123456789abcdef"
	elements := 4 * budget / (len(key) + 8)

	var request strings.Builder
	fmt.Fprintf(&request, "*%d\r\n$3\r\nDEL\r\n", elements)
	for range elements - 1 {
		fmt.Fprintf(&request, "$%d\r\n%s\r\n", len(key), key)
	}
	c.Logf("the over-budget request is %d bytes of %d elements against a %d byte budget",
		request.Len(), elements, budget)
	return request.String(), nil
}

// floodWith sends one request shape repeatedly, each on its own connection,
// sampling the resident set between rounds.
//
// Each round gets a fresh connection because the requests are all refused and a
// refusal closes the connection; reusing one would measure the reconnect rather
// than the request.
func floodWith(c *runner.Ctx, request string, rounds, baseline int) (int, error) {
	peak := baseline
	for round := range rounds {
		conn, err := dialRaw(c)
		if err != nil {
			return peak, fmt.Errorf("round %d: %w", round, err)
		}

		// The write may fail part way: the server refuses the request while it
		// is arriving, which is the behavior being checked, and a client still
		// writing into a closed socket sees that as an error.
		pushIgnoringRefusal(conn, request)

		line, readErr := conn.readLine()
		if readErr != nil {
			conn.close()
			return peak, fmt.Errorf("round %d: no reply to a maximal request: %w", round, readErr)
		}
		if !strings.HasPrefix(line, protoErrPrefix) {
			conn.close()
			return peak, fmt.Errorf("round %d: a maximal request answered %q, want a protocol error", round, line)
		}
		conn.close()

		resident, err := residentBytes(c)
		if err != nil {
			return peak, err
		}
		peak = max(peak, resident)
	}
	return peak, nil
}

// pushIgnoringRefusal writes a request the server is expected to refuse while it
// is still arriving, so a write error is the point rather than a failure.
func pushIgnoringRefusal(conn *rawConn, request string) {
	if _, err := conn.write([]byte(request)); err != nil {
		return
	}
}

// connOutputCapDisconnectsASilentReader is the cap moved in from P6.
//
// KEYS on a large keyspace builds a reply whose size the client chose and the
// server pays for, and a client is under no obligation to read it. Without a
// cap that is unbounded buffering on an unauthenticated port; with one the
// connection is closed and the reason is recorded.
func connOutputCapDisconnectsASilentReader(c *runner.Ctx) error {
	const keys = 4000

	if err := writeManyKeys(c, keys); err != nil {
		return err
	}

	baseline, err := residentBytes(c)
	if err != nil {
		return err
	}

	conn, err := dialRaw(c)
	if err != nil {
		return err
	}
	defer conn.close()

	// Ask for the whole keyspace, repeatedly, and never read a byte of it.
	if _, writeErr := conn.write([]byte(strings.Repeat("*2\r\n$4\r\nKEYS\r\n$1\r\n*\r\n", 64))); writeErr != nil {
		return fmt.Errorf("asking for the keyspace: %w", writeErr)
	}

	if closeErr := conn.waitForCloseWithoutReading(rawTimeout); closeErr != nil {
		return fmt.Errorf("a client that stopped reading was not disconnected: %w", closeErr)
	}

	resident, err := residentBytes(c)
	if err != nil {
		return err
	}
	c.Logf("a client that asked for %d keys 64 times and read none of it was disconnected; "+
		"the resident set moved %+d bytes", keys, resident-baseline)

	if resident-baseline > allocationHeadroom {
		return fmt.Errorf("buffering for a client that would not read grew the resident set by %d bytes",
			resident-baseline)
	}

	closed, err := infoInt(c, "atlascache_output_limit_closed")
	if err != nil {
		return err
	}
	stalled, err := infoInt(c, "atlascache_stalled_closed")
	if err != nil {
		return err
	}
	if closed+stalled < 1 {
		return errors.New("the disconnect was not recorded as an output limit or a stalled client")
	}
	c.Logf("recorded as %d over the output limit and %d stalled", closed, stalled)

	return freshConnectionServes(c)
}

// writeManyKeys writes n keys through one connection, for the scenarios that
// need a keyspace larger than a spec can write a step at a time.
func writeManyKeys(c *runner.Ctx, n int) error {
	conn, err := client.Dial(c.Context(), c.Info().ClientAddr)
	if err != nil {
		return fmt.Errorf("dialing %s: %w", c.Info().ClientAddr, err)
	}
	defer func() { _ = conn.Close() }()

	for i := range n {
		key := fmt.Sprintf("bulk:%s:%06d", strings.Repeat("k", 48), i)
		if _, err := conn.Call(c.Context(), "SET", key, "v"); err != nil {
			return fmt.Errorf("writing key %d of %d: %w", i, n, err)
		}
	}
	return nil
}

// connDrainUnderLoadExitsInTime. A shutdown with nothing happening is the easy
// case and shutdown-graceful already covers it; this one arrives while every
// connection is mid-pipeline, which is when a drain that waits for the wrong
// thing hangs.
func connDrainUnderLoadExitsInTime(c *runner.Ctx) error {
	const (
		clients = 20
		depth   = 200
	)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		errored int
	)
	stop := make(chan struct{})

	for range clients {
		conn, err := dialRaw(c)
		if err != nil {
			close(stop)
			wg.Wait()
			return fmt.Errorf("opening a load connection: %w", err)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.close()

			batch := strings.Repeat("*1\r\n$4\r\nPING\r\n", depth)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := conn.write([]byte(batch)); err != nil {
					mu.Lock()
					errored++
					mu.Unlock()
					return
				}
				if _, err := conn.readLines(depth); err != nil {
					mu.Lock()
					errored++
					mu.Unlock()
					return
				}
			}
		}()
	}

	// Let the load establish itself, so the signal really does land mid-flight.
	if err := c.Sleep(500 * time.Millisecond); err != nil {
		close(stop)
		wg.Wait()
		return err
	}

	started := time.Now()
	stopErr := c.Stop()
	elapsed := time.Since(started)

	close(stop)
	wg.Wait()

	if stopErr != nil {
		return fmt.Errorf("SIGTERM under load: %w", stopErr)
	}
	if elapsed > drainBudget {
		return fmt.Errorf("the server took %s to exit with %d connections pipelining, over the %s budget",
			elapsed.Round(time.Millisecond), clients, drainBudget)
	}
	c.Logf("the server exited %s after SIGTERM with %d connections pipelining (%d saw the close)",
		elapsed.Round(time.Millisecond), clients, errored)

	if pid := c.Info().PID; pid != 0 {
		return fmt.Errorf("the server process %d is still running after the drain", pid)
	}
	return nil
}

// drainBudget is the window a drain under load is held to. FEAT-0024 asks for
// under ten seconds end to end; this is the server's own share of it.
const drainBudget = 8 * time.Second

// churnRounds is how many connect-command-disconnect cycles the soak runs per
// worker. A leak of one goroutine or one buffer per connection is invisible
// below a few thousand and fatal above a few million.
const (
	churnWorkers = 6
	churnRounds  = 250
)

// connChurnLeaksNothing is the scenario FEAT-0024 says finds the real bugs.
//
// Sustained connect, command, disconnect churn, with the goroutine count and
// the resident set read from the server between passes. Neither may follow the
// connection count: a goroutine that outlives its connection is invisible in
// every short test and takes the process down after a week.
func connChurnLeaksNothing(c *runner.Ctx) error {
	baselineGoroutines, err := freshGoroutineCount(c)
	if err != nil {
		return err
	}
	baselineResident, err := residentBytes(c)
	if err != nil {
		return err
	}
	c.Logf("before the churn: %d goroutines, %d bytes resident", baselineGoroutines, baselineResident)

	for pass := range 3 {
		if err := churn(c); err != nil {
			return fmt.Errorf("pass %d: %w", pass, err)
		}

		goroutines, err := freshGoroutineCount(c)
		if err != nil {
			return err
		}
		resident, err := residentBytes(c)
		if err != nil {
			return err
		}
		c.Logf("after pass %d (%d connections): %d goroutines (%+d), %d bytes resident (%+d)",
			pass, (pass+1)*churnWorkers*churnRounds,
			goroutines, goroutines-baselineGoroutines, resident, resident-baselineResident)

		// A goroutine per leaked connection would be thousands by now; the
		// allowance is for the sampler, the wheel and whatever is mid-exit.
		if goroutines > baselineGoroutines+32 {
			return fmt.Errorf("goroutines went from %d to %d over %d connections; a connection is leaking one",
				baselineGoroutines, goroutines, (pass+1)*churnWorkers*churnRounds)
		}
		if resident-baselineResident > allocationHeadroom {
			return fmt.Errorf("the resident set grew %d bytes over %d connections",
				resident-baselineResident, (pass+1)*churnWorkers*churnRounds)
		}
	}

	return pingThroughHarness(c)
}

// freshGoroutineCount reads the goroutine count from a sample taken after this
// call, not before it.
//
// The process figures in INFO come from a timer rather than from a read on the
// command path, because runtime.ReadMemStats stops the world and INFO is polled
// by the second (ISSUE-0015). That makes them up to one interval stale, which
// is fine for a dashboard and useless for "did the goroutines the churn created
// go away" — so the scan waits for the sample timestamp to move before reading
// the count beside it.
func freshGoroutineCount(c *runner.Ctx) (int, error) {
	before, err := infoInt(c, "atlascache_memory_sampled_at")
	if err != nil {
		return 0, err
	}

	deadline := time.Now().Add(sampleWait)
	for time.Now().Before(deadline) {
		if err := c.Sleep(250 * time.Millisecond); err != nil {
			return 0, err
		}
		at, err := infoInt(c, "atlascache_memory_sampled_at")
		if err != nil {
			return 0, err
		}
		if at > before {
			return infoInt(c, "atlascache_goroutines")
		}
	}
	return 0, fmt.Errorf("no fresh process sample within %s; the sampler may have stopped", sampleWait)
}

// sampleWait is how long a fresh process sample is waited for. The sampler runs
// every ten seconds by default, so this is that plus room for a slow machine.
const sampleWait = 20 * time.Second

// churn runs one pass of connect, command, disconnect across several workers.
func churn(c *runner.Ctx) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		fail error
	)

	for worker := range churnWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := range churnRounds {
				if err := connectPingClose(c); err != nil {
					mu.Lock()
					if fail == nil {
						fail = fmt.Errorf("worker %d round %d: %w", worker, round, err)
					}
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	return fail
}

func connectPingClose(c *runner.Ctx) error {
	conn, err := dialRaw(c)
	if err != nil {
		return fmt.Errorf("dialing: %w", err)
	}
	defer conn.close()

	return conn.pingOver("\r\n")
}

// compatInlineLFBulkLoad is ISSUE-0019 at the level that made it matter.
//
// `redis-cli --pipe` is the documented way to mass-load a Redis, and with a
// plain-text input file it sends inline commands terminated with a bare LF,
// pipelined, followed by an ECHO of a random token to find the end of the
// replies. Requiring CRLF broke every part of that, and it is the first thing
// somebody trying a new server reaches for.
//
// What runs here is that byte stream rather than the tool, so the suite keeps
// no dependency on a Redis installation; the tool itself is checked against a
// real redis-cli out of band.
func compatInlineLFBulkLoad(c *runner.Ctx) error {
	const entries = 500

	conn, err := dialRaw(c)
	if err != nil {
		return err
	}
	defer conn.close()

	// Exactly what `redis-cli --pipe < data.txt` puts on the wire for a text
	// file authored on any Unix: no CR anywhere, and no waiting for replies.
	var load strings.Builder
	for i := range entries {
		fmt.Fprintf(&load, "SET pipe:key:%04d value-%04d\n", i, i)
	}
	const sentinel = "e2e-pipe-sentinel-0f1e2d3c"
	fmt.Fprintf(&load, "ECHO %s\n", sentinel)

	if _, writeErr := conn.write([]byte(load.String())); writeErr != nil {
		return fmt.Errorf("writing a %d byte plain-text load: %w", load.Len(), writeErr)
	}

	lines, err := conn.readLines(entries + 2)
	if err != nil {
		return fmt.Errorf("reading the replies to a bare-LF pipeline: %w", err)
	}
	for i := range entries {
		if lines[i] != "+OK" {
			return fmt.Errorf("SET %d answered %q, want +OK", i, lines[i])
		}
	}
	if lines[entries+1] != sentinel {
		return fmt.Errorf("the sentinel ECHO answered %q, want %s", lines[entries+1], sentinel)
	}
	c.Logf("%d bare-LF inline commands loaded in one pipelined write, sentinel echoed back", entries)

	// The data is really there, read back through the ordinary path.
	reply, err := c.Send("GET pipe:key:0499")
	if err != nil {
		return fmt.Errorf("reading back a piped key: %w", err)
	}
	if reply.String() != "value-0499" {
		return fmt.Errorf("a key loaded over a bare-LF pipe reads back as %q", reply.String())
	}

	// A bare-LF command on its own, which is what `printf 'PING\n' | nc` sends
	// and how the issue was reported; and then CRLF, because the fix is an
	// addition rather than a swap.
	for _, terminator := range []string{"\n", "\r\n"} {
		if err := conn.pingOver(terminator); err != nil {
			return err
		}
	}
	return nil
}

// idleTimeout reads the timeout the server is running with, so a scenario waits
// on the configured value rather than on one copied from the spec.
func idleTimeout(c *runner.Ctx) (time.Duration, error) {
	millis, err := infoInt(c, "atlascache_client_idle_timeout_ms")
	if err != nil {
		return 0, err
	}
	if millis <= 0 {
		return 0, fmt.Errorf("the server reports an idle timeout of %dms; this scenario needs one set", millis)
	}
	return time.Duration(millis) * time.Millisecond, nil
}

// infoInt reads one numeric INFO field from the running server.
func infoInt(c *runner.Ctx, field string) (int, error) {
	reply, err := c.Send("INFO")
	if err != nil {
		return 0, fmt.Errorf("reading INFO: %w", err)
	}
	if reply.Kind == runner.KindError {
		return 0, fmt.Errorf("INFO answered an error: %s", reply.Text)
	}

	text, ok := reply.Fields()[field]
	if !ok {
		return 0, fmt.Errorf("INFO has no %s field", field)
	}
	value, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("INFO reports %s as %q, which is not a number", field, text)
	}
	return value, nil
}

// probe sends one PING and reports whether the connection was served.
//
// A connection over the limit answers the limit error instead, so the two are
// told apart by what came back rather than by whether anything did.
func (rc *rawConn) probe() (served bool, line string, err error) {
	if _, err = rc.write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		// The server may have closed on us before the write landed, which is
		// what a refusal looks like if the reply raced ahead of it.
		refusal, readErr := rc.readLine()
		if readErr != nil {
			return false, "", fmt.Errorf("writing PING to a new connection: %w", err)
		}
		return false, refusal, nil
	}

	line, err = rc.readLine()
	if err != nil {
		return false, "", fmt.Errorf("reading the reply on a new connection: %w", err)
	}
	if line == "+"+pong {
		return true, line, nil
	}
	return false, line, nil
}

// pingOver sends an inline PING terminated with the given bytes and checks the
// reply. Redis accepts both terminators for an inline request and this server
// now does too (ISSUE-0019).
func (rc *rawConn) pingOver(terminator string) error {
	if _, err := rc.write([]byte("PING" + terminator)); err != nil {
		return fmt.Errorf("writing a PING terminated with %q: %w", terminator, err)
	}
	line, err := rc.readLine()
	if err != nil {
		return fmt.Errorf("reading the reply to a PING terminated with %q: %w", terminator, err)
	}
	if line != "+"+pong {
		return fmt.Errorf("a PING terminated with %q answered %q, want +%s", terminator, line, pong)
	}
	return nil
}

// readLines reads n reply lines, each without its terminator.
func (rc *rawConn) readLines(n int) ([]string, error) {
	lines := make([]string, 0, n)
	for len(lines) < n {
		line, err := rc.readLine()
		if err != nil {
			return lines, fmt.Errorf("after %d of %d replies: %w", len(lines), n, err)
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// waitForCloseWithoutReading waits for the server to hang up on a client that
// is deliberately not reading.
//
// It cannot read to find out, because reading is the thing being withheld: a
// client that drained the socket to check whether it had been disconnected
// would have unblocked the server and stopped being the client this is about.
// So it probes by writing instead. A bare newline is an empty inline request
// the decoder skips, so the probe asks for nothing and is answered with
// nothing; once the peer has fully closed, writing to the socket fails.
func (rc *rawConn) waitForCloseWithoutReading(within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := rc.write([]byte("\n")); err != nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the connection was still open after %s, with the client reading none of what it asked for",
		within)
}

// waitForClose blocks until the server closes the connection, or reports what
// it was doing instead when the wait runs out.
func (rc *rawConn) waitForClose(within time.Duration) error {
	if err := rc.conn.SetReadDeadline(time.Now().Add(within)); err != nil {
		return err
	}

	extra, err := io.ReadAll(rc.r)
	switch {
	case err == nil, errors.Is(err, io.EOF):
		return nil
	default:
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return fmt.Errorf("the connection was still open after %s, having carried %d more bytes",
				within, len(extra))
		}
		// A reset is a close as far as this is concerned.
		return nil
	}
}
