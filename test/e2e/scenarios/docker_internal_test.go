package scenarios

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/b3vet/atlascache/test/e2e/runner"
)

// The container scenarios reach for Docker, and most of what they do cannot be
// unit tested without it. What can be — and is, here — is everything that
// decides whether an assertion is right: the Dockerfile comparison, the uid
// parsing, the "is this really AtlasCache" guard, and the skip path that keeps
// the whole suite runnable on a machine with no Docker at all.

// ---------------------------------------------------------------------------
// Skipping
// ---------------------------------------------------------------------------

// TestTheContainerScenariosSkipWithoutDocker is the property the suite depends
// on: the E2E gate stays runnable, and green, on a machine with no Docker.
//
// It is asserted for every container scenario rather than for one, because a
// scenario that forgot the check would fail the gate for everyone without
// Docker, and that is exactly the kind of thing that gets noticed once and then
// worked around with `--spec`.
func TestTheContainerScenariosSkipWithoutDocker(t *testing.T) {
	t.Setenv("PATH", "")

	scenarios := map[string]runner.ScenarioFunc{
		"docker_image_serves":           dockerImageServes,
		"docker_compose_stack":          dockerComposeStack,
		"docker_health_reachable":       dockerHealthReachable,
		"docker_admin_loopback_default": dockerAdminLoopbackDefault,
		"docker_runs_as_nonroot":        dockerRunsAsNonroot,
	}

	for name, fn := range scenarios {
		t.Run(name, func(t *testing.T) {
			c := &runner.Ctx{Spec: &runner.Spec{Name: name}}
			if err := fn(c); err != nil {
				t.Fatalf("%s failed instead of skipping without Docker: %v", name, err)
			}
			if !containsLine(c.Log(), "skipped:") {
				t.Errorf("%s passed without recording that it skipped: %v", name, c.Log())
			}
		})
	}
}

// TestDockerAvailabilityIsReportedWithAReason. A skip that does not say why
// reads as a pass, so the reason is part of the contract.
func TestDockerAvailabilityIsReportedWithAReason(t *testing.T) {
	t.Setenv("PATH", "")

	ok, reason := dockerAvailable(context.Background())
	if ok {
		t.Fatal("docker was reported available with an empty PATH")
	}
	if !strings.Contains(reason, "docker") {
		t.Errorf("the reason does not mention docker: %q", reason)
	}

	ok, reason = composeAvailable(context.Background())
	if ok {
		t.Fatal("docker compose was reported available with an empty PATH")
	}
	if reason == "" {
		t.Error("composeAvailable reported no reason")
	}
}

func TestDockerSkipRecordsTheReason(t *testing.T) {
	c := &runner.Ctx{Spec: &runner.Spec{Name: "docker-test"}}
	dockerSkip(c, "no daemon here")

	if !containsLine(c.Log(), "no daemon here") {
		t.Errorf("the reason did not reach the scenario log: %v", c.Log())
	}
}

// ---------------------------------------------------------------------------
// The two Dockerfiles
// ---------------------------------------------------------------------------

// TestTheTwoDockerfilesAgree runs the drift check against the repository's real
// files, which is the only place it means anything.
func TestTheTwoDockerfilesAgree(t *testing.T) {
	c := &runner.Ctx{Spec: &runner.Spec{Name: "docker-run"}}
	if err := dockerReleaseDockerfileMatches(c); err != nil {
		t.Fatalf("Dockerfile and Dockerfile.release have drifted: %v", err)
	}
}

func TestTheDriftCheckCatchesDrift(t *testing.T) {
	build := `
FROM golang:1.24 AS build
RUN go build -o /out/atlascache ./cmd/atlascache

FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/atlascache /atlascache
EXPOSE 6379 8080
USER nonroot:nonroot
ENTRYPOINT ["/atlascache"]
`
	// The release file exposes a different port and runs as root: either alone
	// must fail, since either alone means the published image is not the image
	// the suite tested.
	release := `
ARG BASE=gcr.io/distroless/static-debian12:nonroot
FROM ${BASE}
COPY atlascache /atlascache
EXPOSE 6379
USER root
ENTRYPOINT ["/atlascache"]
`
	runtimeStage := stageDirectives(build, "runtime")
	releaseStage := stageDirectives(release, "")

	if runtimeStage["EXPOSE"] == releaseStage["EXPOSE"] {
		t.Error("the parser did not see the differing EXPOSE lines")
	}
	if runtimeStage["USER"] == releaseStage["USER"] {
		t.Error("the parser did not see the differing USER lines")
	}
	if runtimeStage["ENTRYPOINT"] != releaseStage["ENTRYPOINT"] {
		t.Errorf("identical ENTRYPOINT lines were read differently: %q vs %q",
			runtimeStage["ENTRYPOINT"], releaseStage["ENTRYPOINT"])
	}
}

