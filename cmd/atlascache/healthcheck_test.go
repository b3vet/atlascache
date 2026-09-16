package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b3vet/atlascache/internal/config"
)

// TestProbeAddrDialsLoopbackForAWildcardBind covers the case the container
// makes routine: a server told to bind every interface.
//
// 0.0.0.0 is an address to listen on, not one to connect to. Dialing it
// literally happens to reach loopback on Linux and does not elsewhere, and ::
// fails outright where IPv6 is disabled — so a probe that passed the bind
// address straight through would work on the maintainer's machine and fail in
// the image.
func TestProbeAddrDialsLoopbackForAWildcardBind(t *testing.T) {
	tests := []struct {
		name string
		bind string
		port int
		want string
	}{
		{name: "the IPv4 wildcard becomes loopback", bind: "0.0.0.0", port: 8080, want: "127.0.0.1:8080"},
		{name: "the IPv6 wildcard becomes IPv6 loopback", bind: "::", port: 8080, want: "[::1]:8080"},
		{name: "an empty bind address becomes loopback", bind: "", port: 9000, want: "127.0.0.1:9000"},
		{name: "loopback is dialed as written", bind: "127.0.0.1", port: 8080, want: "127.0.0.1:8080"},
		{name: "a specific address is dialed as written", bind: "10.1.2.3", port: 8081, want: "10.1.2.3:8081"},
		{name: "a hostname is dialed as written", bind: "localhost", port: 8080, want: "localhost:8080"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Admin.BindAddr = tc.bind
			cfg.Admin.Port = tc.port

			assert.Equal(t, tc.want, probeAddr(cfg))
		})
	}
}

// TestHealthCheckExitCodes is the contract Docker's HEALTHCHECK reads: 0 means
// ready, anything else means not.
func TestHealthCheckExitCodes(t *testing.T) {
	t.Run("a ready server exits 0", func(t *testing.T) {
		code := healthCheck(configPointingAt(t, readinessServer(t, http.StatusOK)))
		assert.Equal(t, 0, code)
	})

	// The distinction that matters to an orchestrator. A starting node answers
	// /health/ready with 503, and reporting it healthy would send traffic to a
	// node with nothing to answer with.
	t.Run("a server that is not ready exits 1", func(t *testing.T) {
		code := healthCheck(configPointingAt(t, readinessServer(t, http.StatusServiceUnavailable)))
		assert.Equal(t, 1, code)
	})

	t.Run("nothing listening exits 1", func(t *testing.T) {
		// A port nothing is bound to: the probe must fail rather than hang,
		// because Docker reports a timed-out health check with no message.
		code := healthCheck(writeConfig(t, "127.0.0.1", freeishPort(t)))
		assert.Equal(t, 1, code)
	})

	t.Run("a configuration that cannot be read exits 1", func(t *testing.T) {
		assert.Equal(t, 1, healthCheck(filepath.Join(t.TempDir(), "absent.yaml")))
	})
}

// readinessServer stands in for the admin API, answering /health/ready with a
// fixed status.
func readinessServer(t *testing.T, status int) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		if _, err := w.Write([]byte(`{"status":"probe"}`)); err != nil {
			t.Errorf("writing the probe response: %v", err)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// configPointingAt writes a config file whose admin section names the test
// server, so the probe reaches it the same way it would reach a real one.
func configPointingAt(t *testing.T, srv *httptest.Server) string {
	t.Helper()

	parsed, err := url.Parse(srv.URL)
	require.NoError(t, err)

	port, err := strconv.Atoi(parsed.Port())
	require.NoError(t, err)

	return writeConfig(t, parsed.Hostname(), port)
}

func writeConfig(t *testing.T, host string, port int) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "admin:\n  bind_addr: \"" + host + "\"\n  port: " + strconv.Itoa(port) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// freeishPort returns a port that was free a moment ago. Nothing binds it, so
// the probe gets connection-refused, which is the case under test.
func freeishPort(t *testing.T) int {
	t.Helper()

	srv := httptest.NewServer(http.NotFoundHandler())
	parsed, err := url.Parse(srv.URL)
	require.NoError(t, err)
	srv.Close()

	port, err := strconv.Atoi(parsed.Port())
	require.NoError(t, err)
	return port
}

// TestAnswerAndExitHandlesOnlyTheFlagsThatStopEarly. Both --version and
// --health-check answer and exit without starting a server, and everything else
// has to fall through to the server — a flag handler that swallowed the normal
// case would produce a binary that starts nothing and says nothing.
func TestAnswerAndExitHandlesOnlyTheFlagsThatStopEarly(t *testing.T) {
	t.Run("--version is handled", func(t *testing.T) {
		code, handled := answerAndExit(true, false, "")
		assert.True(t, handled)
		assert.Equal(t, 0, code)
	})

	t.Run("--health-check is handled, and reports the probe's verdict", func(t *testing.T) {
		code, handled := answerAndExit(false, true, configPointingAt(t, readinessServer(t, http.StatusOK)))
		assert.True(t, handled)
		assert.Equal(t, 0, code)

		code, handled = answerAndExit(false, true, filepath.Join(t.TempDir(), "absent.yaml"))
		assert.True(t, handled)
		assert.Equal(t, 1, code)
	})

	// --version wins, so `--version --health-check` prints a version rather
	// than silently probing a server that may not exist.
	t.Run("--version takes precedence", func(t *testing.T) {
		code, handled := answerAndExit(true, true, filepath.Join(t.TempDir(), "absent.yaml"))
		assert.True(t, handled)
		assert.Equal(t, 0, code)
	})

	t.Run("neither flag falls through to the server", func(t *testing.T) {
		code, handled := answerAndExit(false, false, "")
		assert.False(t, handled)
		assert.Equal(t, 0, code)
	})
}
