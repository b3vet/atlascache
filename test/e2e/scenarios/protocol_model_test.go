package scenarios_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/b3vet/atlascache/test/e2e/client"
	"github.com/b3vet/atlascache/test/e2e/runner"
	"github.com/b3vet/atlascache/test/e2e/runner/fakeharness"
)

// pongText is what a healthy server answers PING with, as the fake harness has
// to be told to.
const pongText = "PONG"

// The four FEAT-0025 scenarios are run here against a model server that speaks
// RESP, and again against one carrying the exact defect each scenario exists to
// catch. The second half is the half that matters: a scenario that cannot fail
// reports green while the property it names goes unchecked, and every property
// here is one that used to hold and then did not (ISSUE-0016).
//
// The model is not AtlasCache and proves nothing about it. It is an independent
// reading of the same wire format, written so that "the scenario passes" and
// "the scenario fails for the right reason" are both observable.

// protoDefects are the ways a server can be wrong about untrusted input. Each
// one is a real failure mode: three of them are ISSUE-0016's bugs, one is what
// FuzzDecode found in the error path, and one is a parser that reads a bulk
// string as text.
type protoDefects struct {
	// keepsOpenAfterProtocolError answers the error and carries on reading. The
	// stream is unsynchronized by then, so whatever follows is parsed as a
	// fresh request — a malformed frame becomes a smuggled one.
	keepsOpenAfterProtocolError bool

	// diesOnMalformedInput stops the listener, standing in for the server that
	// refuses one bad frame and takes the process down with it.
	diesOnMalformedInput bool

	// pastesTheOffendingByte writes the byte it did not expect into the error
	// text raw. FuzzDecode found this in the shipped parser: the byte is
	// attacker-chosen, so a CR in it frames a second reply.
	pastesTheOffendingByte bool

	// reservesFromDeclaredSizes is ISSUE-0016's second and third bugs: the
	// allocation is sized from the number the client declared, before the limit
	// that would have refused it.
	reservesFromDeclaredSizes bool

	// readsInlineWithoutBound is ISSUE-0016's first: the line is accumulated
	// until a newline arrives and only then compared against the limit.
	readsInlineWithoutBound bool

	// truncatesValuesAtNUL reads a bulk string as text. RESP is length-prefixed
	// precisely so it is not, and a value cut at its first NUL is corrupted
	// silently.
	truncatesValuesAtNUL bool

	// answersTruncatedFrames replies to a frame that ended at EOF. There is no
	// client left to answer, and a server that writes anyway is writing into a
	// half-closed socket it should have given up on.
	answersTruncatedFrames bool

	// silentOnMalformedInput closes without saying why, so the client is left
	// to guess whether its frame was wrong or the server fell over.
	silentOnMalformedInput bool

	// vagueProtocolErrors answers every malformed frame with the same text.
	// The reply is a protocol error, so a check that only looked at the prefix
	// would pass while the client learns nothing about what it got wrong.
	vagueProtocolErrors bool

	// unprefixedProtocolErrors answers a malformed frame as though it were a
	// bad command. A client cannot tell from that that its stream is now
	// unsynchronized, and will keep sending.
	unprefixedProtocolErrors bool

	// dropsAfterAnUnknownCommand closes on a command it does not recognize. A
	// client that mistyped one command has not lost the right to send the next.
	dropsAfterAnUnknownCommand bool
}

// Limits the model enforces, matching internal/protocol's defaults closely
// enough that the same inputs land on the same side of them.
const (
	modelMaxInline    = 64 << 10
	modelMaxBulk      = (16 << 20) + (64 << 10)
	modelMaxMultiBulk = 1 << 20
	modelConnBuffer   = 16 << 10
)

// reservationScale shrinks what the defective model reserves. The real bug
// reserved the whole declared size — 512MB for a bulk header, 24MB of slice
// headers for a million-element one — and doing that two hundred times inside a
// test process would move a hundred gigabytes to demonstrate something the
// scaled version demonstrates in a fraction of a second. A model reserving an
// eighth of it still trips the scenario, which is the point: the assertion has
// margin to spare.
const reservationScale = 8

// reservationCeiling bounds what the defective model holds at once, so a test
// cannot exhaust the machine it runs on. It is comfortably over the headroom
// the scenario allows and comfortably under a real 512MB reservation.
const reservationCeiling = 192 << 20