// TestStageDirectivesReadsOnlyTheNamedStage. A multi-stage Dockerfile's build
// stage has its own USER and ENTRYPOINT in many projects, and comparing the
// wrong stage would compare the builder with the runtime.
func TestStageDirectivesReadsOnlyTheNamedStage(t *testing.T) {
	dockerfile := `
FROM golang:1.24 AS build
USER builder
EXPOSE 1234

FROM scratch AS runtime
# a comment, and a blank line, neither of which is a directive

USER nonroot:nonroot
EXPOSE 6379 8080

FROM scratch AS debug
USER root
`
	runtimeStage := stageDirectives(dockerfile, "runtime")
	if got := runtimeStage["USER"]; got != "nonroot:nonroot" {
		t.Errorf("USER read as %q, want nonroot:nonroot", got)
	}
	if got := runtimeStage["EXPOSE"]; got != "6379 8080" {
		t.Errorf("EXPOSE read as %q, want 6379 8080", got)
	}

	// A file with one stage and no name: every directive after the FROM counts,
	// which is what Dockerfile.release looks like.
	single := stageDirectives("FROM alpine\nUSER nobody\n", "")
	if got := single["USER"]; got != "nobody" {
		t.Errorf("the unnamed stage read USER as %q, want nobody", got)
	}

	if len(stageDirectives(dockerfile, "absent")) != 0 {
		t.Error("a stage that does not exist yielded directives")
	}
}

// ---------------------------------------------------------------------------
// The uid
// ---------------------------------------------------------------------------

