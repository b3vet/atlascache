package client

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/b3vet/atlascache/test/e2e/runner"
)

// Wire-format limits. RESP itself caps a bulk string at 512MB; this client is a
// test tool, so it refuses anything large enough to be a bug rather than a
// value, and refuses it before allocating.
const (
	maxLineLength = 64 << 10
	maxBulkLength = 64 << 20
	maxDepth      = 32
)

// Type bytes, from the RESP specification. The first five are RESP2; the rest
// are RESP3 additions, decoded so that a server negotiating HELLO 3 does not
// look like a malformed one.
const (
	typeSimpleString = '+'
	typeError        = '-'
	typeInteger      = ':'
	typeBulkString   = '$'
	typeArray        = '*'

	typeNull     = '_'
	typeDouble   = ','
	typeBoolean  = '#'
	typeBlobErr  = '!'
	typeVerbatim = '='
	typeBigNum   = '('
	typeMap      = '%'
	typeSet      = '~'
	typePush     = '>'
)

const crlf = "\r\n"

// writeArray encodes a command as a RESP array of bulk strings, which is the
// only request form a server is required to accept.
func writeArray(w *bufio.Writer, args []string) error {
	if _, err := fmt.Fprintf(w, "*%d%s", len(args), crlf); err != nil {
		return err
	}
	for _, arg := range args {
		if _, err := fmt.Fprintf(w, "$%d%s%s%s", len(arg), crlf, arg, crlf); err != nil {
			return err
		}
	}
	return nil
}

// readReply decodes one reply. depth bounds nesting so a hostile or broken
// server cannot drive the decoder into unbounded recursion.
func readReply(r *bufio.Reader, depth int) (runner.Reply, error) {
	if depth > maxDepth {
		return runner.Reply{}, &ProtocolError{Message: fmt.Sprintf("reply nests deeper than %d levels", maxDepth)}
	}

	prefix, err := r.ReadByte()
	if err != nil {
		return runner.Reply{}, &ConnError{Op: "read reply type", Err: err}
	}

	line, err := readLine(r)
	if err != nil {
		return runner.Reply{}, err
	}

	switch prefix {
	case typeBulkString:
		return readBulk(r, line, false)

	case typeVerbatim:
		return readBulk(r, line, true)

	case typeBlobErr:
		reply, err := readBulk(r, line, false)
		if err != nil {
			return runner.Reply{}, err
		}
		return runner.ErrorReply(reply.Text), nil

	case typeArray, typeSet, typePush:
		return readAggregate(r, line, depth)

	case typeMap:
		return readMap(r, line, depth)

	default:
		return readScalar(prefix, line)
	}
}

// readScalar decodes the reply types that carry their whole value on the type
// line, so nothing further is read from the connection.
func readScalar(prefix byte, line string) (runner.Reply, error) {
	switch prefix {
	case typeSimpleString:
		return runner.StatusReply(line), nil

	case typeError:
		return runner.ErrorReply(line), nil

	case typeInteger:
		n, err := parseInt(line, "integer")
		if err != nil {
			return runner.Reply{}, err
		}
		return runner.IntegerReply(n), nil

	case typeNull:
		if line != "" {
			return runner.Reply{}, &ProtocolError{Message: fmt.Sprintf("null carries data: %q", line)}
		}
		return runner.NilReply(), nil

	case typeBoolean:
		// RESP3 booleans are the RESP2 integers 1 and 0, which is how every
		// assertion in the suite is written.
		switch line {
		case "t":
			return runner.IntegerReply(1), nil
		case "f":
			return runner.IntegerReply(0), nil
		default:
			return runner.Reply{}, &ProtocolError{Message: fmt.Sprintf("boolean is %q, want t or f", line)}
		}

	case typeDouble, typeBigNum:
		// Kept as text: the runner compares replies as strings, and rounding a
		// double through float64 would change what an assertion sees.
		return runner.BulkReply(line), nil

	default:
		return runner.Reply{}, &ProtocolError{
			Message: fmt.Sprintf("unknown reply type %q (line %q)", string(prefix), line),
		}
	}
}

