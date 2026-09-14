package admin

import "net/http"

// The status words the health endpoints answer with. They are the entire
// payload: ADR-0023 makes these endpoints unauthenticated, so anything in a
// response is public, and a key count or a memory figure or a version string
// would be a disclosure bought for nothing. A probe reads the status code; the
// word is for the operator running curl.
const (
	statusOK       = "ok"
	statusAlive    = "alive"
	statusReady    = "ready"
	statusStarting = "starting"
	statusStopping = "stopping"
)

// healthResponse is the whole of a health body. One field, on purpose: the
// struct is what stops an obliging future change from adding "and the key
// count, while we are here".
type healthResponse struct {
	Status string `json:"status"`
}

// handleHealth is the aggregate probe: 200 when the node is serving.
//
// It predates the split into liveness and readiness and is kept because
// orchestrators, the E2E harness, and every operator's muscle memory point at
// it. It tracks readiness, which is the useful answer for "is this node
// serving": a node that is alive but not yet ready is not one to send traffic
// to.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	if s.phase() == phaseReady {
		s.writeJSON(w, http.StatusOK, healthResponse{Status: statusOK})
		return
	}
	s.writeJSON(w, http.StatusServiceUnavailable, healthResponse{Status: s.notReadyStatus()})
}

// handleLive reports that the process is alive and must not be restarted.
//
// It deliberately reads nothing: not the store, not the configuration, not even
// the readiness flag. Every data source it could consult is a way for a wedged
// component to make liveness fail, and a failed liveness probe means Kubernetes
// kills the pod. Restarting a process that is merely still starting up, or
// whose store is slow, turns a delay into a crash loop.
//
// If this handler runs at all, the answer is yes.
func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, healthResponse{Status: statusAlive})
}

// handleReady reports whether the node can serve traffic.
//
// This is the one that is allowed to say no, and it says no during startup,
// during the drain, and — once P5 adds recovery and P7 adds slot migration —
// while either of those is in progress. Its failure takes the node out of a
// load balancer; it never restarts anything.
func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if s.phase() == phaseReady {
		s.writeJSON(w, http.StatusOK, healthResponse{Status: statusReady})
		return
	}
	s.writeJSON(w, http.StatusServiceUnavailable, healthResponse{Status: s.notReadyStatus()})
}

// notReadyStatus names why the node is not ready. Starting and stopping are
// different situations — one is about to be able to serve, the other will not
// be again — and an operator reading a probe by hand needs to tell them apart.
func (s *Server) notReadyStatus() string {
	if s.phase() == phaseStopping {
		return statusStopping
	}
	return statusStarting
}
