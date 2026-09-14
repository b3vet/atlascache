package protocol

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The round-trip property: a reply that is encoded and read back by a client
// parses as the RESP2 form of what the handler built.
//
// For the types RESP2 has a frame for, that form is the reply itself, so this
// really is decode(encode(x)) == x. For the five it does not, the form is the
// documented downgrade — a map is an array, a boolean is 1 or 0 — and asserting
// against that is what pins the downgrade rather than merely exercising it. The
// parser below is deliberately a second implementation: a property checked with
// the encoder's own code would pass no matter what either of them did.

// TestRoundTripProperty generates reply trees and checks the property over each
// of them.
func TestRoundTripProperty(t *testing.T) {
	seed := time.Now().UnixNano()
	rng := rand.New(rand.NewSource(seed))
	t.Logf("seed %d — rerun a failure with it pinned", seed)

	codec := NewRESP()
	for i := range 2000 {
		reply := generateReply(rng, 3)

		var buf bytes.Buffer
		require.NoErrorf(t, codec.Encode(&buf, reply), "case %d: %#v", i, reply)
		wire := buf.String()

		r := bufio.NewReader(&buf)
		got, err := parseReply(r, 0)
		require.NoErrorf(t, err, "case %d: parsing %q", i, wire)
		assert.Equalf(t, resp2Form(reply), got, "case %d: reply %#v encoded as %q", i, reply, wire)

		_, err = r.ReadByte()
		assert.ErrorIsf(t, err, io.EOF, "case %d: %q left bytes on the wire after one reply", i, wire)
	}
}

// TestRoundTripEveryReplyType runs the same property over one value of every
// type in the hierarchy, so a type that is never generated is still covered and
// the list itself is the enumeration FEAT-0048 is held to.
func TestRoundTripEveryReplyType(t *testing.T) {
	tests := map[string]Reply{
		"simple string": SimpleString("OK"),
		"error":         Error{Kind: "ERR", Message: "boom"},
		"bulk error":    BulkError{Kind: "ERR", Message: "boom"},
		"bulk string":   BulkString("hello"),
		"verbatim":      Verbatim{Format: VerbatimText, Text: "hello"},
		"big number":    BigNumber("129381203984120310293858109238587"),
		"integer":       Integer(42),
		"boolean":       Boolean(true),
		"double":        Double(1.5),
		"null":          Nil,
		"array":         Array{Integer(1), BulkString("x")},
		"set":           Set{Integer(1)},
		"push":          Push{BulkString("message")},
		"map":           Map{{Key: BulkString("proto"), Value: Integer(2)}},
		"attribute":     Attribute{Attrs: []KV{{Key: BulkString("k"), Value: Integer(1)}}, Value: Integer(7)},
	}

	codec := NewRESP()
	for name, reply := range tests {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, codec.Encode(&buf, reply))

			got, err := parseReply(bufio.NewReader(&buf), 0)
			require.NoError(t, err)
			assert.Equal(t, resp2Form(reply), got)
		})
	}
}

// resp2Form is what a RESP2 client can see of a reply: the reply itself for the
// types RESP2 has a frame for, and the documented downgrade for the rest.
func resp2Form(reply Reply) Reply {
	switch v := reply.(type) {
	case SimpleString:
		return SimpleString(foldLines(string(v)))
	case Error:
		return Error{Kind: v.Kind, Message: foldLines(v.Message)}
	case BulkError:
		return Error{Kind: v.Kind, Message: foldLines(v.Message)}
	case BulkString:
		return bulk(string(v))
	case Verbatim:
		return bulk(v.Text)
	case BigNumber:
		return bulk(string(v))
	case Double:
		return bulk(formatDouble(float64(v)))
	case Boolean:
		if v {
			return Integer(1)
		}
		return Integer(0)
	default:
		return resp2AggregateForm(reply)
	}
}

// resp2AggregateForm is the half of resp2Form that deals with the types RESP2
// renders as an array, or as nothing at all.
func resp2AggregateForm(reply Reply) Reply {
	switch v := reply.(type) {
	case Array:
		return flatten(v)
	case Set:
		return flatten(v)
	case Push:
		return flatten(v)
	case Map:
		pairs := make([]Reply, 0, 2*len(v))
		for _, kv := range v {
			pairs = append(pairs, kv.Key, kv.Value)
		}
		return flatten(pairs)
	case Attribute:
		if v.Value == nil {
			return Nil
		}
		return resp2Form(v.Value)
	default:
		return reply
	}
}

func flatten(items []Reply) Array {
	out := make(Array, 0, len(items))
	for _, item := range items {
		out = append(out, resp2Form(item))
	}
	return out
}

// bulk normalizes a bulk string so that a nil payload and an empty one compare
// equal, which they are on the wire.
func bulk(s string) BulkString {
	return BulkString(append([]byte{}, s...))
}

