// Package protocol implements the RESP wire format behind a codec interface.
package protocol

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Protocol limits, mirroring the Redis defaults
const (
	maxMultiBulkLength = 1024 * 1024       // Elements in one request
	maxBulkLength      = 512 * 1024 * 1024 // Bytes in one argument
	maxInlineLength    = 64 * 1024         // Bytes in one inline request
)

// Command is a decoded client request
type Command struct {
	Name string   // Upper-cased command name
	Args [][]byte // Arguments following the name
}

// Reply is a value the codec can serialize as a RESP response
type Reply interface {
	isReply()
}

// SimpleString is a RESP simple string, e.g. "+PONG"
type SimpleString string

func (SimpleString) isReply() {}

// Error is a RESP error, e.g. "-ERR unknown command"
type Error struct {
	Kind    string // ERR, WRONGTYPE, NOAUTH, ...
	Message string
}

func (Error) isReply() {}

func (e Error) Error() string {
	if e.Kind == "" {
		return e.Message
	}
	return e.Kind + " " + e.Message
}

// BulkString is a RESP bulk string, e.g. "$5\r\nhello"
type BulkString []byte

func (BulkString) isReply() {}

// Integer is a RESP integer, e.g. ":1"
type Integer int64

func (Integer) isReply() {}

// Array is a RESP array of replies
type Array []Reply

func (Array) isReply() {}

type nilReply struct{}

func (nilReply) isReply() {}

// Nil is the RESP null bulk string
var Nil Reply = nilReply{}

// Errorf builds a generic ERR reply
func Errorf(format string, args ...any) Error {
	return Error{Kind: "ERR", Message: fmt.Sprintf(format, args...)}
}

// ProtocolError indicates a malformed request. The connection cannot be
// resynchronised after one, so the caller must close it.
type ProtocolError struct {
	Message string
}

func (e *ProtocolError) Error() string {
	return "Protocol error: " + e.Message
}

// Reply renders the protocol error as a RESP error
func (e *ProtocolError) Reply() Error {
	return Error{Kind: "ERR", Message: e.Error()}
}

func protocolErrorf(format string, args ...any) *ProtocolError {
	return &ProtocolError{Message: fmt.Sprintf(format, args...)}
}

// Codec decodes client requests and encodes server replies
type Codec interface {
	Decode(r *bufio.Reader) (Command, error)
	Encode(w io.Writer, reply Reply) error
}

// RESP implements Codec for the RESP2 wire format
type RESP struct{}

// NewRESP returns a RESP2 codec
func NewRESP() *RESP {
	return &RESP{}
}

// Decode reads the next command, blocking until one is available.
// Empty requests are skipped, matching Redis.
func (c *RESP) Decode(r *bufio.Reader) (Command, error) {
	for {
		line, err := readLine(r)
		if err != nil {
			return Command{}, err
		}

		if len(line) == 0 {
			continue
		}

		var cmd Command
		if line[0] == '*' {
			cmd, err = decodeMultiBulk(r, line)
			if err != nil {
				return Command{}, err
			}
		} else {
			cmd = decodeInline(line)
		}
		if cmd.Name == "" {
			continue
		}

		return cmd, nil
	}
}

// Encode writes a reply in RESP2 form
func (c *RESP) Encode(w io.Writer, reply Reply) error {
	buf, err := appendReply(nil, reply)
	if err != nil {
		return err
	}
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("write reply: %w", err)
	}
	return nil
}

func decodeMultiBulk(r *bufio.Reader, header []byte) (Command, error) {
	count, err := strconv.Atoi(string(header[1:]))
	if err != nil || count > maxMultiBulkLength {
		return Command{}, protocolErrorf("invalid multibulk length")
	}
	if count <= 0 {
		return Command{}, nil
	}

	parts := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		line, err := readLine(r)
		if err != nil {
			return Command{}, err
		}
		if len(line) == 0 || line[0] != '$' {
			return Command{}, protocolErrorf("expected '$', got '%s'", printableByte(line))
		}

		length, err := strconv.Atoi(string(line[1:]))
		if err != nil || length < 0 || length > maxBulkLength {
			return Command{}, protocolErrorf("invalid bulk length")
		}

		arg := make([]byte, length+2) // Payload plus CRLF
		if _, err := io.ReadFull(r, arg); err != nil {
			return Command{}, err
		}
		if arg[length] != '\r' || arg[length+1] != '\n' {
			return Command{}, protocolErrorf("expected CRLF after bulk string")
		}

		parts = append(parts, arg[:length])
	}

	return commandFromParts(parts), nil
}

func decodeInline(line []byte) Command {
	fields := strings.Fields(string(line))
	if len(fields) == 0 {
		return Command{}
	}

	parts := make([][]byte, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, []byte(f))
	}

	return commandFromParts(parts)
}

func commandFromParts(parts [][]byte) Command {
	return Command{
		Name: strings.ToUpper(string(parts[0])),
		Args: parts[1:],
	}
}

// readLine reads one CRLF-terminated line, returning it without the terminator
func readLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	if len(line) > maxInlineLength {
		return nil, protocolErrorf("too big inline request")
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return nil, protocolErrorf("expected CRLF line terminator")
	}
	return line[:len(line)-2], nil
}

func appendReply(dst []byte, reply Reply) ([]byte, error) {
	switch v := reply.(type) {
	case SimpleString:
		dst = append(dst, '+')
		dst = append(dst, string(v)...)
		return appendCRLF(dst), nil

	case Error:
		dst = append(dst, '-')
		dst = append(dst, v.Error()...)
		return appendCRLF(dst), nil

	case BulkString:
		dst = append(dst, '$')
		dst = strconv.AppendInt(dst, int64(len(v)), 10)
		dst = appendCRLF(dst)
		dst = append(dst, v...)
		return appendCRLF(dst), nil

	case Integer:
		dst = append(dst, ':')
		dst = strconv.AppendInt(dst, int64(v), 10)
		return appendCRLF(dst), nil

	case Array:
		dst = append(dst, '*')
		dst = strconv.AppendInt(dst, int64(len(v)), 10)
		dst = appendCRLF(dst)
		for _, item := range v {
			var err error
			if dst, err = appendReply(dst, item); err != nil {
				return nil, err
			}
		}
		return dst, nil

	case nilReply:
		return append(dst, "$-1\r\n"...), nil

	default:
		return nil, fmt.Errorf("protocol: unsupported reply type %T", reply)
	}
}

func appendCRLF(dst []byte) []byte {
	return append(dst, '\r', '\n')
}

func printableByte(line []byte) string {
	if len(line) == 0 {
		return ""
	}
	return string(line[:1])
}
