package harness

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/b3vet/atlascache/test/e2e/runner"
)

// buildDir holds the server binary built for this test run, and is removed by
// TestMain. These tests hold the harness to leaving nothing behind, so they had
// better not leave anything behind themselves.
var buildDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "atlas-e2e-harness-test-")
	if err != nil {
		panic("creating the build directory: " + err.Error())
	}
	buildDir = dir

	code := m.Run()

	if err := os.RemoveAll(buildDir); err != nil {
		panic("removing the build directory: " + err.Error())
	}
	os.Exit(code)
}

// serverBinary builds the server under test once per test binary. Building it
// here rather than depending on `make build` keeps `go test ./...` self
// contained: a harness test that silently skipped when the binary was missing
// would be a harness test that never ran on CI.
var serverBinary = sync.OnceValues(func() (string, error) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		return "", err
	}
	out := filepath.Join(buildDir, "atlascache")

	cmd := exec.Command("go", "build", "-o", out, "./cmd/atlascache")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", errors.New("building the server: " + err.Error() + "\n" + string(output))
	}
	return out, nil
})

// standIn writes an executable shell script and returns its path, for the
// failure modes a working server cannot be made to produce on demand.
func standIn(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stand-in")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatalf("writing the stand-in: %v", err)
	}
	return path
}

