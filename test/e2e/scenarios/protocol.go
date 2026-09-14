package scenarios

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/b3vet/atlascache/test/e2e/client"
	"github.com/b3vet/atlascache/test/e2e/runner"
)

func init() {
	runner.RegisterScenario("proto_malformed_input_is_refused", protoMalformedInputIsRefused)
	runner.RegisterScenario("proto_binary_values_round_trip", protoBinaryValuesRoundTrip)
	runner.RegisterScenario("proto_declared_sizes_cost_nothing", protoDeclaredSizesCostNothing)
	runner.RegisterScenario("proto_unterminated_line_is_refused", protoUnterminatedLineIsRefused)
}

// These four are FEAT-0025 at the level a client meets it. internal/protocol
// asserts the same properties over the decoder in isolation; these assert them
// over a real process, over a real socket, because the two fail differently.
// A codec that refuses a malformed frame proves nothing about a server that
// then leaks the connection, hangs, or dies — and a value that survives the
// decoder can still be corrupted between the decoder and the keyspace.
//
// Raw sockets are the reason these are Go rather than YAML: a spec step sends a
// command, and none of the inputs below is one.

const (
	// protoErrPrefix is what every protocol error arrives as. The message after
	// it is the codec's, and each case below names the one it expects.
	protoErrPrefix = "-ERR Protocol error: "

	// The two messages a declared length is refused with. They are named
	// because each is expected by several cases, and a server that answered
	// "invalid bulk length" to a multibulk header would be telling the client
	// to look in the wrong place.
	errBulkLength      = "invalid bulk length"
	errMultiBulkLength = "invalid multibulk length"

	// errUnknownCommand is what an input that is a bad command rather than a
	// bad frame gets. The connection survives it, which is the difference the
	// cases below turn on.
	errUnknownCommand = "unknown command"

	// rawTimeout bounds every read and write on a raw connection. A server bug
	// that hangs must fail the spec, not the run.
	rawTimeout = 10 * time.Second
)

// malformedCase is one input class, what the server must answer, and whether it
// may answer at all.
type malformedCase struct {
	class string
	input string

	// wantErr is the text the protocol error must contain. Empty means the
	// server must close without replying, which is what a frame cut off at EOF
	// gets: there is nothing to report to a client that already hung up.
	wantErr string

	// halfClose sends EOF after the input, which is how a truncated frame is
	// expressed on a socket that is otherwise still open.
	halfClose bool

	// survives marks the inputs that are not protocol errors at all. An inline
	// request the server does not recognize is answered and the connection
	// carries on, because a client that typed one command wrong has not lost
	// the right to send the next.
	survives bool
}

