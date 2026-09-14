// Package client is the E2E suite's own RESP client.
//
// It is written from the RESP specification and deliberately shares no code
// with the server under test ([[ADR-0003]]). If the suite spoke through the
// server's own codec, a shared misreading of the protocol would make client and
// server agree with each other and the gate would go green while both were
// wrong. Nothing here may import the parent module.
//
// Its correctness is established by driving a real Redis server — see
// TestAgainstRealRedis — not by agreeing with AtlasCache. Any disagreement
// between this client and AtlasCache is therefore AtlasCache's bug until proven
// otherwise.
//
// The client is deliberately small: connect, send one command, read one reply,
// close. It is not pipelined, not concurrent, and not a general-purpose SDK.
package client

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/b3vet/atlascache/test/e2e/runner"
)

// DefaultTimeout bounds a command when the caller's context has no deadline.
// A test client that can block forever turns a server bug into a hung run.
const DefaultTimeout = 10 * time.Second

// Client is one connection to a RESP server. It is not safe for concurrent use:
// a request and its reply are a matched pair on one connection, and the harness
// serializes steps anyway.
type Client struct {
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
}

// Dial opens a connection to addr. The context bounds the dial only; each
// command carries its own deadline.
func Dial(ctx context.Context, addr string) (*Client, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, &ConnError{Op: "dial " + addr, Err: err}
	}
	return &Client{
		conn: conn,
		r:    bufio.NewReaderSize(conn, maxLineLength),
		w:    bufio.NewWriter(conn),
	}, nil
}

// DialTLS opens a TLS connection to addr, verifying the server against cfg.
//
// The handshake happens here rather than on the first command, so a certificate
// problem is reported as a dial failure — which is what it is — instead of
// surfacing later as an unreadable reply.
func DialTLS(ctx context.Context, addr string, cfg *tls.Config) (*Client, error) {
	dialer := &tls.Dialer{Config: cfg}

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, &ConnError{Op: "dial " + addr + " over TLS", Err: err}
	}

	return &Client{
		conn: conn,
		r:    bufio.NewReaderSize(conn, maxLineLength),
		w:    bufio.NewWriter(conn),
	}, nil
}

// ConnectionState reports the TLS state of the connection, and false for a
// plaintext one. It is how a test asserts which protocol version was negotiated
// and which certificate was presented.
func (c *Client) ConnectionState() (tls.ConnectionState, bool) {
	if c == nil || c.conn == nil {
		return tls.ConnectionState{}, false
	}
	conn, ok := c.conn.(*tls.Conn)
	if !ok {
		return tls.ConnectionState{}, false
	}
	return conn.ConnectionState(), true
}

// Close closes the connection. It is safe to call more than once.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// LocalAddr reports the local side of the connection, which identifies the
// connection in server logs.
func (c *Client) LocalAddr() string {
	if c == nil || c.conn == nil {
		return ""
	}
	return c.conn.LocalAddr().String()
}

// Do parses a text command line such as `SET k1 "hello world"` and sends it.
func (c *Client) Do(ctx context.Context, line string) (runner.Reply, error) {
	args, err := ParseCommand(line)
	if err != nil {
		return runner.Reply{}, err
	}
	return c.Call(ctx, args...)
}

// Call sends one command as a RESP array of bulk strings and reads one reply.
//
// An error *reply* from the server is a value, not a Go error: it comes back as
// a Reply of kind KindError with a nil error. The error return is reserved for
// transport failures, timeouts and malformed wire data.
func (c *Client) Call(ctx context.Context, args ...string) (runner.Reply, error) {
	if len(args) == 0 {
		return runner.Reply{}, errors.New("a command needs at least a name")
	}
	if c == nil || c.conn == nil {
		return runner.Reply{}, &ConnError{Op: "send", Err: net.ErrClosed}
	}

	if err := c.conn.SetDeadline(deadline(ctx)); err != nil {
		return runner.Reply{}, &ConnError{Op: "set deadline", Err: err}
	}

	if err := c.writeCommand(args); err != nil {
		return runner.Reply{}, err
	}
	return c.ReadReply()
}

func (c *Client) writeCommand(args []string) error {
	if err := writeArray(c.w, args); err != nil {
		return &ConnError{Op: "write", Err: err}
	}
	if err := c.w.Flush(); err != nil {
		return &ConnError{Op: "flush", Err: err}
	}
	return nil
}

// ReadReply reads a single reply from the connection, for callers that sent a
// command by other means or that expect an unprompted reply.
func (c *Client) ReadReply() (runner.Reply, error) {
	if c == nil || c.conn == nil {
		return runner.Reply{}, &ConnError{Op: "read", Err: net.ErrClosed}
	}
	reply, err := readReply(c.r, 0)
	if err != nil {
		return runner.Reply{}, err
	}
	return reply, nil
}

func deadline(ctx context.Context) time.Time {
	fallback := time.Now().Add(DefaultTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(fallback) {
		return d
	}
	return fallback
}

// ConnError is a transport failure: the connection was refused, lost, or timed
// out. The harness reconnects on one of these and fails the spec on anything
// else, so misclassifying a protocol bug as a transport blip would hide it.
type ConnError struct {
	Op  string
	Err error
}

func (e *ConnError) Error() string { return fmt.Sprintf("%s: %v", e.Op, e.Err) }

// Unwrap exposes the underlying network error.
func (e *ConnError) Unwrap() error { return e.Err }

// IsConnError reports whether err is a transport failure rather than a
// protocol failure.
func IsConnError(err error) bool {
	var connErr *ConnError
	return errors.As(err, &connErr)
}

// ProtocolError is a reply this client could not read as RESP. It is never
// retried: the wire format is either right or it is a bug worth failing on.
type ProtocolError struct {
	Message string
}

func (e *ProtocolError) Error() string { return "malformed RESP reply: " + e.Message }

// IsProtocolError reports whether err is a wire-format violation.
func IsProtocolError(err error) bool {
	var protoErr *ProtocolError
	return errors.As(err, &protoErr)
}
