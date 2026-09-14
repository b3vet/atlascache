package client

import (
	"bufio"
	"errors"
	"io"
	"strconv"
)

// The RESP wire format, written from the specification rather than shared with
// the server.
//
// Sharing internal/protocol is not possible across the module boundary and
// would not be wanted if it were (ADR-0015): a client that decodes with the
// server's own encoder proves only that the two agree with each other. Two
// independent implementations of one specification disagree loudly, which is
// the property worth having.

const crlf = "\r\n"

// statusOK is the status reply every write answers with.
const statusOK = "OK"

// The RESP type bytes. The first five are RESP2; the rest are RESP3 additions,
// decoded so that a server which starts answering HELLO 3 does not read as a
// broken one (ADR-0028, FEAT-0048).
const (
	typeStatus = '+'
	typeError  = '-'
	typeInt    = ':'
	typeBulk   = '$'
	typeArray  = '*'

	typeNull     = '_'
	typeDouble   = ','
	typeBool     = '#'
	typeBlobErr  = '!'
	typeVerbatim = '='
	typeBigNum   = '('
	typeMap      = '%'
	typeSet      = '~'
	typePush     = '>'
)

// What the decoder will accept before deciding the server is broken or hostile.
//
// A length header is a claim, and every one of these bounds an allocation made
// from one. They are generous enough that no reply a v0.1.0 server can produce
// hits them: the server's own ceiling on a value is 16MB (internal/config), and
// KEYS on the largest plausible keyspace is well under a million elements.
const (
	maxBulkLength = 512 << 20 // RESP's own ceiling on a bulk string
	maxAggregate  = 1 << 24   // elements in one array, pairs in one map
	maxReplyDepth = 32        // nesting, so a hostile reply cannot recurse without bound

	// readBufferSize is also the ceiling on one header or status line: readLine
	// reads through the buffer, so an unterminated line is refused when the
	// buffer fills rather than accumulated without bound.
	readBufferSize = 16 << 10
)

// appendCommand renders a command as a RESP array of bulk strings, which is the
// only request form a server is required to accept.
//
// It appends to dst so that one buffer per connection serves every command it
// ever sends, and it takes [][]byte because arguments are bytes: a value with a
// null byte in it is a value, not a mistake.
func appendCommand(dst []byte, args [][]byte) []byte {
	dst = append(dst, typeArray)
	dst = strconv.AppendInt(dst, int64(len(args)), 10)
	dst = append(dst, crlf...)
	for _, arg := range args {
		dst = append(dst, typeBulk)
		dst = strconv.AppendInt(dst, int64(len(arg)), 10)
		dst = append(dst, crlf...)
		dst = append(dst, arg...)
		dst = append(dst, crlf...)
	}
	return dst
}

// decodeReply reads one reply.
//
// Errors are of two kinds and the difference decides a connection's fate: a
// *Error with the ErrProtocol category means the stream is at an unknown
// position and the connection must be discarded, while anything else is the
// socket failing and is classified by the caller.
func decodeReply(r *bufio.Reader, depth int) (Reply, error) {
	if depth > maxReplyDepth {
		return Reply{}, protocolError("", "", "reply nests deeper than "+strconv.Itoa(maxReplyDepth)+" levels")
	}

	prefix, err := r.ReadByte()
	if err != nil {
		return Reply{}, err
	}

	line, err := readLine(r)
	if err != nil {
		return Reply{}, err
	}

	switch prefix {
	case typeBulk:
		return decodeBulk(r, line, false)
	case typeVerbatim:
		return decodeBulk(r, line, true)
	case typeBlobErr:
		reply, err := decodeBulk(r, line, false)
		if err != nil {
			return Reply{}, err
		}
		return errorReply(string(reply.Str)), nil
	case typeArray, typeSet:
		return decodeAggregate(r, line, depth, TypeArray)
	case typePush:
		return decodeAggregate(r, line, depth, TypePush)
	case typeMap:
		return decodeMap(r, line, depth)
	default:
		return decodeScalar(prefix, line)
	}
}

// decodeScalar reads the types that carry their whole value on the type line,
// so nothing further is taken from the connection.
func decodeScalar(prefix byte, line []byte) (Reply, error) {
	switch prefix {
	case typeStatus:
		return Reply{Type: TypeStatus, Str: copyLine(line)}, nil

	case typeError:
		return errorReply(string(line)), nil

	case typeInt:
		n, err := parseInt(line, "integer")
		if err != nil {
			return Reply{}, err
		}
		return Reply{Type: TypeInteger, Int: n}, nil

	case typeNull:
		if len(line) != 0 {
			return Reply{}, protocolError("", "", "null reply carries data "+strconv.Quote(string(line)))
		}
		return Reply{Type: TypeNil}, nil

	case typeBool:
		switch string(line) {
		case "t":
			return Reply{Type: TypeBool, Int: 1}, nil
		case "f":
			return Reply{Type: TypeBool, Int: 0}, nil
		default:
			return Reply{}, protocolError("", "", "boolean reply is "+strconv.Quote(string(line))+", want t or f")
		}

	case typeDouble:
		f, err := parseFloat(line)
		if err != nil {
			return Reply{}, err
		}
		// The text is kept alongside the float so that a caller who must not
		// round-trip through float64 does not have to.
		return Reply{Type: TypeDouble, Float: f, Str: copyLine(line)}, nil

	case typeBigNum:
		return Reply{Type: TypeBigNumber, Str: copyLine(line)}, nil

	default:
		return Reply{}, protocolError("", "", "unknown reply type "+strconv.Quote(string(prefix))+" (line "+strconv.Quote(string(line))+")")
	}
}

