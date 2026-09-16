package scenarios

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/b3vet/atlascache/test/e2e/client"
	"github.com/b3vet/atlascache/test/e2e/runner"
)

// The container specs (FEAT-0032). They are the only scenarios that reach
// outside the test process for something other than the server binary, which
// makes two things true of all of them.
//
// First, they skip rather than fail when Docker is not available. The E2E
// suite is this project's gate: a developer without Docker must still be able
// to run it and trust the result, so "Docker is missing" has to be a reported
// absence and not a red run. Every skip says so on stderr, loudly enough that
// nobody mistakes a skipped container spec for a passing one.
//
// Second, they never trust a port. There is almost always a Redis on 6379
// somewhere, and a PING to the wrong server answers +PONG exactly like the
// right one. Every container these specs start is published on a port the
// kernel handed out, and every connection is confirmed with `INFO server` and
// its atlascache_version field before any other assertion is believed.
func init() {
	runner.RegisterScenario("docker_image_serves", dockerImageServes)
	runner.RegisterScenario("docker_release_dockerfile_matches", dockerReleaseDockerfileMatches)
	runner.RegisterScenario("docker_compose_stack", dockerComposeStack)
	runner.RegisterScenario("docker_health_reachable", dockerHealthReachable)
	runner.RegisterScenario("docker_admin_loopback_default", dockerAdminLoopbackDefault)
	runner.RegisterScenario("docker_runs_as_nonroot", dockerRunsAsNonroot)
}

const (
	// defaultE2EImage is the tag the container specs run. `make docker-build
	// IMAGE_TAG=e2e` builds it; the specs build it themselves if it is absent.
	defaultE2EImage = "atlascache:e2e"

	// rootUser is what the image must never declare or run as.
	rootUser = "root"

	// nonrootUID is the uid distroless's `nonroot` variant runs as (ADR-0024).
	// It is asserted as a number rather than as the name "nonroot", because the
	// name is a label in /etc/passwd and the number is what the kernel checks.
	nonrootUID = "65532"

	// containerReadyTimeout bounds waiting for a container to serve. Generous
	// relative to how long it takes (about a second), because a loaded CI
	// machine is the case that matters and a flaky gate is worth less than a
	// slow one.
	containerReadyTimeout = 45 * time.Second

	dockerCommandTimeout = 90 * time.Second
)

// e2eImage is the image the container specs run.
func e2eImage() string {
	if name := os.Getenv("ATLASCACHE_E2E_IMAGE"); name != "" {
		return name
	}
	return defaultE2EImage
}

// dockerSkip reports that a container spec did not run, and why.
//
// It returns nil, so the spec passes. That is a deliberate trade: the
// alternative is a suite that fails on any machine without Docker, which makes
// the gate unusable exactly where it is most often run by hand. The message
// goes to stderr rather than to the scenario log, because a scenario log is
// only printed on failure and a skip that nobody can see is a skip nobody
// knows about.
func dockerSkip(c *runner.Ctx, reason string) {
	fmt.Fprintf(os.Stderr, "SKIP  %s: %s\n", c.Spec.Name, reason)
	c.Logf("skipped: %s", reason)
}

// dockerAvailable reports whether a Docker daemon is reachable, and says what
// was wrong when it is not.
func dockerAvailable(ctx context.Context) (bool, string) {
	if _, err := exec.LookPath("docker"); err != nil {
		return false, "the docker CLI is not on PATH"
	}
	// `docker version` on the *server* half: the CLI answers even with no
	// daemon behind it, and a spec that only checked for the binary would fail
	// at the first `docker run` instead of skipping.
	out, err := dockerOutput(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return false, "no Docker daemon is reachable: " + firstLine(out+" "+err.Error())
	}
	return true, ""
}

func composeAvailable(ctx context.Context) (bool, string) {
	out, err := dockerOutput(ctx, "compose", "version", "--short")
	if err != nil {
		return false, "docker compose is not available: " + firstLine(out+" "+err.Error())
	}
	return true, ""
}

// ---------------------------------------------------------------------------
// The image
// ---------------------------------------------------------------------------

var (
	imageOnce sync.Once
	imageErr  error
)

