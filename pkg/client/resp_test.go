package client

import (
	"bufio"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func decodeString(t *testing.T, wire string) (Reply, error) {
	t.Helper()
	r := bufio.NewReaderSize(strings.NewReader(wire), readBufferSize)
	return decodeReply(r, 0)
}

func TestAppendCommandRendersAMultibulkRequest(t *testing.T) {
	got := string(appendCommand(nil, [][]byte{[]byte("SET"), []byte("k"), {0x00, 0xff}}))
	want := "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$2\r\n\x00\xff\r\n"
	if got != want {
		t.Fatalf("appendCommand rendered %q, want %q", got, want)
	}
}

func TestAppendCommandReusesItsBuffer(t *testing.T) {
	buf := make([]byte, 0, 64)
	buf = appendCommand(buf, [][]byte{[]byte("PING")})
	first := string(buf)
	buf = appendCommand(buf[:0], [][]byte{[]byte("PING")})
	if string(buf) != first {
		t.Fatalf("a reused buffer rendered %q, want %q", buf, first)
	}
}

// The decoded reply is compared whole rather than field by field: a decoder
// that put the right bytes in the wrong field, or left a stale one set, would
// pass a spot check and fail a caller.
type decodeCase struct {
	name string
	wire string
	want Reply
}

func runDecodeCases(t *testing.T, cases []decodeCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeString(t, tc.wire)
			if err != nil {
				t.Fatalf("decoding %q: %v", tc.wire, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("decoding %q gave %+v, want %+v", tc.wire, got, tc.want)
			}
		})
	}
}

func TestDecodeRESP2Types(t *testing.T) {
	runDecodeCases(t, []decodeCase{
		{"simple status", "+OK\r\n", Reply{Type: TypeStatus, Str: []byte("OK")}},
		{"empty status", "+\r\n", Reply{Type: TypeStatus, Str: []byte{}}},
		{
			"error reply",
			"-WRONGPASS invalid username-password pair\r\n",
			Reply{Type: TypeError, Kind: "WRONGPASS", Str: []byte("WRONGPASS invalid username-password pair")},
		},
		{"error without a message", "-ERR\r\n", Reply{Type: TypeError, Kind: "ERR", Str: []byte("ERR")}},
		{"integer reply", ":-7\r\n", Reply{Type: TypeInteger, Int: -7}},
		{"bulk string reply", "$5\r\nhello\r\n", Reply{Type: TypeBulk, Str: []byte("hello")}},
		{"empty bulk string", "$0\r\n\r\n", Reply{Type: TypeBulk, Str: []byte{}}},
		{"binary bulk string", "$3\r\n\x00\xff\n\r\n", Reply{Type: TypeBulk, Str: []byte("\x00\xff\n")}},
		{"null bulk string", "$-1\r\n", Reply{Type: TypeNil}},
		{"null array", "*-1\r\n", Reply{Type: TypeNil}},
		{
			"array reply",
			"*2\r\n$1\r\na\r\n:3\r\n",
			Reply{Type: TypeArray, Arr: []Reply{
				{Type: TypeBulk, Str: []byte("a")},
				{Type: TypeInteger, Int: 3},
			}},
		},
		{
			"nested array",
			"*1\r\n*1\r\n$1\r\nx\r\n",
			Reply{Type: TypeArray, Arr: []Reply{
				{Type: TypeArray, Arr: []Reply{{Type: TypeBulk, Str: []byte("x")}}},
			}},
		},
		{"empty array", "*0\r\n", Reply{Type: TypeArray, Arr: []Reply{}}},
	})
}

