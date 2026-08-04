// Package admin serves the administrative HTTP API.
package admin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 10 * time.Second
)

// Server exposes the admin HTTP API. Only /health exists in the walking
// skeleton, and it is unauthenticated by design (ADR-0023).
type Server struct {
	http  *http.Server
	ln    net.Listener
	ready atomic.Bool
	log   zerolog.Logger
}

// New binds the admin port and returns a server ready to Serve
func New(ctx context.Context, addr string, log zerolog.Logger) (*Server, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}

	s := &Server{
		ln:  ln,
		log: log,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)

	s.http = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
	}

	return s, nil
}

// Addr returns the bound admin address, which is resolved when the port is 0
func (s *Server) Addr() string {
	return s.ln.Addr().String()
}

// SetReady marks the node ready to serve traffic
func (s *Server) SetReady(ready bool) {
	s.ready.Store(ready)
}

// Serve blocks until the server is shut down
func (s *Server) Serve() error {
	if err := s.http.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("admin http: %w", err)
	}
	return nil
}

// Shutdown stops the admin server, draining in-flight requests
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

// handleHealth reports readiness only; never key counts, memory, or config
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	status, code := "ok", http.StatusOK
	if !s.ready.Load() {
		status, code = "starting", http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if _, err := fmt.Fprintf(w, "{\"status\":%q}\n", status); err != nil {
		s.log.Debug().Err(err).Msg("health response write failed")
	}
}
