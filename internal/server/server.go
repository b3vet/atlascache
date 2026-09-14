package server

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/rs/zerolog"

	"github.com/b3vet/atlascache/internal/protocol"
)

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

	// conns is the connection accounting INFO reports, and the source of the
	// per-connection ids SCAN cursors are scoped to. See commands.go.
	conns connCounters
}

// New binds the client port and returns a server ready to Serve.
//
// The store is required: a server with no keyspace could answer PING and
// nothing else, which is a wiring mistake worth failing at startup rather than
// discovering one GET later.
func New(ctx context.Context, addr string, log zerolog.Logger, store Store) (*Server, error) {
	if store == nil {
		return nil, errors.New("server: a keyspace is required")
	}

	transport, err := newNetTransport(ctx, addr, log)
	if err != nil {
		return nil, err
	}

	return &Server{
		transport: transport,
		codec:     protocol.NewRESPWithLimits(protocol.LimitsForValueSize(store.MaxValueSize())),
		store:     store,
		addr:      transport.Addr(),
		log:       log,
	}, nil
}

// Addr returns the bound client address
func (s *Server) Addr() string {
	return s.addr
}

// Serve blocks until the server is shut down
func (s *Server) Serve() error {
	return s.transport.Serve(s)
}

// Shutdown drains connections and stops the server
func (s *Server) Shutdown(ctx context.Context) error {
	return s.transport.Shutdown(ctx)
}

// Handle implements ConnHandler
func (s *Server) Handle(ctx context.Context, c Conn) {
	log := s.log.With().Str("remote_addr", c.RemoteAddr()).Logger()
	log.Debug().Msg("connection opened")
	defer log.Debug().Msg("connection closed")

	// The session holds what is scoped to this connection rather than to the
	// server: its id, and through it the SCAN cursors filed under that id.
	// Closing it gives the snapshots back immediately instead of at the idle
	// timeout, for a connection that can never come back to finish them.
	sess := s.newSession()
	defer sess.close()

	for {
		cmd, err := s.codec.Decode(c.Reader())
		if err != nil {
			s.handleDecodeError(ctx, c, err, log)
			return
		}

		reply, closeConn := sess.dispatch(cmd)
		if err := s.reply(c, reply); err != nil {
			log.Debug().Err(err).Msg("write failed")
			return
		}

		// The in-flight command is complete, so shutdown can proceed
		if closeConn || ctx.Err() != nil {
			return
		}
	}
}

func (s *Server) reply(c Conn, reply protocol.Reply) error {
	if err := s.codec.Encode(c.Writer(), reply); err != nil {
		return err
	}
	return c.Flush()
}

// handleDecodeError reports malformed input to the client and stays silent for
// the ordinary end-of-connection cases
func (s *Server) handleDecodeError(ctx context.Context, c Conn, err error, log zerolog.Logger) {
	var protoErr *protocol.ProtocolError
	if errors.As(err, &protoErr) {
		log.Debug().Err(err).Msg("protocol error")
		if replyErr := s.reply(c, protoErr.Reply()); replyErr != nil {
			log.Debug().Err(replyErr).Msg("write failed")
		}
		return
	}

	switch {
	case errors.Is(err, io.EOF), errors.Is(err, net.ErrClosed):
		return
	case ctx.Err() != nil:
		// Read deadline expired because the transport is shutting down
		return
	default:
		log.Debug().Err(err).Msg("read failed")
	}
}
