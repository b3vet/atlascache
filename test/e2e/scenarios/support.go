package scenarios

import (
	"context"
	"fmt"

	"github.com/b3vet/atlascache/test/e2e/runner"
)

// pong is what a healthy server answers PING with.
const pong = "PONG"

// serverBinary is the escape hatch for the handful of behaviors that happen
// outside the protocol: a config that must be rejected before a server exists,
// and --version, which prints and exits.
//
// It is a type assertion rather than an import so the scenarios stay independent
// of any one harness. A scenario needing it says so by failing clearly when the
// harness it was handed cannot provide it — the fake harness in the runner's own
// tests, for instance.
type serverBinary interface {
	// RunBinary runs the server binary out of band and reports its exit code
	// and combined output.
	RunBinary(ctx context.Context, args ...string) (int, string, error)
	// Root is a directory the harness owns and removes on cleanup, for
	// scratch files such as a deliberately broken config.
	Root() string
}

func binaryOf(c *runner.Ctx) (serverBinary, error) {
	binary, ok := c.Harness.(serverBinary)
	if !ok {
		return nil, fmt.Errorf(
			"this scenario needs a harness that owns the server binary, and %T does not; run it against --harness process",
			c.Harness)
	}
	return binary, nil
}
