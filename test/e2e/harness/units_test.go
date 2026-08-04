package harness

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/b3vet/atlascache/test/e2e/runner"
)

// TestResolveBinaryRefusesWhatCannotBeLaunched guards the pre-flight check that
// turns a forgotten `make build` into one clear message instead of a launch
// failure per spec. Every case below exists on disk or is nearly right, so a
// resolver that only asked "does this path exist?" would accept them all and
// each spec would then fail separately, deep inside Start.
func TestResolveBinaryRefusesWhatCannotBeLaunched(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	notExecutable := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "no binary at all", path: "", want: "--binary"},
		{name: "a path that does not exist", path: filepath.Join(dir, "absent"), want: "make build"},
		{name: "a directory", path: dir, want: "not an executable file"},
		{name: "a file without the executable bit", path: notExecutable, want: "not an executable file"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resolved, err := resolveBinary(tc.path)
			if err == nil {
				t.Fatalf("resolveBinary(%q) = %q, want an error", tc.path, resolved)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestResolveBinaryReturnsAnAbsolutePath. The server is launched with cmd.Dir
// set to the harness's own temp directory, so a --binary kept relative would be
// resolved against that directory and never found.
func TestResolveBinaryReturnsAnAbsolutePath(t *testing.T) {
	t.Parallel()

	absolute := standIn(t, "exit 0\n")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	relative, err := filepath.Rel(cwd, absolute)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}

	resolved, err := resolveBinary(relative)
	if err != nil {
		t.Fatalf("resolveBinary(%q): %v", relative, err)
	}
	if !filepath.IsAbs(resolved) {
		t.Errorf("resolveBinary(%q) = %q, which is still relative", relative, resolved)
	}
	if resolved != absolute {
		t.Errorf("resolveBinary(%q) = %q, want %q", relative, resolved, absolute)
	}
}

// TestNewRefusesAHarnessThatCannotIsolateItsSpec. A harness whose private
// directory could not be created has nothing to isolate the spec with, and
// carrying on would put two parallel specs in the same place.
func TestNewRefusesAHarnessThatCannotIsolateItsSpec(t *testing.T) {
	t.Parallel()

	binary := standIn(t, "exit 0\n")

	t.Run("no spec name", func(t *testing.T) {
		t.Parallel()
		// The spec name is the harness's directory name and the node id, so a
		// nameless harness would collide with every other nameless one.
		h, err := New(runner.HarnessOptions{Binary: binary}, Settings{})
		if err == nil {
			_ = h.Close()
			t.Fatal("New accepted a harness with no spec name")
		}
		if !strings.Contains(err.Error(), "spec name") {
			t.Errorf("error = %v, want it to name the missing field", err)
		}
	})

	t.Run("a base directory that does not exist", func(t *testing.T) {
		t.Parallel()
		h, err := New(runner.HarnessOptions{SpecName: "isolated", Binary: binary},
			Settings{BaseDir: filepath.Join(t.TempDir(), "absent")})
		if err == nil {
			_ = h.Close()
			t.Fatal("New accepted a base directory that does not exist")
		}
		if !strings.Contains(err.Error(), "temp directory") {
			t.Errorf("error = %v, want it to say which directory could not be made", err)
		}
	})
}

// TestNewSanitizesTheSpecNameIntoADirectoryName. Spec names come from files a
// contributor writes; one containing a path separator would put the harness's
// directory somewhere other than the base directory — outside it, in the case
// of "..", where cleanup would then remove the wrong tree.
func TestNewSanitizesTheSpecNameIntoADirectoryName(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	h, err := New(
		runner.HarnessOptions{SpecName: "../evil/spec name:1", Binary: standIn(t, "exit 0\n")},
		Settings{BaseDir: base},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	if parent := filepath.Dir(h.Root()); parent != base {
		t.Errorf("root %s sits in %s, not in the base directory %s", h.Root(), parent, base)
	}
	name := filepath.Base(h.Root())
	for _, forbidden := range []string{"..", string(filepath.Separator), ":", " "} {
		if strings.Contains(name, forbidden) {
			t.Errorf("directory name %q still contains %q", name, forbidden)
		}
	}
	if !strings.Contains(name, "evil") {
		t.Errorf("directory name %q kept nothing of the spec name; it is unreadable in a temp listing", name)
	}
}

// TestFactoryHandsOutAnIsolatedHarnessPerSpec. The runner calls the factory once
// per spec and runs specs in parallel, so a factory that shared anything between
// two harnesses would let one spec's data and ports leak into another's.
func TestFactoryHandsOutAnIsolatedHarnessPerSpec(t *testing.T) {
	t.Parallel()

	binary := standIn(t, "exit 0\n")
	base := t.TempDir()
	factory := Factory(Settings{BaseDir: base})

	seen := map[string]bool{}
	for _, name := range []string{"spec-one", "spec-two"} {
		built, err := factory(runner.HarnessOptions{SpecName: name, Binary: binary})
		if err != nil {
			t.Fatalf("factory(%s): %v", name, err)
		}
		t.Cleanup(func() { _ = built.Close() })

		process, ok := built.(*Process)
		if !ok {
			t.Fatalf("factory returned %T, want *Process", built)
		}
		if got := process.Binary(); got != binary {
			t.Errorf("Binary() = %q, want the binary the factory was given, %q", got, binary)
		}
		if filepath.Dir(process.Root()) != base {
			t.Errorf("root %s ignores the factory's base directory %s", process.Root(), base)
		}
		for _, owned := range []string{process.Root(), process.Info().DataDir} {
			if seen[owned] {
				t.Errorf("two specs were handed the same directory %s", owned)
			}
			seen[owned] = true
		}
	}
}

// TestDescribeExitNamesHowTheServerDied. A crash-recovery spec reads this line
// to tell "the server chose to exit 1" from "the kernel killed it". Rendering a
// signaled death through ExitCode would print "exit status -1", which reads as
// a broken harness rather than as a server that was killed.
func TestDescribeExitNamesHowTheServerDied(t *testing.T) {
	t.Parallel()

	t.Run("a clean exit", func(t *testing.T) {
		t.Parallel()
		if got := describeExit(nil); got != "exit status 0" {
			t.Errorf("describeExit(nil) = %q, want exit status 0", got)
		}
		if err := exitError(nil); err != nil {
			t.Errorf("exitError(nil) = %v, want nil; a clean exit is not a failure", err)
		}
	})

	t.Run("a non-zero exit", func(t *testing.T) {
		t.Parallel()
		waitErr := runToFailure(t, "exit 3\n")
		if got := describeExit(waitErr); got != "exit status 3" {
			t.Errorf("describeExit = %q, want exit status 3", got)
		}
		err := exitError(waitErr)
		if err == nil {
			t.Fatal("exitError treated a non-zero exit as clean")
		}
		if got := err.Error(); got != "exit status 3" {
			t.Errorf("exitError = %q, want exit status 3", got)
		}
	})

	t.Run("killed by a signal", func(t *testing.T) {
		t.Parallel()
		waitErr := killToFailure(t)
		got := describeExit(waitErr)
		if !strings.Contains(got, "killed by") {
			t.Errorf("describeExit = %q, want it to name the signal", got)
		}
		if strings.Contains(got, "exit status") {
			t.Errorf("describeExit = %q; a signaled death has no exit status to report", got)
		}
	})

	t.Run("a failure that is not an exit at all", func(t *testing.T) {
		t.Parallel()
		// os/exec reports a pipe that could not be drained this way: the process
		// may well have exited cleanly, so the cause must be passed through
		// rather than translated into an exit status.
		waitErr := errors.New("read |0: file already closed")
		if got := describeExit(waitErr); got != waitErr.Error() {
			t.Errorf("describeExit = %q, want the cause verbatim", got)
		}
		if err := exitError(waitErr); !errors.Is(err, waitErr) {
			t.Errorf("exitError = %v, want the original error so callers can inspect it", err)
		}
	})
}

// TestClosesConnectionOnlyForQuit. Send drops its own side of the connection
// after a command the server is expected to hang up on. Dropping it after the
// wrong command would silently reconnect mid-spec and hide a server that closed
// a connection it should have kept; not dropping it after QUIT costs every
// later Send a round trip to discover the socket is gone.
func TestClosesConnectionOnlyForQuit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		cmd  string
		want bool
	}{
		{cmd: "QUIT", want: true},
		{cmd: "quit", want: true},
		{cmd: "  QUIT  ", want: true},
		{cmd: "PING", want: false},
		{cmd: "SET quit 1", want: false},
		{cmd: "QUITE", want: false},
		// An unparseable command is not a QUIT, and deciding it was would drop a
		// connection over a typo in a spec.
		{cmd: "", want: false},
		{cmd: `SET k "never closed`, want: false},
	}

	for _, tc := range tests {
		if got := closesConnection(tc.cmd); got != tc.want {
			t.Errorf("closesConnection(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

// TestSettingsFallBackToTheDocumentedDefaults. The zero value of Settings is
// documented as the default for every field, and an explicit value must be
// honored: a graceful window that silently reverted to ten seconds would make a
// shutdown-budget spec unable to fail.
func TestSettingsFallBackToTheDocumentedDefaults(t *testing.T) {
	t.Parallel()

	var zero Settings
	if got := zero.readyTimeout(); got != DefaultReadyTimeout {
		t.Errorf("zero readyTimeout = %s, want %s", got, DefaultReadyTimeout)
	}
	if got := zero.gracefulWindow(); got != DefaultGracefulWindow {
		t.Errorf("zero gracefulWindow = %s, want %s", got, DefaultGracefulWindow)
	}
	if got := zero.startAttempts(); got != DefaultStartAttempts {
		t.Errorf("zero startAttempts = %d, want %d", got, DefaultStartAttempts)
	}

	explicit := Settings{ReadyTimeout: time.Second, GracefulWindow: 2 * time.Second, StartAttempts: 3}
	if got := explicit.readyTimeout(); got != time.Second {
		t.Errorf("readyTimeout = %s, want the configured 1s", got)
	}
	if got := explicit.gracefulWindow(); got != 2*time.Second {
		t.Errorf("gracefulWindow = %s, want the configured 2s", got)
	}
	if got := explicit.startAttempts(); got != 3 {
		t.Errorf("startAttempts = %d, want the configured 3", got)
	}

	// A negative value is a caller's mistake, and running with a negative
	// timeout would fail every spec instantly rather than reporting the mistake.
	nonsense := Settings{ReadyTimeout: -time.Second, GracefulWindow: -time.Second, StartAttempts: -1}
	if got := nonsense.readyTimeout(); got != DefaultReadyTimeout {
		t.Errorf("negative readyTimeout = %s, want the default %s", got, DefaultReadyTimeout)
	}
	if got := nonsense.gracefulWindow(); got != DefaultGracefulWindow {
		t.Errorf("negative gracefulWindow = %s, want the default %s", got, DefaultGracefulWindow)
	}
	if got := nonsense.startAttempts(); got != DefaultStartAttempts {
		t.Errorf("negative startAttempts = %d, want the default %d", got, DefaultStartAttempts)
	}
}

// TestChildEnvStripsTheServersOwnOverrides. The server reads ATLAS_-prefixed
// variables as config overrides, so a developer with ATLAS_EVICTION_POLICY
// exported would silently be running a different server from CI — and the spec
// that was supposed to pin the policy would be asserting nothing. This test
// cannot use t.Parallel: it sets process-wide environment variables.
func TestChildEnvStripsTheServersOwnOverrides(t *testing.T) {
	t.Setenv("ATLAS_EVICTION_POLICY", "telepathy")
	t.Setenv("ATLASCACHE_NOT_A_PREFIX_MATCH", "kept")
	t.Setenv("PATH_LIKE_VARIABLE_THE_SERVER_NEEDS", "kept")

	var sawOverride, sawUnrelated bool
	for _, entry := range childEnv() {
		if strings.HasPrefix(entry, "ATLAS_") {
			sawOverride = true
		}
		if entry == "PATH_LIKE_VARIABLE_THE_SERVER_NEEDS=kept" {
			sawUnrelated = true
		}
	}
	if sawOverride {
		t.Error("an ATLAS_ variable reached the server; a developer's shell must not reconfigure a spec")
	}
	if !sawUnrelated {
		t.Error("childEnv dropped a variable that has nothing to do with the server's config")
	}
}

// runToFailure runs a stand-in to completion and returns what Wait reported,
// which is the same *exec.ExitError the harness gets from a server that exited.
func runToFailure(t *testing.T, script string) error {
	t.Helper()
	err := exec.CommandContext(t.Context(), standIn(t, script)).Run()
	if err == nil {
		t.Fatal("the stand-in exited cleanly; this case needs a failed exit")
	}
	return err
}

// killToFailure launches a stand-in and kills it, for the wait error a SIGKILLed
// server produces.
func killToFailure(t *testing.T) error {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), standIn(t, "sleep 60\n"))
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the stand-in: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing the stand-in: %v", err)
	}
	err := cmd.Wait()
	if err == nil {
		t.Fatal("a killed process reported a clean exit")
	}
	return err
}
