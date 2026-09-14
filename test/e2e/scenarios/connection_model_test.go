package scenarios_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/b3vet/atlascache/test/e2e/client"
	"github.com/b3vet/atlascache/test/e2e/runner"
	"github.com/b3vet/atlascache/test/e2e/runner/fakeharness"
)

// The FEAT-0024 scenarios, run against a model server that behaves correctly
// and again against one carrying the exact defect each scenario exists to catch.
//
// The second half is the half that matters. A connection scenario that cannot
// fail is worse than no scenario: connection bugs are the ones that look fine
// in every short test, and a green tick against a server that leaks a goroutine
// per client is an active claim that it does not.
//
// The model is not AtlasCache and proves nothing about it. It is an independent
// implementation of the same properties, written so that "the scenario passes"
// and "the scenario fails for the right reason" are both observable.

// connDefects are the ways a connection layer can be wrong. Each is a real
// failure mode and each is what one scenario is for.
type connDefects struct {
	// refusesAtAccept closes a connection over the limit without saying why.
	// The client sees a connection that was accepted and then dropped, which is
	// indistinguishable from a server falling over (ADR-0021).
	refusesAtAccept bool

	// noConnectionLimit serves every connection. Unbounded connections are
	// unbounded goroutines and buffers, which is the whole reason the limit is
	// mandatory rather than optional.
	noConnectionLimit bool

	// noIdleTimeout holds a connection that has said nothing, forever.
	noIdleTimeout bool

	// idleClockPerByte refreshes the idle deadline on read activity rather than
	// on a completed command, so a client dribbling bytes it never terminates
	// keeps its connection alive indefinitely.
	idleClockPerByte bool

	// idleClockPerConnection never refreshes the deadline, so a busy client is
	// disconnected mid-conversation and the timeout is a connection lifetime.
	idleClockPerConnection bool

	// buffersUntilTheReadBufferIsEmpty keeps decoding while any bytes are
	// buffered, holding the replies it has. A batch ending in a partial frame
	// then deadlocks: the server waits for the rest of the frame and the client
	// waits for the replies it is holding.
	buffersUntilTheReadBufferIsEmpty bool

	// noRequestBudget reads a request of any size, and holds what decoding it
	// cost. Every field is inside its own limit; nothing bounds the product
	// (ISSUE-0018).
	noRequestBudget bool

	// noOutputCap buffers a reply of any size for a client that may never read
	// it, which is what KEYS on a large keyspace costs.
	noOutputCap bool

	// leaksAGoroutinePerConnection is the one that is invisible everywhere
	// else: the connection closes, the client is served correctly, and one
	// goroutine stays behind.
	leaksAGoroutinePerConnection bool
}

// The model's connection limits. They are small so a scenario finishes in a
// test's time rather than in a client's, and the scenarios read them back from
// INFO rather than assuming them.
const (
	modelMaxConnections = 12
	modelIdleTimeout    = 2 * time.Second
	modelMaxRequestSize = 256 << 10
	modelMaxOutput      = 128 << 10
	modelMaxPipeline    = 1024
)

// connServer is a RESP server with a connection layer, over real sockets.
//
// It reuses the protocol model's decoding and command execution, because what
// is under test here is everything around those: how many connections there
// are, how long one may be silent, how much of a request will be read, and how
// much reply will be buffered for a client that is not reading.
type connServer struct {
	*protoServer

	defects connDefects
	idle    time.Duration

	live     atomic.Int64
	rejected atomic.Uint64
	idleShut atomic.Uint64
	outShut  atomic.Uint64

	// leaked holds the goroutines the leak defect strands, so the test process
	// gets them back when the model is closed.
	leaked chan struct{}
}

func newConnServer(defects connDefects) *connServer {
	model := newProtoServer(protoDefects{})
	// The element cap follows the byte budget, the way the server derives it:
	// every element costs at least seven bytes to send, so a budget implies a
	// count and the two cannot drift.
	model.maxMultiBulk = modelMaxRequestSize / 7

	idle := modelIdleTimeout
	if defects.noIdleTimeout {
		idle = 0
	}

	return &connServer{
		protoServer: model,
		defects:     defects,
		idle:        idle,
		leaked:      make(chan struct{}),
	}
}

