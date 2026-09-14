package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestServer(t *testing.T, opts ...Option) *Server {
	t.Helper()

	srv, err := New(context.Background(), "127.0.0.1:0", zerolog.Nop(), opts...)
	require.NoError(t, err)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, srv.Shutdown(ctx))
		require.NoError(t, <-serveErr)
	})

	return srv
}

// response is one answer from the admin API, kept whole. The body is kept as
// text rather than decoded because several tests are about what is *in* the
// bytes, not about what they decode to.
type response struct {
	status  int
	body    string
	headers http.Header
}

// fields decodes the body as a JSON object.
func (r response) fields(t *testing.T) map[string]any {
	t.Helper()

	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(r.body), &decoded), "body: %s", r.body)
	return decoded
}

// request issues one request, optionally with headers.
func request(t *testing.T, srv *Server, method, path string, headers map[string]string) response {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, "http://"+srv.Addr()+path, nil)
	require.NoError(t, err)
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return response{status: resp.StatusCode, body: string(body), headers: resp.Header}
}

func get(t *testing.T, srv *Server, path string) response {
	t.Helper()
	return request(t, srv, http.MethodGet, path, nil)
}

func TestHealthReportsReadiness(t *testing.T) {
	srv := newTestServer(t)

	resp := get(t, srv, "/health")
	assert.Equal(t, http.StatusServiceUnavailable, resp.status)
	assert.Contains(t, resp.body, statusStarting)

	srv.SetReady(true)

	resp = get(t, srv, "/health")
	assert.Equal(t, http.StatusOK, resp.status)
	assert.Contains(t, resp.body, statusOK)
	assert.Contains(t, resp.headers.Get("Content-Type"), "application/json")
}

// TestLivenessAndReadinessAreDifferentSignals is the distinction the feature
// turns on, walked rather than asserted.
//
// The three states are driven in the order a real node passes through them, and
// at every one of them liveness is 200. Conflating the two signals would make
// Kubernetes restart a pod that was merely still starting — and again while it
// was draining, which is the case that turns a rolling deploy into an outage.
//
// A single assertion that "live returns 200" would pass against an
// implementation that answered readiness from the same flag, because a ready
// node answers both. The walk is what separates them.
func TestLivenessAndReadinessAreDifferentSignals(t *testing.T) {
	srv := newTestServer(t)

	t.Run("starting: alive, not ready", func(t *testing.T) {
		live := get(t, srv, "/health/live")
		assert.Equal(t, http.StatusOK, live.status, "a process that is starting must not be restarted")
		assert.Equal(t, statusAlive, live.fields(t)["status"])

		ready := get(t, srv, "/health/ready")
		assert.Equal(t, http.StatusServiceUnavailable, ready.status, "it cannot serve traffic yet")
		assert.Equal(t, statusStarting, ready.fields(t)["status"])

		assert.Equal(t, http.StatusServiceUnavailable, get(t, srv, "/health").status)
	})

	srv.SetReady(true)

	t.Run("serving: alive and ready", func(t *testing.T) {
		live := get(t, srv, "/health/live")
		assert.Equal(t, http.StatusOK, live.status)
		assert.Equal(t, statusAlive, live.fields(t)["status"])

		ready := get(t, srv, "/health/ready")
		assert.Equal(t, http.StatusOK, ready.status)
		assert.Equal(t, statusReady, ready.fields(t)["status"])

		assert.Equal(t, http.StatusOK, get(t, srv, "/health").status)
	})

	srv.SetReady(false)

	t.Run("draining: still alive, no longer ready", func(t *testing.T) {
		live := get(t, srv, "/health/live")
		assert.Equal(t, http.StatusOK, live.status,
			"a process shutting down on purpose must not be restarted on its way out")
		assert.Equal(t, statusAlive, live.fields(t)["status"])

		ready := get(t, srv, "/health/ready")
		assert.Equal(t, http.StatusServiceUnavailable, ready.status)
		assert.Equal(t, statusStopping, ready.fields(t)["status"],
			"a node that has served and stopped is not a node that has not started")
	})
}

// TestHealthEndpointsNeedNoCredential covers the half of ADR-0023 that makes
// the deployment model work: a Kubernetes probe cannot present a token, so a
// token must never be required here — including on a server that has one.
func TestHealthEndpointsNeedNoCredential(t *testing.T) {
	srv := newTestServer(t, WithToken("an-admin-token"))
	srv.SetReady(true)

	for _, path := range []string{"/health", "/health/live", "/health/ready"} {
		resp := get(t, srv, path)
		assert.Equal(t, http.StatusOK, resp.status, "GET %s with no credential", path)
	}
}

