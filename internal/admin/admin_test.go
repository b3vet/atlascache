package admin

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()

	srv, err := New(context.Background(), "127.0.0.1:0", zerolog.Nop())
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

func get(t *testing.T, srv *Server, path string) (int, string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+srv.Addr()+path, nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp.StatusCode, string(body)
}

func TestHealthReportsReadiness(t *testing.T) {
	srv := newTestServer(t)

	code, body := get(t, srv, "/health")
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Contains(t, body, "starting")

	srv.SetReady(true)

	code, body = get(t, srv, "/health")
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "ok")
}

func TestHealthRejectsOtherMethods(t *testing.T) {
	srv := newTestServer(t)
	srv.SetReady(true)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+srv.Addr()+"/health", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

func TestUnknownPathIsNotFound(t *testing.T) {
	srv := newTestServer(t)

	code, _ := get(t, srv, "/stats")
	assert.Equal(t, http.StatusNotFound, code)
}

func TestNewFailsOnBusyPort(t *testing.T) {
	srv := newTestServer(t)

	_, err := New(context.Background(), srv.Addr(), zerolog.Nop())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listen on")
}
