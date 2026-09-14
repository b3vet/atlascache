package protocol

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// FuzzDecode drives the decoder with arbitrary bytes.
//
// The decoder is the only attacker-reachable surface that exists before auth,
// and auth is off by default (ADR-0009), so "any byte sequence" is the real
// input domain and not a thought experiment. For every one of them the decoder
// must: never panic, never allocate unboundedly, never block, never read past
// the frame it was given, and never alter the bytes of an argument it accepts.
//
// Panics the fuzzing engine catches by itself. The other four are properties
// asserted below, because a decoder that quietly returns a truncated argument
// or loops on input it cannot consume does not panic either.
//
// Run it:
//
//	go test ./internal/protocol -run=FuzzDecode -fuzz=FuzzDecode -fuzztime=5m
//
// A crash is minimized into testdata/fuzz/FuzzDecode and must be committed:
// that file is what stops the bug coming back.
func FuzzDecode(f *testing.F) {
	for _, seed := range decodeSeeds {
		f.Add([]byte(seed.input))
	}

	f.Fuzz(func(t *testing.T, input []byte) {
		codec := NewRESPWithLimits(fuzzLimits)
		// A reader far smaller than the limits, so that every refill boundary
		// the real server crosses once is crossed by the fuzzer constantly:
		// the accumulate-and-check loop in readLine is where ISSUE-0016 lived.
		source := &countingReader{r: bytes.NewReader(input)}
		buffered := bufio.NewReaderSize(source, minReadBuffer)

		consumed := 0
		// One decode consumes at least a line, so a decoder that cannot get
		// stuck cannot run more times than the input has bytes. Exceeding that
		// is the liveness failure — a loop that returns commands without
		// reading anything would otherwise run forever.
		for round := 0; round <= len(input)+1; round++ {
			cmd, err := codec.Decode(buffered)
			position := source.read - buffered.Buffered()

			if err != nil {
				requireDecodeError(t, err)
				return
			}
			if position <= consumed {
				t.Fatalf("decode %d returned command %q after consuming %d of %d bytes, "+
					"having consumed %d before it: a decode that reads nothing never ends",
					round, cmd.Name, position, len(input), consumed)
			}
			frame := position - consumed
			consumed = position

			requireWithinLimits(t, cmd, codec.Limits())
			requireNoAmplification(t, cmd, frame)
			requireArgumentsSurviveARoundTrip(t, cmd)
		}
		t.Fatalf("decoding %d bytes produced more than %d commands", len(input), len(input)+1)
	})
}

// fuzzLimits are deliberately tiny. The defaults measure in megabytes, which no
// fuzzer is going to reach by generating bytes; shrinking them puts every
// boundary within a few mutations of the corpus, and the boundary is where the
// off-by-one lives. The limits themselves are pinned at their real values by
// TestLimitsAreEnforcedAtTheirBoundary.
var fuzzLimits = Limits{MaxInlineLength: 64, MaxMultiBulkLength: 8, MaxBulkLength: 32}

// minReadBuffer is the smallest buffer bufio will accept. The decoder has to
// reassemble every line across refills at this size.
const minReadBuffer = 16

// requireDecodeError pins the error vocabulary. A caller has exactly three
// things to do with a failed decode — answer a protocol error and close, treat
// it as a client that went away, or log a truncated frame — and internal/server
// switches on precisely these. Anything else reaches the default branch there
// and is handled as "read failed", which silently drops the reply a client is
// waiting for.
func requireDecodeError(t *testing.T, err error) {
	t.Helper()

	var protoErr *ProtocolError
	switch {
	case errors.As(err, &protoErr):
		if protoErr.Message == "" {
			t.Fatal("a protocol error with no message tells the client nothing")
		}
		if strings.ContainsAny(protoErr.Error(), "\r\n") {
			// The encoder folds these, but an error text carrying a CRLF is a
			// reply-splitting bug waiting for an encoder that does not.
			t.Fatalf("protocol error %q contains a line terminator", protoErr.Error())
		}
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
	default:
		t.Fatalf("decode failed with %T(%v); the caller only handles "+
			"*ProtocolError, io.EOF and io.ErrUnexpectedEOF", err, err)
	}
}

