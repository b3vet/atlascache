package scenarios

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/b3vet/atlascache/test/e2e/runner"
)

func init() {
	runner.RegisterScenario("unknown_config_key_refuses_startup", unknownConfigKeyRefusesStartup)
	runner.RegisterScenario("reload_keeps_running_config_on_unknown_key", reloadKeepsRunningConfigOnUnknownKey)
}

// unknownConfigKeyRefusesStartup covers ISSUE-0024.
//
// A key outside the schema used to be dropped in silence, and the option kept
// its default: `server.port` started a server on 6379 and `auth.enbaled: true`
// started one with authentication off, both exiting 0 and both reporting
// themselves healthy. The refusal is the fix, and the message is most of its
// value — an error naming only "invalid config" sends an operator back to
// guessing at the very moment they were confident they had it right.
func unknownConfigKeyRefusesStartup(c *runner.Ctx) error {
	binary, err := binaryOf(c)
	if err != nil {
		return err
	}

	cases := []struct {
		name    string
		content string
		// mentions is what the message must contain to be actionable: the key
		// at fault, where it sits, and what was probably meant.
		mentions []string
	}{
		{
			// The report itself: the real key is server.client_port.
			name:     "unknown-server-key.yaml",
			content:  "server:\n  port: 7911\n",
			mentions: []string{`unknown field "port"`, "in server", `did you mean "client_port"`},
		},
		{
			// The one that matters: this config runs with auth off.
			name:     "misspelled-auth-enabled.yaml",
			content:  "auth:\n  enbaled: true\n  token: \"e2e-unknown-keys-token\"\n",
			mentions: []string{`unknown field "enbaled"`, "in auth", `did you mean "enabled"`},
		},
		{
			// Every unknown key at once: an operator restarting once per typo
			// is the experience the fix exists to replace.
			name:     "several-unknown-keys.yaml",
			content:  "server:\n  max_conections: 50000\nauth:\n  enbaled: true\nlgging:\n  level: debug\n",
			mentions: []string{"3 unknown fields", "max_conections", "enbaled", "lgging"},
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
			return fmt.Errorf("%s: the server started with a key it does not understand and exited 0", tc.name)
		}
		if elapsed > startupBudget {
			return fmt.Errorf("%s: the server took %s to reject an unknown key, over the %s budget",
				tc.name, elapsed.Round(time.Millisecond), startupBudget)
		}
		// The line number is what turns the message into a jump rather than a
		// search, and the file has to be named for it to mean anything.
		if !strings.Contains(output, path+":") {
			return fmt.Errorf("%s: the message does not locate the key as file:line: %s",
				tc.name, strings.TrimSpace(output))
		}
		for _, want := range tc.mentions {
			if !strings.Contains(output, want) {
				return fmt.Errorf("%s: the message does not mention %q, so it does not say what to fix: %s",
					tc.name, want, strings.TrimSpace(output))
			}
		}
	}

	return nil
}

// reloadKeepsRunningConfigOnUnknownKey is the other half of ISSUE-0024, and the
// half that could turn the fix into an outage.
//
// Refusing to start on a config the server cannot honor is right. Refusing to
// *keep running* on one is not: a reload is triggered by a file write and by
// SIGHUP, and neither is something an operator expects to be able to take a
// live node down with. A rejected reload therefore logs, keeps the running
// configuration, and serves.
func reloadKeepsRunningConfigOnUnknownKey(c *runner.Ctx) error {
	binary, err := binaryOf(c)
	if err != nil {
		return err
	}

	path := filepath.Join(binary.Root(), "config.yaml")
	original, err := readConfigFile(path)
	if err != nil {
		return err
	}
	// Restored whichever way this returns, so a failure partway through does
	// not leave a broken config behind for a later restart in the same spec.
	defer restoreConfigFile(c, path, original)

	pidBefore := c.Info().PID
	policyBefore, err := effectivePolicy(c)
	if err != nil {
		return err
	}
	c.Logf("running on pid %d with eviction.policy %q", pidBefore, policyBefore)

	if err = rejectedReloadChangesNothing(c, path, original, pidBefore, policyBefore); err != nil {
		return err
	}
	return aValidReloadStillApplies(c, path, original, pidBefore)
}

// rejectedReloadChangesNothing sends a config carrying a typo and checks that
// the server logged it, kept what it was running, and kept serving.
func rejectedReloadChangesNothing(c *runner.Ctx, path string, original []byte, pidBefore int, policyBefore string) error {
	broken, err := withUnknownKey(original)
	if err != nil {
		return err
	}

	mark := len(c.Harness.Logs())
	if err = writeConfigFile(path, broken); err != nil {
		return err
	}
	// SIGHUP explicitly, rather than relying on the write the file watcher
	// also sees: the signal is the operator-facing reload, and it is the one
	// ISSUE-0024 requires to be survivable. Both paths end in the same reload.
	if err = reloadSignal(pidBefore); err != nil {
		return err
	}

	// The rejection has to name the key, for the same reason startup does: a
	// log line saying only that a reload failed leaves the operator with a
	// running server and no idea which of their edits was refused.
	if err = waitForLogAfter(c, mark, "config reload rejected"); err != nil {
		return fmt.Errorf("the server never reported rejecting the reload: %w", err)
	}
	if err = waitForLogAfter(c, mark, "enbaled"); err != nil {
		return fmt.Errorf("the rejection does not name the offending key: %w", err)
	}

	// Still serving, still the same process — not restarted, not degraded.
	if err = stillAnswering(c); err != nil {
		return fmt.Errorf("after a rejected reload: %w", err)
	}
	if pidAfter := c.Info().PID; pidAfter != pidBefore {
		return fmt.Errorf("the server was pid %d before the rejected reload and is pid %d after it",
			pidBefore, pidAfter)
	}

	// And still running the configuration it started with. The broken file
	// also changed the eviction policy, so a server that had applied any part
	// of it would report the new one here.
	policyAfter, err := effectivePolicy(c)
	if err != nil {
		return err
	}
	if policyAfter != policyBefore {
		return fmt.Errorf("a rejected reload changed eviction.policy from %q to %q; it must change nothing",
			policyBefore, policyAfter)
	}
	c.Logf("rejected reload left pid %d serving on eviction.policy %q", pidBefore, policyAfter)
	return nil
}