func (s *connServer) close() { close(s.leaked) }

func (s *connServer) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}

		if !s.admit() {
			s.rejected.Add(1)
			go s.refuse(conn)
			continue
		}

		go func() {
			defer s.live.Add(-1)
			defer func() { _ = conn.Close() }()
			if s.defects.leaksAGoroutinePerConnection {
				go func() { <-s.leaked }()
			}
			s.handle(conn)
		}()
	}
}

func (s *connServer) admit() bool {
	if s.defects.noConnectionLimit {
		s.live.Add(1)
		return true
	}
	for {
		live := s.live.Load()
		if live >= modelMaxConnections {
			return false
		}
		if s.live.CompareAndSwap(live, live+1) {
			return true
		}
	}
}

// refuse tells a connection over the limit why it is being closed, unless the
// defect is that it does not.
func (s *connServer) refuse(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	if s.defects.refusesAtAccept {
		return
	}
	if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return
	}
	if _, err := conn.Write([]byte(maxClientsReplyText)); err != nil {
		return
	}
}

// maxClientsReplyText is Redis's wording, which is what a client matches on.
const maxClientsReplyText = "-ERR max number of clients reached\r\n"

// handle is the connection loop: decode, execute, answer.
//
// It flushes after every command rather than batching, because what the
// scenarios check is where a batch ends and not how many syscalls it cost —
// the syscall count is asserted in internal/server, where the connection's
// net.Conn can be counted. The one defect below that does buffer is there to
// show the scenario catching a server that never flushes.
func (s *connServer) handle(conn net.Conn) {
	budget := &modelBudgetReader{src: conn, limit: modelMaxRequestSize}
	if s.defects.noRequestBudget {
		budget.limit = 0
	}
	if s.defects.idleClockPerByte {
		budget.onRead = func() { s.arm(conn) }
	}

	r := bufio.NewReaderSize(budget, modelConnBuffer)
	out := &modelOutput{conn: conn, w: bufio.NewWriter(conn), deadline: !s.defects.noOutputCap}
	s.arm(conn)

	for {
		args, protoErr, err := s.decode(r)
		switch {
		case protoErr != "":
			out.add("-ERR Protocol error: " + protoErr + "\r\n")
			out.flush()
			return
		case err != nil:
			s.endConnection(out, budget, err)
			return
		case len(args) == 0:
			continue
		}

		// A complete command: the budget is returned and the idle clock
		// restarts. Per command, never per byte.
		budget.reset()
		if !s.defects.idleClockPerConnection {
			s.arm(conn)
		}

		out.add(s.exec(args))
		if !s.defects.noOutputCap && out.len() > modelMaxOutput {
			s.outShut.Add(1)
			return
		}
		if s.defects.buffersUntilTheReadBufferIsEmpty && r.Buffered() > 0 {
			continue
		}
		if !out.flush() {
			return
		}
	}
}

// endConnection answers whatever the read failure owes the client and records
// why the connection ended.
func (s *connServer) endConnection(out *modelOutput, budget *modelBudgetReader, err error) {
	if budget.spentOut {
		out.add("-ERR Protocol error: request is too large\r\n")
		out.flush()
		return
	}
	if isTimeout(err) {
		s.idleShut.Add(1)
	}
	if !s.defects.buffersUntilTheReadBufferIsEmpty {
		out.flush()
	}
}

// modelOutput is the connection's reply buffer.
type modelOutput struct {
	conn     net.Conn
	w        *bufio.Writer
	deadline bool
	batch    []byte
}

func (o *modelOutput) add(reply string) { o.batch = append(o.batch, reply...) }
func (o *modelOutput) len() int         { return len(o.batch) }

func (o *modelOutput) flush() bool {
	if len(o.batch) == 0 {
		return true
	}
	if o.deadline {
		if err := o.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return false
		}
	}
	if _, err := o.w.Write(o.batch); err != nil {
		return false
	}
	o.batch = o.batch[:0]
	return o.w.Flush() == nil
}

