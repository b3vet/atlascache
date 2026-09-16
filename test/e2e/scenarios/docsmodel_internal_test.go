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

// The scenario body is what the spec actually runs, and the E2E suite is the
// only thing that exercises it — which means a change that breaks it is caught
// by a compiled binary rather than by `go test`. These drive it against a model
// server instead, so the failure modes it exists to detect are pinned here too:
// a documented command the server refuses, and a documented output the server
// contradicts.
//
// The model also has to be wrong in the right ways. A test that only ever runs
// the scenario against a correct server proves the scenario does not crash, not
// that it detects anything.

// docsHarness is a fake whose client address carries a port. fakeharness.Harness
// reports "fake", and the scenario rewrites the documented port into the real
// one, so without a port there is nothing to rewrite to.
type docsHarness struct {
	*fakeharness.Harness
}

func (h docsHarness) Info() runner.ServerInfo {
	info := h.Harness.Info()
	info.ClientAddr = "127.0.0.1:6379"
	return info
}

// docsModel answers the commands docs/getting-started.md documents. defects
// selects the ways it is allowed to be wrong.
type docsDefects struct {
	getReturnsTheWrongValue bool
	refusesTTL              bool
	ttlHasCollapsed         bool
}

func docsModelHarness(defects docsDefects) runner.Harness {
	fake := &fakeharness.Harness{
		Respond: func(cmd string) (runner.Reply, error) {
			verb, rest, _ := strings.Cut(cmd, " ")
			switch strings.ToUpper(verb) {
			case "PING":
				return runner.StatusReply("PONG"), nil
			case "SET":
				return runner.StatusReply("OK"), nil
			case "GET":
				if defects.getReturnsTheWrongValue {
					return runner.BulkReply("something else"), nil
				}
				return runner.BulkReply("world"), nil
			case "TTL":
				switch {
				case defects.refusesTTL:
					return runner.ErrorReply("ERR unknown command 'TTL'"), nil
				case defects.ttlHasCollapsed:
					return runner.IntegerReply(5), nil
				}
				return runner.IntegerReply(60), nil
			case "SCAN":
				return runner.ArrayReply(runner.BulkReply("0"),
					runner.ArrayReply(runner.BulkReply("user:1"), runner.BulkReply("user:2"))), nil
			case "INFO":
				return runner.BulkReply("# Server\r\natlascache_version:0.1.0-dev\r\n"), nil
			default:
				return runner.Reply{}, fmt.Errorf("the model was not asked to answer %q (rest %q)", verb, rest)
			}
		},
	}
	return docsHarness{Harness: fake}
}

// runDocsScenario drives the docs scenario through the real executor, which is
// the only way to get the *runner.Ctx a scenario is handed.
func runDocsScenario(t *testing.T, h runner.Harness) *runner.Failure {
	t.Helper()

	const name = "docs_quickstart_executes"
	source := fmt.Sprintf(
		"version: 1\nname: unit-docs-quickstart\ntier: full\nfeature: FEAT-0034\nsteps:\n  - scenario: %s\n",
		name)

	spec, err := runner.ParseSpec("unit-docs-quickstart.yaml", []byte(source))
	if err != nil {
		t.Fatalf("parsing the spec: %v", err)
	}

	executor := &runner.Executor{
		Factory:     func(runner.HarnessOptions) (runner.Harness, error) { return h, nil },
		SpecTimeout: 30 * time.Second,
	}
	return executor.Run(context.Background(), spec).Failure
}

func TestDocsQuickstartPassesAgainstAServerThatMatchesTheGuide(t *testing.T) {
	t.Parallel()

	if failure := runDocsScenario(t, docsModelHarness(docsDefects{})); failure != nil {
		t.Fatalf("a server behaving as documented failed: %s", failure.Message)
	}
}

// The two failures the scenario exists to produce.
func TestDocsQuickstartCatchesAServerThatContradictsTheGuide(t *testing.T) {
	t.Parallel()

	t.Run("a documented value the server does not return", func(t *testing.T) {
		t.Parallel()
		failure := runDocsScenario(t, docsModelHarness(docsDefects{getReturnsTheWrongValue: true}))
		requireDocsFailure(t, failure, "but it prints")
	})

	t.Run("a documented command the server refuses", func(t *testing.T) {
		t.Parallel()
		failure := runDocsScenario(t, docsModelHarness(docsDefects{refusesTTL: true}))
		requireDocsFailure(t, failure, "which the server refuses")
	})
}

// The TTL tolerance must not be wide enough to swallow a real defect. A TTL of
// 5 where the guide documents 60 is a server that lost the expiry, and it has
// to fail even though the comparison is deliberately loose.
func TestDocsQuickstartToleranceDoesNotHideACollapsedTTL(t *testing.T) {
	t.Parallel()

	failure := runDocsScenario(t, docsModelHarness(docsDefects{ttlHasCollapsed: true}))
	requireDocsFailure(t, failure, "but it prints")
}

// A harness whose address has no port cannot have the documented commands
// rewritten onto it, and the scenario must say so rather than send them at the
// port the guide happens to name.
func TestDocsQuickstartRefusesAHarnessWithNoPort(t *testing.T) {
	t.Parallel()

	fake := fakeharness.New(map[string]runner.Reply{"PING": runner.StatusReply("PONG")})
	requireDocsFailure(t, runDocsScenario(t, fake), "carries no port")
}

func requireDocsFailure(t *testing.T, failure *runner.Failure, want string) {
	t.Helper()

	if failure == nil {
		t.Fatalf("the scenario passed; it should have failed with something like %q", want)
	}
	if !strings.Contains(failure.Message, want) {
		t.Errorf("failure = %q, want it to mention %q", failure.Message, want)
	}
}