// protoServer is a RESP server with switchable defects, over a real socket.
type protoServer struct {
	defects protoDefects

	mu       sync.Mutex
	values   map[string]string
	reserved [][]byte
	held     int
	dead     bool
}

func newProtoServer(defects protoDefects) *protoServer {
	return &protoServer{defects: defects, values: map[string]string{}}
}

func (s *protoServer) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		if s.isDead() {
			_ = conn.Close()
			continue
		}
		go s.handle(conn)
	}
}

func (s *protoServer) isDead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dead
}

func (s *protoServer) die() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dead = true
}

// handle is the connection loop: decode a request, answer it, and close on a
// protocol error because the stream cannot be resynchronised after one.
func (s *protoServer) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	r := bufio.NewReaderSize(conn, modelConnBuffer)
	w := bufio.NewWriter(conn)
	for {
		args, protoErr, err := s.decode(r)
		switch {
		case protoErr != "":
			if s.refuse(w, protoErr) {
				return
			}
			continue
		case err != nil:
			if s.defects.answersTruncatedFrames {
				// Deliberately writing into a connection whose client has
				// already gone. Whether the write lands is the socket's
				// business; that the server tried is the defect.
				if sendErr := send(w, truncatedFrameReply); sendErr != nil {
					return
				}
			}
			return
		case len(args) == 0:
			continue
		}

		reply := s.exec(args)
		if err := send(w, reply); err != nil {
			return
		}
		if s.defects.dropsAfterAnUnknownCommand && strings.Contains(reply, "unknown command") {
			return
		}
	}
}

// refuse answers a malformed frame and, unless the server is defective, ends
// the connection: the byte stream cannot be resynchronized after one.
//
// It returns rather than loops for the keepsOpenAfterProtocolError defect
// because the caller resumes the loop only when this says nothing.
func (s *protoServer) refuse(w *bufio.Writer, protoErr string) (done bool) {
	reply, keepOpen := s.protocolErrorReply(protoErr)
	if reply != "" {
		if err := send(w, reply); err != nil {
			return true
		}
	}
	if s.defects.diesOnMalformedInput {
		s.die()
		return true
	}
	return !keepOpen
}

func send(w *bufio.Writer, reply string) error {
	if _, err := w.WriteString(reply); err != nil {
		return err
	}
	return w.Flush()
}

// truncatedFrameReply is what a server with the answersTruncatedFrames defect
// says to a client that has already hung up.
const truncatedFrameReply = "-ERR Protocol error: your frame stopped early\r\n"

// protocolErrorReply is what the server answers a malformed frame with, and
// whether the connection may carry on afterwards. Correct behavior is to name
// the fault and close; every other branch is a defect a scenario must catch.
func (s *protoServer) protocolErrorReply(protoErr string) (reply string, keepOpen bool) {
	switch {
	case s.defects.silentOnMalformedInput:
		reply = ""
	case s.defects.vagueProtocolErrors:
		reply = "-ERR Protocol error: bad request\r\n"
	case s.defects.unprefixedProtocolErrors:
		reply = "-ERR bad request\r\n"
	default:
		reply = fmt.Sprintf("-ERR Protocol error: %s\r\n", protoErr)
	}
	return reply, s.defects.keepsOpenAfterProtocolError
}

// decode reads one request. It returns the arguments, or a protocol error
// message, or a transport error meaning the connection ended.
func (s *protoServer) decode(r *bufio.Reader) ([]string, string, error) {
	line, protoErr, err := s.readLine(r)
	if protoErr != "" || err != nil {
		return nil, protoErr, err
	}
	if line == "" {
		return nil, "", nil
	}
	if line[0] != '*' {
		return strings.Fields(line), "", nil
	}

	count, convErr := strconv.Atoi(line[1:])
	if convErr != nil || count < 0 || count > modelMaxMultiBulk {
		return nil, "invalid multibulk length", nil
	}
	if s.defects.reservesFromDeclaredSizes {
		// make([][]byte, 0, count): a twelve-byte header reserving a slice
		// header per element it merely claims to be about to send.
		s.reserve(count * 24)
	}
	if count == 0 {
		return nil, "", nil
	}

	args := make([]string, 0, min(count, 64))
	for range count {
		arg, protoErr, err := s.decodeBulk(r)
		if protoErr != "" || err != nil {
			return nil, protoErr, err
		}
		args = append(args, arg)
	}
	return args, "", nil
}