// arm puts the idle deadline on the read side.
func (s *connServer) arm(conn net.Conn) {
	if s.idle <= 0 {
		return
	}
	if err := conn.SetReadDeadline(time.Now().Add(s.idle)); err != nil {
		return
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errorsAs(err, &netErr) && netErr.Timeout()
}

// errorsAs is errors.As, named locally so this file does not import errors for
// one call and shadow the protocol model's use of it.
func errorsAs(err error, target *net.Error) bool {
	for err != nil {
		if e, ok := err.(net.Error); ok { //nolint:errorlint // walking the chain by hand below
			*target = e
			return true
		}
		unwrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}

// exec adds the fields a connection scenario reads back, and delegates the rest
// to the protocol model.
func (s *connServer) exec(args []string) string {
	if strings.EqualFold(args[0], "INFO") {
		return bulkReply(s.connInfoText())
	}
	return s.protoServer.exec(args)
}

func (s *connServer) connInfoText() string {
	limit := modelMaxConnections
	if s.defects.noConnectionLimit {
		// A limit it claims and does not keep, which is the defect: a scenario
		// that could not reach the claimed number would fail for want of
		// sockets rather than for want of a limit.
		limit = 40
	}
	output := modelMaxOutput
	if s.defects.noOutputCap {
		output = 0
	}
	budget := modelMaxRequestSize
	if s.defects.noRequestBudget {
		budget = 0
	}

	return fmt.Sprintf("# Clients\r\n"+
		"connected_clients:%d\r\n"+
		"maxclients:%d\r\n"+
		"atlascache_client_idle_timeout_ms:%d\r\n"+
		"atlascache_max_request_size:%d\r\n"+
		"atlascache_max_request_elements:%d\r\n"+
		"atlascache_max_pipeline_commands:%d\r\n"+
		"atlascache_max_output_buffer:%d\r\n"+
		"# Stats\r\n"+
		"rejected_connections:%d\r\n"+
		"atlascache_idle_closed:%d\r\n"+
		"atlascache_output_limit_closed:%d\r\n"+
		"atlascache_stalled_closed:0\r\n"+
		"atlascache_goroutines:%d\r\n"+
		"atlascache_memory_sampled_at:%d\r\n",
		s.live.Load(), limit, s.idle.Milliseconds(), budget, s.maxMultiBulk, modelMaxPipeline, output,
		s.rejected.Load(), s.idleShut.Load(), s.outShut.Load(),
		runtime.NumGoroutine(), time.Now().UnixNano())
}

// modelBudgetReader bounds the bytes one request may draw from the socket, and
// — for the defect — does not.
type modelBudgetReader struct {
	src      io.Reader
	limit    int
	spent    int
	spentOut bool
	onRead   func()
}

func (b *modelBudgetReader) Read(p []byte) (int, error) {
	if b.onRead != nil {
		b.onRead()
	}
	if b.limit <= 0 {
		return b.src.Read(p)
	}
	remaining := b.limit - b.spent
	if remaining <= 0 {
		b.spentOut = true
		return 0, io.ErrUnexpectedEOF
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	n, err := b.src.Read(p)
	b.spent += n
	return n, err
}

func (b *modelBudgetReader) reset() { b.spent = 0 }

// connHarness puts a runner.Harness in front of a connServer. Its PID is this
// test process, which is what the resident-set scenarios measure.
type connHarness struct {
	server *connServer
	addr   string
	root   string

	mu      sync.Mutex
	ln      net.Listener
	stopped bool
}

func newConnHarness(t *testing.T, defects connDefects) *connHarness {
	t.Helper()

	var config net.ListenConfig
	ln, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	server := newConnServer(defects)
	go server.serve(ln)

	h := &connHarness{server: server, addr: ln.Addr().String(), root: t.TempDir(), ln: ln}
	t.Cleanup(func() {
		if err := h.Stop(context.Background()); err != nil {
			t.Logf("stopping the model harness: %v", err)
		}
		server.close()
	})
	return h
}

func (h *connHarness) Start(context.Context) error   { return nil }
func (h *connHarness) Restart(context.Context) error { return nil }
func (h *connHarness) Kill() error                   { return nil }
func (h *connHarness) Close() error                  { return nil }
func (h *connHarness) Logs() string                  { return "" }
func (h *connHarness) Root() string                  { return h.root }

// Stop stands in for SIGTERM: the listener closes and the process is gone, so
// Info reports no pid and nothing new is served.
func (h *connHarness) Stop(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return nil
	}
	h.stopped = true
	return h.ln.Close()
}

func (h *connHarness) Info() runner.ServerInfo {
	h.mu.Lock()
	defer h.mu.Unlock()

	pid := os.Getpid()
	if h.stopped {
		pid = 0
	}
	return runner.ServerInfo{ClientAddr: h.addr, AdminAddr: h.addr, DataDir: h.root, PID: pid}
}

func (h *connHarness) Send(ctx context.Context, cmd string) (runner.Reply, error) {
	args, err := client.ParseCommand(cmd)
	if err != nil {
		return runner.Reply{}, err
	}
	conn, err := client.Dial(ctx, h.addr)
	if err != nil {
		return runner.Reply{}, err
	}
	defer func() { _ = conn.Close() }()
	return conn.Call(ctx, args...)
}

// The scenarios under test. Named here so a rename has one place to fail.
const (
	scenarioPipelineOrder   = "conn_pipeline_returns_in_order"
	scenarioPartialFrame    = "conn_pipeline_retains_a_partial_frame"
	scenarioConnLimit       = "conn_limit_reports_then_closes"
	scenarioIdleReaps       = "conn_idle_timeout_reaps_the_silent"
	scenarioIdleSpares      = "conn_idle_timeout_spares_the_busy"
	scenarioDribbling       = "conn_dribbling_does_not_defeat_the_timeout"
	scenarioRequestBudget   = "conn_request_budget_costs_nothing"
	scenarioOutputCap       = "conn_output_cap_disconnects_a_silent_reader"
	scenarioDrainUnderLoad  = "conn_drain_under_load_exits_in_time"
	scenarioChurn           = "conn_churn_leaks_nothing"
	scenarioInlineLFBulk    = "compat_inline_lf_bulk_load"
	scenarioGracefulClosing = "graceful_shutdown_closes_connections"
)

func TestConnScenariosAreRegistered(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		scenarioPipelineOrder, scenarioPartialFrame, scenarioConnLimit,
		scenarioIdleReaps, scenarioIdleSpares, scenarioDribbling,
		scenarioRequestBudget, scenarioOutputCap, scenarioDrainUnderLoad,
		scenarioChurn, scenarioInlineLFBulk, scenarioGracefulClosing,
	} {
		if _, ok := runner.LookupScenario(name); !ok {
			t.Errorf("%s did not register itself, so no spec can name it", name)
		}
	}
}

// TestConnScenariosFailAgainstAnUnreachableServer is the vacuity check.
//
// A scenario that returns nil when there is nothing to connect to is a green
// tick for a property nobody measured, and connection scenarios are especially
// exposed to it: most of what they assert is that something did not happen.
func TestConnScenariosFailAgainstAnUnreachableServer(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		scenarioPipelineOrder, scenarioPartialFrame, scenarioConnLimit,
		scenarioIdleReaps, scenarioIdleSpares, scenarioDribbling,
		scenarioRequestBudget, scenarioOutputCap, scenarioDrainUnderLoad,
		scenarioChurn, scenarioInlineLFBulk,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := fakeharness.New(map[string]runner.Reply{"PING": runner.StatusReply(pongText)})
			if failure := runScenario(t, name, h); failure == nil {
				t.Fatal("the scenario passed against a harness with no server behind it")
			}
		})
	}
}

