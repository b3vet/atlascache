package scenarios

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/b3vet/atlascache/test/e2e/client"
	"github.com/b3vet/atlascache/test/e2e/runner"
)

func init() {
	runner.RegisterScenario("graceful_shutdown_closes_connections", gracefulShutdownClosesConnections)
	runner.RegisterScenario("restart_preserves_data_dir", restartPreservesDataDir)
}

// shutdownBudget is the graceful window a spec holds the server to. The harness
// allows longer before it resorts to SIGKILL; this is the budget the product
// promises, and it is asserted separately so that a server creeping towards the
// harness's limit is caught while it is still merely slow.
const shutdownBudget = 5 * time.Second

// gracefulShutdownClosesConnections checks what SIGTERM is supposed to do: stop
// the server within the window, and close the connections it was serving rather
// than leaving clients hanging on a socket nobody is reading.
//
// The open connection is this scenario's own, not the harness's, because the
// harness closes its connection before signaling — a client cannot tell a
// graceful shutdown from a hang if it hung up first.
func gracefulShutdownClosesConnections(c *runner.Ctx) error {
	addr := c.Info().ClientAddr
	conn, err := client.Dial(c.Context(), addr)
	if err != nil {
		return fmt.Errorf("opening a connection to %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	reply, err := conn.Do(c.Context(), "PING")
	if err != nil {
		return fmt.Errorf("the connection is not usable before shutdown: %w", err)
	}
	if reply.String() != pong {
		return fmt.Errorf("PING answered %q before shutdown, want PONG", reply.String())
	}
	c.Logf("holding an idle connection from %s", conn.LocalAddr())

	// Block on the idle connection. Nothing more will be sent on it, so the
	// only thing that can end this read is the server closing the connection.
	closed := make(chan error, 1)
	go func() {
		_, err := conn.ReadReply()
		closed <- err
	}()

	started := time.Now()
	if err := c.Stop(); err != nil {
		return fmt.Errorf("SIGTERM: %w", err)
	}
	elapsed := time.Since(started)
	c.Logf("the server exited %s after SIGTERM", elapsed.Round(time.Millisecond))

	if elapsed > shutdownBudget {
		return fmt.Errorf("the server took %s to exit after SIGTERM, over the %s window",
			elapsed.Round(time.Millisecond), shutdownBudget)
	}

	select {
	case err := <-closed:
		if err == nil {
			return errors.New("the idle connection received a reply during shutdown, rather than being closed")
		}
		if !client.IsConnError(err) {
			return fmt.Errorf("the idle connection failed with a protocol error rather than a close: %w", err)
		}
		c.Logf("the idle connection was closed by the server: %v", err)
	case <-time.After(time.Second):
		return errors.New("the server exited without closing the connection it was serving")
	}

	if pid := c.Info().PID; pid != 0 {
		return fmt.Errorf("the server process %d is still running after shutdown", pid)
	}
	if _, err := c.Send("PING"); err == nil {
		return errors.New("the server answered PING after it was shut down")
	}
	return nil
}

// restartPreservesDataDir checks that a restart is a restart of the same node,
// not a fresh one: same data directory, new process, still serving.
//
// The marker file stands in for real state. The walking skeleton stores nothing
// and never touches node.data_dir, so a spec cannot yet assert that data
// survives — P1 gives this something real to check. What it does assert today
// is the property P5's crash-recovery specs depend on: Restart does not discard
// the directory.
func restartPreservesDataDir(c *runner.Ctx) error {
	before := c.Info()
	if before.DataDir == "" {
		return errors.New("the harness reports no data directory")
	}

	marker := filepath.Join(before.DataDir, "restart-marker")
	if err := os.WriteFile(marker, []byte("written before the restart\n"), 0o600); err != nil {
		return fmt.Errorf("writing the marker into %s: %w", before.DataDir, err)
	}
	c.Logf("wrote %s, then restarting pid %d", marker, before.PID)

	if err := c.Restart(); err != nil {
		return fmt.Errorf("restarting: %w", err)
	}

	after := c.Info()
	if after.DataDir != before.DataDir {
		return fmt.Errorf("the data directory changed across the restart: %s -> %s", before.DataDir, after.DataDir)
	}
	if after.PID == before.PID {
		return fmt.Errorf("the process id is still %d, so nothing was restarted", after.PID)
	}
	content, err := os.ReadFile(marker)
	if err != nil {
		return fmt.Errorf("the data directory did not survive the restart: %w", err)
	}
	c.Logf("pid %d -> %d, %s still holds %q", before.PID, after.PID, marker, string(content))

	reply, err := c.Send("PING")
	if err != nil {
		return fmt.Errorf("the restarted server did not answer PING: %w", err)
	}
	if reply.String() != pong {
		return fmt.Errorf("the restarted server answered PING with %q, want PONG", reply.String())
	}
	return nil
}
