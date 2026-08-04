package server

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/rs/zerolog"

	"github.com/b3vet/atlascache/internal/protocol"
)

// Server serves RESP commands over a Transport. The walking skeleton answers
// PING and QUIT only; every other command is an error (FEAT-0010).
type Server struct {
	transport Transport
	codec     protocol.Codec
	addr      string
	log       zerolog.Logger
}

// New binds the client port and returns a server ready to Serve
func New(ctx context.Context, addr string, log zerolog.Logger) (*Server, error) {
	transport, err := newNetTransport(ctx, addr, log)
	if err != nil {
		return nil, err
	}

	return &Server{
		transport: transport,
		codec:     protocol.NewRESP(),
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

	for {
		cmd, err := s.codec.Decode(c.Reader())
		if err != nil {
			s.handleDecodeError(ctx, c, err, log)
			return
		}

		reply, closeConn := s.dispatch(cmd)
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

// dispatch resolves a command to a reply, reporting whether the connection
// should close afterwards
func (s *Server) dispatch(cmd protocol.Command) (protocol.Reply, bool) {
	switch cmd.Name {
	case "PING":
		switch len(cmd.Args) {
		case 0:
			return protocol.SimpleString("PONG"), false
		case 1:
			return protocol.BulkString(cmd.Args[0]), false
		default:
			return protocol.Errorf("wrong number of arguments for 'ping' command"), false
		}

	case "QUIT":
		return protocol.SimpleString("OK"), true

	default:
		return protocol.Errorf("unknown command '%s'", cmd.Name), false
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