// TestServerUIDReadsWhicheverColumnLayoutTheDaemonUses. `docker top` shells out
// to the host's ps: a Linux daemon leads with UID, Docker Desktop's VM answers
// PID first. A fixed column index would assert the wrong number on half the
// machines that run this.
func TestServerUIDReadsWhicheverColumnLayoutTheDaemonUses(t *testing.T) {
	tests := []struct {
		name string
		top  string
		want string
	}{
		{
			name: "PID first, as Docker Desktop reports it",
			top: "PID                 USER                TIME                COMMAND\n" +
				"2802146             65532               0:00                /atlascache\n",
			want: "65532",
		},
		{
			name: "UID first, as ps -ef reports it",
			top: "UID   PID   PPID  C STIME TTY   TIME     CMD\n" +
				"65532 12345 12300 0 10:00 ?     00:00:00 /atlascache\n",
			want: "65532",
		},
		{
			name: "root is read as root rather than hidden",
			top: "UID   PID   PPID  C STIME TTY   TIME     CMD\n" +
				"root  12345 12300 0 10:00 ?     00:00:00 /atlascache\n",
			want: "root",
		},
		{
			name: "the server row is picked out of several processes",
			top: "PID     USER   TIME   COMMAND\n" +
				"10      0      0:00   /pause\n" +
				"11      65532  0:00   /atlascache\n",
			want: "65532",
		},
		{
			name: "with no row naming the binary, the first process answers",
			top: "PID     USER   TIME   COMMAND\n" +
				"10      65532  0:00   /some-entrypoint\n",
			want: "65532",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := serverUID(tc.top)
			if err != nil {
				t.Fatalf("serverUID: %v", err)
			}
			if got != tc.want {
				t.Errorf("serverUID = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestServerUIDFailsRatherThanGuessing(t *testing.T) {
	for _, top := range []string{
		"",
		"PID                 USER                TIME                COMMAND\n",
		"WIDGET  GADGET\n1  2\n",
	} {
		if uid, err := serverUID(top); err == nil {
			t.Errorf("serverUID read %q out of %q, which names no uid", uid, top)
		}
	}
}

// ---------------------------------------------------------------------------
// "Is this really AtlasCache?"
// ---------------------------------------------------------------------------

// TestAtlasCacheVersionRejectsSomethingElseListening is the guard that keeps
// these specs honest. Most machines that run this suite already have a Redis
// somewhere, and a PING to the wrong server answers +PONG exactly like the
// right one; only INFO server distinguishes them.
func TestAtlasCacheVersionRejectsSomethingElseListening(t *testing.T) {
	t.Run("an AtlasCache reports its version", func(t *testing.T) {
		addr := fakeInfoServer(t, bulk("# Server\r\natlascache_version:9.9.9\r\natlascache_mode:standalone\r\n"))

		version, err := atlasCacheVersion(context.Background(), addr)
		if err != nil {
			t.Fatalf("atlasCacheVersion: %v", err)
		}
		if version != "9.9.9" {
			t.Errorf("version read as %q, want 9.9.9", version)
		}
	})

	t.Run("something else answering INFO is rejected", func(t *testing.T) {
		// Shaped like a real Redis: a plausible INFO section with no
		// atlascache_version anywhere in it.
		addr := fakeInfoServer(t, bulk("# Server\r\nredis_version:7.2.4\r\nredis_mode:standalone\r\n"))

		_, err := atlasCacheVersion(context.Background(), addr)
		if err == nil {
			t.Fatal("a server that is not AtlasCache was accepted")
		}
		if !strings.Contains(err.Error(), "atlascache_version") {
			t.Errorf("the failure does not say what was missing: %v", err)
		}
	})

	t.Run("an error reply is rejected", func(t *testing.T) {
		addr := fakeInfoServer(t, "-ERR unknown command 'INFO'\r\n")

		if _, err := atlasCacheVersion(context.Background(), addr); err == nil {
			t.Fatal("an error reply was accepted as a version")
		}
	})

	t.Run("nothing listening is rejected", func(t *testing.T) {
		port, err := freePort(context.Background())
		if err != nil {
			t.Fatalf("freePort: %v", err)
		}
		addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))

		if _, err := atlasCacheVersion(context.Background(), addr); err == nil {
			t.Fatal("an address with nothing behind it was accepted")
		}
	})
}

// TestWaitForAtlasCacheGivesUpWhenTheRunIsCanceled. The poll loop must honor
// the spec's deadline: a container that never comes up has to fail its spec,
// not hold the run open.
func TestWaitForAtlasCacheGivesUpWhenTheRunIsCanceled(t *testing.T) {
	port, err := freePort(context.Background())
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := &runner.Ctx{Spec: &runner.Spec{Name: "docker-test"}}
	started := time.Now()
	if _, err := waitForAtlasCache(ctx, c, net.JoinHostPort("127.0.0.1", fmt.Sprint(port))); err == nil {
		t.Fatal("waiting succeeded against nothing")
	}
	if elapsed := time.Since(started); elapsed > containerReadyTimeout {
		t.Errorf("waiting ignored the canceled context and took %s", elapsed)
	}
}

// ---------------------------------------------------------------------------
// Plumbing
// ---------------------------------------------------------------------------

func TestHTTPGetReportsStatusAndBody(t *testing.T) {
	var sawToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawToken = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusTeapot)
		if _, err := w.Write([]byte(`{"status":"ok"}`)); err != nil {
			t.Errorf("writing the response: %v", err)
		}
	}))
	defer srv.Close()

	status, body, err := httpGet(context.Background(), srv.URL+"/health", "a-token")
	if err != nil {
		t.Fatalf("httpGet: %v", err)
	}
	if status != http.StatusTeapot {
		t.Errorf("status = %d, want %d", status, http.StatusTeapot)
	}
	if body != `{"status":"ok"}` {
		t.Errorf("body = %q", body)
	}
	if sawToken != "Bearer a-token" {
		t.Errorf("the token was sent as %q", sawToken)
	}

	// No token means no header at all, because the health endpoints are
	// unauthenticated by design and sending a credential to them would put it
	// in a log for nothing.
	if _, _, err := httpGet(context.Background(), srv.URL+"/health", ""); err != nil {
		t.Fatalf("httpGet without a token: %v", err)
	}
	if sawToken != "" {
		t.Errorf("an Authorization header was sent with no token: %q", sawToken)
	}
}

func TestDockerRepoRootFindsTheRepository(t *testing.T) {
	root, err := dockerRepoRoot()
	if err != nil {
		t.Fatalf("dockerRepoRoot: %v", err)
	}
	for _, name := range []string{"go.work", "Dockerfile", "Dockerfile.release", "docker-compose.yml"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("%s is missing from the repository root %s: %v", name, root, err)
		}
	}
}

func TestE2EImageIsOverridable(t *testing.T) {
	t.Setenv("ATLASCACHE_E2E_IMAGE", "")
	if got := e2eImage(); got != defaultE2EImage {
		t.Errorf("e2eImage = %q, want the default %q", got, defaultE2EImage)
	}

	t.Setenv("ATLASCACHE_E2E_IMAGE", "ghcr.io/b3vet/atlascache:0.1.0")
	if got := e2eImage(); got != "ghcr.io/b3vet/atlascache:0.1.0" {
		t.Errorf("e2eImage ignored the override: %q", got)
	}
}