// The RESP3 types are decoded now so that the SDK needs no change when the
// server starts accepting HELLO 3 (ADR-0028, FEAT-0048). The ones with a RESP2
// equivalent normalize onto it, so a caller's type switch keeps working.
func TestDecodeRESP3Types(t *testing.T) {
	runDecodeCases(t, []decodeCase{
		{"null", "_\r\n", Reply{Type: TypeNil}},
		{"true", "#t\r\n", Reply{Type: TypeBool, Int: 1}},
		{"false", "#f\r\n", Reply{Type: TypeBool, Int: 0}},
		{"double reply", ",3.25\r\n", Reply{Type: TypeDouble, Float: 3.25, Str: []byte("3.25")}},
		{
			"big number reply",
			"(12345678901234567890\r\n",
			Reply{Type: TypeBigNumber, Str: []byte("12345678901234567890")},
		},
		{
			"blob error",
			"!20\r\nERR something is off\r\n",
			Reply{Type: TypeError, Kind: "ERR", Str: []byte("ERR something is off")},
		},
		{"verbatim string", "=15\r\ntxt:hello world\r\n", Reply{Type: TypeBulk, Str: []byte("hello world")}},
		{
			"set reads as an array",
			"~2\r\n:1\r\n:2\r\n",
			Reply{Type: TypeArray, Arr: []Reply{{Type: TypeInteger, Int: 1}, {Type: TypeInteger, Int: 2}}},
		},
		{
			"map flattens to pairs",
			"%1\r\n$4\r\nkeys\r\n:9\r\n",
			Reply{Type: TypeMap, Arr: []Reply{
				{Type: TypeBulk, Str: []byte("keys")},
				{Type: TypeInteger, Int: 9},
			}},
		},
		{"push message", ">1\r\n$1\r\nx\r\n", Reply{Type: TypePush, Arr: []Reply{{Type: TypeBulk, Str: []byte("x")}}}},
	})
}

// A malformed reply must be an ErrProtocol and nothing else: the pool keys the
// discard decision off that category, so anything misclassified here is a
// connection returned in an unknown state.
func TestDecodeRejectsMalformedReplies(t *testing.T) {
	tests := map[string]string{
		"unknown type byte":           "@nonsense\r\n",
		"bare LF":                     "+OK\n",
		"non-numeric bulk length":     "$abc\r\n",
		"non-numeric integer":         ":not-a-number\r\n",
		"non-numeric array length":    "*x\r\n",
		"non-numeric map length":      "%x\r\n",
		"bulk not terminated":         "$2\r\nabxx",
		"null carrying data":          "_junk\r\n",
		"boolean that is not t or f":  "#maybe\r\n",
		"double that is not a number": ",abc\r\n",
		"oversized bulk length":       "$536870913\r\n",
		"oversized array length":      "*16777217\r\n",
		"oversized map length":        "%16777217\r\n",
	}

	for name, wire := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := decodeString(t, wire)
			if err == nil {
				t.Fatalf("decoding %q succeeded, want a protocol error", wire)
			}
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("decoding %q gave %v, want ErrProtocol", wire, err)
			}
		})
	}
}

func TestDecodeRejectsOverlyNestedReplies(t *testing.T) {
	wire := strings.Repeat("*1\r\n", maxReplyDepth+2) + ":1\r\n"
	_, err := decodeString(t, wire)
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("a %d-deep reply gave %v, want ErrProtocol", maxReplyDepth+2, err)
	}
}

func TestDecodeRejectsAnOverlongLine(t *testing.T) {
	_, err := decodeString(t, "+"+strings.Repeat("x", readBufferSize*2)+"\r\n")
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("an overlong line gave %v, want ErrProtocol", err)
	}
}

// A line handed straight out of bufio's buffer is overwritten by the next read.
// This is the test that catches forgetting to copy one.
func TestDecodedLinesSurviveTheNextRead(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader("+first\r\n+second-and-much-longer\r\n"), readBufferSize)

	first, err := decodeReply(r, 0)
	if err != nil {
		t.Fatalf("decoding the first reply: %v", err)
	}
	if _, err := decodeReply(r, 0); err != nil {
		t.Fatalf("decoding the second reply: %v", err)
	}
	if string(first.Str) != "first" {
		t.Fatalf("the first reply became %q after a second read", first.Str)
	}
}

func TestDecodeReportsAShortStream(t *testing.T) {
	_, err := decodeString(t, "")
	if err == nil {
		t.Fatal("decoding an empty stream succeeded")
	}
	if errors.Is(err, ErrProtocol) {
		t.Fatalf("a truncated stream was reported as a protocol error: %v", err)
	}
}