// malformedCases is the input table from FEAT-0025, one row per class, as bytes
// on a socket rather than as decoder calls.
var malformedCases = []malformedCase{
	{
		class:     "truncated frame",
		input:     "*3\r\n$3\r\nSET\r\n",
		halfClose: true,
	},
	{
		class:     "bulk payload cut short",
		input:     "*1\r\n$4\r\nPI",
		halfClose: true,
	},
	{
		class:   "negative bulk length",
		input:   "*1\r\n$-5\r\n",
		wantErr: errBulkLength,
	},
	{
		class:   "negative multibulk length",
		input:   "*-3\r\n",
		wantErr: errMultiBulkLength,
	},
	{
		class:   "enormous declared bulk length",
		input:   "*1\r\n$999999999999\r\n",
		wantErr: errBulkLength,
	},
	{
		class:   "enormous declared multibulk length",
		input:   "*999999999999\r\n",
		wantErr: errMultiBulkLength,
	},
	{
		class:   "bulk length past the int64 range",
		input:   "*1\r\n$99999999999999999999\r\n",
		wantErr: errBulkLength,
	},
	{
		class:   "multibulk length past the int64 range",
		input:   "*99999999999999999999\r\n",
		wantErr: errMultiBulkLength,
	},
	{
		class:   "declared length longer than the payload",
		input:   "*1\r\n$2\r\nhiXX",
		wantErr: "expected CRLF after bulk string",
	},
	{
		class:     "line with no terminator",
		input:     "+OK",
		halfClose: true,
	},
	{
		// ISSUE-0019: a bare LF terminates an inline request, as it does in
		// Redis. "+OK" is then a command nobody implements, which is answered
		// and survived rather than being a framing error.
		class:    "inline request terminated with a bare LF",
		input:    "+OK\n",
		wantErr:  errUnknownCommand,
		survives: true,
	},
	{
		// The leniency stops at the frame boundary. A multibulk header with a
		// bare LF is a client library that has lost track of what it wrote, and
		// Redis refuses it too.
		class:   "multibulk header terminated with a bare LF",
		input:   "*1\n$4\r\nPING\r\n",
		wantErr: "expected CRLF line terminator",
	},
	{
		class:   "bulk header terminated with a bare LF",
		input:   "*1\r\n$4\nPING\r\n",
		wantErr: "expected CRLF line terminator",
	},
	{
		class: "deeply nested aggregates",
		// Ten thousand levels. A recursive parser with no cap descends into
		// every one of them; this server must refuse at the first.
		input:   strings.Repeat("*1\r\n", 10_000) + "$4\r\nPING\r\n",
		wantErr: `expected '$', got '*'`,
	},
	{
		class:   "RESP3 map where an element belongs",
		input:   "*1\r\n%1\r\n$1\r\na\r\n$1\r\nb\r\n",
		wantErr: `expected '$', got '%'`,
	},
	{
		class: "bare CR where an element belongs",
		// Found by FuzzDecode: the offending byte used to be pasted into the
		// error text raw, so this reply carried a literal CR into the client's
		// parser. It must arrive escaped.
		input:   "*1\r\n\r\r\n",
		wantErr: `expected '$', got '\r'`,
	},
	{
		class:    "RESP3 type byte at the top of a request",
		input:    "%2\r\n",
		wantErr:  errUnknownCommand,
		survives: true,
	},
	{
		class:    "integer overflow in an inline request",
		input:    ":99999999999999999999\r\n",
		wantErr:  errUnknownCommand,
		survives: true,
	},
}

// protoMalformedInputIsRefused sends every malformed class on its own
// connection and checks three things each time: the server answers what it
// should, it closes the connection behind a protocol error, and — the one that
// matters most — the next client to connect is served normally.
//
// A server that refuses malformed input correctly and then dies has still lost.
func protoMalformedInputIsRefused(c *runner.Ctx) error {
	for _, tc := range malformedCases {
		if err := sendMalformed(c, tc); err != nil {
			return fmt.Errorf("%s: %w", tc.class, err)
		}
		// A fresh connection after every single class, rather than once at the
		// end: a crash is attributable to the input that caused it only if
		// nothing else ran in between.
		if err := freshConnectionServes(c); err != nil {
			return fmt.Errorf("after %s: %w", tc.class, err)
		}
		c.Logf("%s: refused, connection closed, server still serving", tc.class)
	}

	// And the harness's own long-lived connection, which none of the above
	// touched, is still good.
	return pingThroughHarness(c)
}

