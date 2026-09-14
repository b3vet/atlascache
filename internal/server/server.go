package server

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"os"

	"github.com/rs/zerolog"

	"github.com/b3vet/atlascache/internal/protocol"
)

// Option configures a server at construction. Both of the P2 security features
// arrive this way rather than as constructor parameters, so that the common
// case — neither enabled, which is the default (ADR-0009) — stays a four
// argument call.
type Option func(*options)

type options struct {
	tls    *tls.Config
	auth   *Authenticator
	limits ConnLimits
}

// WithTLS serves the client port over TLS.
//
// The configuration is applied to the listener, inside the transport. Nothing
// above the transport is told about it: a handler cannot discover whether its
// connection is encrypted, which is what keeps the gnet transport in P8 from
// having to reproduce the leak (ADR-0007).
func WithTLS(cfg *tls.Config) Option {
	return func(o *options) { o.tls = cfg }
}

// WithAuth requires clients to authenticate before running commands outside the
// pre-auth allowlist. Enforcement is in dispatch, not in the handlers — see
// session.dispatch.
func WithAuth(auth *Authenticator) Option {
	return func(o *options) { o.auth = auth }
}

// WithConnLimits bounds what one client may cost: how many connections there
// may be, how long one may be silent, how large one request may be, how many
// pipelined commands are answered in one batch, and how much reply may be
// buffered for a client that has stopped reading.
//
// It replaces the whole set rather than merging with the defaults, and two of
// the fields read a zero as "off" rather than as "unset" — IdleTimeout and
// MaxOutputBytes, because switching those off is a decision an operator is
// allowed to make. A caller that means "the defaults but with one change"
// should therefore start from DefaultConnLimits and change the one field. A
// server built with no option at all gets DefaultConnLimits whole.
func WithConnLimits(limits ConnLimits) Option {
	return func(o *options) { o.limits = limits }
}

// pipelineFlushBytes is the size half of the batch cap: the accumulated replies
// are flushed once they reach it, whatever the command count.
//
// It matches the connection's write buffer, which makes the flush a single
// write through it rather than a copy followed by a write, and keeps the
// steady-state cost of a connection at three buffers of this size rather than
// letting the reply buffer grow to whatever a client's pipeline depth allows.
const pipelineFlushBytes = connBufferSize

// Server serves RESP commands over a Transport, against the keyspace it is
// given. The skeleton answered PING and QUIT only (FEAT-0010); P1 adds the five
// data commands that make the engine observable from outside the process
// (FEAT-0017).
type Server struct {
	transport Transport
	codec     protocol.Codec
	store     Store
	addr      string
	log       zerolog.Logger

	// auth is the gate every command passes through. It is never nil; a server
	// built without WithAuth gets one with authentication disabled, so the
	// dispatch path has no special case to forget.
	auth *Authenticator

	// limits are the per-connection bounds (FEAT-0024). The transport enforces
	// the ones it can see — the connection count, the deadlines, the request
	// budget — and the read loop below enforces the two that are only visible
	// where commands are executed.
	limits ConnLimits

	// codecLimits are the parser bounds derived from limits and the engine's
	// value size. They are kept rather than re-derived so INFO can report them:
	// an operator meeting "invalid multibulk length" needs to be able to read
	// back what the limit actually is.
	codecLimits protocol.Limits

	// conns is the connection accounting INFO reports, and the source of the
	// per-connection ids SCAN cursors are scoped to. See commands.go.
	conns connCounters
}

// New binds the client port and returns a server ready to Serve.
//
// The store is required: a server with no keyspace could answer PING and
// nothing else, which is a wiring mistake worth failing at startup rather than
// discovering one GET later.
func New(ctx context.Context, addr string, log zerolog.Logger, store Store, opts ...Option) (*Server, error) {
	if store == nil {
		return nil, errors.New("server: a keyspace is required")
	}

	settings := options{limits: DefaultConnLimits()}
	for _, opt := range opts {
		opt(&settings)
	}
	if settings.auth == nil {
		settings.auth = NewAuthenticator(false, "")
	}
	limits := settings.limits.normalize()

	codec := protocol.NewRESPWithLimits(limits.codecLimits(store.MaxValueSize()))
	srv := &Server{
		codec: codec,
		// Read back from the codec rather than from what it was handed, so what
		// INFO reports is what is enforced even if the codec normalizes it.
		codecLimits: codec.Limits(),
		store:       store,
		log:         log,
		auth:        settings.auth,
		limits:      limits,
	}

	transport, err := newNetTransport(ctx, addr, log, settings.tls, limits, &srv.conns)
	if err != nil {
		return nil, err
	}
	srv.transport = transport
	srv.addr = transport.Addr()

	return srv, nil
}