// ensureImage builds the image the specs run, at most once per run however many
// specs ask for it.
//
// It builds every time rather than reusing whatever is already tagged. An image
// left over from an earlier working tree would make the container specs report
// on code that is no longer here, which is the one thing a regression gate must
// never do — and with a warm layer cache the build is a second or two, so the
// safe behavior is also the cheap one. CI builds the same tag once before the
// suite starts (see .github/workflows/ci.yml), which warms exactly the layers
// this rebuild needs and keeps it inside a spec's time budget.
func ensureImage(ctx context.Context, c *runner.Ctx) error {
	imageOnce.Do(func() {
		image := e2eImage()

		root, err := dockerRepoRoot()
		if err != nil {
			imageErr = err
			return
		}

		c.Logf("building %s from %s (once per run; a no-op when the layers are cached)", image, root)
		started := time.Now()
		out, err := dockerOutputIn(ctx, root,
			"build", "--target", "runtime", "-t", image, root)
		if err != nil {
			imageErr = fmt.Errorf(
				"building %s failed: %w\nbuild it before the suite with `make docker-build IMAGE_TAG=e2e`\n%s",
				image, err, tail(out, 25))
			return
		}
		c.Logf("built %s in %s", image, time.Since(started).Round(time.Second))
	})
	return imageErr
}

// dockerRepoRoot finds the repository from the working directory the suite was
// started in. go.work and Dockerfile together are specific enough that no
// subdirectory answers by accident.
func dockerRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("finding the working directory: %w", err)
	}
	for {
		_, workErr := os.Stat(filepath.Join(dir, "go.work"))
		_, dockerErr := os.Stat(filepath.Join(dir, "Dockerfile"))
		if workErr == nil && dockerErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no repository root above the working directory (looked for go.work beside a Dockerfile)")
		}
		dir = parent
	}
}

// ---------------------------------------------------------------------------
// container: one `docker run` and everything needed to reach and remove it
// ---------------------------------------------------------------------------

type container struct {
	name       string
	clientPort int
	adminPort  int
}

// startContainer runs the image detached, publishing the client port and,
// when adminPort is non-zero, the admin port too.
//
// Both are published on 127.0.0.1 and on ports the kernel chose. A container
// published on 6379 would collide with whatever Redis is already running on
// this machine, and — worse — would sometimes appear to work, because the
// other server answers PING the same way.
func startContainer(ctx context.Context, c *runner.Ctx, name string, env []string, adminPort bool) (*container, error) {
	clientHostPort, err := freePort(ctx)
	if err != nil {
		return nil, err
	}

	args := []string{"run", "--detach", "--name", name,
		"--publish", fmt.Sprintf("127.0.0.1:%d:6379", clientHostPort)}

	adminHostPort := 0
	if adminPort {
		adminHostPort, err = freePort(ctx)
		if err != nil {
			return nil, err
		}
		args = append(args, "--publish", fmt.Sprintf("127.0.0.1:%d:8080", adminHostPort))
	}
	for _, e := range env {
		args = append(args, "--env", e)
	}
	args = append(args, e2eImage())

	out, err := dockerOutput(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("starting container %s: %w\n%s", name, err, tail(out, 15))
	}

	cont := &container{name: name, clientPort: clientHostPort, adminPort: adminHostPort}
	c.Logf("started %s on 127.0.0.1:%d (admin %d)", name, clientHostPort, adminHostPort)
	return cont, nil
}

// remove tears the container down. It runs with its own context so that a spec
// which has already timed out still cleans up after itself rather than leaving
// a container running on somebody's machine.
func (ct *container) remove(c *runner.Ctx) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := dockerOutput(ctx, "rm", "--force", "--volumes", ct.name); err != nil {
		c.Logf("removing container %s failed: %v: %s", ct.name, err, firstLine(out))
	}
}

func (ct *container) clientAddr() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(ct.clientPort))
}

func (ct *container) adminURL(path string) string {
	return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(ct.adminPort)) + path
}

// logs returns the container's output, for a failure message that says what the
// server thought was happening rather than only that a port did not answer.
func (ct *container) logs() string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := dockerOutput(ctx, "logs", "--tail", "20", ct.name)
	if err != nil {
		return fmt.Sprintf("(the container log could not be read: %v)", err)
	}
	return out
}

// ---------------------------------------------------------------------------
// Assertions shared by the container specs
// ---------------------------------------------------------------------------