// sendMalformed drives one case to its conclusion.
func sendMalformed(c *runner.Ctx, tc malformedCase) error {
	conn, err := dialRaw(c)
	if err != nil {
		return err
	}
	defer conn.close()

	if _, err = conn.write([]byte(tc.input)); err != nil {
		return fmt.Errorf("writing %d bytes: %w", len(tc.input), err)
	}
	if tc.halfClose {
		if err = conn.closeWrite(); err != nil {
			return fmt.Errorf("half-closing: %w", err)
		}
	}

	line, err := conn.readLine()
	switch {
	case tc.wantErr == "":
		// A frame that ended at EOF gets no reply: there is no client left to
		// tell. What must happen is the close, which arrives as an EOF or as a
		// reset depending on the stack — either is a close, and only a read
		// that keeps waiting is not.
		if err == nil {
			return fmt.Errorf("the server answered %q; a frame truncated at EOF has nobody to answer", line)
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return fmt.Errorf("the server neither answered a truncated frame nor closed: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("no reply: %w", err)
	}

	if tc.survives {
		return conn.stillUsableAfter(line, tc.wantErr)
	}
	if !strings.HasPrefix(line, protoErrPrefix) {
		return fmt.Errorf("the server answered %q, want a reply starting %q", line, protoErrPrefix)
	}
	if !strings.Contains(line, tc.wantErr) {
		return fmt.Errorf("the server answered %q, want it to say %q", line, tc.wantErr)
	}
	// A protocol error leaves the stream unsynchronized, so the connection
	// cannot be reused and the server must not pretend otherwise.
	return conn.expectClosed()
}

// stillUsableAfter covers the inputs that are bad commands rather than bad
// frames: the server answers, keeps the connection, and serves the next
// request on it.
func (rc *rawConn) stillUsableAfter(line, wantErr string) error {
	if !strings.Contains(line, wantErr) {
		return fmt.Errorf("the server answered %q, want it to say %q", line, wantErr)
	}
	if _, err := rc.write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		return fmt.Errorf("the connection was not usable after the reply: %w", err)
	}
	reply, err := rc.readLine()
	if err != nil {
		return fmt.Errorf("PING on the same connection: %w", err)
	}
	if reply != "+"+pong {
		return fmt.Errorf("PING on the same connection answered %q, want +%s", reply, pong)
	}
	return nil
}

// binaryPayloads are the values a length-prefixed protocol exists to carry. A
// server that treats them as text corrupts them silently, which is worse than
// refusing them, so every one is compared byte for byte after a round trip
// through a real process.
//
// The order is fixed rather than a map's, so a failure names the same payload
// every run and is reproducible from the message alone.
func binaryPayloads() []struct{ name, payload string } {
	every := make([]byte, 256)
	for i := range every {
		every[i] = byte(i)
	}
	return []struct{ name, payload string }{
		{"every byte there is", string(every)},
		{"a NUL in the middle", "a\x00b"},
		{"a leading NUL", "\x00ab"},
		{"a trailing NUL", "ab\x00"},
		{"nothing but NULs", "\x00\x00\x00\x00"},
		{"invalid UTF-8", "\xff\xfe\x80\xc0"},
		{"a surrogate encoding", "\xed\xa0\x80"},
		{"a truncated rune", "\xe2\x82"},
		{"an embedded CRLF", "a\r\nb"},
		{"a lone CR", "a\rb"},
		{"a lone LF", "a\nb"},
		{"a RESP frame inside", "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n"},
	}
}

// protoBinaryValuesRoundTrip is binary safety as a correctness requirement
// rather than a robustness one: a value written through a real socket into a
// real process must read back byte for byte, NULs and invalid UTF-8 included.
//
// It goes through its own connection rather than the harness's because the
// harness parses a command line, and a command line cannot express a NUL.
func protoBinaryValuesRoundTrip(c *runner.Ctx) error {
	conn, dialErr := client.Dial(c.Context(), c.Info().ClientAddr)
	if dialErr != nil {
		return fmt.Errorf("dialing %s: %w", c.Info().ClientAddr, dialErr)
	}
	defer func() { _ = conn.Close() }()

	for _, tc := range binaryPayloads() {
		key := "bin:" + strings.ReplaceAll(tc.name, " ", "-")

		reply, err := conn.Call(c.Context(), "SET", key, tc.payload)
		if err != nil {
			return fmt.Errorf("SET with %s: %w", tc.name, err)
		}
		if reply.Kind != runner.KindStatus || reply.Text != "OK" {
			return fmt.Errorf("SET with %s answered %s %q, want status OK", tc.name, reply.Kind, reply.String())
		}

		reply, err = conn.Call(c.Context(), "GET", key)
		if err != nil {
			return fmt.Errorf("GET after %s: %w", tc.name, err)
		}
		if reply.Kind != runner.KindBulk {
			return fmt.Errorf("GET after %s answered %s, want a bulk string", tc.name, reply.Kind)
		}
		if reply.Text != tc.payload {
			return fmt.Errorf("%s did not survive the round trip: wrote %s, read %s",
				tc.name, hexOf(tc.payload), hexOf(reply.Text))
		}
	}
	c.Logf("%d binary payloads round-tripped byte for byte", len(binaryPayloads()))

	// A binary key is the same requirement one field over: the key is a bulk
	// string too, and a server that scanned it for a terminator would store
	// this value somewhere else entirely.
	binaryKey := "bin:key\x00\xff\r\n"
	if _, err := conn.Call(c.Context(), "SET", binaryKey, "v"); err != nil {
		return fmt.Errorf("SET with a binary key: %w", err)
	}
	reply, err := conn.Call(c.Context(), "GET", binaryKey)
	if err != nil {
		return fmt.Errorf("GET with a binary key: %w", err)
	}
	if reply.Kind != runner.KindBulk || reply.Text != "v" {
		return fmt.Errorf("a key holding a NUL, a high byte and a CRLF read back %s %q",
			reply.Kind, reply.String())
	}

	// The same bytes must not collide with a key that merely looks like them
	// once the NUL is taken as a terminator.
	if reply, err = conn.Call(c.Context(), "GET", "bin:key"); err != nil {
		return fmt.Errorf("GET of the prefix key: %w", err)
	}
	if reply.Kind != runner.KindNil {
		return fmt.Errorf("a key truncated at its NUL answered %s %q; the NUL is part of the key",
			reply.Kind, reply.String())
	}
	return nil
}

// declaredSizeProbe is one header whose declared size is enormous and whose
// wire form is a few bytes.
type declaredSizeProbe struct {
	class string
	input string
	// wantErr is empty where the header itself is legal and the elements behind
	// it never arrive, which is the cheapest way to ask for the reservation.
	wantErr string
}

// declaredSizeProbes are the headers ISSUE-0016 turned into allocations, probed
// against the element cap the server actually runs with.
//
// The cap is read from the server rather than written down here, for the reason
// ISSUE-0018 exists: the million-element header this used to send was legal,
// because it is inside the protocol's own ceiling, and that is precisely what
// made twelve bytes on the wire worth a 154MB decode. FEAT-0024 puts a cap
// below that ceiling, so what matters is that the cap — wherever it is — is
// refused one element over and reserves nothing at it. The conn-request-budget
// spec sends the literal million-element header and measures what refusing it
// costs.
func declaredSizeProbes(c *runner.Ctx) ([]declaredSizeProbe, error) {
	elements, err := infoInt(c, "atlascache_max_request_elements")
	if err != nil {
		return nil, err
	}

	return []declaredSizeProbe{
		{
			class:   "a bulk header declaring 512MB",
			input:   "*1\r\n$536870912\r\n",
			wantErr: errBulkLength,
		},
		{
			class:   fmt.Sprintf("a multibulk header one element over the %d cap", elements),
			input:   fmt.Sprintf("*%d\r\n", elements+1),
			wantErr: errMultiBulkLength,
		},
		{
			class: fmt.Sprintf("a multibulk header at the %d element cap", elements),
			input: fmt.Sprintf("*%d\r\n", elements),
		},
	}, nil
}

// allocationProbeRounds is how many times each probe is sent. Before
// ISSUE-0016's fix one 512MB reservation was made per round, so this is 100GB
// of allocation churn against a resident set that must not move.
const allocationProbeRounds = 200

// allocationHeadroom is how far the server's resident set may grow while it
// refuses those. It is far above the noise of a Go process serving requests and
// far below one reservation of the sizes declared above.
const allocationHeadroom = 96 << 20

// protoDeclaredSizesCostNothing is ISSUE-0016 measured from outside the
// process.
//
// The unit tests assert on bytes the heap was asked for, which is the exact
// instrument but only available in-process. From out here the instrument is the
// server's resident set, sampled while the probes are in flight: a reservation
// made from a declared length is memory the process really does touch, and it
// shows up here whether or not the reply looks correct.
//
// Reading the reply proves nothing on its own. That is the whole lesson of
// ISSUE-0016 — every one of those bugs returned the right error, after
// allocating.
func protoDeclaredSizesCostNothing(c *runner.Ctx) error {
	baseline, err := residentBytes(c)
	if err != nil {
		return err
	}
	c.Logf("the server holds %d bytes resident before the probes", baseline)

	probes, err := declaredSizeProbes(c)
	if err != nil {
		return err
	}

	for _, probe := range probes {
		peak := baseline
		for range allocationProbeRounds {
			if err := sendProbe(c, probe.input, probe.wantErr); err != nil {
				return fmt.Errorf("%s: %w", probe.class, err)
			}
			resident, err := residentBytes(c)
			if err != nil {
				return fmt.Errorf("%s: %w", probe.class, err)
			}
			peak = max(peak, resident)
		}

		c.Logf("%s, refused %d times: resident set peaked at %d bytes, %+d on the baseline",
			probe.class, allocationProbeRounds, peak, peak-baseline)
		if peak-baseline > allocationHeadroom {
			return fmt.Errorf(
				"refusing %s %d times grew the resident set by %d bytes, past the %d byte headroom: "+
					"the declared size is being allocated before it is refused",
				probe.class, allocationProbeRounds, peak-baseline, allocationHeadroom)
		}
	}

	return pingThroughHarness(c)
}

// sendProbe sends one header on its own connection and checks what came back.
// A protocol error is expected where wantErr is set; otherwise the header is
// legal and the connection is closed on the truncated frame behind it.
func sendProbe(c *runner.Ctx, input, wantErr string) error {
	conn, err := dialRaw(c)
	if err != nil {
		return err
	}
	defer conn.close()

	if _, err = conn.write([]byte(input)); err != nil {
		return fmt.Errorf("writing the header: %w", err)
	}
	if wantErr == "" {
		if err = conn.closeWrite(); err != nil {
			return fmt.Errorf("half-closing: %w", err)
		}
		return conn.expectClosed()
	}

	line, err := conn.readLine()
	if err != nil {
		return fmt.Errorf("no reply to the header: %w", err)
	}
	if !strings.Contains(line, wantErr) {
		return fmt.Errorf("the server answered %q, want it to say %q", line, wantErr)
	}
	return conn.expectClosed()
}

// floodSize is how much a client writes with no newline in it. It is three
// orders of magnitude over the 64KB inline limit, so a parser that accumulates
// before checking is unmistakable and one that checks as it goes is unmoved.
const floodSize = 64 << 20

// floodChunk is written at a time. Large enough to be quick, small enough that
// the write that fails does so soon after the server closes.
const floodChunk = 64 << 10

// protoUnterminatedLineIsRefused is the first of ISSUE-0016's three bugs, at the
// level a client meets it: a connection that streams bytes and never sends a
// newline used to grow the server's buffer until the process died.
//
// Two things are measured, and both are about what the server spent rather than
// what it said. It must stop accepting the flood long before the flood ends,
// and its resident set must not follow what was sent.
func protoUnterminatedLineIsRefused(c *runner.Ctx) error {
	baseline, err := residentBytes(c)
	if err != nil {
		return err
	}

	conn, err := dialRaw(c)
	if err != nil {
		return err
	}
	defer conn.close()

	// The reply arrives while the flood is still being written, so it is read
	// concurrently: the server closes the connection behind the error, and a
	// client still writing into a closed socket may never get to read what was
	// already sent to it.
	replies := make(chan string, 1)
	go func() {
		line, readErr := conn.readLine()
		if readErr != nil {
			line = "read failed: " + readErr.Error()
		}
		replies <- line
	}()

	payload := make([]byte, floodChunk)
	for i := range payload {
		payload[i] = 'x'
	}
	sent := 0
	for sent < floodSize {
		n, writeErr := conn.write(payload)
		sent += n
		if writeErr != nil {
			// The server closed on us, which is how this loop is meant to end.
			break
		}
	}

	// What the server was willing to take is checked before what it said, and
	// it is the stronger of the two. The limit is 64KB and a connection buffer
	// is 16KB, so a server enforcing it as the line accumulates stops taking
	// bytes within a megabyte or so of socket buffering; one that accumulates
	// first swallows the whole flood and has not replied at all yet, so waiting
	// for its reply would only report the timeout rather than the cause.
	if sent >= floodSize {
		return fmt.Errorf(
			"the server accepted all %d bytes of a line it can never accept: "+
				"the limit is being applied after the accumulation, not during it", sent)
	}

	var reply string
	select {
	case reply = <-replies:
	case <-time.After(rawTimeout):
		return fmt.Errorf("no reply after writing %d bytes with no newline in them", sent)
	}

	resident, err := residentBytes(c)
	if err != nil {
		return err
	}
	c.Logf("wrote %d bytes with no newline; the server answered %q and holds %d bytes resident (%+d)",
		sent, reply, resident, resident-baseline)

	if !strings.Contains(reply, "too big inline request") {
		return fmt.Errorf("the server answered %q after %d bytes with no newline, want the inline limit",
			reply, sent)
	}
	if resident-baseline > allocationHeadroom {
		return fmt.Errorf("serving %d bytes of unterminated line grew the resident set by %d bytes",
			sent, resident-baseline)
	}
	if err := conn.expectClosed(); err != nil {
		return err
	}
	return freshConnectionServes(c)
}

// freshConnectionServes is the "and the server survived" half of every case
// above. It is a new connection every time, because a server that crashed is
// indistinguishable from one that is healthy until something tries to reach it.
func freshConnectionServes(c *runner.Ctx) error {
	conn, err := client.Dial(c.Context(), c.Info().ClientAddr)
	if err != nil {
		return fmt.Errorf("the server did not accept a new connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	reply, err := conn.Call(c.Context(), "PING")
	if err != nil {
		return fmt.Errorf("a new connection did not get a reply to PING: %w", err)
	}
	if reply.String() != pong {
		return fmt.Errorf("a new connection got %q for PING, want %s", reply.String(), pong)
	}
	return nil
}

// pingThroughHarness checks the connection the spec's own steps run on, which
// none of the raw sockets above touched.
func pingThroughHarness(c *runner.Ctx) error {
	reply, err := c.Send("PING")
	if err != nil {
		return fmt.Errorf("the harness connection did not survive: %w", err)
	}
	if reply.String() != pong {
		return fmt.Errorf("the harness connection answered PING with %q, want %s", reply.String(), pong)
	}
	return nil
}

// rawConn is a socket with no protocol on it. Everything above needs to put
// bytes on the wire that no client would send, so nothing here parses RESP:
// it writes, it reads a line, and it reports whether the server hung up.
type rawConn struct {
	conn net.Conn
	r    *bufio.Reader
}

func dialRaw(c *runner.Ctx) (*rawConn, error) {
	addr := c.Info().ClientAddr
	var dialer net.Dialer
	conn, err := dialer.DialContext(c.Context(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", addr, err)
	}
	return &rawConn{conn: conn, r: bufio.NewReader(conn)}, nil
}

func (rc *rawConn) write(b []byte) (int, error) {
	if err := rc.conn.SetWriteDeadline(time.Now().Add(rawTimeout)); err != nil {
		return 0, err
	}
	return rc.conn.Write(b)
}

// readLine reads one CRLF- or LF-terminated line, returned without it.
func (rc *rawConn) readLine() (string, error) {
	if err := rc.conn.SetReadDeadline(time.Now().Add(rawTimeout)); err != nil {
		return "", err
	}
	line, err := rc.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// closeWrite sends EOF while keeping the read side open, which is how a
// truncated frame reaches a server that is still expected to answer.
func (rc *rawConn) closeWrite() error {
	tcp, ok := rc.conn.(*net.TCPConn)
	if !ok {
		return fmt.Errorf("a %T cannot be half-closed", rc.conn)
	}
	return tcp.CloseWrite()
}

// expectClosed requires the server to have hung up. A protocol error leaves the
// byte stream unsynchronized, so a server that keeps the connection is offering
// to parse whatever follows as a fresh request — which is how a malformed frame
// becomes a smuggled one.
func (rc *rawConn) expectClosed() error {
	if err := rc.conn.SetReadDeadline(time.Now().Add(rawTimeout)); err != nil {
		return err
	}
	extra, err := io.ReadAll(rc.r)
	if err == nil {
		if len(extra) > 0 {
			return fmt.Errorf("the connection stayed open and carried %d more bytes: %q", len(extra), extra)
		}
		return nil
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return errors.New("the server left the connection open after a protocol error")
	}
	// A reset is a close as far as this assertion is concerned.
	return nil
}

func (rc *rawConn) close() { _ = rc.conn.Close() }

// residentBytes reports the server process's resident set.
//
// It is read with ps rather than from the server, because the point is to
// measure the process from outside: a server that is asked how much memory it
// is using answers with its own accounting, and an allocation made by the
// parser before a request was ever accepted is in nobody's accounting.
func residentBytes(c *runner.Ctx) (int, error) {
	pid := c.Info().PID
	if pid == 0 {
		return 0, errors.New("the harness reports no server process, so its memory cannot be measured")
	}

	ctx, cancel := context.WithTimeout(c.Context(), rawTimeout)
	defer cancel()

	// The only argument is a pid the harness owns; nothing a client sent
	// reaches this command line.
	out, err := exec.CommandContext(ctx, "ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // the pid is the harness's, not input
	if err != nil {
		return 0, fmt.Errorf("reading the resident set of pid %d: %w", pid, err)
	}
	kb, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("ps reported %q for pid %d: %w", string(out), pid, err)
	}
	return kb * 1024, nil
}

// hexOf renders bytes that cannot be printed, so a failure names what actually
// came back rather than a terminal's rendering of it. Nothing is elided: the
// payloads are short, and a corrupted value is only diagnosable in full.
func hexOf(s string) string {
	var out strings.Builder
	out.WriteString(strconv.Itoa(len(s)))
	out.WriteString(" bytes ")
	for i := range len(s) {
		fmt.Fprintf(&out, "%02x", s[i])
	}
	return out.String()
}