func foldLines(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// generateReply builds a random reply, nesting no deeper than depth.
func generateReply(rng *rand.Rand, depth int) Reply {
	scalars := []func() Reply{
		func() Reply { return SimpleString(generateText(rng)) },
		func() Reply { return Error{Kind: generateKind(rng), Message: generateText(rng)} },
		func() Reply { return BulkError{Kind: generateKind(rng), Message: generateText(rng)} },
		func() Reply { return BulkString(generateBytes(rng)) },
		func() Reply { return Verbatim{Format: VerbatimText, Text: generateText(rng)} },
		func() Reply { return BigNumber(strconv.FormatUint(rng.Uint64(), 10)) },
		func() Reply { return Integer(rng.Int63() - rng.Int63()) },
		func() Reply { return Boolean(rng.Intn(2) == 0) },
		func() Reply { return Double(generateFloat(rng)) },
		func() Reply { return Nil },
	}

	if depth <= 0 || rng.Intn(3) > 0 {
		return scalars[rng.Intn(len(scalars))]()
	}

	n := rng.Intn(4)
	items := make([]Reply, 0, n)
	for range n {
		items = append(items, generateReply(rng, depth-1))
	}

	switch rng.Intn(5) {
	case 0:
		return Array(items)
	case 1:
		return Set(items)
	case 2:
		return Push(items)
	case 3:
		pairs := make(Map, 0, len(items))
		for _, item := range items {
			pairs = append(pairs, KV{Key: BulkString(generateText(rng)), Value: item})
		}
		return pairs
	default:
		return Attribute{
			Attrs: []KV{{Key: BulkString("ttl"), Value: Integer(rng.Int63n(1000))}},
			Value: generateReply(rng, depth-1),
		}
	}
}

// generateText includes the bytes a single-line reply has to fold, since the
// property covers that downgrade too.
func generateText(rng *rand.Rand) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789 '\"-\r\n"

	n := rng.Intn(24)
	var b strings.Builder
	for range n {
		b.WriteByte(alphabet[rng.Intn(len(alphabet))])
	}
	return b.String()
}

// generateKind is the machine-readable error prefix, which never carries a
// space: a client reads everything up to the first one as the kind.
func generateKind(rng *rand.Rand) string {
	kinds := []string{"ERR", "WRONGTYPE", "NOAUTH", "NOPROTO", "OOM"}
	return kinds[rng.Intn(len(kinds))]
}

func generateBytes(rng *rand.Rand) []byte {
	n := rng.Intn(32)
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.Intn(256))
	}
	return b
}

func generateFloat(rng *rand.Rand) float64 {
	switch rng.Intn(8) {
	case 0:
		return math.Inf(1)
	case 1:
		return math.Inf(-1)
	case 2:
		return 0
	default:
		return (rng.Float64() - 0.5) * math.Pow(10, float64(rng.Intn(12)-6))
	}
}

// parseReply reads one RESP2 reply the way a client library does. It is a
// second implementation on purpose, and it understands only the five frames
// RESP2 defines — a RESP3 frame reaching it is a bug in the encoder.
func parseReply(r *bufio.Reader, depth int) (Reply, error) {
	if depth > 16 {
		return nil, fmt.Errorf("reply nests deeper than 16 levels")
	}

	prefix, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	line, err := parseLine(r)
	if err != nil {
		return nil, err
	}

	switch prefix {
	case '+':
		return SimpleString(line), nil
	case '-':
		kind, message, _ := strings.Cut(line, " ")
		return Error{Kind: kind, Message: message}, nil
	case ':':
		n, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("integer %q: %w", line, err)
		}
		return Integer(n), nil
	case '$':
		return parseBulk(r, line)
	case '*':
		return parseArray(r, line, depth)
	default:
		return nil, fmt.Errorf("unknown reply type %q", string(prefix))
	}
}

func parseBulk(r *bufio.Reader, line string) (Reply, error) {
	n, err := strconv.Atoi(line)
	if err != nil {
		return nil, fmt.Errorf("bulk length %q: %w", line, err)
	}
	if n < 0 {
		return Nil, nil
	}

	payload := make([]byte, n+2)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	if string(payload[n:]) != "\r\n" {
		return nil, fmt.Errorf("bulk string of %d bytes is not CRLF terminated", n)
	}
	return BulkString(payload[:n]), nil
}

func parseArray(r *bufio.Reader, line string, depth int) (Reply, error) {
	n, err := strconv.Atoi(line)
	if err != nil {
		return nil, fmt.Errorf("array length %q: %w", line, err)
	}
	if n < 0 {
		return Nil, nil
	}

	items := make(Array, 0, n)
	for range n {
		item, err := parseReply(r, depth+1)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func parseLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(line, "\r\n") {
		return "", fmt.Errorf("line %q is not CRLF terminated", line)
	}
	return line[:len(line)-2], nil
}