// waitForAtlasCache waits until addr answers PING *and* identifies itself as
// AtlasCache, then returns the version it reported.
//
// The identity check is the point. This machine, and most machines a developer
// runs this on, already has a Redis listening somewhere; if a published port
// silently landed on the wrong server, every assertion after a bare PING would
// pass against it. INFO server naming atlascache_version is the cheapest thing
// that cannot be true of anything else.
func waitForAtlasCache(ctx context.Context, c *runner.Ctx, addr string) (string, error) {
	deadline := time.Now().Add(containerReadyTimeout)
	var lastErr error

	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		version, err := atlasCacheVersion(ctx, addr)
		if err == nil {
			c.Logf("%s is AtlasCache %s", addr, version)
			return version, nil
		}
		lastErr = err
		if err := dockerSleep(ctx, 250*time.Millisecond); err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("%s did not become a serving AtlasCache within %s: %w", addr, containerReadyTimeout, lastErr)
}

// atlasCacheVersion opens one connection, sends INFO server, and returns the
// reported version. An error means the address is not a ready AtlasCache —
// including the case where it is something else entirely.
func atlasCacheVersion(ctx context.Context, addr string) (string, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	conn, err := client.Dial(dialCtx, addr)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()

	reply, err := conn.Do(dialCtx, "INFO server")
	if err != nil {
		return "", err
	}
	if reply.Kind == runner.KindError {
		return "", fmt.Errorf("INFO server returned an error: %s", reply.Text)
	}

	version, ok := reply.Fields()["atlascache_version"]
	if !ok {
		return "", fmt.Errorf(
			"INFO server has no atlascache_version field, so %s is not AtlasCache: %s",
			addr, clip(reply.String()))
	}
	return version, nil
}

// ---------------------------------------------------------------------------
// docker-run
// ---------------------------------------------------------------------------

// dockerImageServes is the whole of what `docker run` has to do: start, serve
// the RESP protocol, hold data, and report the build that was stamped into it.
func dockerImageServes(c *runner.Ctx) error {
	ctx := c.Context()
	if ok, reason := dockerAvailable(ctx); !ok {
		dockerSkip(c, reason)
		return nil
	}
	if err := ensureImage(ctx, c); err != nil {
		return err
	}

	// The image's entrypoint takes no configuration here, so this is the
	// default shape of a container: admin on loopback, client port on 0.0.0.0.
	cont, err := startContainer(ctx, c, uniqueName("run"), nil, false)
	if err != nil {
		return err
	}
	defer cont.remove(c)

	version, err := waitForAtlasCache(ctx, c, cont.clientAddr())
	if err != nil {
		c.Logf("container log:\n%s", cont.logs())
		return err
	}

	conn, err := client.Dial(ctx, cont.clientAddr())
	if err != nil {
		return fmt.Errorf("connecting to the container: %w", err)
	}
	defer func() { _ = conn.Close() }()

	reply, err := conn.Do(ctx, ping)
	if err != nil {
		return fmt.Errorf("PING against the container: %w", err)
	}
	if got := reply.String(); got != pong {
		return fmt.Errorf("the container answered PING with %q, want PONG", got)
	}

	// A cache that answers PING but drops writes would pass a liveness check
	// and be useless, so the round trip is asserted too.
	if _, setErr := conn.Do(ctx, "SET docker-run-key docker-run-value"); setErr != nil {
		return fmt.Errorf("SET: %w", setErr)
	}
	got, err := conn.Do(ctx, "GET docker-run-key")
	if err != nil {
		return fmt.Errorf("GET: %w", err)
	}
	if got.String() != "docker-run-value" {
		return fmt.Errorf("GET returned %q, want docker-run-value", got.String())
	}

	// --version and INFO must agree, because they are stamped by the same
	// ldflags and a container that reports one build while running another is
	// the kind of thing nobody notices until an incident.
	out, err := dockerOutput(ctx, "run", "--rm", e2eImage(), "--version")
	if err != nil {
		return fmt.Errorf("`--version` in the container failed: %w\n%s", err, tail(out, 10))
	}
	c.Logf("--version reported:\n%s", strings.TrimSpace(out))
	if !strings.Contains(out, version) {
		return fmt.Errorf("--version printed %q but INFO server reports version %q",
			firstLine(out), version)
	}
	for _, field := range []string{"commit:", "built:"} {
		if !strings.Contains(out, field) {
			return fmt.Errorf("--version does not report %s: %s", field, firstLine(out))
		}
	}
	return nil
}

