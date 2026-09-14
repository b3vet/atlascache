// Package protocol implements the RESP wire format behind a codec interface.
//
// v0.1.0 speaks RESP2 only (ADR-0028). The codec is stateless and shared by
// every connection: there is no negotiated protocol version to keep per
// connection, and HELLO answers -NOPROTO to a client asking for RESP3 so that
// it falls back rather than failing.
package protocol

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Limits bound what one request may cost before any of it has been trusted.
//
// Every limit is applied while the input is being read, never after: a declared
// length is a claim by an unauthenticated client, and sizing an allocation from
// it is a memory exhaustion vector (ISSUE-0016).
type Limits struct {
	// MaxInlineLength is the largest single line, in bytes, including its CRLF.
	// It bounds inline commands and every length header, and is enforced as the
	// line accumulates, so a client that never sends a newline is disconnected
	// at the limit rather than after the damage.
	MaxInlineLength int

	// MaxMultiBulkLength is the largest element count a multibulk header may
	// declare. The elements themselves are allocated as they arrive.
	MaxMultiBulkLength int

	// MaxBulkLength is the largest single argument, in bytes.
	MaxBulkLength int
}

const (
	defaultMaxInlineLength    = 64 * 1024   // Bytes in one inline request
	defaultMaxMultiBulkLength = 1024 * 1024 // Elements in one request

	// maxValueSizeCeiling mirrors the ceiling internal/config's validation puts
	// on storage.max_value_size. It is duplicated rather than imported: a codec
	// that depended on the configuration package would invert the dependency
	// the Transport seam exists to keep one-way. LimitsForValueSize is how a
	// caller that does know the configured value ties the two together.
	maxValueSizeCeiling = 16 * 1024 * 1024

	// bulkMargin is the protocol overhead a request carries alongside a value:
	// the command name, the key, and any options. A bulk string larger than the
	// value ceiling plus this margin cannot be part of a request the engine
	// would accept, so reading one is work done to reach a rejection.
	bulkMargin = 64 * 1024

	// multiBulkPrealloc caps how many element slots a declared count reserves
	// up front. A header claiming a million elements is cheap to send and, left
	// unclamped, reserves ~24MB of slice headers before a single element has
	// arrived; beyond this the slice grows as the elements are actually read.
	multiBulkPrealloc = 64
)

// DefaultLimits returns the limits a codec uses when it has not been told the
// configured value size: the bulk limit is derived from the largest value any
// valid configuration can accept.
func DefaultLimits() Limits {
	return Limits{
		MaxInlineLength:    defaultMaxInlineLength,
		MaxMultiBulkLength: defaultMaxMultiBulkLength,
		MaxBulkLength:      maxValueSizeCeiling + bulkMargin,
	}
}

// LimitsForValueSize derives limits from the configured storage.max_value_size,
// so the largest argument the parser will read and the largest value the engine
// will store cannot drift apart. A value at or above the configuration
// ceiling — or a nonsensical one — falls back to the default.
func LimitsForValueSize(maxValueSize int) Limits {
	limits := DefaultLimits()
	if maxValueSize > 0 && maxValueSize < maxValueSizeCeiling {
		limits.MaxBulkLength = maxValueSize + bulkMargin
	}
	return limits
}

// normalize replaces unset or nonsensical fields with their defaults, so a
// partially filled Limits cannot silently disable a limit.
func (l Limits) normalize() Limits {
	defaults := DefaultLimits()
	if l.MaxInlineLength <= 0 {
		l.MaxInlineLength = defaults.MaxInlineLength
	}
	if l.MaxMultiBulkLength <= 0 {
		l.MaxMultiBulkLength = defaults.MaxMultiBulkLength
	}
	if l.MaxBulkLength <= 0 {
		l.MaxBulkLength = defaults.MaxBulkLength
	}
	return l
}