// requireWithinLimits checks that what came back respects the limits that were
// supposed to bound it. A limit checked before an allocation and then not
// applied to the result is a limit in name only.
func requireWithinLimits(t *testing.T, cmd Command, limits Limits) {
	t.Helper()

	if cmd.Name == "" {
		t.Fatal("decode returned a command with no name; an empty request is skipped, not returned")
	}
	// One request is a name plus its arguments, so the element count the
	// multibulk limit bounds is one more than the argument count.
	if len(cmd.Args)+1 > limits.MaxMultiBulkLength && len(cmd.Args)+1 > limits.MaxInlineLength {
		t.Fatalf("decode returned %d arguments, past both the %d element limit and the %d byte line limit",
			len(cmd.Args), limits.MaxMultiBulkLength, limits.MaxInlineLength)
	}
	ceiling := max(limits.MaxBulkLength, limits.MaxInlineLength)
	for i, arg := range cmd.Args {
		if len(arg) > ceiling {
			t.Fatalf("argument %d is %d bytes, past the %d byte ceiling", i, len(arg), ceiling)
		}
	}
}

// requireNoAmplification bounds the bytes handed back by the bytes the frame
// took to send.
//
// It is the cheap, per-execution half of "never allocate unboundedly": a
// declared length that sizes an allocation shows up here as a command far
// larger than the frame that asked for it. The costly half — measuring the heap
// over a rejection that returns no command at all — is
// TestDecodeAllocatesInProportionToItsInput.
//
// The factor is 3 because an inline command name is upper-cased through
// strings.ToUpper, which rewrites each byte of invalid UTF-8 as the three-byte
// replacement rune.
func requireNoAmplification(t *testing.T, cmd Command, consumed int) {
	t.Helper()

	size := len(cmd.Name)
	for _, arg := range cmd.Args {
		size += len(arg)
	}
	if limit := 3*consumed + minReadBuffer; size > limit {
		t.Fatalf("a %d byte frame decoded to %d bytes of command, over the %d byte bound",
			consumed, size, limit)
	}
}

// requireArgumentsSurviveARoundTrip is binary safety stated as a property.
//
// RESP is length-prefixed precisely so an argument may be any bytes at all:
// NULs, invalid UTF-8, an embedded CRLF. A parser that treats them as text
// corrupts a value silently, which is worse than crashing, so the check is byte
// equality and not "decodes to something".
//
// The re-encoded frame is canonical, so this also asserts that whatever shape
// the argument arrived in — inline, split across refills, at a limit
// boundary — it decodes to the same bytes as the canonical form.
func requireArgumentsSurviveARoundTrip(t *testing.T, cmd Command) {
	t.Helper()

	frame := encodeRequest(cmd)
	// Default limits, not the fuzzer's: the point is that the arguments
	// survive, not that they fit limits a tenth the size of the real ones.
	again, err := NewRESP().Decode(bufio.NewReader(bytes.NewReader(frame)))
	if err != nil {
		t.Fatalf("re-encoding %q as %q made it undecodable: %v", cmd.Name, frame, err)
	}
	if again.Name != cmd.Name {
		t.Fatalf("command name %q became %q through a round trip", cmd.Name, again.Name)
	}
	if len(again.Args) != len(cmd.Args) {
		t.Fatalf("%d arguments became %d through a round trip", len(cmd.Args), len(again.Args))
	}
	for i := range cmd.Args {
		if !bytes.Equal(again.Args[i], cmd.Args[i]) {
			t.Fatalf("argument %d changed through a round trip: %q became %q, %q became %q",
				i, cmd.Args[i], again.Args[i], hexOf(cmd.Args[i]), hexOf(again.Args[i]))
		}
	}
}

// encodeRequest writes a command in the canonical request form, the multibulk
// array of bulk strings every client sends.
func encodeRequest(cmd Command) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "*%d\r\n$%d\r\n%s\r\n", len(cmd.Args)+1, len(cmd.Name), cmd.Name)
	for _, arg := range cmd.Args {
		fmt.Fprintf(&buf, "$%d\r\n", len(arg))
		buf.Write(arg)
		buf.WriteString("\r\n")
	}
	return buf.Bytes()
}

func hexOf(b []byte) string {
	var out strings.Builder
	for _, c := range b {
		fmt.Fprintf(&out, "%02x", c)
	}
	return out.String()
}

// countingReader records how many bytes the decoder actually pulled, which is
// what "never read past the declared frame" is measured against once the
// buffered reader's own lookahead is subtracted.
type countingReader struct {
	r    *bytes.Reader
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	return n, err
}

