package scenarios

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/b3vet/atlascache/test/e2e/runner"
)

func init() {
	runner.RegisterScenario("health_endpoint_ready", healthEndpointReady)
	runner.RegisterScenario("invalid_config_fails_fast", invalidConfigFailsFast)
	runner.RegisterScenario("version_flag_reports_build", versionFlagReportsBuild)
}

// startupBudget is how long a server that cannot start is allowed to take to
// say so. Failing fast is the requirement; a server that hangs on a bad config
// is as bad as one that starts with it.
const startupBudget = 5 * time.Second

// healthEndpointReady checks the admin health endpoint directly. The harness
// polls it to decide readiness, so a spec that only sent commands would never
// notice /health regressing until every spec in the suite mysteriously timed
// out at startup.
func healthEndpointReady(c *runner.Ctx) error {
	url := "http://" + c.Info().AdminAddr + "/health"

	req, err := http.NewRequestWithContext(c.Context(), http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Errorf("reading the response: %w", err)
	}
	c.Logf("GET %s -> %d %s", url, resp.StatusCode, strings.TrimSpace(string(body)))

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s returned %d, want 200", url, resp.StatusCode)
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		return fmt.Errorf("GET %s returned Content-Type %q, want JSON", url, contentType)
	}
	if !strings.Contains(string(body), `"status":"ok"`) {
		return fmt.Errorf(`GET %s returned %q, want a body reporting "status":"ok"`, url, body)
	}
	return nil
}

// invalidConfigFailsFast checks that a server given a config it cannot honor
// says so and exits, rather than starting in some half-configured state.
//
// It runs the binary out of band: the harness's own server is already up with a
// valid config, and the point of the check is what happens when the config is
// not valid.
func invalidConfigFailsFast(c *runner.Ctx) error {
	binary, err := binaryOf(c)
	if err != nil {
		return err
	}

	cases := []struct {
		name    string
		content string
		// mentions is what the message must contain to be actionable: the
		// field at fault, not merely the fact that something was wrong.
		mentions []string
	}{
		{
			name:     "unknown-eviction-policy.yaml",
			content:  "eviction:\n  policy: telepathy\n",
			mentions: []string{"eviction.policy", "lru"},
		},
		{
			name:     "port-out-of-range.yaml",
			content:  "server:\n  client_port: 70000\n",
			mentions: []string{"server.client_port", "65535"},
		},
		{
			name:     "not-yaml.yaml",
			content:  "server: [this: is not\n  valid yaml\n",
			mentions: []string{"config"},
		},
	}

	for _, tc := range cases {
		path := filepath.Join(binary.Root(), tc.name)
		if writeErr := os.WriteFile(path, []byte(tc.content), 0o600); writeErr != nil {
			return fmt.Errorf("writing %s: %w", tc.name, writeErr)
		}

		started := time.Now()
		code, output, runErr := binary.RunBinary(c.Context(), "--config", path)
		elapsed := time.Since(started)
		if runErr != nil {
			return fmt.Errorf("running the server with %s: %w", tc.name, runErr)
		}
		c.Logf("%s: exit %d in %s: %s", tc.name, code, elapsed.Round(time.Millisecond), strings.TrimSpace(output))

		if code == 0 {
			return fmt.Errorf("%s: the server started with an invalid config and exited 0", tc.name)
		}
		if elapsed > startupBudget {
			return fmt.Errorf("%s: the server took %s to reject an invalid config, over the %s budget",
				tc.name, elapsed.Round(time.Millisecond), startupBudget)
		}
		for _, want := range tc.mentions {
			if !strings.Contains(output, want) {
				return fmt.Errorf("%s: the message does not mention %q, so it does not say what to fix: %s",
					tc.name, want, strings.TrimSpace(output))
			}
		}
	}

	// A config file that was asked for by name and does not exist must be an
	// error too: silently falling back to defaults would run a server nobody
	// configured.
	missing := filepath.Join(binary.Root(), "does-not-exist.yaml")
	code, output, err := binary.RunBinary(c.Context(), "--config", missing)
	if err != nil {
		return fmt.Errorf("running the server with a missing config: %w", err)
	}
	c.Logf("missing config: exit %d: %s", code, strings.TrimSpace(output))
	if code == 0 {
		return errors.New("the server started with a config file that does not exist")
	}
	if !strings.Contains(output, missing) {
		return fmt.Errorf("the message does not name the missing file: %s", strings.TrimSpace(output))
	}
	return nil
}

// versionFlagReportsBuild checks that --version prints something a bug report
// can be traced with, and exits without starting a server.
//
// The assertions are containment, not equality: the version, the commit and the
// build date are injected at link time and are different on every build.
func versionFlagReportsBuild(c *runner.Ctx) error {
	binary, err := binaryOf(c)
	if err != nil {
		return err
	}

	code, output, err := binary.RunBinary(c.Context(), "--version")
	if err != nil {
		return fmt.Errorf("running --version: %w", err)
	}
	c.Logf("--version: exit %d: %s", code, strings.TrimSpace(output))

	if code != 0 {
		return fmt.Errorf("--version exited %d, want 0: %s", code, strings.TrimSpace(output))
	}
	if !strings.HasPrefix(output, "atlascache ") {
		return fmt.Errorf("--version printed %q, want it to start with the program name", strings.TrimSpace(output))
	}
	for _, want := range []string{"commit:", "built:"} {
		if !strings.Contains(output, want) {
			return fmt.Errorf("--version does not report %s, so a build cannot be traced: %s",
				want, strings.TrimSpace(output))
		}
	}
	return nil
}