// dockerReleaseDockerfileMatches guards the one thing two Dockerfiles are
// guaranteed to do eventually, which is disagree.
//
// ./Dockerfile compiles the binary from source and is what compose and these
// specs build. Dockerfile.release takes the binary GoReleaser already built, so
// the published image contains the artifact the checksums and signature cover
// (FEAT-0033). Everything else about the two runtimes must be identical, and
// "must be identical" without a check is a wish.
func dockerReleaseDockerfileMatches(c *runner.Ctx) error {
	root, err := dockerRepoRoot()
	if err != nil {
		return err
	}

	build, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		return fmt.Errorf("reading Dockerfile: %w", err)
	}
	release, err := os.ReadFile(filepath.Join(root, "Dockerfile.release"))
	if err != nil {
		return fmt.Errorf("reading Dockerfile.release: %w", err)
	}

	runtimeStage := stageDirectives(string(build), "runtime")
	releaseStage := stageDirectives(string(release), "")

	for _, directive := range []string{"EXPOSE", "HEALTHCHECK", "USER", "ENTRYPOINT"} {
		a, b := runtimeStage[directive], releaseStage[directive]
		if a == "" {
			return fmt.Errorf("the runtime stage of Dockerfile has no %s", directive)
		}
		if a != b {
			return fmt.Errorf(
				"the two Dockerfiles disagree on %s, so the released image is not the image this suite tests:\n  Dockerfile:         %s\n  Dockerfile.release: %s",
				directive, a, b)
		}
	}

	// The base image is written differently in the two files -- one names it,
	// the other takes it as a build argument so the debug variant can override
	// it -- so the substring is what is compared rather than the whole line.
	const base = "gcr.io/distroless/static-debian12:nonroot"
	if !strings.Contains(string(build), base) || !strings.Contains(string(release), base) {
		return fmt.Errorf("both Dockerfiles must build on %s (ADR-0024)", base)
	}
	c.Logf("Dockerfile and Dockerfile.release agree on base, ports, health check, user and entrypoint")
	return nil
}

// stageDirectives collects the last occurrence of each directive within one
// build stage. stage is the name after `AS`; an empty name means the file has
// only one stage and all of it counts.
func stageDirectives(dockerfile, stage string) map[string]string {
	directives := map[string]string{}
	inStage := stage == ""

	for _, raw := range strings.Split(dockerfile, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// A FROM opens a stage. With no stage name to look for, the file has
		// only one and every FROM in it is still that one -- Dockerfile.release
		// takes its base from an ARG, so its FROM names no stage at all.
		if strings.HasPrefix(strings.ToUpper(line), "FROM ") {
			if stage != "" {
				inStage = strings.HasSuffix(strings.ToUpper(line), " AS "+strings.ToUpper(stage))
			}
			continue
		}
		if !inStage {
			continue
		}
		keyword, rest, found := strings.Cut(line, " ")
		if !found {
			continue
		}
		directives[strings.ToUpper(keyword)] = strings.TrimSpace(rest)
	}
	return directives
}

// ---------------------------------------------------------------------------
// docker-health
// ---------------------------------------------------------------------------

