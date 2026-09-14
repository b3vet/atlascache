package scenarios_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/b3vet/atlascache/test/e2e/runner"
)

// The CLI scenarios are driven here against the real atlasctl and a model
// server, and then against atlasctl wrapped in a script that breaks one of the
// properties the scenarios exist to protect.
//
// The wrappers are the point. A CLI is a contract made of bytes and exit codes,
// and each wrapper breaks exactly one clause of it: a newline appended to a
// value, every failure collapsed onto exit 1, an error printed as prose where
// JSON was promised, a warning about a leaked secret silenced. Each is
// something a well-meaning change to the CLI could do tomorrow, and each must
// turn a spec red.

// ctlBuild builds atlasctl once for the whole test binary. The CLI lives in the
// root module, so this is the one place these tests reach across the module
// boundary — and they reach for the real binary rather than a stand-in, because
// what is under test is a scenario's ability to judge one.
var ctlBuild struct {
	once sync.Once
	dir  string
	err  error
}

func builtCtl(t *testing.T) string {
	t.Helper()

	ctlBuild.once.Do(func() {
		dir, err := os.MkdirTemp("", "atlasctl-scenarios-")
		if err != nil {
			ctlBuild.err = err
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		build := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(dir, "atlasctl"), "./cmd/atlasctl")
		build.Dir = filepath.Join("..", "..", "..")
		if output, buildErr := build.CombinedOutput(); buildErr != nil {
			ctlBuild.err = fmt.Errorf("building atlasctl: %w\n%s", buildErr, output)
			_ = os.RemoveAll(dir)
			return
		}
		ctlBuild.dir = dir
	})

	if ctlBuild.err != nil {
		t.Fatalf("%v", ctlBuild.err)
	}
	return ctlBuild.dir
}

func TestMain(m *testing.M) {
	code := m.Run()
	if ctlBuild.dir != "" {
		_ = os.RemoveAll(ctlBuild.dir)
	}
	os.Exit(code)
}

// cliHarness is a model server with a CLI beside it.
//
// binary is where the harness says the server binary lives, because that is
// where the scenarios look for atlasctl: next to it, which is where the
// Makefile puts it.
func cliHarness(t *testing.T, defects sdkDefects) *sdkHarness {
	t.Helper()

	harness := newSDKHarness(t, defects)
	harness.binary = filepath.Join(builtCtl(t), "atlascache")
	return harness
}

// brokenCLI wraps the real atlasctl in a script that breaks one property, and
// returns a harness pointing at the wrapper.
func brokenCLI(t *testing.T, defects sdkDefects, script string) *sdkHarness {
	t.Helper()

	dir := t.TempDir()
	wrapper := "#!/bin/sh\nREAL=" + filepath.Join(builtCtl(t), "atlasctl") + "\n" + script
	if err := os.WriteFile(filepath.Join(dir, "atlasctl"), []byte(wrapper), 0o700); err != nil {
		t.Fatalf("writing the wrapper: %v", err)
	}

	harness := newSDKHarness(t, defects)
	harness.binary = filepath.Join(dir, "atlascache")
	return harness
}

// cliCatches runs a scenario against a harness whose CLI is broken.
func cliCatches(t *testing.T, scenario string, harness runner.Harness, wants string) {
	t.Helper()

	failure := runScenario(t, scenario, harness)
	if failure == nil {
		t.Fatalf("%s passed against a broken CLI", scenario)
	}
	if !strings.Contains(failure.Message, wants) {
		t.Errorf("%s reported %q, which does not mention %q", scenario, failure.Message, wants)
	}
}

func TestCLIScenariosAreRegistered(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"cli_commands_work_against_a_live_server",
		"cli_values_round_trip_through_stdout",
		"cli_json_is_stable_and_parsable",
		"cli_auth_prefers_the_environment",
		"cli_exit_codes_distinguish_outcomes",
	} {
		if _, ok := runner.LookupScenario(name); !ok {
			t.Errorf("%s did not register itself", name)
		}
	}
}

func TestCLIScenariosNeedTheBinaries(t *testing.T) {
	t.Parallel()

	// A harness that does not own the binaries cannot run these, and must say
	// so rather than failing somewhere further in with something obscure.
	failure := runScenario(t, "cli_commands_work_against_a_live_server", newSDKHarness(t, sdkDefects{}))
	if failure == nil {
		t.Fatal("the scenario passed without an atlasctl to run")
	}
	if !strings.Contains(failure.Message, "atlasctl") {
		t.Errorf("message = %q, want it to name the binary it could not find", failure.Message)
	}
}

func TestCLICommandsWorkAgainstALiveServer(t *testing.T) {
	t.Parallel()

	const scenario = "cli_commands_work_against_a_live_server"

	t.Run("the real CLI over TLS passes", func(t *testing.T) {
		t.Parallel()
		// TLS as the spec runs it, so --tls-ca is exercised by every command
		// rather than by a test of its own.
		if failure := runScenario(t, scenario, cliHarness(t, sdkDefects{serveTLS: true})); failure != nil {
			t.Fatalf("%s failed against the real CLI: %s", scenario, failure.Message)
		}
	})

	t.Run("a server answering with the wrong value is caught", func(t *testing.T) {
		t.Parallel()
		if failure := runScenario(t, scenario, cliHarness(t, sdkDefects{wrongValues: true})); failure == nil {
			t.Fatal("the scenario passed against a server that answers with the wrong value")
		}
	})
}

