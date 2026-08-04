package client_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/b3vet/atlascache/test/e2e/client"
	"github.com/b3vet/atlascache/test/e2e/runner"
)

// scriptedServer answers every connection with a fixed byte string, so a reply
// shape can be tested without a server that produces it. The request each
// connection sent is recorded, which is how the request encoding is checked.
type scriptedServer struct {
	addr     string
	requests chan string
}

// listen opens a loopback listener on a free port.
func listen(t *testing.T) net.Listener {
	t.Helper()
	var config net.ListenConfig
	ln, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

func serveScript(t *testing.T, reply string) *scriptedServer {
	t.Helper()

	ln := listen(t)
	t.Cleanup(func() { _ = ln.Close() })

	server := &scriptedServer{addr: ln.Addr().String(), requests: make(chan string, 8)}
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				reader := bufio.NewReader(conn)
				for {
					request, err := readRequest(reader)
					if err != nil {
						return
					}
					server.requests <- request
					if _, err := io.WriteString(conn, reply); err != nil {
						return
					}
				}
			}()
		}
	}()
	return server
}

// readRequest reads one RESP array of bulk strings and returns it as text.
func readRequest(r *bufio.Reader) (string, error) {
	header, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(header, "*") {
		return "", errors.New("not an array: " + header)
	}
	count, err := parseCount(strings.TrimSpace(header[1:]))
	if err != nil {
		return "", err
	}

	var args []string
	for i := 0; i < count; i++ {
		sizeLine, err := r.ReadString('\n')
		if err != nil {
			return "", err
		}
		size, err := parseCount(strings.TrimSpace(sizeLine[1:]))
		if err != nil {
			return "", err
		}
		payload := make([]byte, size+2)
		if _, err := io.ReadFull(r, payload); err != nil {
			return "", err
		}
		args = append(args, string(payload[:size]))
	}
	return strings.Join(args, "|"), nil
}

func parseCount(s string) (int, error) {
	value := 0
	sign := 1
	for i, c := range s {
		if i == 0 && c == '-' {
			sign = -1
			continue
		}
		if c < '0' || c > '9' {
			return 0, errors.New("not a number: " + s)
		}
		value = value*10 + int(c-'0')
	}
	return sign * value, nil
}

