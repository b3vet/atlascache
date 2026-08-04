package scenarios

import (
	"fmt"
	"strings"

	"github.com/b3vet/atlascache/test/e2e/runner"
)

func init() {
	runner.RegisterScenario("stored_values_survive_buffer_reuse", storedValuesSurviveBufferReuse)
}

// bufferProbeSize is comfortably larger than a connection's read buffer, so a
// value cannot help but share backing bytes with the request that follows it.
const bufferProbeSize = 24 * 1024

// storedValuesSurviveBufferReuse is ISSUE-0009 from the outside.
//
// The server reads every request on one connection through one buffer — that is
// what a read buffer is for. An engine that kept a reference to the caller's
// bytes instead of copying them would therefore see stored values rewrite
// themselves as later commands arrive on the same connection, silently and with
// nothing in the logs.
//
// Every command here goes down the harness's single connection, so the writes
// that follow really do land in the buffer the earlier ones were parsed from.
// The values are large and distinct so that a shared backing array shows up as
// a corrupted read rather than as a coincidence.
func storedValuesSurviveBufferReuse(c *runner.Ctx) error {
	values := map[string]string{
		"buf:a": strings.Repeat("A", bufferProbeSize),
		"buf:b": strings.Repeat("B", bufferProbeSize),
		"buf:c": strings.Repeat("C", bufferProbeSize),
	}
	order := []string{"buf:a", "buf:b", "buf:c"}

	for _, key := range order {
		if err := expectOK(c, "SET %s %s", key, values[key]); err != nil {
			return err
		}
	}
	c.Logf("wrote %d keys of %d bytes down one connection", len(order), bufferProbeSize)

	// The first key was parsed out of a buffer that has since been overwritten
	// twice. If the engine kept the caller's slice, it now reads as B's or C's.
	for _, key := range order {
		if err := expectBulk(c, values[key], "GET %s", key); err != nil {
			return err
		}
	}

	// A read hands back the engine's own memory (ADR-0013). Reading twice with
	// unrelated traffic in between is what would expose a view that the next
	// request had scribbled over.
	if err := expectOK(c, "SET buf:d %s", strings.Repeat("D", bufferProbeSize)); err != nil {
		return err
	}
	if err := expectBulk(c, values["buf:a"], "GET buf:a"); err != nil {
		return err
	}

	// The same key overwritten in place: the second write must replace the
	// value outright rather than mutate the bytes the first one is made of.
	if err := expectOK(c, "SET buf:a %s", values["buf:b"]); err != nil {
		return err
	}
	if err := expectBulk(c, values["buf:b"], "GET buf:a"); err != nil {
		return err
	}

	// A value with an embedded NUL and CRLF survives the round trip too: it is
	// the case a length-prefixed protocol exists to carry, and a server that
	// re-scanned the buffer for a terminator would truncate it.
	awkward := "line\r\none\x00two\r\n"
	if err := expectOK(c, `SET buf:e "line\r\none\x00two\r\n"`); err != nil {
		return err
	}
	if err := expectBulk(c, awkward, "GET buf:e"); err != nil {
		return err
	}

	c.Logf("all values read back byte for byte")
	return nil
}

// expectOK sends a command and requires the +OK every write answers with.
func expectOK(c *runner.Ctx, format string, args ...any) error {
	reply, err := send(c, format, args...)
	if err != nil {
		return err
	}
	if reply.Kind != runner.KindStatus || reply.Text != "OK" {
		return fmt.Errorf("%s answered %s %q, want status OK",
			describe(format, args...), reply.Kind, reply.String())
	}
	return nil
}

// expectBulk sends a command and requires a particular bulk string back.
func expectBulk(c *runner.Ctx, want, format string, args ...any) error {
	reply, err := send(c, format, args...)
	if err != nil {
		return err
	}
	if reply.Kind != runner.KindBulk {
		return fmt.Errorf("%s answered %s %q, want a bulk string of %d bytes",
			describe(format, args...), reply.Kind, truncate(reply.String()), len(want))
	}
	if reply.Text != want {
		return fmt.Errorf("%s answered %d bytes (%s), want %d bytes (%s)",
			describe(format, args...), len(reply.Text), truncate(reply.Text), len(want), truncate(want))
	}
	return nil
}

// expectInteger sends a command and requires a particular integer reply.
func expectInteger(c *runner.Ctx, want int64, format string, args ...any) error {
	reply, err := send(c, format, args...)
	if err != nil {
		return err
	}
	if reply.Kind != runner.KindInteger || reply.Integer != want {
		return fmt.Errorf("%s answered %s %q, want the integer %d",
			describe(format, args...), reply.Kind, reply.String(), want)
	}
	return nil
}

// expectNil sends a command and requires the null reply — a cache miss, which
// is never an error.
func expectNil(c *runner.Ctx, format string, args ...any) error {
	reply, err := send(c, format, args...)
	if err != nil {
		return err
	}
	if reply.Kind != runner.KindNil {
		return fmt.Errorf("%s answered %s %q, want nil",
			describe(format, args...), reply.Kind, truncate(reply.String()))
	}
	return nil
}

// send issues a formatted command, turning a transport failure into an error
// that names the command rather than the connection.
func send(c *runner.Ctx, format string, args ...any) (runner.Reply, error) {
	cmd := fmt.Sprintf(format, args...)
	reply, err := c.Send(cmd)
	if err != nil {
		return runner.Reply{}, fmt.Errorf("%s: %w", describe(format, args...), err)
	}
	return reply, nil
}

// describe names a command in failure output without pasting a 24KB value into
// it.
func describe(format string, args ...any) string {
	return truncate(fmt.Sprintf(format, args...))
}

func truncate(s string) string {
	const limit = 60
	if len(s) <= limit {
		return s
	}
	return fmt.Sprintf("%s...<%d bytes>", s[:limit], len(s))
}