func TestContainerAddressesAreLoopback(t *testing.T) {
	ct := &container{name: "x", clientPort: 16379, adminPort: 18080}

	if got := ct.clientAddr(); got != "127.0.0.1:16379" {
		t.Errorf("clientAddr = %q", got)
	}
	if got := ct.adminURL("/health"); got != "http://127.0.0.1:18080/health" {
		t.Errorf("adminURL = %q", got)
	}
}

func TestUniqueNamesDoNotCollide(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		name := uniqueName("run")
		if seen[name] {
			t.Fatalf("uniqueName repeated %q, so two parallel specs would collide", name)
		}
		seen[name] = true
		if !strings.HasPrefix(name, "atlascache-e2e-run-") {
			t.Fatalf("uniqueName = %q, which does not identify the suite that made it", name)
		}
	}
}

func TestFreePortIsUsable(t *testing.T) {
	port, err := freePort(context.Background())
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}

	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
	if err != nil {
		t.Fatalf("the port freePort returned could not be bound: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Errorf("closing the listener: %v", err)
	}
}

func TestDockerSleepHonoursCancellation(t *testing.T) {
	if err := dockerSleep(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("dockerSleep: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := dockerSleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("dockerSleep returned %v, want context.Canceled", err)
	}
}

func TestDockerOutputFailsWithoutDocker(t *testing.T) {
	t.Setenv("PATH", "")

	if _, err := dockerOutput(context.Background(), "version"); err == nil {
		t.Fatal("dockerOutput succeeded with an empty PATH")
	}
	if _, err := waitForHealthState(context.Background(), "nothing", "healthy"); err == nil {
		t.Fatal("waitForHealthState succeeded with an empty PATH")
	}
}

func TestTextHelpers(t *testing.T) {
	if got := firstLine("  first\nsecond\n"); got != "first" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine("only"); got != "only" {
		t.Errorf("firstLine = %q", got)
	}
	if got := tail("a\nb\nc\nd\n", 2); got != "c\nd" {
		t.Errorf("tail = %q", got)
	}
	if got := tail("a\nb", 10); got != "a\nb" {
		t.Errorf("tail shortened a short input: %q", got)
	}
	if got := clip("short"); got != "short" {
		t.Errorf("clip shortened a short input: %q", got)
	}
	long := strings.Repeat("x", 200)
	if got := clip(long); len([]rune(got)) != 121 {
		t.Errorf("clip produced %d runes, want 120 plus an ellipsis", len([]rune(got)))
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// fakeInfoServer answers every command on every connection with the same RESP
// reply, so the identity check and the round trip can be tested against
// something that is not a container.
//
// It parses the command frame properly rather than reading a line and hoping:
// a RESP command is an array of bulk strings spanning several lines, and a
// server that replied after the first of them would desynchronise on the
// second command of a connection.
func fakeInfoServer(t *testing.T, reply string) string {
	t.Helper()

	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveFakeReplies(conn, reply)
		}
	}()

	return listener.Addr().String()
}

func serveFakeReplies(conn net.Conn, reply string) {
	defer func() { _ = conn.Close() }()

	reader := bufio.NewReader(conn)
	for {
		header, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		// "*N\r\n" is followed by N bulk strings, each a length line and a
		// payload line.
		count := 0
		if strings.HasPrefix(header, "*") {
			count, err = strconv.Atoi(strings.TrimSpace(header[1:]))
			if err != nil {
				return
			}
		}
		for range count * 2 {
			if _, err := reader.ReadString('\n'); err != nil {
				return
			}
		}
		if _, err := conn.Write([]byte(reply)); err != nil {
			return
		}
	}
}

// bulk frames a payload as a RESP bulk string, so the length prefix cannot
// drift from the payload the way a hand-counted one does.
func bulk(payload string) string {
	return fmt.Sprintf("$%d\r\n%s\r\n", len(payload), payload)
}

func containsLine(lines []string, substring string) bool {
	for _, line := range lines {
		if strings.Contains(line, substring) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Failure paths
// ---------------------------------------------------------------------------
//
// What a container spec does when something goes wrong is most of its value: a
// spec that fails with "exit status 1" costs more time than it saves. These
// drive the failure branches with no Docker at all, which is the cheapest way
// to reach every one of them.

func TestTheContainerLogIsNeverTheReasonAFailureIsUnreadable(t *testing.T) {
	t.Setenv("PATH", "")

	ct := &container{name: "absent", clientPort: 1, adminPort: 2}
	logs := ct.logs()
	if logs == "" {
		t.Fatal("logs() returned nothing at all; a failure would print an empty block")
	}
	if !strings.Contains(logs, "could not be read") {
		t.Errorf("logs() did not say why it had nothing: %q", logs)
	}
}

func TestStartingAContainerFailsLoudly(t *testing.T) {
	t.Setenv("PATH", "")

	c := &runner.Ctx{Spec: &runner.Spec{Name: "docker-run"}}
	if _, err := startContainer(context.Background(), c, "atlascache-e2e-unit", nil, true); err == nil {
		t.Fatal("startContainer succeeded with no docker on PATH")
	}
}

func TestRemovingAContainerThatIsNotThereIsRecordedRatherThanIgnored(t *testing.T) {
	t.Setenv("PATH", "")

	c := &runner.Ctx{Spec: &runner.Spec{Name: "docker-run"}}
	(&container{name: "absent"}).remove(c)

	if !containsLine(c.Log(), "removing container") {
		t.Errorf("a failed removal left no trace: %v", c.Log())
	}
}

func TestTheComposeHelpersFailWithoutDocker(t *testing.T) {
	t.Setenv("PATH", "")
	ctx := context.Background()
	c := &runner.Ctx{Spec: &runner.Spec{Name: "docker-compose"}}

	// Tagging the image is the first thing it does, and the first thing that
	// can fail.
	if _, err := newComposeStack(ctx, c); err == nil {
		t.Fatal("newComposeStack succeeded with no docker on PATH")
	}

	// The rest of the helpers, on a stack built by hand so they can be reached
	// at all.
	root, err := dockerRepoRoot()
	if err != nil {
		t.Fatalf("dockerRepoRoot: %v", err)
	}
	stack := &composeStack{root: root, project: "atlascache-e2e-unit", clientPort: 1, adminPort: 2}

	if err := stack.up(ctx, c); err == nil {
		t.Error("compose up succeeded with no docker on PATH")
	}

	// down never returns an error -- it runs in a defer -- so it has to say
	// what happened in the scenario log instead.
	stack.down(c)
	if !containsLine(c.Log(), "compose down failed") {
		t.Errorf("a failed teardown left no trace: %v", c.Log())
	}
}

// TestUsableReportsTheComposeLogsWhenTheStackNeverServes. The logs are the only
// thing that explains a stack that came up and then did not answer.
func TestUsableReportsTheComposeLogsWhenTheStackNeverServes(t *testing.T) {
	t.Setenv("PATH", "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := &runner.Ctx{Spec: &runner.Spec{Name: "docker-compose"}}
	stack := &composeStack{root: ".", project: "atlascache-e2e-unit", clientPort: 1, adminPort: 2}

	if err := stack.usable(ctx, c); err == nil {
		t.Fatal("usable succeeded against a stack that does not exist")
	}
	if !containsLine(c.Log(), "compose logs") {
		t.Errorf("the failure did not carry the compose logs: %v", c.Log())
	}
}

// TestComposeRoundTripRejectsAServerThatDoesNotStore. A cache that answers but
// does not hold what it was given is the failure this catches, and it is
// exactly what a misrouted port looks like.
func TestComposeRoundTripRejectsAServerThatDoesNotStore(t *testing.T) {
	// Answers every command with +OK, so SET looks fine and GET returns the
	// wrong thing.
	addr := fakeInfoServer(t, "+OK\r\n")

	err := composeRoundTrip(context.Background(), addr)
	if err == nil {
		t.Fatal("a server that never stored the key was accepted")
	}
	if !strings.Contains(err.Error(), "compose-value") {
		t.Errorf("the failure does not say what was expected: %v", err)
	}

	// And nothing listening at all.
	port, err := freePort(context.Background())
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	if err := composeRoundTrip(context.Background(), net.JoinHostPort("127.0.0.1", fmt.Sprint(port))); err == nil {
		t.Fatal("composeRoundTrip succeeded against nothing")
	}
}

// TestWaitForAtlasCacheKeepsTryingUntilItsBudgetRunsOut against a server that
// answers but is never AtlasCache: the loop has to end, and the failure has to
// name the last thing that went wrong rather than only the timeout.
func TestWaitForAtlasCacheKeepsTryingUntilItsBudgetRunsOut(t *testing.T) {
	addr := fakeInfoServer(t, bulk("# Server\r\nredis_version:7.2.4\r\n"))

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()

	c := &runner.Ctx{Spec: &runner.Spec{Name: "docker-run"}}
	_, err := waitForAtlasCache(ctx, c, addr)
	if err == nil {
		t.Fatal("a server that is not AtlasCache was waited into acceptance")
	}
}