// dockerHealthReachable is the spec that catches the admin bind trap.
//
// ADR-0023 binds the admin API to 127.0.0.1, which inside a container means
// "this container only" — unreachable from a Kubernetes probe, a sidecar, or
// anything else outside the network namespace. A container shipped with that
// default has an admin API nothing can probe, and the failure looks like a
// broken health endpoint rather than a configuration choice.
//
// So this runs a container configured the way the documentation says to
// configure one — non-loopback bind *and* an admin token, since the server
// refuses the first without the second — and probes it from outside.
func dockerHealthReachable(c *runner.Ctx) error {
	ctx := c.Context()
	if ok, reason := dockerAvailable(ctx); !ok {
		dockerSkip(c, reason)
		return nil
	}
	if err := ensureImage(ctx, c); err != nil {
		return err
	}

	// Generated per run rather than written here: a literal credential in a
	// source file is a credential, however temporary its use.
	token := uniqueName("token")
	cont, err := startContainer(ctx, c, uniqueName("health"), []string{
		"ATLAS_ADMIN_BIND_ADDR=0.0.0.0",
		"ATLAS_ADMIN_TOKEN=" + token,
	}, true)
	if err != nil {
		return err
	}
	defer cont.remove(c)

	if _, readyErr := waitForAtlasCache(ctx, c, cont.clientAddr()); readyErr != nil {
		c.Logf("container log:\n%s", cont.logs())
		return readyErr
	}

	// Unauthenticated, from outside the container, exactly as a probe would.
	for _, path := range []string{"/health", "/health/live", "/health/ready"} {
		status, body, probeErr := httpGet(ctx, cont.adminURL(path), "")
		if probeErr != nil {
			c.Logf("container log:\n%s", cont.logs())
			return fmt.Errorf("GET %s from outside the container failed: %w"+
				"\nthe admin API is probably still bound to 127.0.0.1, which a probe outside the container cannot reach (ADR-0023)",
				path, probeErr)
		}
		if status != http.StatusOK {
			return fmt.Errorf("GET %s answered %d: %s", path, status, clip(body))
		}
		c.Logf("GET %s -> %d %s", path, status, strings.TrimSpace(body))
	}

	// Health is unauthenticated by design and everything else is not. If the
	// split were the other way round, exposing the admin API to make probes
	// work would expose the statistics with it.
	status, _, err := httpGet(ctx, cont.adminURL("/stats"), "")
	if err != nil {
		return fmt.Errorf("GET /stats: %w", err)
	}
	if status != http.StatusUnauthorized {
		return fmt.Errorf("GET /stats without a token answered %d, want 401: exposing the admin API must not expose the statistics", status)
	}
	status, body, err := httpGet(ctx, cont.adminURL("/stats"), token)
	if err != nil {
		return fmt.Errorf("GET /stats with the admin token: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("GET /stats with the admin token answered %d: %s", status, clip(body))
	}

	// Docker's own view. HEALTHCHECK runs `atlascache --health-check` inside
	// an image with no shell and no curl, so this is where that works or does
	// not: `healthy` means the binary probed itself and exited 0.
	state, err := waitForHealthState(ctx, cont.name, "healthy")
	if err != nil {
		c.Logf("container log:\n%s", cont.logs())
		return err
	}
	c.Logf("docker reports the container %s", state)
	return nil
}

// dockerAdminLoopbackDefault asserts the other half of the tradeoff: that the
// default really is unreachable from outside, so the setting above is a
// decision an operator makes rather than a formality.
//
// If this ever passes by accident — if the image started binding the admin API
// to every interface by default — the container would be shipping an
// unauthenticated statistics endpoint to whatever network it is attached to.
func dockerAdminLoopbackDefault(c *runner.Ctx) error {
	ctx := c.Context()
	if ok, reason := dockerAvailable(ctx); !ok {
		dockerSkip(c, reason)
		return nil
	}
	if err := ensureImage(ctx, c); err != nil {
		return err
	}

	cont, err := startContainer(ctx, c, uniqueName("loopback"), nil, true)
	if err != nil {
		return err
	}
	defer cont.remove(c)

	if _, readyErr := waitForAtlasCache(ctx, c, cont.clientAddr()); readyErr != nil {
		c.Logf("container log:\n%s", cont.logs())
		return readyErr
	}

	status, body, err := httpGet(ctx, cont.adminURL("/health"), "")
	if err == nil {
		return fmt.Errorf(
			"the admin API answered %d from outside the container with the default configuration: %s"+
				"\nthe default must stay 127.0.0.1 (ADR-0023); reaching it from outside is what a deployment opts into",
			status, clip(body))
	}
	c.Logf("as expected, the default admin bind is unreachable from outside: %v", err)

	// And the container is nonetheless healthy, because HEALTHCHECK runs
	// inside it, where loopback is exactly the right address. This is what
	// makes leaving the default in place a real option for anyone who does not
	// need an external probe.
	if _, healthErr := waitForHealthState(ctx, cont.name, "healthy"); healthErr != nil {
		c.Logf("container log:\n%s", cont.logs())
		return fmt.Errorf("a container with the default loopback admin bind must still report healthy, since its health check runs inside it: %w", healthErr)
	}
	c.Logf("the container is healthy anyway: the health check runs inside it, where loopback reaches the admin API")
	return nil
}

// ---------------------------------------------------------------------------
// docker-nonroot
// ---------------------------------------------------------------------------

// dockerRunsAsNonroot checks the uid the server process actually runs as, not
// the uid the image says it should.
func dockerRunsAsNonroot(c *runner.Ctx) error {
	ctx := c.Context()
	if ok, reason := dockerAvailable(ctx); !ok {
		dockerSkip(c, reason)
		return nil
	}
	if err := ensureImage(ctx, c); err != nil {
		return err
	}

	// The image's declared user first: it is what a `docker run` with no
	// options honors, and what a Kubernetes runAsNonRoot admission check reads.
	declared, err := dockerOutput(ctx, "image", "inspect", e2eImage(), "--format", "{{.Config.User}}")
	if err != nil {
		return fmt.Errorf("inspecting the image user: %w", err)
	}
	declared = strings.TrimSpace(declared)
	if declared == "" || declared == rootUser || strings.HasPrefix(declared, "0:") || declared == "0" {
		return fmt.Errorf("the image declares USER %q, which is root", declared)
	}
	c.Logf("the image declares USER %s", declared)

	cont, err := startContainer(ctx, c, uniqueName("nonroot"), nil, false)
	if err != nil {
		return err
	}
	defer cont.remove(c)

	if _, readyErr := waitForAtlasCache(ctx, c, cont.clientAddr()); readyErr != nil {
		c.Logf("container log:\n%s", cont.logs())
		return readyErr
	}

	// `docker top` reports the uid the daemon sees, which is the only answer
	// that cannot be faked by the image's own /etc/passwd -- and the only one
	// available at all, since the image has no shell to run `id` in.
	top, err := dockerOutput(ctx, "top", cont.name)
	if err != nil {
		return fmt.Errorf("reading the container's process table: %w", err)
	}
	c.Logf("docker top:\n%s", strings.TrimSpace(top))

	uid, err := serverUID(top)
	if err != nil {
		return err
	}
	// Non-zero is the security property, and it is the one that holds
	// everywhere: it is a property of the image rather than of the machine the
	// suite happens to run on.
	if uid == "0" || uid == rootUser {
		return fmt.Errorf("the server process runs as uid %s -- the container is running as root", uid)
	}
	// And the exact uid, because the base image chose it. If this changes, the
	// image's base changed, which is worth knowing at the gate rather than from
	// a Kubernetes securityContext that no longer matches.
	if uid != nonrootUID {
		return fmt.Errorf("the server process runs as uid %s, want %s (distroless nonroot, ADR-0024)", uid, nonrootUID)
	}
	c.Logf("the server process runs as uid %s", uid)

	// What is deliberately *not* asserted here: that the container cannot bind
	// a port below 1024. That is a property of the runtime's
	// net.ipv4.ip_unprivileged_port_start, not of this image -- Docker commonly
	// ships it as 0, which lets uid 65532 bind 443 perfectly happily, while a
	// hardened runtime or a Kubernetes pod without the sysctl allowance refuses
	// it. A spec asserting either outcome would report on the machine CI ran on
	// rather than on AtlasCache. docs/deployment.md states the dependency, and
	// the practical advice that follows from it: 6379 works under both, so keep
	// it inside the container and map a privileged port on the host side.
	return nil
}

// serverUID pulls the server process's uid out of `docker top` output.
//
// The column layout is not fixed: a Linux daemon runs `ps -ef` and leads with
// UID, Docker Desktop's VM answers with PID first and USER second. So the
// header row is what is read, rather than a column index that happens to be
// right on the machine this was written on.
func serverUID(top string) (string, error) {
	lines := strings.Split(strings.TrimSpace(top), "\n")
	if len(lines) < 2 {
		return "", fmt.Errorf("the container has no processes:\n%s", top)
	}

	column := -1
	for i, name := range strings.Fields(lines[0]) {
		if upper := strings.ToUpper(name); upper == "UID" || upper == "USER" {
			column = i
			break
		}
	}
	if column < 0 {
		return "", fmt.Errorf("`docker top` reported no UID or USER column, so the uid cannot be read:\n%s", top)
	}

	row := ""
	for _, line := range lines[1:] {
		if strings.Contains(line, "atlascache") {
			row = line
			break
		}
	}
	if row == "" {
		// No row named the binary: fall back to the first process, which is
		// the entrypoint, rather than reporting nothing.
		row = lines[1]
	}

	fields := strings.Fields(row)
	if column >= len(fields) {
		return "", fmt.Errorf("could not read a uid from:\n%s", top)
	}
	return fields[column], nil
}

// ---------------------------------------------------------------------------
// docker-compose
// ---------------------------------------------------------------------------

// dockerComposeStack brings up the repository's own docker-compose.yml and uses
// the cache it produces.
//
// It runs the real file rather than a copy: the point of the spec is that
// `docker compose up` works for someone who cloned the repository, and a copy
// with the awkward parts adjusted would prove nothing. The two things it does
// change are both about not colliding with the machine it runs on -- a project
// name of its own, and the host ports the compose file already takes from the
// environment, because a laptop that runs this suite very often has something
// on 6379 already.
func dockerComposeStack(c *runner.Ctx) error {
	ctx := c.Context()
	if ok, reason := dockerAvailable(ctx); !ok {
		dockerSkip(c, reason)
		return nil
	}
	if ok, reason := composeAvailable(ctx); !ok {
		dockerSkip(c, reason)
		return nil
	}
	if err := ensureImage(ctx, c); err != nil {
		return err
	}

	stack, err := newComposeStack(ctx, c)
	if err != nil {
		return err
	}
	// Down first thing, and with a context of its own: a spec that has already
	// timed out must not leave a stack running on somebody's machine.
	defer stack.down(c)

	if err := stack.up(ctx, c); err != nil {
		return err
	}
	return stack.usable(ctx, c)
}

// composeStack is one `docker compose` project: the real docker-compose.yml,
// run under a project name of its own and on host ports the kernel handed out.
type composeStack struct {
	root       string
	project    string
	env        []string
	clientPort int
	adminPort  int
}

func newComposeStack(ctx context.Context, c *runner.Ctx) (*composeStack, error) {
	root, err := dockerRepoRoot()
	if err != nil {
		return nil, err
	}

	// The compose file builds `atlascache:local` when it is missing. Tagging
	// the image the suite already built as that name keeps the spec inside its
	// time budget and still runs the compose file exactly as written -- the two
	// images are built from the same tree by the same Dockerfile.
	if out, tagErr := dockerOutput(ctx, "tag", e2eImage(), "atlascache:local"); tagErr != nil {
		return nil, fmt.Errorf("tagging %s as atlascache:local: %w\n%s", e2eImage(), tagErr, firstLine(out))
	}

	clientPort, err := freePort(ctx)
	if err != nil {
		return nil, err
	}
	adminPort, err := freePort(ctx)
	if err != nil {
		return nil, err
	}
	c.Logf("compose will publish 6379 on %d and 8080 on %d", clientPort, adminPort)

	return &composeStack{
		root:    root,
		project: uniqueName("compose"),
		env: append(os.Environ(),
			"ATLASCACHE_CLIENT_PORT="+strconv.Itoa(clientPort),
			"ATLASCACHE_ADMIN_PORT="+strconv.Itoa(adminPort),
		),
		clientPort: clientPort,
		adminPort:  adminPort,
	}, nil
}

func (s *composeStack) run(ctx context.Context, args ...string) (string, error) {
	full := append([]string{
		"compose",
		"--project-name", s.project,
		"--project-directory", s.root,
		"--file", filepath.Join(s.root, "docker-compose.yml"),
	}, args...)
	return dockerOutputEnv(ctx, s.root, s.env, full...)
}

func (s *composeStack) up(ctx context.Context, c *runner.Ctx) error {
	// `config` first: it parses and resolves the file, so a malformed compose
	// file fails here with the parser's own message rather than as a confusing
	// startup failure.
	if out, err := s.run(ctx, "config", "--quiet"); err != nil {
		return fmt.Errorf("docker-compose.yml is not valid: %w\n%s", err, tail(out, 15))
	}

	// --wait blocks until the service is healthy, which means the image's own
	// HEALTHCHECK ran inside a container with no shell and succeeded.
	out, err := s.run(ctx, "up", "--detach", "--wait", "--wait-timeout", "60")
	if err != nil {
		return fmt.Errorf("`docker compose up` failed: %w\n%s", err, tail(out, 25))
	}
	c.Logf("compose up:\n%s", tail(out, 10))
	return nil
}

func (s *composeStack) down(c *runner.Ctx) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if out, err := s.run(ctx, "down", "--volumes", "--remove-orphans"); err != nil {
		c.Logf("compose down failed: %v: %s", err, tail(out, 10))
	}
}