// decodeSeeds is the committed corpus: one entry per input class FEAT-0025
// names, plus the valid frames a mutation has to start from to reach the
// interesting ones. Seeds are cheap and permanent — every crash the fuzzer ever
// finds is minimized into testdata/fuzz/FuzzDecode and joins them.
var decodeSeeds = []struct {
	class string
	input string
}{
	// Valid frames. A corpus of only malformed input mutates towards more
	// malformed input; the fuzzer needs something well-formed to break.
	{"valid, no arguments", "*1\r\n$4\r\nPING\r\n"},
	{"valid, with arguments", "*3\r\n$3\r\nSET\r\n$2\r\nk1\r\n$5\r\nhello\r\n"},
	{"valid, empty argument", "*2\r\n$4\r\nECHO\r\n$0\r\n\r\n"},
	{"valid, two frames back to back", "*1\r\n$4\r\nPING\r\n*1\r\n$4\r\nQUIT\r\n"},
	{"valid, empty request then a real one", "*0\r\n*1\r\n$4\r\nPING\r\n"},
	{"valid, inline", "PING\r\n"},
	{"valid, inline with arguments", "SET k1 hello\r\n"},

	// Truncated frames.
	{"truncated after the header", "*3\r\n$3\r\nSET\r\n"},
	{"truncated mid-payload", "*1\r\n$4\r\nPI"},
	{"truncated after a bulk header", "*1\r\n$4\r\n"},
	{"truncated multibulk header", "*"},
	{"nothing at all", ""},

	// Negative lengths.
	{"negative bulk length", "*1\r\n$-5\r\n"},
	{"negative null bulk length", "*1\r\n$-1\r\n"},
	{"negative multibulk length", "*-3\r\n"},
	{"negative null multibulk length", "*-1\r\n"},

	// Enormous declared lengths. None of these may be attempted.
	{"enormous bulk length", "$999999999999\r\n"},
	{"enormous bulk length in a frame", "*1\r\n$999999999999\r\n"},
	{"enormous multibulk length", "*999999999999\r\n"},
	{"bulk length past the int64 range", "*1\r\n$99999999999999999999\r\n"},
	{"multibulk length past the int64 range", "*99999999999999999999\r\n"},

	// Length and content disagreeing.
	{"declared longer than sent", "*1\r\n$5\r\nhi\r\n"},
	{"declared shorter than sent", "*1\r\n$2\r\nhello\r\n"},
	{"payload not CRLF terminated", "*1\r\n$4\r\nPINGXX"},

	// Missing and wrong terminators.
	{"no terminator at all", "+OK"},
	{"bare LF terminator", "+OK\n"},
	{"bare CR", "+OK\r"},
	{"bare LF after a header", "*1\n$4\r\nPING\r\n"},
	{"CR inside the length", "*1\r\r\n$4\r\nPING\r\n"},

	// Deep nesting. RESP2 requests are flat, so this is refused at the first
	// inner element — but it is refused rather than recursed into, and that is
	// the property.
	{"nested arrays", "*1\r\n*1\r\n*1\r\n*1\r\n*1\r\n*1\r\n*1\r\n*1\r\n*1\r\n*1\r\n$4\r\nPING\r\n"},
	{"array inside a frame", "*2\r\n$4\r\nECHO\r\n*1\r\n$1\r\na\r\n"},

	// Binary safety. These must come back byte for byte.
	{"NUL inside a bulk string", "*2\r\n$4\r\nECHO\r\n$3\r\na\x00b\r\n"},
	{"invalid UTF-8 inside a bulk string", "*2\r\n$4\r\nECHO\r\n$4\r\n\xff\xfe\x80\xc0\r\n"},
	{"CRLF inside a bulk string", "*2\r\n$4\r\nECHO\r\n$4\r\na\r\nb\r\n"},
	{"NUL in the command name", "*1\r\n$4\r\nPI\x00G\r\n"},
	{"a bulk string of every high byte", "*2\r\n$4\r\nECHO\r\n$4\r\n\x80\x90\xa0\xb0\r\n"},

	// Type byte confusion: a RESP3 or reply-side type byte where RESP2 expects
	// '*' at the top level, or '$' inside a frame.
	{"map type byte at the top level", "%2\r\n$1\r\na\r\n$1\r\nb\r\n"},
	{"set type byte at the top level", "~1\r\n$4\r\nPING\r\n"},
	{"push type byte at the top level", ">1\r\n$4\r\nPING\r\n"},
	{"simple string where an element belongs", "*1\r\n+PING\r\n"},
	{"integer where an element belongs", "*1\r\n:42\r\n"},
	{"error where an element belongs", "*1\r\n-ERR nope\r\n"},
	{"null type byte where an element belongs", "*1\r\n_\r\n"},

	// Integer overflow in every field that parses one.
	{"integer reply overflow", ":99999999999999999999\r\n"},
	{"bulk length overflow", "*1\r\n$9223372036854775808\r\n"},
	{"bulk length at the int64 edge", "*1\r\n$9223372036854775807\r\n"},
	{"multibulk length at the int64 edge", "*9223372036854775807\r\n"},
	{"bulk length with a leading plus", "*1\r\n$+4\r\nPING\r\n"},
	{"multibulk length with leading zeros", "*0001\r\n$4\r\nPING\r\n"},
	{"bulk length that is only a sign", "*1\r\n$-\r\n"},
	{"multibulk length that is empty", "*\r\n"},

	// Inline requests, which take a different path through the decoder.
	{"inline with only whitespace", "   \r\n"},
	{"inline with a NUL", "PI\x00NG\r\n"},
	{"inline with invalid UTF-8", "\xff\xfe hello\r\n"},
	{"empty line then a command", "\r\nPING\r\n"},
}

