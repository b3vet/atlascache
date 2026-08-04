// Package scenarios holds the Go escape hatch specs reach for when a case
// needs real control flow — timing, concurrency, crash recovery.
//
// Every scenario registers itself in init, and a spec invokes one by name.
// The runner validates those names while parsing, so a typo fails before a
// server is started. Import this package for its side effects wherever specs
// are parsed:
//
//	import _ "github.com/b3vet/atlascache/test/e2e/scenarios"
package scenarios

import (
	"fmt"

	"github.com/b3vet/atlascache/test/e2e/runner"
)

func init() {
	runner.RegisterScenario("crash_and_recover", crashAndRecover)
}

// crashAndRecover kills the server outright and checks that a fresh start
// against the same data directory comes back serving. It is the crash half of
// crash recovery; what survives the crash is asserted by the spec's later
// steps, which run against the restarted server.
func crashAndRecover(c *runner.Ctx) error {
	if err := c.Kill(); err != nil {
		return fmt.Errorf("killing the server: %w", err)
	}
	c.Logf("sent SIGKILL")

	if err := c.Start(); err != nil {
		return fmt.Errorf("restarting after the kill: %w", err)
	}
	c.Logf("server restarted")

	reply, err := c.Send("PING")
	if err != nil {
		return fmt.Errorf("the restarted server did not answer PING: %w", err)
	}
	if reply.Kind == runner.KindError {
		return fmt.Errorf("the restarted server answered PING with an error: %s", reply.Text)
	}
	if got := reply.String(); got != pong {
		return fmt.Errorf("the restarted server answered PING with %q, want PONG", got)
	}
	c.Logf("server answered PING")
	return nil
}
