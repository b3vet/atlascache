// Package server accepts client connections and serves them over RESP.
package server

import (
	"bufio"
	"context"
	"io"
)

// Conn is the transport-agnostic view of a client connection. Handlers see
// only this, never net.Conn, so a different transport (gnet, ADR-0007) can be
// added without touching the handler layer.
type Conn interface {
	// Reader returns the buffered request stream
	Reader() *bufio.Reader

	// Writer returns the buffered response stream
	Writer() io.Writer

	// Flush pushes buffered response bytes to the peer
	Flush() error

	// RemoteAddr returns the peer address for logging
	RemoteAddr() string

	// Close releases the connection
	Close() error
}

// ConnHandler serves a single connection until it ends. The context is
// canceled when the transport begins shutting down; handlers must return
// once the in-flight command completes.
type ConnHandler interface {
	Handle(ctx context.Context, c Conn)
}

// Transport accepts connections and hands them to a handler
type Transport interface {
	// Serve blocks accepting connections until Shutdown is called
	Serve(handler ConnHandler) error

	// Shutdown stops accepting, drains in-flight work, and closes connections.
	// It returns the context error if the drain does not finish in time.
	Shutdown(ctx context.Context) error
}