func TestConnPipelineReturnsInOrder(t *testing.T) {
	t.Parallel()

	h := newConnHarness(t, connDefects{})
	if failure := runScenario(t, scenarioPipelineOrder, h); failure != nil {
		t.Fatalf("a server answering a pipeline in order failed: %s", failure.Message)
	}
}

// TestConnPipelineRetainsAPartialFrame is the anti-deadlock property. The
// defective model is the naive loop everyone writes first: keep decoding while
// anything is buffered, flush when it runs out.
func TestConnPipelineRetainsAPartialFrame(t *testing.T) {
	t.Parallel()

	t.Run("a server that flushes before it blocks passes", func(t *testing.T) {
		t.Parallel()
		h := newConnHarness(t, connDefects{})
		if failure := runScenario(t, scenarioPartialFrame, h); failure != nil {
			t.Fatalf("a server flushing before a blocking read failed: %s", failure.Message)
		}
	})

	t.Run("a server that blocks holding its replies is caught", func(t *testing.T) {
		t.Parallel()
		h := newConnHarness(t, connDefects{buffersUntilTheReadBufferIsEmpty: true})
		requireFailure(t, runScenario(t, scenarioPartialFrame, h), "were not answered")
	})
}

func TestConnLimitReportsThenCloses(t *testing.T) {
	t.Parallel()

	t.Run("a server that reports the limit passes", func(t *testing.T) {
		t.Parallel()
		h := newConnHarness(t, connDefects{})
		if failure := runScenario(t, scenarioConnLimit, h); failure != nil {
			t.Fatalf("a server reporting its connection limit failed: %s", failure.Message)
		}
	})

	t.Run("a server that drops the connection without saying why is caught", func(t *testing.T) {
		t.Parallel()
		h := newConnHarness(t, connDefects{refusesAtAccept: true})
		requireFailure(t, runScenario(t, scenarioConnLimit, h), "new connection")
	})

	t.Run("a server with no limit at all is caught", func(t *testing.T) {
		t.Parallel()
		h := newConnHarness(t, connDefects{noConnectionLimit: true})
		requireFailure(t, runScenario(t, scenarioConnLimit, h), "none was refused")
	})
}

