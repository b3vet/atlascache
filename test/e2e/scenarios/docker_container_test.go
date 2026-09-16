package scenarios

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/b3vet/atlascache/test/e2e/runner"
	"github.com/b3vet/atlascache/test/e2e/runner/fakeharness"
)

// The half of the container work that needs a Docker daemon.
//
// These skip without Docker, for the same reason the specs they support do:
// the suite has to stay runnable, and green, on a machine that has none. Where
// Docker is present — CI, and any machine that can run the container specs at
// all — they run.
//
// They drive the scenarios through the real executor rather than calling them
// directly. A scenario is handed a runner.Ctx carrying the run's context, and
// the executor is what builds one; calling a scenario with a hand-made Ctx
// would test it under conditions it never actually runs in.

func requireDocker(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)

	if ok, reason := dockerAvailable(ctx); !ok {
		t.Skipf("no Docker: %s", reason)
	}
	return ctx
}

// runScenario executes one scenario the way a spec does, and returns the
// failure the runner would have reported.
func runScenario(t *testing.T, name string) *runner.Failure {
	t.Helper()

	// The runner requires a spec's name to match its file name, so both are
	// derived from the scenario's.
	specName := "unit-" + strings.ReplaceAll(name, "_", "-")
	source := fmt.Sprintf("version: 1\nname: %s\ntier: full\nfeature: FEAT-0032\nsteps:\n  - scenario: %s\n",
		specName, name)

	spec, err := runner.ParseSpec(specName+".yaml", []byte(source))
	if err != nil {
		t.Fatalf("parsing the spec for %s: %v", name, err)
	}

	executor := &runner.Executor{
		// A fake harness with no scripted replies at all. The container
		// scenarios drive containers of their own and never send a command
		// through the harness -- and a fake that answers nothing is what turns
		// "never" from a claim into a check.
		Factory: func(runner.HarnessOptions) (runner.Harness, error) {
			return fakeharness.New(nil), nil
		},
		// Generous: a cold image build plus a container that has to go healthy
		// is well past the default, and this is not the gate's time budget.
		SpecTimeout: 4 * time.Minute,
	}
	return executor.Run(context.Background(), spec).Failure
}

// TestEnsureImageIsIdempotent. Several specs ask for the image and they run in
// parallel; building it more than once would put a `docker build` inside a
// spec's time budget for no reason.
func TestEnsureImageIsIdempotent(t *testing.T) {
	ctx := requireDocker(t)
	c := &runner.Ctx{Spec: &runner.Spec{Name: "docker-run"}}

	if err := ensureImage(ctx, c); err != nil {
		t.Fatalf("ensureImage: %v", err)
	}

	// Twice: the second call must be free, because sync.Once already ran.
	started := time.Now()
	if err := ensureImage(ctx, c); err != nil {
		t.Fatalf("ensureImage on the second call: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("the second ensureImage took %s; it should do nothing at all", elapsed)
	}

	if _, err := dockerOutput(ctx, "image", "inspect", e2eImage()); err != nil {
		t.Errorf("%s does not exist after ensureImage: %v", e2eImage(), err)
	}
}

// TestTheContainerScenariosPassAgainstARealDaemon runs each of them for real.
//
// The E2E specs run these too, but from a compiled binary — so this is the only
// place their failure paths, their cleanup and their assertions are exercised
// under `go test`, with the race detector on and their coverage counted.
func TestTheContainerScenariosPassAgainstARealDaemon(t *testing.T) {
	requireDocker(t)

	for _, name := range []string{
		"docker_image_serves",
		"docker_runs_as_nonroot",
		"docker_admin_loopback_default",
		"docker_health_reachable",
		"docker_compose_stack",
	} {
		t.Run(name, func(t *testing.T) {
			if failure := runScenario(t, name); failure != nil {
				t.Fatalf("%s failed: %s\n%s", name, failure.Message, strings.Join(failure.Notes, "\n"))
			}
		})
	}
}

// TestContainersAreRemovedEvenWhenAScenarioFails. Every container scenario
// removes what it started in a defer, with a context of its own so that a spec
// which has already timed out still cleans up. A leak here is invisible until
// somebody's machine has forty stopped containers on it.
func TestContainersAreRemovedEvenWhenAScenarioFails(t *testing.T) {
	ctx := requireDocker(t)

	before, err := dockerOutput(ctx, "ps", "--all", "--quiet", "--filter", "name=atlascache-e2e-")
	if err != nil {
		t.Fatalf("listing containers: %v", err)
	}

	if failure := runScenario(t, "docker_image_serves"); failure != nil {
		t.Fatalf("docker_image_serves failed: %s", failure.Message)
	}

	after, err := dockerOutput(ctx, "ps", "--all", "--quiet", "--filter", "name=atlascache-e2e-")
	if err != nil {
		t.Fatalf("listing containers: %v", err)
	}
	if countLines(after) > countLines(before) {
		t.Errorf("the scenario left containers behind: %d before, %d after", countLines(before), countLines(after))
	}
}

func countLines(s string) int {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}