func (s *protoServer) decodeBulk(r *bufio.Reader) (string, string, error) {
	line, protoErr, err := s.readLine(r)
	if protoErr != "" || err != nil {
		return "", protoErr, err
	}
	if line == "" || line[0] != '$' {
		return "", fmt.Sprintf("expected '$', got '%s'", s.renderByte(line)), nil
	}

	length, convErr := strconv.Atoi(line[1:])
	if s.defects.reservesFromDeclaredSizes && convErr == nil && length > 0 {
		// make([]byte, length+2) before the limit that refuses the length.
		s.reserve(length)
	}
	if convErr != nil || length < 0 || length > modelMaxBulk {
		return "", "invalid bulk length", nil
	}

	payload := make([]byte, length+2)
	if _, err := io.ReadFull(r, payload); err != nil {
		return "", "", io.ErrUnexpectedEOF
	}
	if payload[length] != '\r' || payload[length+1] != '\n' {
		return "", "expected CRLF after bulk string", nil
	}
	return string(payload[:length]), "", nil
}

// renderByte is the error path FuzzDecode found. Correct behavior quotes the
// byte; the defect pastes it in raw, so a client-chosen CR lands in a reply.
func (s *protoServer) renderByte(line string) string {
	if line == "" {
		return ""
	}
	if s.defects.pastesTheOffendingByte {
		return line[:1]
	}
	quoted := strconv.Quote(line[:1])
	return quoted[1 : len(quoted)-1]
}

// readLine accumulates one CRLF-terminated line under the inline limit, or —
// with the defect — reads to the newline first and checks the limit afterwards.
func (s *protoServer) readLine(r *bufio.Reader) (string, string, error) {
	if s.defects.readsInlineWithoutBound {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return "", "", err
		}
		if len(line) > modelMaxInline {
			return "", "too big inline request", nil
		}
		return trimTerminator(string(line))
	}

	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > modelMaxInline {
			return "", "too big inline request", nil
		}
		line = append(line, chunk...)
		switch {
		case err == nil:
			return trimTerminator(string(line))
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return "", "", err
		}
	}
}

func trimTerminator(line string) (string, string, error) {
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return "", "expected CRLF line terminator", nil
	}
	return line[:len(line)-2], "", nil
}

// reserve makes the allocation the declared size asked for, scaled down, and
// holds on to it. Holding is what makes the resident set move: an allocation
// freed before the next sample would be invisible to the instrument the
// scenario uses, and a real parser freeing 512MB two hundred times in a row is
// not invisible to it either.
func (s *protoServer) reserve(size int) {
	scaled := size / reservationScale
	if scaled <= 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held >= reservationCeiling {
		return
	}
	buf := make([]byte, scaled)
	// Touch a byte per page, so the reservation is resident and not merely
	// promised — which is exactly what happens when a parser reads into it.
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = 1
	}
	s.reserved = append(s.reserved, buf)
	s.held += scaled
}

func (s *protoServer) exec(args []string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch strings.ToUpper(args[0]) {
	case "PING":
		if len(args) > 1 {
			return bulkReply(args[1])
		}
		return "+PONG\r\n"
	case "SET":
		if len(args) < 3 {
			return "-ERR wrong number of arguments for 'set' command\r\n"
		}
		value := args[2]
		if s.defects.truncatesValuesAtNUL {
			value, _, _ = strings.Cut(value, "\x00")
		}
		s.values[args[1]] = value
		return "+OK\r\n"
	case "GET":
		if len(args) < 2 {
			return "-ERR wrong number of arguments for 'get' command\r\n"
		}
		value, ok := s.values[args[1]]
		if !ok {
			return "$-1\r\n"
		}
		return bulkReply(value)
	case "DEL":
		if _, ok := s.values[args[1]]; ok {
			delete(s.values, args[1])
			return ":1\r\n"
		}
		return ":0\r\n"
	default:
		return "-ERR unknown command '" + args[0] + "'\r\n"
	}
}

func bulkReply(s string) string {
	return "$" + strconv.Itoa(len(s)) + "\r\n" + s + "\r\n"
}