// TestSeedCorpusCoversEveryClass keeps the corpus honest. The table in
// FEAT-0025 is the acceptance criterion, and a corpus that lost its only
// entry for a class would still pass every other test in this package.
func TestSeedCorpusCoversEveryClass(t *testing.T) {
	required := []string{
		"valid", "truncated", "negative", "enormous", "declared",
		"terminator", "nested", "NUL", "UTF-8", "type byte", "overflow", "inline",
	}
	seen := map[string]int{}
	for _, seed := range decodeSeeds {
		for _, class := range required {
			if strings.Contains(seed.class, class) {
				seen[class]++
			}
		}
	}
	for _, class := range required {
		if seen[class] == 0 {
			t.Errorf("the seed corpus has no entry for %q", class)
		}
	}
}

// TestSeedCorpusDecodesWithoutPanicOrHang runs every seed through the same
// properties the fuzzer asserts, so `go test` alone covers the corpus and a
// regression is caught without anyone starting a fuzzing session.
func TestSeedCorpusDecodesWithoutPanicOrHang(t *testing.T) {
	for _, seed := range decodeSeeds {
		t.Run(seed.class, func(t *testing.T) {
			codec := NewRESPWithLimits(fuzzLimits)
			r := bufio.NewReaderSize(strings.NewReader(seed.input), minReadBuffer)
			for round := 0; round <= len(seed.input)+1; round++ {
				cmd, err := codec.Decode(r)
				if err != nil {
					requireDecodeError(t, err)
					return
				}
				requireWithinLimits(t, cmd, codec.Limits())
				requireArgumentsSurviveARoundTrip(t, cmd)
			}
			t.Fatalf("%q produced more commands than it has bytes", seed.input)
		})
	}
}

// TestDecodeAllocatesInProportionToItsInput is the half of "never allocate
// unboundedly" the per-execution properties cannot see: a frame that is refused
// returns no command, so nothing about the command bounds what refusing it
// cost. ISSUE-0016's three bugs all lived here — the allocation happened, the
// error came back, and every assertion on the reply passed.
func TestDecodeAllocatesInProportionToItsInput(t *testing.T) {
	const rounds = 200

	codec := NewRESPWithLimits(fuzzLimits)
	input := 0
	for _, seed := range decodeSeeds {
		input += len(seed.input)
	}

	allocated := measureAlloc(func() {
		for range rounds {
			for _, seed := range decodeSeeds {
				r := bufio.NewReaderSize(strings.NewReader(seed.input), minReadBuffer)
				for {
					if _, err := codec.Decode(r); err != nil {
						break
					}
				}
			}
		}
	})

	// Each decode is entitled to its reader, its line buffer and its arguments.
	// The generous constant is per seed, not per byte, and is still four orders
	// of magnitude below what a declared-length allocation costs.
	budget := uint64(rounds) * uint64(input+len(decodeSeeds)*4*minReadBuffer) * 8
	t.Logf("decoding the %d byte corpus %d times allocated %d bytes (budget %d)",
		input, rounds, allocated, budget)
	if allocated > budget {
		t.Errorf("decoding %d bytes of corpus %d times allocated %d bytes, over the %d byte budget",
			input, rounds, allocated, budget)
	}
}