// decodeBulk reads a length-prefixed string. A negative length is the RESP2
// null bulk string. verbatim strips the mandatory three-byte format prefix, so
// `=15\r\ntxt:hello world\r\n` reads as the bulk string "hello world" —
// which is what the same reply looks like over RESP2.
func decodeBulk(r *bufio.Reader, line []byte, verbatim bool) (Reply, error) {
	n, err := parseInt(line, "bulk length")
	if err != nil {
		return Reply{}, err
	}
	if n < 0 {
		return Reply{Type: TypeNil}, nil
	}
	if n > maxBulkLength {
		return Reply{}, protocolError("", "", "bulk string of "+strconv.FormatInt(n, 10)+" bytes exceeds the limit")
	}

	buf := make([]byte, n+2) // the payload and its CRLF
	if _, err := io.ReadFull(r, buf); err != nil {
		return Reply{}, err
	}
	if string(buf[n:]) != crlf {
		return Reply{}, protocolError("", "", "bulk string is not terminated by CRLF")
	}

	payload := buf[:n]
	if verbatim {
		if len(payload) < 4 || payload[3] != ':' {
			return Reply{}, protocolError("", "", "verbatim string has no format prefix")
		}
		payload = payload[4:]
	}
	return Reply{Type: TypeBulk, Str: payload}, nil
}

// decodeAggregate reads an array, a set or a push message. A negative length is
// the RESP2 null array.
func decodeAggregate(r *bufio.Reader, line []byte, depth int, kind ReplyType) (Reply, error) {
	n, err := parseInt(line, "array length")
	if err != nil {
		return Reply{}, err
	}
	if n < 0 {
		return Reply{Type: TypeNil}, nil
	}
	if n > maxAggregate {
		return Reply{}, protocolError("", "", "array of "+strconv.FormatInt(n, 10)+" elements exceeds the limit")
	}

	items, err := decodeItems(r, n, depth)
	if err != nil {
		return Reply{}, err
	}
	return Reply{Type: kind, Arr: items}, nil
}

// decodeMap reads a RESP3 map, whose length counts pairs rather than elements.
// The pairs are flattened into the same alternating array a RESP2 server sends,
// so Reply.Map reads either without knowing which it got.
func decodeMap(r *bufio.Reader, line []byte, depth int) (Reply, error) {
	pairs, err := parseInt(line, "map length")
	if err != nil {
		return Reply{}, err
	}
	if pairs < 0 {
		return Reply{Type: TypeNil}, nil
	}
	if pairs > maxAggregate {
		return Reply{}, protocolError("", "", "map of "+strconv.FormatInt(pairs, 10)+" pairs exceeds the limit")
	}

	items, err := decodeItems(r, pairs*2, depth)
	if err != nil {
		return Reply{}, err
	}
	return Reply{Type: TypeMap, Arr: items}, nil
}

// decodeItems reads n consecutive replies.
//
// The slice is grown as elements arrive rather than sized from n: n is a claim
// by the other end, and preallocating from it is how a five-byte reply header
// becomes a large allocation.
func decodeItems(r *bufio.Reader, n int64, depth int) ([]Reply, error) {
	const prealloc = 64
	capacity := n
	if capacity > prealloc {
		capacity = prealloc
	}

	items := make([]Reply, 0, capacity)
	for i := int64(0); i < n; i++ {
		item, err := decodeReply(r, depth+1)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

// readLine reads up to the next CRLF and returns the content without it.
//
// RESP terminates every line with CRLF, so a bare LF is a protocol violation
// rather than something to be lenient about: accepting one lets a truncated
// frame read as a complete one.
func readLine(r *bufio.Reader) ([]byte, error) {
	raw, err := r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, protocolError("", "", "reply line exceeds the "+strconv.Itoa(readBufferSize)+" byte limit")
	}
	if err != nil {
		return nil, err
	}
	if len(raw) < 2 || raw[len(raw)-2] != '\r' {
		return nil, protocolError("", "", "reply line is not terminated by CRLF")
	}

	// ReadSlice returns the reader's own buffer, which the next read reuses.
	// Every caller here either parses the bytes immediately or copies them, and
	// decodeScalar's Str fields are copied by the one place that keeps them.
	return raw[:len(raw)-2], nil
}

// errorReply splits a server error into its kind and its full text. The kind is
// the first word — ERR, WRONGPASS, OOM — and it is what decides the category a
// caller sees.
func errorReply(line string) Reply {
	kind := line
	for i := 0; i < len(line); i++ {
		if line[i] == ' ' {
			kind = line[:i]
			break
		}
	}
	return Reply{Type: TypeError, Kind: kind, Str: []byte(line)}
}

// copyLine takes a line out of the reader's buffer.
//
// readLine returns a slice of bufio's own buffer, which the next read
// overwrites. Every reply that keeps its line past the next read copies it
// here; forgetting to would hand a caller bytes that change underneath it,
// which is the sort of bug that only shows up under pipelining or load.
func copyLine(line []byte) []byte {
	if len(line) == 0 {
		return []byte{}
	}
	out := make([]byte, len(line))
	copy(out, line)
	return out
}

func parseInt(line []byte, what string) (int64, error) {
	n, err := strconv.ParseInt(string(line), 10, 64)
	if err != nil {
		return 0, protocolError("", "", what+" "+strconv.Quote(string(line))+" is not a number")
	}
	return n, nil
}

func parseFloat(line []byte) (float64, error) {
	f, err := strconv.ParseFloat(string(line), 64)
	if err != nil {
		return 0, protocolError("", "", "double "+strconv.Quote(string(line))+" is not a number")
	}
	return f, nil
}