// protoHarness puts a runner.Harness in front of a protoServer. Its PID is this
// test process, which is what the allocation scenario measures: the model's
// reservations are this process's resident set.
type protoHarness struct {
	server *protoServer
	addr   string
	root   string
}

func (h *protoHarness) Start(context.Context) error   { return nil }
func (h *protoHarness) Stop(context.Context) error    { return nil }
func (h *protoHarness) Restart(context.Context) error { return nil }
func (h *protoHarness) Kill() error                   { return nil }
func (h *protoHarness) Close() error                  { return nil }
func (h *protoHarness) Logs() string                  { return "" }
func (h *protoHarness) Root() string                  { return h.root }

func (h *protoHarness) Info() runner.ServerInfo {
	return runner.ServerInfo{ClientAddr: h.addr, AdminAddr: h.addr, DataDir: h.root, PID: os.Getpid()}
}

// Send goes over a real connection to the model, so the harness path a spec's
// own steps take is the same wire the scenarios use.
func (h *protoHarness) Send(ctx context.Context, cmd string) (runner.Reply, error) {
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

func newProtoHarness(t *testing.T, defects protoDefects) *protoHarness {
	t.Helper()

	var config net.ListenConfig
	ln, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	server := newProtoServer(defects)
	go server.serve(ln)

	return &protoHarness{server: server, addr: ln.Addr().String(), root: t.TempDir()}
}

// The scenarios under test. Named here so a rename has one place to fail.
const (
	scenarioMalformed    = "proto_malformed_input_is_refused"
	scenarioBinary       = "proto_binary_values_round_trip"
	scenarioDeclared     = "proto_declared_sizes_cost_nothing"
	scenarioUnterminated = "proto_unterminated_line_is_refused"
)

func TestProtoScenariosAreRegistered(t *testing.T) {
	t.Parallel()

	for _, name := range []string{scenarioMalformed, scenarioBinary, scenarioDeclared, scenarioUnterminated} {
		if _, ok := runner.LookupScenario(name); !ok {
			t.Errorf("%s did not register itself, so no spec can name it", name)
		}
	}
}

func TestProtoMalformedInputIsRefused(t *testing.T) {
	t.Parallel()

	t.Run("a server that refuses and closes passes", func(t *testing.T) {
		t.Parallel()
		h := newProtoHarness(t, protoDefects{})
		if failure := runScenario(t, scenarioMalformed, h); failure != nil {
			t.Fatalf("a server handling malformed input correctly failed: %s\n%s",
				failure.Message, strings.Join(failure.Notes, "\n"))
		}
	})

	t.Run("a server that keeps the connection after a protocol error is caught", func(t *testing.T) {
		t.Parallel()
		h := newProtoHarness(t, protoDefects{keepsOpenAfterProtocolError: true})
		requireFailure(t, runScenario(t, scenarioMalformed, h), "left the connection open")
	})

	t.Run("a server that dies on malformed input is caught", func(t *testing.T) {
		t.Parallel()
		h := newProtoHarness(t, protoDefects{diesOnMalformedInput: true})
		requireFailure(t, runScenario(t, scenarioMalformed, h), "PING")
	})

	t.Run("a server pasting the offending byte into the error is caught", func(t *testing.T) {
		t.Parallel()
		// The FuzzDecode finding: "*1\r\n\r\r\n" made the server echo a raw CR
		// back inside an error reply.
		h := newProtoHarness(t, protoDefects{pastesTheOffendingByte: true})
		requireFailure(t, runScenario(t, scenarioMalformed, h), `expected '$', got '\r'`)
	})

	t.Run("a server answering a frame that ended at EOF is caught", func(t *testing.T) {
		t.Parallel()
		h := newProtoHarness(t, protoDefects{answersTruncatedFrames: true})
		requireFailure(t, runScenario(t, scenarioMalformed, h), "has nobody to answer")
	})

	t.Run("a server that closes without saying why is caught", func(t *testing.T) {
		t.Parallel()
		h := newProtoHarness(t, protoDefects{silentOnMalformedInput: true})
		requireFailure(t, runScenario(t, scenarioMalformed, h), "no reply")
	})

	t.Run("a server answering every malformed frame the same way is caught", func(t *testing.T) {
		t.Parallel()
		h := newProtoHarness(t, protoDefects{vagueProtocolErrors: true})
		requireFailure(t, runScenario(t, scenarioMalformed, h), "want it to say")
	})

	t.Run("a server reporting a malformed frame as a bad command is caught", func(t *testing.T) {
		t.Parallel()
		h := newProtoHarness(t, protoDefects{unprefixedProtocolErrors: true})
		requireFailure(t, runScenario(t, scenarioMalformed, h), "want a reply starting")
	})

	t.Run("a server hanging up on an unknown command is caught", func(t *testing.T) {
		t.Parallel()
		h := newProtoHarness(t, protoDefects{dropsAfterAnUnknownCommand: true})
		requireFailure(t, runScenario(t, scenarioMalformed, h), "same connection")
	})

	t.Run("a harness with no server to reach fails rather than passing vacuously", func(t *testing.T) {
		t.Parallel()
		h := fakeharness.New(map[string]runner.Reply{"PING": runner.StatusReply(pongText)})
		requireFailure(t, runScenario(t, scenarioMalformed, h), "dialing")
	})
}

func TestProtoBinaryValuesRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("a binary-safe server passes", func(t *testing.T) {
		t.Parallel()
		h := newProtoHarness(t, protoDefects{})
		if failure := runScenario(t, scenarioBinary, h); failure != nil {
			t.Fatalf("a binary-safe server failed: %s", failure.Message)
		}
	})

	t.Run("a server that reads a value as text is caught", func(t *testing.T) {
		t.Parallel()
		h := newProtoHarness(t, protoDefects{truncatesValuesAtNUL: true})
		requireFailure(t, runScenario(t, scenarioBinary, h), "did not survive the round trip")
	})
}