func TestConnIdleTimeout(t *testing.T) {
	t.Parallel()

	t.Run("a server that reaps a silent connection passes", func(t *testing.T) {
		t.Parallel()
		h := newConnHarness(t, connDefects{})
		if failure := runScenario(t, scenarioIdleReaps, h); failure != nil {
			t.Fatalf("a server reaping an idle connection failed: %s", failure.Message)
		}
	})

	t.Run("a server holding a silent connection forever is caught", func(t *testing.T) {
		t.Parallel()
		h := newConnHarness(t, connDefects{noIdleTimeout: true})
		requireFailure(t, runScenario(t, scenarioIdleReaps, h), "needs one set")
	})

	t.Run("a server that spares a busy connection passes", func(t *testing.T) {
		t.Parallel()
		h := newConnHarness(t, connDefects{})
		if failure := runScenario(t, scenarioIdleSpares, h); failure != nil {
			t.Fatalf("a server sparing a busy connection failed: %s", failure.Message)
		}
	})

	t.Run("a server that never restarts the clock is caught", func(t *testing.T) {
		t.Parallel()
		h := newConnHarness(t, connDefects{idleClockPerConnection: true})
		requireFailure(t, runScenario(t, scenarioIdleSpares, h), "the connection was")
	})
}

// TestConnDribblingDoesNotDefeatTheTimeout is why the clock is per command. The
// defective model arms it on read activity, which is the obvious implementation
// and the wrong one.
func TestConnDribblingDoesNotDefeatTheTimeout(t *testing.T) {
	t.Parallel()

	t.Run("a server counting commands passes", func(t *testing.T) {
		t.Parallel()
		h := newConnHarness(t, connDefects{})
		if failure := runScenario(t, scenarioDribbling, h); failure != nil {
			t.Fatalf("a server refreshing the idle clock per command failed: %s", failure.Message)
		}
	})

	t.Run("a server counting bytes is caught", func(t *testing.T) {
		t.Parallel()
		h := newConnHarness(t, connDefects{idleClockPerByte: true})
		requireFailure(t, runScenario(t, scenarioDribbling, h), "was not reaped")
	})
}

// TestConnRequestBudgetCostsNothing measures this process's resident set, so it
// does not run alongside anything else allocating: a test doing that would be
// indistinguishable from the defect.
func TestConnRequestBudgetCostsNothing(t *testing.T) {
	t.Run("a server that bounds one request passes", func(t *testing.T) {
		h := newConnHarness(t, connDefects{})
		if failure := runScenario(t, scenarioRequestBudget, h); failure != nil {
			t.Fatalf("a server bounding a request failed: %s\n%s",
				failure.Message, strings.Join(failure.Notes, "\n"))
		}
	})

	t.Run("a server with no budget is caught", func(t *testing.T) {
		h := newConnHarness(t, connDefects{noRequestBudget: true})
		requireFailure(t, runScenario(t, scenarioRequestBudget, h), "budget")
	})
}