// aValidReloadStillApplies checks the watcher is not wedged by a rejection. One
// that stopped later reloads from being seen would make the first typo
// permanent until a restart, which is a subtler version of the same outage.
func aValidReloadStillApplies(c *runner.Ctx, path string, original []byte, pid int) error {
	mark := len(c.Harness.Logs())
	if err := writeConfigFile(path, original); err != nil {
		return err
	}
	if err := reloadSignal(pid); err != nil {
		return err
	}
	if err := waitForLogAfter(c, mark, "config reloaded successfully"); err != nil {
		return fmt.Errorf("after a rejected reload, a valid one was not applied: %w", err)
	}
	return stillAnswering(c)
}

// readConfigFile reads the config file the running server was started with.
func readConfigFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the running server's config at %s: %w", path, err)
	}
	return data, nil
}

// writeConfigFile replaces the running server's config file, which is what a
// reload is triggered by.
func writeConfigFile(path string, data []byte) error {
	// The path is the harness's own config file, inside a directory it created
	// and removes.
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// restoreConfigFile puts the original config back, reporting rather than
// returning a failure: it runs in a defer, where the scenario's own verdict
// has already been decided.
func restoreConfigFile(c *runner.Ctx, path string, original []byte) {
	if err := writeConfigFile(path, original); err != nil {
		c.Logf("restoring the original config failed: %v", err)
	}
}

// withUnknownKey returns the running config with a typo in it, and with one
// visible setting changed so that a server applying any of it can be caught.
func withUnknownKey(original []byte) ([]byte, error) {
	var document map[string]any
	if err := yaml.Unmarshal(original, &document); err != nil {
		return nil, fmt.Errorf("parsing the running server's config: %w", err)
	}

	// Dropped before the block below is appended, so the file cannot end up
	// with two auth keys — which is a different error than the one under test.
	delete(document, "auth")
	document["eviction"] = map[string]any{"policy": "fifo", "sample_size": 100}

	rendered, err := yaml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("rendering the broken config: %w", err)
	}
	return append(rendered, []byte("auth:\n  enbaled: true\n  token: \"e2e-reload-token\"\n")...), nil
}

// effectivePolicy reads eviction.policy back from the admin API, which reports
// the configuration the server is actually running.
func effectivePolicy(c *runner.Ctx) (string, error) {
	tokens, err := configuredAdminTokens(c)
	if err != nil {
		return "", err
	}
	if tokens.admin == "" {
		return "", errors.New("this scenario needs a spec that sets " + adminTokenField)
	}

	resp, err := adminGet(c, "/config", bearer(tokens.admin))
	if err != nil {
		return "", err
	}
	if resp.status != http.StatusOK {
		return "", fmt.Errorf("GET /config returned %d: %s", resp.status, strings.TrimSpace(resp.body))
	}

	fields, err := resp.fields()
	if err != nil {
		return "", err
	}
	eviction, ok := fields["eviction"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("/config has no eviction section: %s", resp.body)
	}
	policy, ok := eviction["policy"].(string)
	if !ok {
		return "", fmt.Errorf("/config reports eviction.policy as %v, which is not a policy name", eviction["policy"])
	}
	return policy, nil
}

// stillAnswering checks the server answers, which is the whole claim a rejected
// reload makes: nothing happened.
func stillAnswering(c *runner.Ctx) error {
	reply, err := c.Send("PING")
	if err != nil {
		return fmt.Errorf("the server stopped serving: %w", err)
	}
	if reply.String() != pong {
		return fmt.Errorf("PING answered %q, want %s", reply.String(), pong)
	}
	return nil
}

// reloadSignal sends the reload signal to the server process.
func reloadSignal(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("finding the server process %d: %w", pid, err)
	}
	if err := process.Signal(syscall.SIGHUP); err != nil {
		return fmt.Errorf("sending SIGHUP to pid %d: %w", pid, err)
	}
	return nil
}

// waitForLogAfter waits for want to appear in the server log past offset.
//
// Past it, rather than anywhere in it, because the same line is logged by every
// reload: a check over the whole log would pass on a rejection that happened
// before the one being tested.
func waitForLogAfter(c *runner.Ctx, offset int, want string) error {
	deadline := time.Now().Add(logBudget)
	for {
		logs := c.Harness.Logs()
		if offset < len(logs) && strings.Contains(logs[offset:], want) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the server did not log %q within %s", want, logBudget)
		}
		if err := c.Sleep(pollInterval); err != nil {
			return err
		}
	}
}