// TestProtoDeclaredSizesCostNothing is deliberately not parallel: it measures
// this process's resident set, and a test allocating alongside it would be
// indistinguishable from the defect it is looking for.
func TestProtoDeclaredSizesCostNothing(t *testing.T) {
	t.Run("a server that refuses before reserving passes", func(t *testing.T) {
		h := newProtoHarness(t, protoDefects{})
		failure := runScenario(t, scenarioDeclared, h)
		if failure != nil {
			t.Fatalf("a server that refuses a declared size before reserving failed: %s\n%s",
				failure.Message, strings.Join(failure.Notes, "\n"))
		}
	})

	t.Run("a server that reserves from the declared size is caught", func(t *testing.T) {
		h := newProtoHarness(t, protoDefects{reservesFromDeclaredSizes: true})
		requireFailure(t, runScenario(t, scenarioDeclared, h), "grew the resident set")
		// And it is caught on the memory rather than on the reply: the replies
		// were correct the whole time, which is exactly why ISSUE-0016 shipped.
		h.server.mu.Lock()
		held := h.server.held
		h.server.mu.Unlock()
		if held == 0 {
			t.Error("the defective model reserved nothing, so the scenario failed for some other reason")
		}
		t.Logf("the defective model held %d bytes reserved from declared sizes", held)
	})
}

func TestProtoUnterminatedLineIsRefused(t *testing.T) {
	t.Run("a server bounding the line as it accumulates passes", func(t *testing.T) {
		h := newProtoHarness(t, protoDefects{})
		if failure := runScenario(t, scenarioUnterminated, h); failure != nil {
			t.Fatalf("a server enforcing the inline limit while reading failed: %s\n%s",
				failure.Message, strings.Join(failure.Notes, "\n"))
		}
	})

	t.Run("a server checking the limit after the read is caught", func(t *testing.T) {
		h := newProtoHarness(t, protoDefects{readsInlineWithoutBound: true})
		requireFailure(t, runScenario(t, scenarioUnterminated, h), "applied after the accumulation")
	})
}

// TestProtoMemoryScenariosNeedAProcessToMeasure. Both allocation scenarios read
// the server's resident set, which a harness with no process behind it cannot
// provide. They must say so rather than skipping the measurement and reporting
// a pass that measured nothing.
func TestProtoMemoryScenariosNeedAProcessToMeasure(t *testing.T) {
	t.Parallel()

	for _, name := range []string{scenarioDeclared, scenarioUnterminated} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := fakeharness.New(map[string]runner.Reply{"PING": runner.StatusReply(pongText)})
			requireFailure(t, runScenario(t, name, h), "cannot be measured")
		})
	}
}