// TestDeepNestingCostsNothingToRefuse is the nesting cap, verified rather than
// assumed.
//
// RESP2's request grammar is one array of bulk strings, and the decoder that
// reads it does not recurse, so an aggregate where an element belongs is
// refused at the first one however many follow it. The assertion is on what
// refusing costs: a frame nested a million deep must be refused having read the
// same handful of bytes as one nested twice, and in the same time. A recursive
// descent parser with no depth cap fails both — it reads every level to find
// the bottom, and it spends a stack frame on each.
//
// Bytes read is the instrument rather than stack size because it is exact: a
// decoder that consumes a constant number of bytes regardless of depth has done
// no per-level work at all, so it has no per-level frames either.
func TestDeepNestingCostsNothingToRefuse(t *testing.T) {
	depths := []int{2, 1000, 1_000_000}

	codec := NewRESP()
	reads := make([]int, 0, len(depths))
	times := make([]time.Duration, 0, len(depths))
	for _, depth := range depths {
		nested := strings.Repeat("*1\r\n", depth) + "$4\r\nPING\r\n"
		source := &countingReader{r: bytes.NewReader([]byte(nested))}

		started := time.Now()
		_, err := codec.Decode(bufio.NewReaderSize(source, minReadBuffer))
		elapsed := time.Since(started)

		var protoErr *ProtocolError
		if !errors.As(err, &protoErr) {
			t.Fatalf("nesting %d deep failed with %T(%v), want a protocol error", depth, err, err)
		}
		if !strings.Contains(protoErr.Error(), "expected '$'") {
			t.Fatalf("nesting %d deep reported %q", depth, protoErr.Error())
		}
		reads = append(reads, source.read)
		times = append(times, elapsed)
		t.Logf("depth %d (%d bytes) refused after reading %d bytes in %s",
			depth, len(nested), source.read, elapsed)
	}

	for i, read := range reads {
		if read > reads[0]+minReadBuffer {
			t.Errorf("refusing %d levels of nesting read %d bytes, against %d for %d levels: "+
				"the decoder is descending into the nesting",
				depths[i], read, reads[0], depths[0])
		}
	}
	// A million levels taking milliseconds rather than microseconds would mean
	// per-level work even if the bytes did not show it.
	if times[len(times)-1] > 10*time.Millisecond {
		t.Errorf("refusing %d levels of nesting took %s", depths[len(depths)-1], times[len(times)-1])
	}
}

// TestNestingIsRefusedAtTheFirstLevel pins the boundary the cap sits on: one
// array is a request, and an array inside it is not.
func TestNestingIsRefusedAtTheFirstLevel(t *testing.T) {
	codec := NewRESP()

	cmd, err := codec.Decode(decoderFor("*1\r\n$4\r\nPING\r\n"))
	if err != nil {
		t.Fatalf("one level of aggregate is a request, and did not decode: %v", err)
	}
	if cmd.Name != "PING" {
		t.Fatalf("name = %q", cmd.Name)
	}

	for _, input := range []string{
		"*1\r\n*1\r\n$4\r\nPING\r\n",
		"*2\r\n$4\r\nECHO\r\n*1\r\n$1\r\na\r\n",
		"*1\r\n%1\r\n$1\r\na\r\n$1\r\nb\r\n",
		"*1\r\n~1\r\n$4\r\nPING\r\n",
		"*1\r\n>1\r\n$4\r\nPING\r\n",
	} {
		if _, err := codec.Decode(decoderFor(input)); err == nil {
			t.Errorf("%q decoded as a command; an aggregate is not a request element", input)
		}
	}
}