// TestHealthResponsesCarryNothingButStatus is the leak check.
//
// These endpoints are unauthenticated by design, so everything in a response is
// public. Asserting on the field set rather than on the absence of particular
// strings is what catches the next well-meant addition: a version, an uptime, a
// key count — each individually harmless-looking, each a fact about the host
// handed to anyone who can reach the port.
func TestHealthResponsesCarryNothingButStatus(t *testing.T) {
	srv := newTestServer(t, WithToken("an-admin-token"))

	for _, ready := range []bool{false, true} {
		srv.SetReady(ready)

		for _, path := range []string{"/health", "/health/live", "/health/ready"} {
			fields := get(t, srv, path).fields(t)

			require.Len(t, fields, 1, "GET %s (ready=%v) returned more than a status: %v", path, ready, fields)
			status, ok := fields["status"].(string)
			require.True(t, ok, "GET %s returned a non-string status", path)
			assert.Contains(t,
				[]string{statusOK, statusAlive, statusReady, statusStarting, statusStopping},
				status, "GET %s returned an unexpected status word", path)
		}
	}
}

func TestHealthRejectsOtherMethods(t *testing.T) {
	srv := newTestServer(t)
	srv.SetReady(true)

	resp := request(t, srv, http.MethodPost, "/health", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, resp.status)
}

func TestUnknownPathIsNotFound(t *testing.T) {
	srv := newTestServer(t)

	// /backup is P5's, /cluster is P7's and /metrics is P6's. Until they exist
	// they are 404s and not half-answers.
	for _, path := range []string{"/", "/backup", "/cluster/slots", "/metrics", "/health/other"} {
		assert.Equal(t, http.StatusNotFound, get(t, srv, path).status, "GET %s", path)
	}
}

func TestNewFailsOnBusyPort(t *testing.T) {
	srv := newTestServer(t)

	_, err := New(context.Background(), srv.Addr(), zerolog.Nop())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listen on")
}

// TestHandlerPanicReturns500AndTheServerSurvives is the requirement that the
// admin port must not become a way to kill the cache.
//
// The panicking route is registered by the test rather than shipped, which is
// the point: there is no endpoint an attacker can reach to trigger this, and
// the recovery still has to be proven against the real middleware chain rather
// than against a copy of it assembled in a test.
//
// Surviving is asserted by the requests after the panic, not by the absence of
// a crash: a test process that died would fail anyway, but a chain that
// recovered and then left the server unusable would pass a weaker check.
func TestHandlerPanicReturns500AndTheServerSurvives(t *testing.T) {
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("a handler bug nobody predicted")
	})
	srv := newTestServer(t, withRoute("GET /boom", panicking))
	srv.SetReady(true)

	resp := get(t, srv, "/boom")
	assert.Equal(t, http.StatusInternalServerError, resp.status)
	assert.Equal(t, "internal error", resp.fields(t)["error"],
		"a 500 says that something broke and never what")

	// Twice, because a chain that recovered by leaking the connection would
	// pass on the first repeat and fail once the pool reused it.
	for range 2 {
		assert.Equal(t, http.StatusInternalServerError, get(t, srv, "/boom").status)
	}
	assert.Equal(t, http.StatusOK, get(t, srv, "/health").status, "the server is still serving")
	assert.Equal(t, http.StatusOK, get(t, srv, "/health/live").status)
}

// TestPanicAfterAHeaderIsWrittenStillSurvives covers the harder half: a handler
// that has already written a status and then panics cannot be given a 500, and
// the recovery must not make things worse by trying.
func TestPanicAfterAHeaderIsWrittenStillSurvives(t *testing.T) {
	half := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		//nolint:errcheck // the response is deliberately abandoned halfway through
		w.Write([]byte(`{"partial":`))
		panic("halfway through a response")
	})
	srv := newTestServer(t, withRoute("GET /half", half))
	srv.SetReady(true)

	// The response itself is a broken one; what matters is that asking for it
	// does not take the process with it.
	_ = request(t, srv, http.MethodGet, "/half", nil)

	assert.Equal(t, http.StatusOK, get(t, srv, "/health").status, "the server is still serving")
}
