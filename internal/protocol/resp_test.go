package protocol

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decoderFor(input string) *bufio.Reader {
	return bufio.NewReader(strings.NewReader(input))
}

func TestDecodeMultiBulk(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantName string
		wantArgs []string
	}{
		{
			name:     "ping",
			input:    "*1\r\n$4\r\nPING\r\n",
			wantName: "PING",
		},
		{
			name:     "lowercase is upper-cased",
			input:    "*1\r\n$4\r\nping\r\n",
			wantName: "PING",
		},
		{
			name:     "command with arguments",
			input:    "*3\r\n$3\r\nSET\r\n$2\r\nk1\r\n$5\r\nhello\r\n",
			wantName: "SET",
			wantArgs: []string{"k1", "hello"},
		},
		{
			name:     "empty argument",
			input:    "*2\r\n$4\r\nECHO\r\n$0\r\n\r\n",
			wantName: "ECHO",
			wantArgs: []string{""},
		},
		{
			name:     "argument containing CRLF",
			input:    "*2\r\n$4\r\nECHO\r\n$4\r\na\r\nb\r\n",
			wantName: "ECHO",
			wantArgs: []string{"a\r\nb"},
		},
		{
			name:     "empty request is skipped",
			input:    "*0\r\n*1\r\n$4\r\nPING\r\n",
			wantName: "PING",
		},
	}

	codec := NewRESP()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := codec.Decode(decoderFor(tt.input))
			require.NoError(t, err)
			assert.Equal(t, tt.wantName, cmd.Name)

			args := make([]string, 0, len(cmd.Args))
			for _, a := range cmd.Args {
				args = append(args, string(a))
			}
			assert.Equal(t, tt.wantArgs, nilIfEmpty(args))
		})
	}
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

func TestDecodeInline(t *testing.T) {
	codec := NewRESP()

	cmd, err := codec.Decode(decoderFor("PING\r\n"))
	require.NoError(t, err)
	assert.Equal(t, "PING", cmd.Name)
	assert.Empty(t, cmd.Args)

	cmd, err = codec.Decode(decoderFor("echo  hello world\r\n"))
	require.NoError(t, err)
	assert.Equal(t, "ECHO", cmd.Name)
	require.Len(t, cmd.Args, 2)
	assert.Equal(t, "hello", string(cmd.Args[0]))
	assert.Equal(t, "world", string(cmd.Args[1]))
}

// TestInlineRequestsTakeABareLF is ISSUE-0019. Redis's inline parser splits on
// LF and strips an optional CR in front of it, which is what `redis-cli --pipe`
// with a plain-text file, a shell heredoc and `printf 'PING\n' | nc` all send.
// Requiring CRLF broke every one of them.
func TestInlineRequestsTakeABareLF(t *testing.T) {
	codec := NewRESP()

	accepted := map[string]string{
		"bare LF":                      "PING\n",
		"CRLF still works":             "PING\r\n",
		"arguments after a bare LF":    "echo hello\n",
		"a blank LF line is skipped":   "\n\nPING\n",
		"a blank CRLF line is skipped": "\r\n\r\nPING\r\n",
	}
	for name, input := range accepted {
		t.Run(name, func(t *testing.T) {
			cmd, err := codec.Decode(decoderFor(input))
			require.NoError(t, err)
			assert.Contains(t, []string{"PING", "ECHO"}, cmd.Name)
		})
	}

	t.Run("a whole pipeline of LF-terminated lines", func(t *testing.T) {
		r := decoderFor("SET k v\nGET k\nPING\n")
		for _, want := range []string{"SET", "GET", "PING"} {
			cmd, err := codec.Decode(r)
			require.NoError(t, err)
			assert.Equal(t, want, cmd.Name)
		}
	})

	// The bound ISSUE-0016 set has to survive the terminator change: a client
	// streaming bytes with neither CR nor LF in them is still cut off at the
	// limit, not after it.
	t.Run("the inline limit still applies while the line accumulates", func(t *testing.T) {
		source := &floodReader{fill: 'x', cap: 8 << 20}
		_, err := codec.Decode(bufio.NewReaderSize(source, connBufferSize))
		require.ErrorContains(t, err, "too big inline request")
		assert.LessOrEqual(t, source.read, codec.Limits().MaxInlineLength+2*connBufferSize,
			"the parser read %d bytes of a line it may not accept", source.read)
	})
}