// readBulk reads a length-prefixed string. A negative length is the RESP2 null
// bulk string. verbatim strips the mandatory three-byte format prefix, so
// `=15\r\ntxt:hello world\r\n` reads as "hello world".
func readBulk(r *bufio.Reader, line string, verbatim bool) (runner.Reply, error) {
	n, err := parseInt(line, "bulk length")
	if err != nil {
		return runner.Reply{}, err
	}
	if n < 0 {
		return runner.NilReply(), nil
	}
	if n > maxBulkLength {
		return runner.Reply{}, &ProtocolError{Message: fmt.Sprintf("bulk string of %d bytes exceeds the %d byte limit", n, maxBulkLength)}
	}

	buf := make([]byte, n+2) // the payload plus its CRLF
	if _, err := io.ReadFull(r, buf); err != nil {
		return runner.Reply{}, &ConnError{Op: "read bulk string", Err: err}
	}
	if string(buf[n:]) != crlf {
		return runner.Reply{}, &ProtocolError{Message: "bulk string is not terminated by CRLF"}
	}

	text := string(buf[:n])
	if verbatim {
		if len(text) < 4 || text[3] != ':' {
			return runner.Reply{}, &ProtocolError{Message: fmt.Sprintf("verbatim string %q has no format prefix", text)}
		}
		text = text[4:]
	}
	return runner.BulkReply(text), nil
}

// readAggregate reads an array, set or push reply. A negative length is the
// RESP2 null array, which the runner models as NIL.
func readAggregate(r *bufio.Reader, line string, depth int) (runner.Reply, error) {
	n, err := parseInt(line, "array length")
	if err != nil {
		return runner.Reply{}, err
	}
	if n < 0 {
		return runner.NilReply(), nil
	}
	if n > maxBulkLength {
		return runner.Reply{}, &ProtocolError{Message: fmt.Sprintf("array of %d elements is implausible", n)}
	}

	items := make([]runner.Reply, 0, n)
	for i := int64(0); i < n; i++ {
		item, err := readReply(r, depth+1)
		if err != nil {
			return runner.Reply{}, err
		}
		items = append(items, item)
	}
	return runner.ArrayReply(items...), nil
}

// readMap reads a RESP3 map, whose length counts pairs rather than elements.
func readMap(r *bufio.Reader, line string, depth int) (runner.Reply, error) {
	n, err := parseInt(line, "map length")
	if err != nil {
		return runner.Reply{}, err
	}
	if n < 0 {
		return runner.NilReply(), nil
	}
	if n > maxBulkLength {
		return runner.Reply{}, &ProtocolError{Message: fmt.Sprintf("map of %d pairs is implausible", n)}
	}

	pairs := make(map[string]runner.Reply, n)
	for i := int64(0); i < n; i++ {
		key, err := readReply(r, depth+1)
		if err != nil {
			return runner.Reply{}, err
		}
		value, err := readReply(r, depth+1)
		if err != nil {
			return runner.Reply{}, err
		}
		pairs[key.String()] = value
	}
	return runner.MapReply(pairs), nil
}

// readLine reads up to the next CRLF and returns the content without it. RESP
// terminates every line with CRLF, so a bare LF is a protocol violation rather
// than something to be lenient about.
// The reader is sized to maxLineLength so an unterminated line is refused by
// the buffer rather than accumulated without bound.
func readLine(r *bufio.Reader) (string, error) {
	raw, err := r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return "", &ProtocolError{Message: fmt.Sprintf("line exceeds the %d byte limit", maxLineLength)}
	}
	if err != nil {
		return "", &ConnError{Op: "read line", Err: err}
	}

	line := string(raw)
	if !strings.HasSuffix(line, crlf) {
		return "", &ProtocolError{Message: fmt.Sprintf("line %q is not terminated by CRLF", line)}
	}
	return line[:len(line)-2], nil
}

func parseInt(line, what string) (int64, error) {
	n, err := strconv.ParseInt(line, 10, 64)
	if err != nil {
		return 0, &ProtocolError{Message: fmt.Sprintf("%s %q is not a number", what, line)}
	}
	return n, nil
}
