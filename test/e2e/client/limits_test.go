package client_test

import (
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/b3vet/atlascache/test/e2e/client"
)

// TestRefusesRepliesTooBigToBeAValue. This client is a test tool pointed at a
// server that may be broken in the way under test, so a length header is not
// something to trust: a bogus one must be refused before anything is allocated
// for it. A client that believed a 4GB bulk header would take the whole run down
// with it instead of failing one spec with a legible message.
func TestRefusesRepliesTooBigToBeAValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		wire string
		want string
	}{
		{
			name: "a bulk string longer than the limit",
			wire: "$67108865\r\n",
			want: "exceeds the",
		},
		{
			name: "an array claiming more elements than could exist",
			wire: "*67108865\r\n",
			want: "implausible",
		},
		{
			name: "a map claiming more pairs than could exist",
			wire: "%67108865\r\n",
			want: "implausible",
		},
		{
			name: "a line longer than the read buffer",
			wire: "+" + strings.Repeat("x", 70<<10) + "\r\n",
			want: "line exceeds the",
		},
		{
			name: "a reply nested deeper than the limit",
			wire: strings.Repeat("*1\r\n", 40) + "+deep\r\n",
			want: "nests deeper",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			conn, _, ctx := dialScript(t, tc.wire)
			reply, err := conn.Do(ctx, "PING")
			if err == nil {
				t.Fatalf("the client accepted the reply as %s %q", reply.Kind, reply.String())
			}
			if !client.IsProtocolError(err) {
				t.Fatalf("error = %v, want a protocol error: a bogus header is a bug to fail on, not a blip to retry", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestAggregateRepliesCarryTheirElementsErrors. A malformed element inside an
// array or a map has to fail the whole reply. Swallowing it would hand the
// runner a short array or a map missing a pair, and an expect_field assertion
// would then report a field as absent when it was really unreadable.
func TestAggregateRepliesCarryTheirElementsErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		wire string
		want string
	}{
		{name: "an array element that is not a reply", wire: "*2\r\n+ok\r\n?bogus\r\n", want: "unknown reply type"},
		{name: "a map key that is not a reply", wire: "%1\r\n?bogus\r\n+value\r\n", want: "unknown reply type"},
		{name: "a map value that is not a reply", wire: "%1\r\n+key\r\n?bogus\r\n", want: "unknown reply type"},
		{name: "a map length that is not a number", wire: "%many\r\n", want: "map length"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			conn, _, ctx := dialScript(t, tc.wire)
			reply, err := conn.Do(ctx, "STATS")
			if err == nil {
				t.Fatalf("a malformed aggregate was accepted as %s %q", reply.Kind, reply.String())
			}
			if !client.IsProtocolError(err) {
				t.Fatalf("error = %v, want a protocol error", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestNullAggregatesReadAsNil. A RESP2 server answers a missing collection with
// a negative length rather than an empty one, and the runner models that as NIL
// — the same thing a spec writes. Reading it as an empty array instead would
// make `expect: NIL` fail against a correct server.
func TestNullAggregatesReadAsNil(t *testing.T) {
	t.Parallel()

	for name, wire := range map[string]string{
		"a null array": "*-1\r\n",
		"a null map":   "%-1\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			conn, _, ctx := dialScript(t, wire)
			reply, err := conn.Do(ctx, "STATS")
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			if got := reply.String(); got != "NIL" {
				t.Errorf("reply = %q, want NIL", got)
			}
		})
	}
}

// TestLocalAddrIdentifiesTheConnection. A scenario logs the local address so a
// failure can be lined up against the server's own log for that connection.
// After Close there is nothing to name, and asking anyway must not panic: the
// graceful-shutdown scenario logs the address of a connection it is about to
// lose.
func TestLocalAddrIdentifiesTheConnection(t *testing.T) {
	t.Parallel()

	conn, server, _ := dialScript(t, "+PONG\r\n")

	addr := conn.LocalAddr()
	if addr == "" {
		t.Fatal("LocalAddr is empty for an open connection")
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("LocalAddr = %q, want the loopback address of this side", addr)
	}
	if addr == server.addr {
		t.Error("LocalAddr reported the server's address rather than the client's")
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := conn.LocalAddr(); got != "" {
		t.Errorf("LocalAddr after Close = %q, want empty", got)
	}

	var never *client.Client
	if got := never.LocalAddr(); got != "" {
		t.Errorf("LocalAddr on a client that was never dialed = %q, want empty", got)
	}
}

// TestConnErrorCarriesTheCauseTheHarnessSwitchesOn. The harness reconnects on a
// transport error and fails the spec on anything else, and it decides with
// errors.As. A ConnError that did not unwrap would be indistinguishable from a
// protocol bug, so a dropped connection would fail a spec that should have
// retried it — and a protocol bug that unwrapped to a network error would be
// retried forever instead of reported.
func TestConnErrorCarriesTheCauseTheHarnessSwitchesOn(t *testing.T) {
	t.Parallel()

	err := &client.ConnError{Op: "dial 127.0.0.1:1", Err: net.ErrClosed}

	if got, want := err.Error(), "dial 127.0.0.1:1: "+net.ErrClosed.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, net.ErrClosed) {
		t.Error("a ConnError does not unwrap to its cause, so callers cannot tell one network failure from another")
	}
	if !client.IsConnError(err) {
		t.Error("IsConnError does not recognize a ConnError")
	}
	if client.IsProtocolError(err) {
		t.Error("a transport failure must not also look like a wire-format violation")
	}

	// And the classification survives wrapping, which is how the harness sees
	// it after a scenario has added context of its own.
	wrapped := errors.Join(errors.New("sending PING"), err)
	if !client.IsConnError(wrapped) {
		t.Error("a wrapped ConnError is no longer recognized as a transport failure")
	}
}
