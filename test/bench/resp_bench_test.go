package netbench

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// The load generator speaks RESP2 itself rather than importing the server's
// codec, for the same reason the E2E client does (ADR-0003): a shared
// misreading would make client and server agree with each other, and a
// benchmark that measures a server through its own encoder is measuring half
// the thing twice.
//
// It is deliberately allocation-free on the hot path. Requests are pre-encoded
// into a table before the clock starts, and replies are consumed without ever
// materializing the payload -- a bulk body is discarded straight out of the
// read buffer. A generator that allocated per operation would be measuring its
// own garbage collector.

const (
	crlf        = "\r\n"
	readBufSize = 64 * 1024
)

var errProtocol = errors.New("malformed reply")

// encodeCmd builds one RESP2 array-of-bulk-strings request.
func encodeCmd(args ...[]byte) []byte {
	n := len("*") + len(strconv.Itoa(len(args))) + len(crlf)
	for _, a := range args {
		n += len("$") + len(strconv.Itoa(len(a))) + len(crlf) + len(a) + len(crlf)
	}
	buf := make([]byte, 0, n)
	buf = append(buf, '*')
	buf = strconv.AppendInt(buf, int64(len(args)), 10)
	buf = append(buf, crlf...)
	for _, a := range args {
		buf = append(buf, '$')
		buf = strconv.AppendInt(buf, int64(len(a)), 10)
		buf = append(buf, crlf...)
		buf = append(buf, a...)
		buf = append(buf, crlf...)
	}
	return buf
}

func encodeStrs(args ...string) []byte {
	b := make([][]byte, len(args))
	for i, a := range args {
		b[i] = []byte(a)
	}
	return encodeCmd(b...)
}

// reply is what the generator needs to know about one server reply: its type
// byte, and the integer for an `:n` reply so DEL hits can be counted.
type reply struct {
	kind byte
	n    int64
}

// readReply consumes exactly one reply and reports its shape. A `-ERR` reply
// comes back as a Go error: during a benchmark it means the workload is wrong,
// and a run that quietly counted error replies as throughput would be a lie.
func readReply(r *bufio.Reader) (reply, error) {
	line, err := readLine(r)
	if err != nil {
		return reply{}, err
	}
	if len(line) == 0 {
		return reply{}, errProtocol
	}
	switch line[0] {
	case '+':
		return reply{kind: '+'}, nil
	case '-':
		return reply{kind: '-'}, fmt.Errorf("server replied %s", line)
	case ':':
		n, convErr := strconv.ParseInt(string(line[1:]), 10, 64)
		if convErr != nil {
			return reply{}, errProtocol
		}
		return reply{kind: ':', n: n}, nil
	case '$':
		return readBulk(r, line)
	case '*':
		return readArray(r, line)
	default:
		return reply{}, fmt.Errorf("%w: unknown type byte %q", errProtocol, line[0])
	}
}

func readBulk(r *bufio.Reader, line []byte) (reply, error) {
	n, err := strconv.Atoi(string(line[1:]))
	if err != nil {
		return reply{}, errProtocol
	}
	if n < 0 {
		return reply{kind: '$', n: -1}, nil
	}
	if _, err := r.Discard(n + len(crlf)); err != nil {
		return reply{}, err
	}
	return reply{kind: '$', n: int64(n)}, nil
}

func readArray(r *bufio.Reader, line []byte) (reply, error) {
	n, err := strconv.Atoi(string(line[1:]))
	if err != nil {
		return reply{}, errProtocol
	}
	for i := 0; i < n; i++ {
		if _, err := readReply(r); err != nil {
			return reply{}, err
		}
	}
	return reply{kind: '*', n: int64(n)}, nil
}

// readLine returns one CRLF-terminated line without its terminator. The slice
// points into the read buffer and is only valid until the next read, which is
// exactly the lifetime the callers above need and the reason nothing here
// allocates.
func readLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, fmt.Errorf("%w: reply line longer than %d bytes", errProtocol, readBufSize)
		}
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return nil, io.EOF
		}
		return nil, err
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return nil, fmt.Errorf("%w: line not terminated by CRLF", errProtocol)
	}
	return line[:len(line)-2], nil
}