func TestConnOutputCapDisconnectsASilentReader(t *testing.T) {
	t.Run("a server that caps buffered output passes", func(t *testing.T) {
		h := newConnHarness(t, connDefects{})
		if failure := runScenario(t, scenarioOutputCap, h); failure != nil {
			t.Fatalf("a server capping its output failed: %s\n%s",
				failure.Message, strings.Join(failure.Notes, "\n"))
		}
	})

	t.Run("a server that buffers for a client that will not read is caught", func(t *testing.T) {
		h := newConnHarness(t, connDefects{noOutputCap: true})
		requireFailure(t, runScenario(t, scenarioOutputCap, h), "was not disconnected")
	})
}

func TestConnDrainUnderLoadExitsInTime(t *testing.T) {
	t.Parallel()

	h := newConnHarness(t, connDefects{})
	if failure := runScenario(t, scenarioDrainUnderLoad, h); failure != nil {
		t.Fatalf("a server draining under load failed: %s\n%s",
			failure.Message, strings.Join(failure.Notes, "\n"))
	}
}

// TestConnChurnLeaksNothing is the soak property, and the defect is the one it
// exists for: every client is served correctly and one goroutine stays behind
// each time.
func TestConnChurnLeaksNothing(t *testing.T) {
	t.Run("a server that gives its goroutines back passes", func(t *testing.T) {
		h := newConnHarness(t, connDefects{})
		if failure := runScenario(t, scenarioChurn, h); failure != nil {
			t.Fatalf("a server that leaks nothing failed: %s\n%s",
				failure.Message, strings.Join(failure.Notes, "\n"))
		}
	})

	t.Run("a server leaking a goroutine per connection is caught", func(t *testing.T) {
		h := newConnHarness(t, connDefects{leaksAGoroutinePerConnection: true})
		requireFailure(t, runScenario(t, scenarioChurn, h), "leaking one")
	})
}

// TestCompatInlineLFBulkLoad is ISSUE-0019: the byte stream `redis-cli --pipe`
// puts on the wire for a plain-text file.
func TestCompatInlineLFBulkLoad(t *testing.T) {
	t.Parallel()

	t.Run("a server accepting a bare LF passes", func(t *testing.T) {
		t.Parallel()
		h := newConnHarness(t, connDefects{})
		if failure := runScenario(t, scenarioInlineLFBulk, h); failure != nil {
			t.Fatalf("a server accepting bare-LF inline commands failed: %s", failure.Message)
		}
	})

	t.Run("a server requiring CRLF is caught", func(t *testing.T) {
		t.Parallel()
		h := newCRLFOnlyHarness(t)
		requireFailure(t, runScenario(t, scenarioInlineLFBulk, h), "bare-LF pipeline")
	})
}

// crlfOnlyError is what the server said before ISSUE-0019 was fixed.
const crlfOnlyError = "-ERR Protocol error: expected CRLF line terminator\r\n"

// newCRLFOnlyHarness is the server as it was before ISSUE-0019 was fixed: an
// inline request terminated with a bare LF is a protocol error.
//
// It is a socket rather than a defect flag on the model, because the model's
// decoding is the protocol model's and the terminator rule lives there — this
// answers the one reply the scenario reads first, which is all it takes to show
// the scenario failing.
func newCRLFOnlyHarness(t *testing.T) *connHarness {
	t.Helper()

	var config net.ListenConfig
	ln, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				r := bufio.NewReader(conn)
				for {
					line, readErr := r.ReadString('\n')
					if readErr != nil {
						return
					}
					if !strings.HasSuffix(line, "\r\n") {
						if _, writeErr := conn.Write([]byte(crlfOnlyError)); writeErr != nil {
							return
						}
						return
					}
					if _, writeErr := conn.Write([]byte("+PONG\r\n")); writeErr != nil {
						return
					}
				}
			}()
		}
	}()

	server := newConnServer(connDefects{})
	h := &connHarness{server: server, addr: ln.Addr().String(), root: t.TempDir(), ln: ln}
	t.Cleanup(func() { server.close() })
	return h
}