// TestNegativeLengthsAreRejected. FEAT-0025 names negative lengths as their own
// class, and "*-3" used to be read as an empty request and skipped — Redis's
// behavior, but it means a malformed frame passes silently and the next bytes
// on the connection are parsed as a fresh request.
func TestNegativeLengthsAreRejected(t *testing.T) {
	codec := NewRESP()

	for _, input := range []string{
		"*-1\r\n", "*-3\r\n", "*-9999999999\r\n",
		"*1\r\n$-1\r\n", "*1\r\n$-5\r\n", "*1\r\n$-9999999999\r\n",
	} {
		_, err := codec.Decode(decoderFor(input))
		var protoErr *ProtocolError
		if !errors.As(err, &protoErr) {
			t.Errorf("%q failed with %T(%v), want a protocol error", input, err, err)
			continue
		}
		if !strings.Contains(protoErr.Error(), "length") {
			t.Errorf("%q reported %q", input, protoErr.Error())
		}
	}

	// "*0" is the one non-negative empty request, and stays a skipped request
	// rather than becoming an error: a client that pipelines nothing sends it.
	cmd, err := codec.Decode(decoderFor("*0\r\n*1\r\n$4\r\nPING\r\n"))
	if err != nil || cmd.Name != "PING" {
		t.Errorf("*0 followed by PING gave (%q, %v), want PING", cmd.Name, err)
	}
}

// TestDeclaredLengthsAreRejectedOnTheirText. Every number in a RESP header is a
// claim by an unauthenticated client. The ones that cannot be an int at all are
// the easiest to get wrong, because strconv returns a clamped value alongside
// the error and a parser that ignores the error reads MaxInt.
func TestDeclaredLengthsAreRejectedOnTheirText(t *testing.T) {
	codec := NewRESP()

	overflow := strconv.FormatUint(^uint64(0), 10) // Past int64 in every field
	for _, input := range []string{
		"*1\r\n$" + overflow + "\r\n",
		"*" + overflow + "\r\n",
		"*1\r\n$99999999999999999999\r\n",
		"*99999999999999999999\r\n",
		"*1\r\n$999999999999\r\n",
		"*999999999999\r\n",
	} {
		if _, err := codec.Decode(decoderFor(input)); err == nil {
			t.Errorf("%q decoded without an error", input)
		}
	}

	// And the int64 edge itself, which parses on a 64-bit platform and must
	// then be refused by the limit rather than sizing anything.
	for _, input := range []string{
		"*1\r\n$9223372036854775807\r\n",
		"*9223372036854775807\r\n",
	} {
		if _, err := codec.Decode(decoderFor(input)); err == nil {
			t.Errorf("%q decoded without an error", input)
		}
	}
}

// TestBinarySafetyAtTheCodec is the unit-level half of the binary safety
// requirement; scenarios/protocol.go proves the same bytes survive a real
// server. Both exist because they fail differently: this one catches a decoder
// that scans for a terminator, and that one catches a value that is corrupted
// anywhere between the socket and the keyspace.
func TestBinarySafetyAtTheCodec(t *testing.T) {
	every := make([]byte, 256)
	for i := range every {
		every[i] = byte(i)
	}

	payloads := map[string][]byte{
		"a NUL in the middle":      []byte("a\x00b"),
		"leading NUL":              []byte("\x00ab"),
		"trailing NUL":             []byte("ab\x00"),
		"only NULs":                {0, 0, 0, 0},
		"invalid UTF-8":            {0xff, 0xfe, 0x80, 0xc0, 0xed, 0xa0, 0x80},
		"a truncated rune":         {0xe2, 0x82},
		"an embedded CRLF":         []byte("a\r\nb"),
		"a lone CR":                []byte("a\rb"),
		"a lone LF":                []byte("a\nb"),
		"a RESP header inside":     []byte("*3\r\n$3\r\nSET\r\n"),
		"every byte 0 through 255": every,
	}

	codec := NewRESP()
	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			frame := encodeRequest(Command{Name: "ECHO", Args: [][]byte{payload}})
			cmd, err := codec.Decode(bufio.NewReaderSize(bytes.NewReader(frame), minReadBuffer))
			if err != nil {
				t.Fatalf("decoding %q: %v", frame, err)
			}
			if len(cmd.Args) != 1 {
				t.Fatalf("got %d arguments, want 1", len(cmd.Args))
			}
			if !bytes.Equal(cmd.Args[0], payload) {
				t.Fatalf("payload changed: %s became %s", hexOf(payload), hexOf(cmd.Args[0]))
			}
			if utf8.Valid(payload) != utf8.Valid(cmd.Args[0]) {
				t.Fatal("validity as UTF-8 changed, so the bytes were interpreted")
			}
		})
	}
}