func newHarness(t *testing.T, config map[string]any) *Process {
	t.Helper()

	binary, err := serverBinary()
	if err != nil {
		t.Fatalf("%v", err)
	}
	h, err := New(runner.HarnessOptions{SpecName: t.Name(), Binary: binary, Config: config}, Settings{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return h
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func mustStart(t *testing.T, h *Process) {
	t.Helper()
	if err := h.Start(testContext(t)); err != nil {
		t.Fatalf("Start: %v\n%s", err, h.Logs())
	}
}

func TestStartServesAndStopsCleanly(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	ctx := testContext(t)
	mustStart(t, h)

	info := h.Info()
	if info.PID == 0 {
		t.Error("Info reports no pid for a running server")
	}
	if info.ClientAddr == "" || info.AdminAddr == "" {
		t.Errorf("Info = %+v, want both addresses", info)
	}
	if _, err := os.Stat(info.DataDir); err != nil {
		t.Errorf("the data directory is missing: %v", err)
	}

	reply, err := h.Send(ctx, "PING")
	if err != nil {
		t.Fatalf("Send PING: %v\n%s", err, h.Logs())
	}
	if got := reply.String(); got != "PONG" {
		t.Errorf("PING = %q, want PONG", got)
	}

	if err := h.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v\n%s", err, h.Logs())
	}
	if pid := h.Info().PID; pid != 0 {
		t.Errorf("Info reports pid %d after Stop", pid)
	}
}

func TestStartIsReadyNotAsleep(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	mustStart(t, h)

	// Readiness is a positive signal, so the admin endpoint must already answer
	// by the time Start returns — no settling time, no retry loop in the caller.
	resp, err := http.Get("http://" + h.Info().AdminAddr + "/health") //nolint:noctx // a one-shot probe in a test
	if err != nil {
		t.Fatalf("GET /health straight after Start: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /health = %d, want 200", resp.StatusCode)
	}
}

func TestStopIsIdempotentAndKillIsToo(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	ctx := testContext(t)
	mustStart(t, h)

	if err := h.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := h.Stop(ctx); err != nil {
		t.Errorf("stopping a stopped server must be a no-op, got %v", err)
	}
	if err := h.Kill(); err != nil {
		t.Errorf("killing a stopped server must be a no-op, got %v", err)
	}
}

func TestKillLeavesNoProcessAndSendReconnects(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	ctx := testContext(t)
	mustStart(t, h)

	if _, err := h.Send(ctx, "PING"); err != nil {
		t.Fatalf("Send before kill: %v", err)
	}
	pid := h.Info().PID

	if err := h.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if alive(pid) {
		t.Errorf("pid %d survived SIGKILL", pid)
	}
	if _, err := h.Send(ctx, "PING"); err == nil {
		t.Error("Send against a killed server reported success")
	}

	// A killed server is restartable against the same data directory, which is
	// what the crash-recovery specs in P5 will do.
	if err := h.Start(ctx); err != nil {
		t.Fatalf("Start after Kill: %v\n%s", err, h.Logs())
	}
	reply, err := h.Send(ctx, "PING")
	if err != nil {
		t.Fatalf("Send after restart: %v\n%s", err, h.Logs())
	}
	if got := reply.String(); got != "PONG" {
		t.Errorf("PING after restart = %q, want PONG", got)
	}
}

func TestRestartPreservesTheDataDirectory(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	ctx := testContext(t)
	mustStart(t, h)

	before := h.Info()
	marker := filepath.Join(before.DataDir, "marker")
	if err := os.WriteFile(marker, []byte("survives"), 0o600); err != nil {
		t.Fatalf("writing the marker: %v", err)
	}

	if err := h.Restart(ctx); err != nil {
		t.Fatalf("Restart: %v\n%s", err, h.Logs())
	}

	after := h.Info()
	if after.DataDir != before.DataDir {
		t.Errorf("data dir = %s after restart, want %s", after.DataDir, before.DataDir)
	}
	content, err := os.ReadFile(marker)
	if err != nil || string(content) != "survives" {
		t.Errorf("the marker did not survive the restart: %q, %v", content, err)
	}
	if after.PID == before.PID {
		t.Errorf("pid %d is unchanged, so nothing was actually restarted", after.PID)
	}
	if _, err := h.Send(ctx, "PING"); err != nil {
		t.Fatalf("PING after restart: %v\n%s", err, h.Logs())
	}
}

func TestSendReconnectsAfterQuit(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	ctx := testContext(t)
	mustStart(t, h)

	if _, err := h.Send(ctx, "QUIT"); err != nil {
		t.Fatalf("QUIT: %v", err)
	}
	reply, err := h.Send(ctx, "PING")
	if err != nil {
		t.Fatalf("PING after QUIT closed the connection: %v\n%s", err, h.Logs())
	}
	if got := reply.String(); got != "PONG" {
		t.Errorf("PING after QUIT = %q, want PONG", got)
	}
}

func TestStartFailsFastOnAnInvalidConfig(t *testing.T) {
	t.Parallel()
	// eviction.policy is validated at startup, so this config cannot produce a
	// running server.
	h := newHarness(t, map[string]any{"eviction": map[string]any{"policy": "telepathy"}})

	started := time.Now()
	err := h.Start(testContext(t))
	if err == nil {
		t.Fatal("Start reported success with an invalid config")
	}
	if elapsed := time.Since(started); elapsed > DefaultReadyTimeout {
		t.Errorf("Start took %s to notice a config error; it must not wait out the readiness timeout", elapsed)
	}
	if !strings.Contains(err.Error(), "exited during startup") {
		t.Errorf("error = %v, it should say the server exited", err)
	}
	if !strings.Contains(h.Logs(), "eviction.policy") {
		t.Errorf("the captured log does not explain the failure:\n%s", h.Logs())
	}
}

func TestReadinessTimeoutKillsTheServerAndReportsIt(t *testing.T) {
	t.Parallel()
	// A stand-in that starts and then does nothing: it never binds, so it never
	// becomes ready. Racing a real server against a short timeout would make
	// this test flaky, and a flaky test about flakiness helps nobody.
	binary := standIn(t, "exec sleep 120\n")

	h, err := New(runner.HarnessOptions{SpecName: t.Name(), Binary: binary},
		Settings{ReadyTimeout: 250 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = h.Close() }()

	started := time.Now()
	err = h.Start(testContext(t))
	if err == nil {
		t.Fatal("Start reported success against a server that never answers /health")
	}
	if !strings.Contains(err.Error(), "/health") {
		t.Errorf("error = %v, it should name the probe that timed out", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("Start took %s to give up on a 250ms readiness window", elapsed)
	}
	if pid := h.Info().PID; pid != 0 {
		t.Errorf("a failed Start left pid %d behind", pid)
	}
}

func TestALostPortRaceIsRetriedNotReported(t *testing.T) {
	t.Parallel()
	// A server whose port was taken between the harness probing it and the
	// server binding it. The race is rare and cannot be provoked reliably, so
	// the stand-in produces the symptom: the message the kernel gives, and a
	// non-zero exit.
	binary := standIn(t, "echo 'listen tcp 127.0.0.1:1: bind: address already in use' >&2\nexit 1\n")

	h, err := New(runner.HarnessOptions{SpecName: t.Name(), Binary: binary}, Settings{StartAttempts: 3})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = h.Close() }()

	err = h.Start(testContext(t))
	if err == nil {
		t.Fatal("Start reported success for a server that never bound its port")
	}
	if !strings.Contains(err.Error(), "taken") {
		t.Errorf("error = %v, it should say the port was taken", err)
	}
	if launches := strings.Count(h.Logs(), "launching "); launches != 3 {
		t.Errorf("the server was launched %d times, want 3 attempts with fresh ports:\n%s", launches, h.Logs())
	}

	// A launch that failed for any other reason is reported at once rather than
	// retried, so a broken server does not cost five launches to find out.
	broken := standIn(t, "echo 'atlascache: invalid config' >&2\nexit 1\n")
	other, err := New(runner.HarnessOptions{SpecName: t.Name() + "-other", Binary: broken}, Settings{StartAttempts: 3})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = other.Close() }()

	if err := other.Start(testContext(t)); err == nil {
		t.Fatal("Start reported success for a server that exited")
	}
	if launches := strings.Count(other.Logs(), "launching "); launches != 1 {
		t.Errorf("a server that exited for its own reasons was launched %d times, want 1:\n%s", launches, other.Logs())
	}
}

func TestCloseIsIdempotentAndRemovesEverything(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	mustStart(t, h)

	root, pid := h.Root(), h.Info().PID
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Errorf("Close is not idempotent: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("temp directory %s survived Close (%v)", root, err)
	}
	if alive(pid) {
		t.Errorf("pid %d survived Close", pid)
	}
}

func TestSpecConfigIsMergedButPortsAreNot(t *testing.T) {
	t.Parallel()
	h := newHarness(t, map[string]any{
		"eviction": map[string]any{"policy": "lfu"},
		"server":   map[string]any{"client_port": 6379},
		"node":     map[string]any{"data_dir": "/var/lib/atlascache"},
	})
	mustStart(t, h)

	rendered, err := os.ReadFile(filepath.Join(h.Root(), "config.yaml"))
	if err != nil {
		t.Fatalf("reading the rendered config: %v", err)
	}
	config := string(rendered)
	if !strings.Contains(config, "policy: lfu") {
		t.Errorf("the spec's override was dropped:\n%s", config)
	}
	if strings.Contains(config, "client_port: 6379") {
		t.Errorf("a spec must not be able to claim a fixed port:\n%s", config)
	}
	if strings.Contains(config, "/var/lib/atlascache") {
		t.Errorf("a spec must not be able to redirect the data directory:\n%s", config)
	}
	if !strings.Contains(config, h.Info().DataDir) {
		t.Errorf("the data directory is not the harness's:\n%s", config)
	}
}

func TestRunBinaryReportsExitCodeAndOutput(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	code, output, err := h.RunBinary(testContext(t), "--version")
	if err != nil {
		t.Fatalf("RunBinary --version: %v", err)
	}
	if code != 0 {
		t.Errorf("--version exited %d, want 0: %s", code, output)
	}
	if !strings.Contains(output, "atlascache") {
		t.Errorf("--version printed %q", output)
	}
}

func TestParallelHarnessesDoNotCollide(t *testing.T) {
	t.Parallel()

	const count = 8
	harnesses := make([]*Process, count)
	for i := range harnesses {
		harnesses[i] = newHarness(t, nil)
	}

	var wg sync.WaitGroup
	errs := make([]error, count)
	for i, h := range harnesses {
		wg.Add(1)
		go func(i int, h *Process) {
			defer wg.Done()
			if err := h.Start(testContext(t)); err != nil {
				errs[i] = err
				return
			}
			if _, err := h.Send(testContext(t), "PING"); err != nil {
				errs[i] = err
			}
		}(i, h)
	}
	wg.Wait()

	seen := map[string]int{}
	for i, h := range harnesses {
		if errs[i] != nil {
			t.Errorf("harness %d: %v\n%s", i, errs[i], h.Logs())
			continue
		}
		info := h.Info()
		for _, addr := range []string{info.ClientAddr, info.AdminAddr} {
			if first, taken := seen[addr]; taken {
				t.Errorf("harness %d and %d were both given %s", first, i, addr)
			}
			seen[addr] = i
		}
	}
}

// alive reports whether a process still exists. Signal 0 checks for existence
// without delivering anything.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(nil) == nil
}