// usable is the assertion the spec exists for: not that compose started
// something, but that what it started is a cache someone can use.
func (s *composeStack) usable(ctx context.Context, c *runner.Ctx) error {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(s.clientPort))

	version, err := waitForAtlasCache(ctx, c, addr)
	if err != nil {
		logs, logErr := s.run(ctx, "logs", "--no-log-prefix", "--tail", "25")
		if logErr != nil {
			logs = "(the compose logs could not be read: " + logErr.Error() + ")"
		}
		c.Logf("compose logs:\n%s", logs)
		return err
	}
	c.Logf("the compose stack serves AtlasCache %s on %s", version, addr)

	if roundTripErr := composeRoundTrip(ctx, addr); roundTripErr != nil {
		return roundTripErr
	}

	// The mounted configuration has to be the one in effect, or the compose
	// file is not configuring anything and the admin settings it documents are
	// decoration. The admin port answering from the host proves both: the mount
	// was read, and the bind address it sets is the reason a probe can reach it
	// at all.
	status, body, err := httpGet(ctx,
		"http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(s.adminPort))+"/health", "")
	if err != nil {
		return fmt.Errorf("the compose stack's admin API is not reachable from the host: %w"+
			"\ndocker/atlascache.yaml sets admin.bind_addr to 0.0.0.0 for exactly this reason (ADR-0023)", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("the compose stack's /health answered %d: %s", status, clip(body))
	}
	c.Logf("the compose stack's /health answers %d from the host: %s", status, strings.TrimSpace(body))
	return nil
}

func composeRoundTrip(ctx context.Context, addr string) error {
	conn, err := client.Dial(ctx, addr)
	if err != nil {
		return fmt.Errorf("connecting to the compose stack: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, setErr := conn.Do(ctx, "SET compose-key compose-value"); setErr != nil {
		return fmt.Errorf("SET against the compose stack: %w", setErr)
	}
	got, err := conn.Do(ctx, "GET compose-key")
	if err != nil {
		return fmt.Errorf("GET against the compose stack: %w", err)
	}
	if got.String() != "compose-value" {
		return fmt.Errorf("GET returned %q, want compose-value", got.String())
	}
	return nil
}

// ---------------------------------------------------------------------------
// Plumbing
// ---------------------------------------------------------------------------

func dockerOutput(ctx context.Context, args ...string) (string, error) {
	return dockerOutputIn(ctx, "", args...)
}

func dockerOutputIn(ctx context.Context, dir string, args ...string) (string, error) {
	return dockerOutputEnv(ctx, dir, nil, args...)
}

// dockerOutputEnv runs one docker command and returns its combined output.
//
// The context bounds it: a docker daemon that has wedged must fail this spec
// rather than hang the run, which is the failure mode a gate can least afford.
func dockerOutputEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, dockerCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = dir
	cmd.Env = env

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// waitForHealthState waits for `docker inspect` to report the health state
// wanted. A container with no health check at all is a failure, not a wait:
// the image is supposed to have one.
func waitForHealthState(ctx context.Context, name, want string) (string, error) {
	deadline := time.Now().Add(containerReadyTimeout)
	last := ""

	for time.Now().Before(deadline) {
		out, err := dockerOutput(ctx, "inspect", name, "--format", "{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}")
		if err != nil {
			return "", fmt.Errorf("inspecting %s: %w", name, err)
		}
		last = strings.TrimSpace(out)
		switch last {
		case want:
			return last, nil
		case "none":
			return "", fmt.Errorf("the image declares no HEALTHCHECK, so nothing probes it")
		case "unhealthy":
			return "", fmt.Errorf("the container is unhealthy: `atlascache --health-check` is failing inside it")
		}
		if err := dockerSleep(ctx, 500*time.Millisecond); err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("the container never reported %s (last: %s)", want, last)
}

// httpGet issues one GET, optionally with a bearer token, and returns the
// status and body.
func httpGet(ctx context.Context, url, token string) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return resp.StatusCode, "", fmt.Errorf("reading the response body: %w", err)
	}
	return resp.StatusCode, string(body), nil
}

// freePort asks the kernel for a port nobody is using. There is a race between
// closing this listener and the container binding the port, and it is the same
// race every test harness that publishes a port runs; the alternative is a
// fixed port, which collides for certain rather than occasionally.
func freePort(ctx context.Context) (int, error) {
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("finding a free port: %w", err)
	}
	defer func() { _ = listener.Close() }()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("a TCP listener reported a %T address", listener.Addr())
	}
	return addr.Port, nil
}

// nameCounter makes uniqueName unique even when two calls land in the same
// nanosecond, which they do: the clock's resolution is coarser than a tight
// loop, and two specs running in parallel are a tight loop.
var nameCounter atomic.Uint64

// uniqueName names a container or compose project so that two runs, or two
// specs running in parallel, never collide.
func uniqueName(kind string) string {
	return fmt.Sprintf("atlascache-e2e-%s-%d-%d", kind, time.Now().UnixNano(), nameCounter.Add(1))
}

// dockerSleep waits, or returns early when the run is canceled. The runner's
// own sleep is unexported, and a poll loop that ignored cancellation would keep
// a wedged docker daemon alive past the spec's deadline.
func dockerSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// clip shortens a response body for a failure message. The suite's own
// truncate is tighter than a JSON body wants, and a health payload cut at 60
// bytes loses the part that says why.
func clip(s string) string {
	const limit = 120
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