// Command is a decoded client request
type Command struct {
	Name string   // Upper-cased command name
	Args [][]byte // Arguments following the name
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
	return Error{Kind: kindErr, Message: e.Error()}
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
type RESP struct {
	limits Limits
}

// NewRESP returns a RESP2 codec with the default limits
func NewRESP() *RESP {
	return &RESP{limits: DefaultLimits()}
}

// NewRESPWithLimits returns a RESP2 codec bounded by the given limits. Unset
// fields fall back to the defaults.
func NewRESPWithLimits(limits Limits) *RESP {
	return &RESP{limits: limits.normalize()}
}

// Limits reports the limits this codec enforces
func (c *RESP) Limits() Limits {
	return c.limits
}

// Decode reads the next command, blocking until one is available.
// Empty requests are skipped, matching Redis.
func (c *RESP) Decode(r *bufio.Reader) (Command, error) {
	for {
		line, err := c.readLine(r, false)
		if err != nil {
			return Command{}, err
		}

		if len(line) == 0 {
			continue
		}

		var cmd Command
		if line[0] == '*' {
			cmd, err = c.decodeMultiBulk(r, line)
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

func (c *RESP) decodeMultiBulk(r *bufio.Reader, header []byte) (Command, error) {
	count, err := strconv.Atoi(string(header[1:]))
	if err != nil || count > c.limits.MaxMultiBulkLength {
		return Command{}, protocolErrorf("invalid multibulk length")
	}
	if count <= 0 {
		return Command{}, nil
	}

	// The declared count is a claim, so it buys a bounded reservation and
	// nothing more; the slice grows as elements actually arrive (ISSUE-0016).
	parts := make([][]byte, 0, min(count, multiBulkPrealloc))
	for range count {
		arg, err := c.decodeBulk(r)
		if err != nil {
			return Command{}, err
		}
		parts = append(parts, arg)
	}

	return commandFromParts(parts), nil
}

// decodeBulk reads one "$<length>\r\n<payload>\r\n" element. The length is
// checked against the limit before it sizes anything.
func (c *RESP) decodeBulk(r *bufio.Reader) ([]byte, error) {
	line, err := c.readLine(r, true)
	if err != nil {
		return nil, err
	}
	if len(line) == 0 || line[0] != '$' {
		return nil, protocolErrorf("expected '$', got '%s'", printableByte(line))
	}

	length, err := strconv.Atoi(string(line[1:]))
	if err != nil || length < 0 || length > c.limits.MaxBulkLength {
		return nil, protocolErrorf("invalid bulk length")
	}

	arg := make([]byte, length+2) // Payload plus CRLF
	if _, err := io.ReadFull(r, arg); err != nil {
		return nil, unexpectedEOF(err)
	}
	if arg[length] != '\r' || arg[length+1] != '\n' {
		return nil, protocolErrorf("expected CRLF after bulk string")
	}

	return arg[:length], nil
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

// readLine reads one CRLF-terminated line, returning it without the terminator.
//
// The limit is enforced as the line accumulates rather than once it is complete:
// bufio.Reader.ReadBytes grows without bound until a newline arrives, so a
// client that sends none is a memory exhaustion attack against an unauthenticated
// server (ISSUE-0016). ReadSlice never allocates — it returns what is already
// buffered — so the check runs before every copy, and the most this can hold is
// the limit plus one buffer.
//
// midFrame distinguishes the two ways a connection can end. Between commands, an
// EOF is a client that went away and is reported as io.EOF; part-way through a
// frame it is a truncated request, reported as io.ErrUnexpectedEOF so that a
// short frame is never mistaken for a complete one.
func (c *RESP) readLine(r *bufio.Reader, midFrame bool) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > c.limits.MaxInlineLength {
			return nil, protocolErrorf("too big inline request")
		}
		line = append(line, chunk...)

		switch {
		case err == nil:
			return trimCRLF(line)
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && (midFrame || len(line) > 0):
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

func trimCRLF(line []byte) ([]byte, error) {
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return nil, protocolErrorf("expected CRLF line terminator")
	}
	return line[:len(line)-2], nil
}

// unexpectedEOF reports a connection that ended part-way through a frame as a
// truncation rather than a clean close.
func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

func printableByte(line []byte) string {
	if len(line) == 0 {
		return ""
	}
	return string(line[:1])
}