// Addr returns the bound client address
func (s *Server) Addr() string {
	return s.addr
}

// Limits reports the per-connection bounds this server enforces.
func (s *Server) Limits() ConnLimits { return s.limits }

// Serve blocks until the server is shut down
func (s *Server) Serve() error {
	return s.transport.Serve(s)
}

// Shutdown drains connections and stops the server
func (s *Server) Shutdown(ctx context.Context) error {
	return s.transport.Shutdown(ctx)
}

// Handle implements ConnHandler.
//
// The loop is the whole of pipelining (ADR-0021). It decodes every request
// already sitting in the read buffer, executes them in order, accumulates the
// replies, and flushes once — so a batch of N commands costs one read and one
// write rather than N of each, with ordering guaranteed structurally because
// there is only ever one goroutine and it never runs two commands at a time.
//
// Where it stops draining is the part that matters. It flushes before any read
// that could block, which is what keeps a client that pipelines and then waits
// from deadlocking against a server holding its replies; and it flushes at the
// batch caps, which is what keeps a client that pipelines without reading from
// making the server buffer without bound.
func (s *Server) Handle(ctx context.Context, c Conn) {
	log := s.log.With().Str("remote_addr", c.RemoteAddr()).Logger()
	log.Debug().Msg("connection opened")
	defer log.Debug().Msg("connection closed")

	// The session holds what is scoped to this connection rather than to the
	// server: its id, and through it the SCAN cursors filed under that id.
	// Closing it gives the snapshots back immediately instead of at the idle
	// timeout, for a connection that can never come back to finish them.
	sess := s.newSession(c.RemoteAddr())
	defer sess.close()

	out := newOutputBuffer(s.limits.MaxOutputBytes)

	for {
		cmd, err := s.codec.Decode(c.Reader())
		if err != nil {
			s.handleDecodeError(ctx, c, out, err, log)
			return
		}

		reply, closeConn := sess.dispatch(cmd)

		// The request is complete, so the idle clock restarts and the byte
		// budget for the next one is returned. Per command, never per byte:
		// that is what makes a client dribbling bytes without finishing a
		// request still idle (FEAT-0024).
		c.Touch()

		if err := out.append(s.codec, reply); err != nil {
			s.conns.outputClosed.Add(1)
			log.Warn().Err(err).Int("buffered_bytes", out.Len()).
				Int("max_output_buffer", s.limits.MaxOutputBytes).
				Msg("closing connection: buffered reply is over the output limit")
			return
		}

		if !s.batchEnds(ctx, c, out, closeConn) {
			continue
		}
		if err := out.flush(c); err != nil {
			s.noteWriteFailure(ctx, err, log)
			return
		}
		if closeConn || ctx.Err() != nil {
			// The in-flight command is complete and answered, so shutdown can
			// proceed.
			return
		}
	}
}

// batchEnds reports whether the accumulated replies should go out now.
//
// The first three conditions are the batch caps. The last is the one the design
// turns on: with no further complete request in the buffer, the next decode
// would block, and blocking with replies still buffered is what stalls a client
// that pipelined and is now waiting for them.
func (s *Server) batchEnds(ctx context.Context, c Conn, out *outputBuffer, closeConn bool) bool {
	switch {
	case closeConn, ctx.Err() != nil:
		return true
	case out.commands >= s.limits.MaxPipelineCommands:
		return true
	case out.Len() >= pipelineFlushBytes:
		return true
	default:
		return !requestBuffered(c.Reader())
	}
}

// noteWriteFailure records why a connection's replies could not be delivered.
//
// A write that times out is a client that stopped reading, which is a different
// fault from a client that hung up: it is the case the output limit and the
// write deadline exist for, and it is worth a counter of its own so an operator
// can tell "clients are disconnecting" from "clients are not draining".
func (s *Server) noteWriteFailure(ctx context.Context, err error, log zerolog.Logger) {
	if errors.Is(err, os.ErrDeadlineExceeded) && ctx.Err() == nil {
		s.conns.stalledClosed.Add(1)
		log.Warn().Err(err).Msg("closing connection: the client stopped reading its replies")
		return
	}
	log.Debug().Err(err).Msg("write failed")
}

