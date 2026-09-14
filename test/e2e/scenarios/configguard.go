package scenarios

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/b3vet/atlascache/test/e2e/runner"
)

func init() {
	runner.RegisterScenario("ttl_both_disabled_refused", ttlBothDisabledRefused)
}

// ttlBothDisabledRefused covers ISSUE-0017.
//
// `ttl.active_expiration: false` with `ttl.lazy_expiration: false` is two
// individually legitimate flags that together reintroduce ISSUE-0007: nothing
// reclaims an expired key, the wheel never runs, the read path never deletes,
// and memory grows without bound. The failure is slow, silent, and looks like a
// leak in AtlasCache rather than a configuration mistake — which is exactly why
// refusing the combination is worth more than warning about it.
//
// Validation used to check fields independently, and that is how this got
// through: neither flag is wrong on its own.
func ttlBothDisabledRefused(c *runner.Ctx) error {
	binary, err := binaryOf(c)
	if err != nil {
		return err
	}

	path := filepath.Join(binary.Root(), "ttl-both-disabled.yaml")
	content := "ttl:\n  active_expiration: false\n  lazy_expiration: false\n"
	if err = os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	started := time.Now()
	code, output, err := binary.RunBinary(c.Context(), "--config", path)
	elapsed := time.Since(started)
	if err != nil {
		return fmt.Errorf("running the server with both expiration flags off: %w", err)
	}
	c.Logf("both flags off: exit %d in %s: %s", code, elapsed.Round(time.Millisecond), strings.TrimSpace(output))

	if code == 0 {
		return errors.New("the server started with nothing to reclaim expired keys; that configuration has no valid use")
	}
	if elapsed > startupBudget {
		return fmt.Errorf("the server took %s to refuse the combination, over the %s budget", elapsed, startupBudget)
	}

	// Both fields, because naming one of them does not tell the operator what
	// to change: either flag would fix it, and the message has to say so.
	for _, want := range []string{"ttl.active_expiration", "ttl.lazy_expiration"} {
		if !strings.Contains(output, want) {
			return fmt.Errorf("the message does not name %q, so it does not say what to fix: %s",
				want, strings.TrimSpace(output))
		}
	}

	// That each flag is legitimate on its own is asserted by two running
	// servers rather than here: this spec's own server has lazy expiration off
	// and is serving, and config-ttl-lazy-only has active expiration off and is
	// serving. Launching a second server from this scenario would need ports of
	// its own and would collide with the one the spec is running.
	return nil
}