func TestCLIValuesRoundTripThroughStdout(t *testing.T) {
	t.Parallel()

	const scenario = "cli_values_round_trip_through_stdout"

	t.Run("the real CLI passes", func(t *testing.T) {
		t.Parallel()
		if failure := runScenario(t, scenario, cliHarness(t, sdkDefects{})); failure != nil {
			t.Fatalf("%s failed against the real CLI: %s", scenario, failure.Message)
		}
	})

	t.Run("a trailing newline on a redirected value is caught", func(t *testing.T) {
		t.Parallel()
		// The most tempting change there is: one newline, so the output looks
		// right in a terminal. It corrupts every binary value that is ever
		// redirected to a file, and nothing else in the suite would notice.
		cliCatches(t, scenario,
			brokenCLI(t, sdkDefects{}, `"$REAL" "$@"; code=$?; printf '\n'; exit $code`),
			"binary value came back")
	})

	t.Run("a value escaped when it is not a terminal is caught", func(t *testing.T) {
		t.Parallel()
		// Escaping is right on a terminal and wrong everywhere else. A CLI that
		// did it unconditionally would look fine to every human who tried it.
		cliCatches(t, scenario,
			brokenCLI(t, sdkDefects{}, `out=$("$REAL" "$@" | od -c); code=$?; printf '%s' "$out"; exit $code`),
			"binary value came back")
	})
}

func TestCLIJSONIsStableAndParsable(t *testing.T) {
	t.Parallel()

	const scenario = "cli_json_is_stable_and_parsable"

	t.Run("the real CLI passes", func(t *testing.T) {
		t.Parallel()
		if failure := runScenario(t, scenario, cliHarness(t, sdkDefects{})); failure != nil {
			t.Fatalf("%s failed against the real CLI: %s", scenario, failure.Message)
		}
	})

	t.Run("a bare error string in place of JSON is caught", func(t *testing.T) {
		t.Parallel()
		// --json that stops being JSON exactly when something goes wrong breaks
		// every script in the case the script was written to handle.
		cliCatches(t, scenario, brokenCLI(t, sdkDefects{}, `
tmp=$(mktemp)
"$REAL" "$@" > "$tmp" 2>/dev/null
code=$?
if [ $code -ne 0 ]; then
	rm -f "$tmp"
	echo "error: it did not work"
	exit $code
fi
cat "$tmp"
rm -f "$tmp"
exit 0`), "not JSON")
	})
}

func TestCLIAuthPrefersTheEnvironment(t *testing.T) {
	t.Parallel()

	const scenario = "cli_auth_prefers_the_environment"

	t.Run("the real CLI passes", func(t *testing.T) {
		t.Parallel()
		if failure := runScenario(t, scenario, cliHarness(t, sdkDefects{serveTLS: true, token: modelToken})); failure != nil {
			t.Fatalf("%s failed against the real CLI: %s", scenario, failure.Message)
		}
	})

	t.Run("a silent --auth is caught", func(t *testing.T) {
		t.Parallel()
		// The warning is the only thing standing between a token on a command
		// line and a token in every other user's `ps` output.
		cliCatches(t, scenario,
			brokenCLI(t, sdkDefects{serveTLS: true, token: modelToken}, `exec "$REAL" "$@" 2>/dev/null`),
			"did not warn")
	})

	t.Run("a server that lets any token through is caught", func(t *testing.T) {
		t.Parallel()
		if failure := runScenario(t, scenario,
			cliHarness(t, sdkDefects{serveTLS: true, token: modelToken, acceptsAnyToken: true})); failure == nil {
			t.Fatal("the scenario passed against a server that accepts any token")
		}
	})
}

func TestCLIExitCodesDistinguishOutcomes(t *testing.T) {
	t.Parallel()

	const scenario = "cli_exit_codes_distinguish_outcomes"

	t.Run("the real CLI passes", func(t *testing.T) {
		t.Parallel()
		if failure := runScenario(t, scenario, cliHarness(t, sdkDefects{})); failure != nil {
			t.Fatalf("%s failed against the real CLI: %s", scenario, failure.Message)
		}
	})

	t.Run("every failure collapsed onto one code is caught", func(t *testing.T) {
		t.Parallel()
		// The common CLI failure: "it did not work" for a missing key and for
		// an unreachable server alike, so a script cannot tell whether to retry
		// or to give up.
		cliCatches(t, scenario,
			brokenCLI(t, sdkDefects{}, `"$REAL" "$@"; code=$?; [ $code -eq 3 ] && exit 1; exit $code`),
			"exited 1, want 3")
	})

	t.Run("a usage mistake reported as success is caught", func(t *testing.T) {
		t.Parallel()
		cliCatches(t, scenario,
			brokenCLI(t, sdkDefects{}, `"$REAL" "$@" >/dev/null 2>&1; exit 0`),
			"want")
	})
}