// handleDecodeError delivers whatever the connection had already earned, then
// reports the failure to the client where there is one worth reporting.
//
// Flushing first is not politeness: the replies to the commands before the bad
// one are already owed, and a client matching replies to requests positionally
// would otherwise line the error up against the wrong request.
func (s *Server) handleDecodeError(ctx context.Context, c Conn, out *outputBuffer, err error, log zerolog.Logger) {
	if reply, report := s.decodeErrorReply(ctx, err, log); report {
		if appendErr := out.append(s.codec, reply); appendErr != nil {
			log.Debug().Err(appendErr).Msg("could not buffer the protocol error")
		}
	}
	if flushErr := out.flush(c); flushErr != nil {
		s.noteWriteFailure(ctx, flushErr, log)
	}
}

// decodeErrorReply classifies a decode failure, returning the reply the client
// should get and whether there is anything to say at all.
func (s *Server) decodeErrorReply(ctx context.Context, err error, log zerolog.Logger) (protocol.Reply, bool) {
	var protoErr *protocol.ProtocolError
	switch {
	case errors.As(err, &protoErr):
		log.Debug().Err(err).Msg("protocol error")
		return protoErr.Reply(), true

	case errors.Is(err, errRequestTooLarge):
		// The budget is spent while the request is still arriving, so this is
		// reported before the rest of it has been read — the request is refused
		// rather than accepted and then regretted (ISSUE-0018).
		s.conns.requestClosed.Add(1)
		log.Warn().Int("max_request_size", s.limits.MaxRequestBytes).
			Msg("closing connection: request is over the size budget")
		return protocol.Errorf("Protocol error: request is too large"), true

	case errors.Is(err, io.EOF), errors.Is(err, net.ErrClosed):
		return nil, false

	case ctx.Err() != nil:
		// Read deadline expired because the transport is shutting down
		return nil, false

	case errors.Is(err, os.ErrDeadlineExceeded):
		s.conns.idleClosed.Add(1)
		log.Debug().Dur("client_idle_timeout", s.limits.IdleTimeout).
			Msg("closing connection: idle past the timeout")
		return nil, false

	default:
		log.Debug().Err(err).Msg("read failed")
		return nil, false
	}
}

// errOutputTooLarge is what the output limit reports. It is a crude limit by
// design: the bytes are counted after the reply has been rendered, so the
// breach is detected rather than prevented. FEAT-0046 refines it; what this
// buys today is that a client which asks for a reply it will not read is
// disconnected instead of being buffered for indefinitely.
var errOutputTooLarge = errors.New("buffered reply exceeds the connection output limit")

// outputBuffer accumulates the replies of one pipeline batch.
//
// It exists rather than writing each reply straight through the connection's
// own buffer because two things need counting that a bufio.Writer does not
// expose: how many commands are in the batch, and how many bytes are waiting
// for a client that may not be reading.
type outputBuffer struct {
	buf      []byte
	commands int
	limit    int
}

func newOutputBuffer(limit int) *outputBuffer {
	return &outputBuffer{limit: limit}
}

// Write implements io.Writer so the codec can render straight into the batch.
func (o *outputBuffer) Write(p []byte) (int, error) {
	o.buf = append(o.buf, p...)
	return len(p), nil
}

// Len is the bytes waiting to be flushed.
func (o *outputBuffer) Len() int { return len(o.buf) }

// append renders one reply into the batch, refusing to grow past the limit.
func (o *outputBuffer) append(codec protocol.Codec, reply protocol.Reply) error {
	if err := codec.Encode(o, reply); err != nil {
		return err
	}
	o.commands++

	if o.limit > 0 && len(o.buf) > o.limit {
		return errOutputTooLarge
	}
	return nil
}

// flush writes the batch and empties it.
func (o *outputBuffer) flush(c Conn) error {
	if len(o.buf) == 0 {
		o.commands = 0
		return nil
	}

	_, err := c.Writer().Write(o.buf)
	o.reset()
	if err != nil {
		return err
	}
	return c.Flush()
}

// reset empties the batch, giving back any capacity a single large reply
// forced it to grow to. Without that, one KEYS reply would leave every
// connection that ever served one holding a buffer the size of it.
func (o *outputBuffer) reset() {
	if cap(o.buf) > pipelineFlushBytes {
		o.buf = nil
	} else {
		o.buf = o.buf[:0]
	}
	o.commands = 0
}