func TestDecodeSequentialCommands(t *testing.T) {
	codec := NewRESP()
	r := decoderFor("*1\r\n$4\r\nPING\r\n*1\r\n$4\r\nQUIT\r\n")

	first, err := codec.Decode(r)
	require.NoError(t, err)
	assert.Equal(t, "PING", first.Name)

	second, err := codec.Decode(r)
	require.NoError(t, err)
	assert.Equal(t, "QUIT", second.Name)

	_, err = codec.Decode(r)
	assert.ErrorIs(t, err, io.EOF)
}

func TestDecodeMalformed(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantMsg string
	}{
		{
			name:    "non-numeric multibulk length",
			input:   "*abc\r\n",
			wantMsg: "invalid multibulk length",
		},
		{
			name:    "multibulk length too large",
			input:   "*99999999\r\n",
			wantMsg: "invalid multibulk length",
		},
		{
			name:    "element is not a bulk string",
			input:   "*1\r\n+PING\r\n",
			wantMsg: "expected '$'",
		},
		{
			name:    "non-numeric bulk length",
			input:   "*1\r\n$xx\r\nPING\r\n",
			wantMsg: "invalid bulk length",
		},
		{
			name:    "negative bulk length",
			input:   "*1\r\n$-1\r\n",
			wantMsg: "invalid bulk length",
		},
		{
			name:    "bulk payload not CRLF terminated",
			input:   "*1\r\n$4\r\nPINGXX",
			wantMsg: "expected CRLF after bulk string",
		},
		{
			// Inline requests take a bare LF (ISSUE-0019); framing headers do
			// not, and Redis is strict about them in the same way.
			name:    "multibulk header without a carriage return",
			input:   "*1\n$4\r\nPING\r\n",
			wantMsg: "expected CRLF line terminator",
		},
		{
			name:    "bulk header without a carriage return",
			input:   "*1\r\n$4\nPING\r\n",
			wantMsg: "expected CRLF line terminator",
		},
	}

	codec := NewRESP()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := codec.Decode(decoderFor(tt.input))
			require.Error(t, err)

			var protoErr *ProtocolError
			require.True(t, errors.As(err, &protoErr), "expected a ProtocolError, got %T", err)
			assert.Contains(t, protoErr.Error(), tt.wantMsg)
			assert.Equal(t, "ERR", protoErr.Reply().Kind)
		})
	}
}

// TestDecodeDistinguishesACleanCloseFromATruncatedFrame. A connection that ends
// between commands is a client that went away; one that ends part-way through a
// frame is a request that was cut in half. Reporting both as io.EOF would let a
// truncated frame read as a clean close, which is the "valid short frame" the
// feature forbids — and the server logs the two differently because only one of
// them is a problem.
func TestDecodeDistinguishesACleanCloseFromATruncatedFrame(t *testing.T) {
	codec := NewRESP()

	t.Run("a connection that ends between commands is EOF", func(t *testing.T) {
		r := decoderFor("*1\r\n$4\r\nPING\r\n")
		_, err := codec.Decode(r)
		require.NoError(t, err)

		_, err = codec.Decode(r)
		assert.ErrorIs(t, err, io.EOF)
	})

	truncated := map[string]string{
		"multibulk header promises elements that never arrive": "*2\r\n$4\r\nPING\r\n",
		"bulk header with no payload":                          "*1\r\n$4\r\n",
		"bulk payload cut short":                               "*1\r\n$4\r\nPI",
		"line with no terminator":                              "PING",
	}
	for name, input := range truncated {
		t.Run(name, func(t *testing.T) {
			_, err := codec.Decode(decoderFor(input))
			assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
			assert.NotErrorIs(t, err, io.EOF, "a half-sent frame must not read as a clean close")
		})
	}
}

