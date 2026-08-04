package scenarios_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/b3vet/atlascache/test/e2e/runner"
	"github.com/b3vet/atlascache/test/e2e/runner/fakeharness"
	_ "github.com/b3vet/atlascache/test/e2e/scenarios"
)

// A scenario that cannot fail proves nothing, and a scenario only ever runs
// against a healthy server in the suite. So each one is driven here against a
// stub that misbehaves in the specific way the scenario exists to catch.

// stub is a harness whose answers a test controls: where the admin API is, what
// the server binary prints, and whether a restart really restarts.
type stub struct {
	*fakeharness.Harness
	info      runner.ServerInfo
	root      string
	binaryRun func(args []string) (int, string, error)
	restart   func() error
}

func (s *stub) Info() runner.ServerInfo { return s.info }
func (s *stub) Root() string            { return s.root }

func (s *stub) RunBinary(_ context.Context, args ...string) (int, string, error) {
	if s.binaryRun == nil {
		return 0, "", nil
	}
	return s.binaryRun(args)
}

func (s *stub) Restart(ctx context.Context) error {
	if s.restart != nil {
		return s.restart()
	}
	return s.Harness.Restart(ctx)
}

func newStub() *stub {
	return &stub{Harness: fakeharness.New(map[string]runner.Reply{"PING": runner.StatusReply("PONG")})}
}

// runScenario drives one scenario the way a spec does — through the executor —
// and returns the failure, or nil when it passed.
func runScenario(t *testing.T, name string, h runner.Harness) *runner.Failure {
	t.Helper()

	spec := fmt.Sprintf("version: 1\nname: scenario\ntier: full\nfeature: FEAT-0009\nsteps:\n  - scenario: %s\n", name)
	parsed, err := runner.ParseSpec("scenario.yaml", []byte(spec))
	if err != nil {
		t.Fatalf("parsing the spec: %v", err)
	}
	executor := &runner.Executor{Factory: func(runner.HarnessOptions) (runner.Harness, error) { return h, nil }}
	return executor.Run(context.Background(), parsed).Failure
}

func requireFailure(t *testing.T, failure *runner.Failure, mentions string) {
	t.Helper()
	if failure == nil {
		t.Fatalf("the scenario passed; it should have failed over %q", mentions)
	}
	if !strings.Contains(failure.Message, mentions) {
		t.Errorf("message = %q, want it to mention %q", failure.Message, mentions)
	}
}

// health serves the admin API a test wants the scenario to see.
func health(t *testing.T, status int, contentType, body string) string {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("writing the health response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parsing the test server URL: %v", err)
	}
	return parsed.Host
}

