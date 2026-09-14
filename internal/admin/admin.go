// Package admin serves the administrative HTTP API.
//
// The API is split in two by ADR-0023, and the split is the whole design. The
// health endpoints are unauthenticated, because a Kubernetes liveness or
// readiness probe cannot present a credential, and they therefore disclose
// nothing but liveness and readiness — no key counts, no memory figures, not
// even the version. Everything else requires an admin token distinct from the
// client token, because reading statistics and configuration is strictly more
// powerful than reading the cache.
//
// Two rules hold the design together, and both are easier to break than to
// notice broken:
//
//   - Nothing on the health path reads a data source. A liveness probe that
//     could be made to hang by a wedged store would have Kubernetes restart a
//     pod that was serving.
//   - No handler renders a secret. /config is built from a redacted document
//     produced by internal/config, which redacts by field path and by type, so
//     a secret added later is redacted without anyone remembering to do it.
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

// phase is where the node is in its life, as the readiness probe sees it.
//
// Liveness and readiness are genuinely different signals and this type is why
// they cannot be conflated here: liveness never reads it. A probe that answered
// "not alive" while the node was still starting up would have Kubernetes
// restart a pod that only needed another second, and restarting it would put it
// right back where it was.
type phase int32

const (
	// phaseStarting is bound but not yet serving: the listeners may be up while
	// the engine, the expiry wheel, and in P5 the recovery pass, are not.
	phaseStarting phase = iota
	// phaseReady is serving traffic.
	phaseReady
	// phaseStopping is draining. Readiness goes false first so that traffic is
	// taken away before the listeners close; liveness stays true, because a
	// process shutting down on purpose must not be restarted.
	phaseStopping
)

// Server exposes the admin HTTP API.
type Server struct {
	http  *http.Server
	ln    net.Listener
	state atomic.Int32
	log   zerolog.Logger

	// token is the credential the protected endpoints require. Empty means
	// they are open, which config validation only permits while the API is
	// bound to loopback (ADR-0023).
	token string

	// The data sources are swapped in rather than passed at construction
	// because the admin listener binds and serves before the node has any.
	// That ordering is the point of having a liveness probe at all: an
	// orchestrator probing a process that is still building its engine — or,
	// from P5, still replaying a snapshot — must get an answer rather than a
	// connection refused, and a refused connection is a failed liveness probe,
	// which is a restart of a process that was starting normally.
	stats  atomic.Pointer[statsHolder]
	config atomic.Pointer[configHolder]
}

// The holders exist only because an interface value cannot be stored in an
// atomic.Pointer directly.
type statsHolder struct{ src StatsSource }
type configHolder struct{ src ConfigSource }

// Option configures the admin server.
type Option func(*settings)

// settings is what the options accumulate before the server is built.
type settings struct {
	token  string
	stats  StatsSource
	config ConfigSource
	routes map[string]http.Handler
}

// WithToken sets the admin token the protected endpoints require.
//
// It is the server's own credential and never the client's: a client holding a
// data token must not thereby gain administrative access (ADR-0023). Passing
// an empty token leaves the protected endpoints open, which config validation
// allows only on a loopback bind.
func WithToken(token string) Option {
	return func(s *settings) { s.token = token }
}

// WithStats supplies the figures /stats and /stats/memory render. Without it
// those endpoints report that statistics are unavailable rather than inventing
// any.
func WithStats(src StatsSource) Option {
	return func(s *settings) { s.stats = src }
}

// WithConfig supplies the redacted effective configuration /config renders.
func WithConfig(src ConfigSource) Option {
	return func(s *settings) { s.config = src }
}

// withRoute adds a handler to the router. It is unexported and exists so the
// package's own tests can drive the real middleware chain — the panic path in
// particular, which must not be reachable through any shipped endpoint.
func withRoute(pattern string, handler http.Handler) Option {
	return func(s *settings) {
		if s.routes == nil {
			s.routes = map[string]http.Handler{}
		}
		s.routes[pattern] = handler
	}
}

// New binds the admin port and returns a server ready to Serve.
func New(ctx context.Context, addr string, log zerolog.Logger, opts ...Option) (*Server, error) {
	var set settings
	for _, opt := range opts {
		opt(&set)
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}

	s := &Server{
		ln:    ln,
		log:   log,
		token: set.token,
	}
	// Explicit rather than left to the zero value: a node answers "not ready"
	// from the moment it can answer at all, and nothing about that should
	// depend on which constant happens to be first in the iota.
	s.state.Store(int32(phaseStarting))
	if set.stats != nil {
		s.SetStats(set.stats)
	}
	if set.config != nil {
		s.SetConfig(set.config)
	}

	s.http = &http.Server{
		Handler:           s.handler(set.routes),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
	}

	return s, nil
}

// handler builds the router and the middleware chain around it.
//
// The chain is request logging outside panic recovery, so that a panicking
// request is still logged, and logged with the 500 that recovery produced
// rather than with no status at all.
func (s *Server) handler(extra map[string]http.Handler) http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated by design (ADR-0023). Probes cannot present credentials,
	// and these three disclose nothing that is worth a credential.
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /health/live", s.handleLive)
	mux.HandleFunc("GET /health/ready", s.handleReady)

	// Everything else requires the admin token.
	mux.Handle("GET /stats", s.protected(http.HandlerFunc(s.handleStats)))
	mux.Handle("GET /stats/memory", s.protected(http.HandlerFunc(s.handleStatsMemory)))
	mux.Handle("GET /config", s.protected(http.HandlerFunc(s.handleConfig)))

	for pattern, handler := range extra {
		mux.Handle(pattern, handler)
	}

	return s.logRequests(s.recoverPanics(mux))
}

// SetStats supplies the statistics source, or replaces it.
//
// The composition root calls it once the engine and the client listener exist.
// Until then /stats and /stats/memory report that statistics are unavailable,
// which is the truth: there is nothing yet to count.
func (s *Server) SetStats(src StatsSource) {
	s.stats.Store(&statsHolder{src: src})
}

// SetConfig supplies the effective-configuration source, or replaces it after a
// hot reload.
func (s *Server) SetConfig(src ConfigSource) {
	s.config.Store(&configHolder{src: src})
}

// statsSource returns the wired statistics source, or nil.
func (s *Server) statsSource() StatsSource {
	if holder := s.stats.Load(); holder != nil {
		return holder.src
	}
	return nil
}

// configSource returns the wired configuration source, or nil.
func (s *Server) configSource() ConfigSource {
	if holder := s.config.Load(); holder != nil {
		return holder.src
	}
	return nil
}

// Addr returns the bound admin address, which is resolved when the port is 0
func (s *Server) Addr() string {
	return s.ln.Addr().String()
}

// SetReady marks the node ready to serve traffic, or takes that back.
//
// Taking it back moves a ready node to stopping rather than back to starting:
// the two are different to an operator reading a probe, and a node that has
// served and then stopped is not a node that has not started yet.
func (s *Server) SetReady(ready bool) {
	if ready {
		s.state.Store(int32(phaseReady))
		return
	}
	s.state.CompareAndSwap(int32(phaseReady), int32(phaseStopping))
}

// phase reports where the node is in its life.
func (s *Server) phase() phase { return phase(s.state.Load()) }

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