func TestEncode(t *testing.T) {
	tests := []struct {
		name  string
		reply Reply
		want  string
	}{
		{name: "simple string", reply: SimpleString("PONG"), want: "+PONG\r\n"},
		{name: "error", reply: Errorf("unknown command '%s'", "FOO"), want: "-ERR unknown command 'FOO'\r\n"},
		{name: "error without kind", reply: Error{Message: "bare"}, want: "-bare\r\n"},
		{name: "bulk string", reply: BulkString("hello"), want: "$5\r\nhello\r\n"},
		{name: "empty bulk string", reply: BulkString(""), want: "$0\r\n\r\n"},
		{name: "integer", reply: Integer(-3), want: ":-3\r\n"},
		{name: "nil", reply: Nil, want: "$-1\r\n"},
		{
			name:  "array",
			reply: Array{SimpleString("OK"), BulkString("v"), Nil},
			want:  "*3\r\n+OK\r\n$1\r\nv\r\n$-1\r\n",
		},
		{name: "empty array", reply: Array{}, want: "*0\r\n"},
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

func TestEncodeUnsupportedReply(t *testing.T) {
	var buf bytes.Buffer
	err := NewRESP().Encode(&buf, unsupportedReply{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported reply type")
}

type unsupportedReply struct{}

func (unsupportedReply) isReply() {}

func TestRoundTrip(t *testing.T) {
	codec := NewRESP()

	// A command encoded as a RESP array of bulk strings decodes back unchanged
	request := Array{BulkString("SET"), BulkString("k1"), BulkString("hello")}
	var buf bytes.Buffer
	require.NoError(t, codec.Encode(&buf, request))

	cmd, err := codec.Decode(bufio.NewReader(&buf))
	require.NoError(t, err)
	assert.Equal(t, "SET", cmd.Name)
	require.Len(t, cmd.Args, 2)
	assert.Equal(t, "k1", string(cmd.Args[0]))
	assert.Equal(t, "hello", string(cmd.Args[1]))
}

func TestCodecInterfaceSatisfied(t *testing.T) {
	var codec Codec = NewRESP()
	assert.NotNil(t, codec)
}

// TestDecodeReassemblesASplitFrame. A command does not arrive in one piece: TCP
// segments it wherever it likes, and a parser that read what was available and
// decided would mangle or drop the half it had. The reader below is the worst
// case — one byte per read — and the decode must be identical to the same bytes
// arriving at once.
func TestDecodeReassemblesASplitFrame(t *testing.T) {
	const request = "*3\r\n$3\r\nSET\r\n$2\r\nk1\r\n$11\r\nhello world\r\n"

	// A buffer smaller than the request, so the line accumulator has to span
	// refills as well as reads.
	r := bufio.NewReaderSize(iotest.OneByteReader(strings.NewReader(request)), 16)

	cmd, err := NewRESP().Decode(r)
	require.NoError(t, err)
	assert.Equal(t, "SET", cmd.Name)
	require.Len(t, cmd.Args, 2)
	assert.Equal(t, "k1", string(cmd.Args[0]))
	assert.Equal(t, "hello world", string(cmd.Args[1]))
}

// TestDecodeReassemblesASplitInlineLine. The same for an inline request longer
// than the read buffer, which is the path where the line spans several
// ReadSlice calls and the size check runs between them.
func TestDecodeReassemblesASplitInlineLine(t *testing.T) {
	value := strings.Repeat("a", 200)

	r := bufio.NewReaderSize(iotest.OneByteReader(strings.NewReader("ECHO "+value+"\r\n")), 16)

	cmd, err := NewRESP().Decode(r)
	require.NoError(t, err)
	assert.Equal(t, "ECHO", cmd.Name)
	require.Len(t, cmd.Args, 1)
	assert.Equal(t, value, string(cmd.Args[0]))
}