func TestHealthEndpointReady(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantFailure string
	}{
		{name: "healthy", status: 200, contentType: "application/json", body: `{"status":"ok"}`},
		{name: "not ready", status: 503, contentType: "application/json", body: `{"status":"starting"}`, wantFailure: "returned 503"},
		{name: "wrong content type", status: 200, contentType: "text/plain", body: `{"status":"ok"}`, wantFailure: "Content-Type"},
		{name: "not ok", status: 200, contentType: "application/json", body: `{"status":"degraded"}`, wantFailure: `"status":"ok"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newStub()
			h.info = runner.ServerInfo{AdminAddr: health(t, tc.status, tc.contentType, tc.body)}

			failure := runScenario(t, "health_endpoint_ready", h)
			if tc.wantFailure == "" {
				if failure != nil {
					t.Fatalf("a healthy endpoint failed the scenario: %s", failure.Message)
				}
				return
			}
			requireFailure(t, failure, tc.wantFailure)
		})
	}
}

// rejectsEverything is a server binary that refuses every config it is given,
// naming the field at fault, which is what the scenario requires of it.
func rejectsEverything(args []string) (int, string, error) {
	path := args[len(args)-1]
	return 1, fmt.Sprintf(
		"atlascache: invalid config %s: eviction.policy must be one of lru, lfu, fifo, none; "+
			"server.client_port must be between 1 and 65535\n", path), nil
}

func TestInvalidConfigFailsFast(t *testing.T) {
	t.Run("rejected with a message naming the field", func(t *testing.T) {
		h := newStub()
		h.root = t.TempDir()
		h.binaryRun = rejectsEverything

		if failure := runScenario(t, "invalid_config_fails_fast", h); failure != nil {
			t.Fatalf("a binary that rejects bad configs failed the scenario: %s", failure.Message)
		}
	})

	t.Run("a config that is accepted fails the scenario", func(t *testing.T) {
		h := newStub()
		h.root = t.TempDir()
		h.binaryRun = func([]string) (int, string, error) { return 0, "started\n", nil }

		requireFailure(t, runScenario(t, "invalid_config_fails_fast", h), "exited 0")
	})

	t.Run("a message that does not say what to fix fails the scenario", func(t *testing.T) {
		h := newStub()
		h.root = t.TempDir()
		h.binaryRun = func([]string) (int, string, error) { return 1, "config error\n", nil }

		requireFailure(t, runScenario(t, "invalid_config_fails_fast", h), "does not mention")
	})

	t.Run("a missing config file must not fall back to defaults", func(t *testing.T) {
		h := newStub()
		h.root = t.TempDir()
		h.binaryRun = func(args []string) (int, string, error) {
			if strings.Contains(args[len(args)-1], "does-not-exist") {
				return 0, "started with defaults\n", nil
			}
			return rejectsEverything(args)
		}

		requireFailure(t, runScenario(t, "invalid_config_fails_fast", h), "config file that does not exist")
	})

	t.Run("the scratch files land in the harness's own directory", func(t *testing.T) {
		h := newStub()
		h.root = t.TempDir()
		h.binaryRun = rejectsEverything

		if failure := runScenario(t, "invalid_config_fails_fast", h); failure != nil {
			t.Fatalf("scenario: %s", failure.Message)
		}
		entries, err := os.ReadDir(h.root)
		if err != nil {
			t.Fatalf("reading the harness root: %v", err)
		}
		if len(entries) == 0 {
			t.Error("the scenario wrote its configs somewhere other than the harness's directory")
		}
		for _, entry := range entries {
			if filepath.Ext(entry.Name()) != ".yaml" {
				t.Errorf("unexpected file %s", entry.Name())
			}
		}
	})
}

func TestVersionFlagReportsBuild(t *testing.T) {
	const good = "atlascache 0.1.0\ncommit: abc1234\nbuilt:  2026-08-04T00:00:00Z\n"

	tests := []struct {
		name        string
		code        int
		output      string
		wantFailure string
	}{
		{name: "reports version, commit and build date", code: 0, output: good},
		{name: "non-zero exit", code: 1, output: good, wantFailure: "exited 1"},
		{name: "does not name itself", code: 0, output: "0.1.0\ncommit: abc\nbuilt: now\n", wantFailure: "program name"},
		{name: "no commit", code: 0, output: "atlascache 0.1.0\nbuilt: now\n", wantFailure: "commit:"},
		{name: "no build date", code: 0, output: "atlascache 0.1.0\ncommit: abc\n", wantFailure: "built:"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newStub()
			h.binaryRun = func([]string) (int, string, error) { return tc.code, tc.output, nil }

			failure := runScenario(t, "version_flag_reports_build", h)
			if tc.wantFailure == "" {
				if failure != nil {
					t.Fatalf("a well-formed --version failed the scenario: %s", failure.Message)
				}
				return
			}
			requireFailure(t, failure, tc.wantFailure)
		})
	}
}

func TestScenariosNeedingTheBinarySayWhenTheyCannotRun(t *testing.T) {
	for _, name := range []string{"invalid_config_fails_fast", "version_flag_reports_build"} {
		t.Run(name, func(t *testing.T) {
			h := fakeharness.New(map[string]runner.Reply{"PING": runner.StatusReply("PONG")})
			requireFailure(t, runScenario(t, name, h), "needs a harness that owns the server binary")
		})
	}
}

func TestRestartPreservesDataDir(t *testing.T) {
	t.Run("same directory, new process", func(t *testing.T) {
		h := newStub()
		h.info = runner.ServerInfo{DataDir: t.TempDir(), PID: 100}
		h.restart = func() error {
			h.info.PID = 101
			return nil
		}

		if failure := runScenario(t, "restart_preserves_data_dir", h); failure != nil {
			t.Fatalf("a real restart failed the scenario: %s", failure.Message)
		}
	})

	t.Run("a restart that restarted nothing fails", func(t *testing.T) {
		h := newStub()
		h.info = runner.ServerInfo{DataDir: t.TempDir(), PID: 100}
		h.restart = func() error { return nil }

		requireFailure(t, runScenario(t, "restart_preserves_data_dir", h), "nothing was restarted")
	})

	t.Run("a restart that discarded the directory fails", func(t *testing.T) {
		dir := t.TempDir()
		h := newStub()
		h.info = runner.ServerInfo{DataDir: dir, PID: 100}
		h.restart = func() error {
			h.info.PID = 101
			return os.RemoveAll(dir)
		}

		requireFailure(t, runScenario(t, "restart_preserves_data_dir", h), "did not survive the restart")
	})

	t.Run("a harness with no data directory fails", func(t *testing.T) {
		h := newStub()
		requireFailure(t, runScenario(t, "restart_preserves_data_dir", h), "no data directory")
	})
}
