package protocol

import (
	"bytes"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEncodeEveryReplyType pins the RESP2 rendering of every logical reply
// type, including the five that only RESP3 renders differently. They are the
// reason FEAT-0048 can add an encoder and no reply types: a type with no RESP2
// rendering would have had to wait for P6, and a handler would have had to know
// which kind it was allowed to return.
func TestEncodeEveryReplyType(t *testing.T) {
	tests := []struct {
		name  string
		reply Reply
		want  string
	}{
		{name: "simple string", reply: SimpleString("OK"), want: "+OK\r\n"},
		{name: "error", reply: Error{Kind: "NOPROTO", Message: "unsupported protocol version"},
			want: "-NOPROTO unsupported protocol version\r\n"},
		{name: "bulk error folds to a simple error", reply: BulkError{Kind: "ERR", Message: "line\nbreak"},
			want: "-ERR line break\r\n"},
		{name: "bulk string", reply: BulkString("hello"), want: "$5\r\nhello\r\n"},
		{name: "bulk string is binary safe", reply: BulkString("a\r\nb"), want: "$4\r\na\r\nb\r\n"},
		{name: "verbatim string drops its format hint",
			reply: Verbatim{Format: VerbatimText, Text: "hello world"}, want: "$11\r\nhello world\r\n"},
		{name: "big number is a bulk string", reply: BigNumber("3492890328409238509324850943850943825024385"),
			want: "$43\r\n3492890328409238509324850943850943825024385\r\n"},
		{name: "integer", reply: Integer(-3), want: ":-3\r\n"},
		{name: "true is 1", reply: Boolean(true), want: ":1\r\n"},
		{name: "false is 0", reply: Boolean(false), want: ":0\r\n"},
		{name: "double is a bulk string", reply: Double(3.14), want: "$4\r\n3.14\r\n"},
		{name: "null", reply: Nil, want: "$-1\r\n"},
		{name: "array", reply: Array{SimpleString("OK"), Nil}, want: "*2\r\n+OK\r\n$-1\r\n"},
		{name: "set is an array", reply: Set{BulkString("a"), BulkString("b")},
			want: "*2\r\n$1\r\na\r\n$1\r\nb\r\n"},
		{name: "push is an array", reply: Push{BulkString("message"), BulkString("ch"), BulkString("hi")},
			want: "*3\r\n$7\r\nmessage\r\n$2\r\nch\r\n$2\r\nhi\r\n"},
		{
			name: "map flattens to an array of 2n",
			reply: Map{
				{Key: BulkString("proto"), Value: Integer(2)},
				{Key: BulkString("server"), Value: BulkString("atlascache")},
			},
			want: "*4\r\n$5\r\nproto\r\n:2\r\n$6\r\nserver\r\n$10\r\natlascache\r\n",
		},
		{
			name:  "attribute sends its value and drops the metadata",
			reply: Attribute{Attrs: []KV{{Key: BulkString("ttl"), Value: Integer(3)}}, Value: BulkString("v")},
			want:  "$1\r\nv\r\n",
		},
		{name: "empty aggregates", reply: Array{Map{}, Set{}, Push{}}, want: "*3\r\n*0\r\n*0\r\n*0\r\n"},
	}

	codec := NewRESP()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, codec.Encode(&buf, tt.reply))
			assert.Equal(t, tt.want, buf.String())
		})
	}
}

// TestEncodeFoldsNewlinesOutOfSingleLineReplies. A status or error line is
// terminated by CRLF, so text carrying one would end the reply early and frame
// whatever followed as a second one. An engine error quoted into a message is
// the realistic source, and the client on the other end would desynchronize for
// the rest of the connection.
func TestEncodeFoldsNewlinesOutOfSingleLineReplies(t *testing.T) {
	codec := NewRESP()

	var buf bytes.Buffer
	require.NoError(t, codec.Encode(&buf, SimpleString("two\r\nlines")))
	assert.Equal(t, "+two  lines\r\n", buf.String())

	buf.Reset()
	require.NoError(t, codec.Encode(&buf, Errorf("wrote %q", "a\nb")))
	assert.Equal(t, "-ERR wrote \"a\\nb\"\r\n", buf.String(),
		"a quoted newline is already escaped and must not be folded twice")

	buf.Reset()
	require.NoError(t, codec.Encode(&buf, Error{Kind: "ERR", Message: "raw\nnewline"}))
	assert.Equal(t, "-ERR raw newline\r\n", buf.String())
}

// TestEncodeDouble covers the values a naive formatter gets wrong: the
// infinities, which Redis spells, and a float whose shortest form is the one a
// client parses back to the same number.
func TestEncodeDouble(t *testing.T) {
	tests := []struct {
		name  string
		value float64
		want  string
	}{
		{name: "integral", value: 3, want: "$1\r\n3\r\n"},
		{name: "fractional", value: 3.141592653589793, want: "$17\r\n3.141592653589793\r\n"},
		{name: "negative zero", value: math.Copysign(0, -1), want: "$2\r\n-0\r\n"},
		{name: "positive infinity", value: math.Inf(1), want: "$3\r\ninf\r\n"},
		{name: "negative infinity", value: math.Inf(-1), want: "$4\r\n-inf\r\n"},
		{name: "not a number", value: math.NaN(), want: "$3\r\nnan\r\n"},
	}

	codec := NewRESP()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, codec.Encode(&buf, Double(tt.value)))
			assert.Equal(t, tt.want, buf.String())
		})
	}
}

// TestEncodeReportsAnUnsupportedReplyInsideAnAggregate. A bad reply nested in an
// array must fail the encode rather than emitting a header with fewer elements
// under it than it promised, which would desynchronize the connection.
func TestEncodeReportsAnUnsupportedReplyInsideAnAggregate(t *testing.T) {
	nested := []Reply{
		Array{SimpleString("ok"), unsupportedReply{}},
		Set{unsupportedReply{}},
		Push{unsupportedReply{}},
		Map{{Key: unsupportedReply{}, Value: Integer(1)}},
		Map{{Key: BulkString("k"), Value: unsupportedReply{}}},
		Attribute{Value: unsupportedReply{}},
	}

	codec := NewRESP()
	for _, reply := range nested {
		var buf bytes.Buffer
		err := codec.Encode(&buf, reply)
		require.Error(t, err, "%T must not encode", reply)
		assert.Contains(t, err.Error(), "unsupported reply type")
		assert.Empty(t, buf.String(), "nothing may reach the connection when the reply cannot be rendered")
	}
}

// TestEncodeAttributeWithNoValue. An Attribute is a wrapper, and one built
// without a value would otherwise encode as nothing at all — no reply for a
// command that was answered, which hangs the client until it times out.
func TestEncodeAttributeWithNoValue(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, NewRESP().Encode(&buf, Attribute{}))
	assert.Equal(t, "$-1\r\n", buf.String())
}

// TestErrorText covers what a client reads as the error, which is the kind and
// the message with one space between them, or the message alone when the reply
// carries no kind.
func TestErrorText(t *testing.T) {
	assert.Equal(t, "ERR boom", Error{Kind: "ERR", Message: "boom"}.Error())
	assert.Equal(t, "boom", Error{Message: "boom"}.Error())
	assert.Equal(t, "WRONGTYPE boom", BulkError{Kind: "WRONGTYPE", Message: "boom"}.Error())
	assert.Equal(t, "boom", BulkError{Message: "boom"}.Error())
}