func dialScript(t *testing.T, reply string) (*client.Client, *scriptedServer, context.Context) {
	t.Helper()

	server := serveScript(t, reply)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	conn, err := client.Dial(ctx, server.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, server, ctx
}

func TestDecodesEveryReplyType(t *testing.T) {
	tests := []struct {
		name  string
		wire  string
		kind  runner.ReplyKind
		text  string
		check func(*testing.T, runner.Reply)
	}{
		{name: "simple string", wire: "+PONG\r\n", kind: runner.KindStatus, text: "PONG"},
		{name: "error", wire: "-ERR unknown command 'FROB'\r\n", kind: runner.KindError, text: "ERR unknown command 'FROB'"},
		{name: "integer", wire: ":42\r\n", kind: runner.KindInteger, text: "42"},
		{name: "negative integer", wire: ":-1\r\n", kind: runner.KindInteger, text: "-1"},
		{name: "bulk string", wire: "$5\r\nhello\r\n", kind: runner.KindBulk, text: "hello"},
		{name: "empty bulk string", wire: "$0\r\n\r\n", kind: runner.KindBulk, text: ""},
		{name: "bulk string with CRLF", wire: "$4\r\na\r\nb\r\n", kind: runner.KindBulk, text: "a\r\nb"},
		{name: "null bulk string", wire: "$-1\r\n", kind: runner.KindNil, text: "NIL"},
		{name: "null array", wire: "*-1\r\n", kind: runner.KindNil, text: "NIL"},
		{name: "empty array", wire: "*0\r\n", kind: runner.KindArray, text: "[]"},
		{name: "array", wire: "*2\r\n$1\r\na\r\n:7\r\n", kind: runner.KindArray, text: "[a 7]"},
		{name: "nested array", wire: "*1\r\n*2\r\n+a\r\n+b\r\n", kind: runner.KindArray, text: "[[a b]]"},
		{name: "resp3 null", wire: "_\r\n", kind: runner.KindNil, text: "NIL"},
		{name: "resp3 true", wire: "#t\r\n", kind: runner.KindInteger, text: "1"},
		{name: "resp3 false", wire: "#f\r\n", kind: runner.KindInteger, text: "0"},
		{name: "resp3 double", wire: ",1.5\r\n", kind: runner.KindBulk, text: "1.5"},
		{name: "resp3 big number", wire: "(3492890328409238509324850943850943825024385\r\n", kind: runner.KindBulk,
			text: "3492890328409238509324850943850943825024385"},
		{name: "resp3 blob error", wire: "!21\r\nSYNTAX invalid syntax\r\n", kind: runner.KindError, text: "SYNTAX invalid syntax"},
		{name: "resp3 verbatim", wire: "=15\r\ntxt:hello world\r\n", kind: runner.KindBulk, text: "hello world"},
		{name: "resp3 set", wire: "~2\r\n+a\r\n+b\r\n", kind: runner.KindArray, text: "[a b]"},
		{
			name: "resp3 map", wire: "%2\r\n+keys\r\n:2\r\n+hits\r\n:9\r\n", kind: runner.KindMap,
			check: func(t *testing.T, reply runner.Reply) {
				fields := reply.Fields()
				if fields["keys"] != "2" || fields["hits"] != "9" {
					t.Errorf("fields = %v", fields)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn, _, ctx := dialScript(t, tc.wire)
			reply, err := conn.Do(ctx, "PING")
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			if reply.Kind != tc.kind {
				t.Fatalf("kind = %s, want %s", reply.Kind, tc.kind)
			}
			if tc.check != nil {
				tc.check(t, reply)
				return
			}
			if got := reply.String(); got != tc.text {
				t.Errorf("reply = %q, want %q", got, tc.text)
			}
		})
	}
}

func TestEncodesCommandsAsArraysOfBulkStrings(t *testing.T) {
	conn, server, ctx := dialScript(t, "+OK\r\n")

	if _, err := conn.Do(ctx, `SET k1 "hello world"`); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := <-server.requests; got != "SET|k1|hello world" {
		t.Errorf("request = %q", got)
	}

	if _, err := conn.Call(ctx, "SET", "k2", ""); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got := <-server.requests; got != "SET|k2|" {
		t.Errorf("request = %q", got)
	}
}

func TestRejectsMalformedReplies(t *testing.T) {
	tests := []struct {
		name string
		wire string
		want string
	}{
		{"unknown type", "?what\r\n", "unknown reply type"},
		{"bare LF", "+PONG\n", "CRLF"},
		{"non-numeric length", "$abc\r\n", "not a number"},
		{"bulk not terminated", "$1\r\nabxx", "CRLF"},
		{"null with a payload", "_x\r\n", "null carries data"},
		{"bad boolean", "#maybe\r\n", "want t or f"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn, _, ctx := dialScript(t, tc.wire)
			_, err := conn.Do(ctx, "PING")
			if err == nil {
				t.Fatal("a malformed reply was accepted")
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

func TestErrorRepliesAreValuesNotErrors(t *testing.T) {
	conn, _, ctx := dialScript(t, "-ERR nope\r\n")

	reply, err := conn.Do(ctx, "FROB")
	if err != nil {
		t.Fatalf("an error reply must not be a Go error, got %v", err)
	}
	if reply.Kind != runner.KindError || reply.Text != "ERR nope" {
		t.Errorf("reply = %s %q", reply.Kind, reply.Text)
	}
}

func TestLostConnectionIsATransportError(t *testing.T) {
	ln := listen(t)
	addr := ln.Addr().String()
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		_ = conn.Close() // hang up without answering
	}()

	ctx := context.Background()
	conn, err := client.Dial(ctx, addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = ln.Close()

	if _, err := conn.Do(ctx, "PING"); err == nil {
		t.Fatal("a command on a hung-up connection reported success")
	} else if !client.IsConnError(err) {
		t.Errorf("error = %v, want a transport error", err)
	}
}

func TestDialFailureIsATransportError(t *testing.T) {
	ln := listen(t)
	addr := ln.Addr().String()
	_ = ln.Close() // nothing is listening there now

	if _, err := client.Dial(context.Background(), addr); err == nil {
		t.Fatal("dialing a closed port reported success")
	} else if !client.IsConnError(err) {
		t.Errorf("error = %v, want a transport error", err)
	}
}

func TestUseAfterCloseFails(t *testing.T) {
	conn, _, ctx := dialScript(t, "+PONG\r\n")
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Errorf("Close is not idempotent: %v", err)
	}
	if _, err := conn.Do(ctx, "PING"); !client.IsConnError(err) {
		t.Errorf("error = %v, want a transport error", err)
	}
}

func TestEmptyCommandIsRejectedBeforeTheWire(t *testing.T) {
	conn, _, ctx := dialScript(t, "+PONG\r\n")
	if _, err := conn.Do(ctx, "   "); err == nil {
		t.Error("an empty command was sent")
	}
	if _, err := conn.Call(ctx); err == nil {
		t.Error("a command with no arguments was sent")
	}
}

func TestDeadlineComesFromTheContext(t *testing.T) {
	ln := listen(t)
	defer func() { _ = ln.Close() }()
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		time.Sleep(5 * time.Second) // never answers in time
	}()

	conn, err := client.Dial(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	started := time.Now()
	if _, err := conn.Do(ctx, "PING"); err == nil {
		t.Fatal("a command against a silent server reported success")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("the command took %s; the context deadline was 100ms", elapsed)
	}
}
