//go:build ignore

// This file carries the `ignore` build tag, and it is not a mistake.
//
// These programs live in the root module, so without the tag they would be
// swept up by `go test ./...` — as packages with no test files, contributing
// several hundred uncovered statements each and pulling the module below the
// coverage floor the phase gate enforces. The tag keeps them out of `./...`
// while leaving them buildable, runnable and vettable by name:
//
//	go run  ./examples/errors/main.go -addr 127.0.0.1:6379
//	go vet  ./examples/errors/main.go
//
// examples/run.sh builds and runs every one of them against a real server on
// every CI run, which is the check that actually matters here.

// Command errors produces one failure of every category the SDK defines and
// classifies each one the way a caller should: with errors.Is against a
// category sentinel, never by matching on the message.
//
//	go run ./examples/errors/main.go -addr 127.0.0.1:6379
//
// The authentication case needs a server with auth enabled. Pass one with
// -auth-addr to see it; without it that case is reported as skipped.
//
// It exits 0 when every failure arrived in the category this program expected
// and 1 otherwise.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/b3vet/atlascache/pkg/client"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:6379", "AtlasCache server, as host:port")
	authAddr := flag.String("auth-addr", "", "a server with auth enabled, for the ErrAuth case")
	flag.Parse()

	if err := run(*addr, *authAddr); err != nil {
		fmt.Fprintln(os.Stderr, "errors:", err)
		os.Exit(1)
	}
}

func run(addr, authAddr string) error {
	c, err := client.New(client.WithAddr(addr))
	if err != nil {
		return fmt.Errorf("configuring the client: %w", err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := networkFailure(ctx); err != nil {
		return err
	}
	if err := timeout(ctx, c); err != nil {
		return err
	}
	if err := protocolMismatch(ctx, c); err != nil {
		return err
	}
	if err := serverRefusal(ctx, c); err != nil {
		return err
	}
	if err := authFailure(ctx, authAddr); err != nil {
		return err
	}
	if err := cancellation(c); err != nil {
		return err
	}
	if err := afterClose(ctx, addr); err != nil {
		return err
	}

	fmt.Println("\nevery failure landed in the category the docs promise")
	return nil
}

// report prints one classified failure the way a log line should: the category,
// the operation, the server, and the SDK's own message.
func report(label string, err error) {
	var atlasErr *client.Error
	if !errors.As(err, &atlasErr) {
		fmt.Printf("%-12s %v\n", label, err)
		return
	}
	fmt.Printf("%-12s retryable=%-5v op=%-5s addr=%-20s %v\n",
		label, client.Retryable(err), atlasErr.Op, atlasErr.Addr, err)
}

// networkFailure: nothing is listening. Retryable — the command may not have
// run, and the next attempt may find the server back.
func networkFailure(ctx context.Context) error {
	dead, err := closedPort()
	if err != nil {
		return err
	}

	// No reconnect window, so the first dial failure surfaces at once instead
	// of being retried for five seconds.
	c, err := client.New(client.WithAddr(dead), client.WithReconnectWindow(0))
	if err != nil {
		return fmt.Errorf("configuring the client: %w", err)
	}
	defer func() { _ = c.Close() }()

	_, _, err = c.Get(ctx, "example:errors:key")
	if !errors.Is(err, client.ErrNetwork) {
		return fmt.Errorf("a dial against a closed port gave %v, want ErrNetwork", err)
	}
	if !client.Retryable(err) {
		return errors.New("ErrNetwork is not reported as retryable")
	}
	report("ErrNetwork", err)
	return nil
}

// timeout: a deadline that has already passed. Retryable, and the one category
// where the command may have run — a caller that cannot tolerate that must not
// retry a write on it.
func timeout(ctx context.Context, c client.Client) error {
	expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel()

	_, _, err := c.Get(expired, "example:errors:key")
	if !errors.Is(err, client.ErrTimeout) {
		return fmt.Errorf("an expired deadline gave %v, want ErrTimeout", err)
	}
	report("ErrTimeout", err)
	return nil
}

// protocolMismatch: a perfectly good reply of the wrong shape. Not retryable —
// the same command will produce the same reply.
func protocolMismatch(ctx context.Context, c client.Client) error {
	reply, err := c.Do(ctx, "ECHO", "not a number")
	if err != nil {
		return fmt.Errorf("ECHO: %w", err)
	}

	_, err = reply.Int64()
	if !errors.Is(err, client.ErrProtocol) {
		return fmt.Errorf("reading a bulk string as an integer gave %v, want ErrProtocol", err)
	}
	if client.Retryable(err) {
		return errors.New("ErrProtocol is reported as retryable")
	}
	report("ErrProtocol", err)
	return nil
}

// serverRefusal: the command reached the server and the server said no. Not
// retryable; the argument list will be just as wrong next time.
func serverRefusal(ctx context.Context, c client.Client) error {
	_, err := c.Do(ctx, "SET", "example:errors:key") // SET needs a value
	if !errors.Is(err, client.ErrServer) {
		return fmt.Errorf("a malformed command gave %v, want ErrServer", err)
	}

	// Kind is the server's own error word, and it is what tells an argument
	// mistake from a server that is out of memory.
	var atlasErr *client.Error
	if !errors.As(err, &atlasErr) || atlasErr.Kind == "" {
		return fmt.Errorf("a server error carried no Kind: %v", err)
	}
	report("ErrServer", err)
	return nil
}

// authFailure: the token is wrong, or the server wants one. Not retryable, and
// every attempt is another line in the server's log.
func authFailure(ctx context.Context, authAddr string) error {
	if authAddr == "" {
		fmt.Printf("%-12s skipped: pass -auth-addr to exercise it\n", "ErrAuth")
		return nil
	}

	c, err := client.New(
		client.WithAddr(authAddr),
		client.WithAuth("not-the-right-token"),
		client.WithReconnectWindow(0),
	)
	if err != nil {
		return fmt.Errorf("configuring the client: %w", err)
	}
	defer func() { _ = c.Close() }()

	err = c.Ping(ctx)
	if !errors.Is(err, client.ErrAuth) {
		return fmt.Errorf("a wrong token gave %v, want ErrAuth", err)
	}
	if client.Retryable(err) {
		return errors.New("ErrAuth is reported as retryable")
	}
	report("ErrAuth", err)
	return nil
}

// cancellation belongs to no category. The caller stopped the call, so the
// caller already knows what happened; the error wraps context.Canceled instead.
func cancellation(c client.Client) error {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := c.Get(canceled, "example:errors:key")
	if !errors.Is(err, context.Canceled) {
		return fmt.Errorf("a canceled call gave %v, want context.Canceled", err)
	}
	for _, category := range []error{
		client.ErrNetwork, client.ErrTimeout, client.ErrProtocol,
		client.ErrServer, client.ErrAuth, client.ErrClosed,
	} {
		if errors.Is(err, category) {
			return fmt.Errorf("a canceled call was classified as %v", category)
		}
	}
	report("canceled", err)
	return nil
}

// afterClose: a call on a closed client. Not retryable and never transient —
// it is a bug in the caller's lifecycle, and a retry loop would hide it.
func afterClose(ctx context.Context, addr string) error {
	c, err := client.New(client.WithAddr(addr))
	if err != nil {
		return fmt.Errorf("configuring the client: %w", err)
	}
	_ = c.Close()

	_, _, err = c.Get(ctx, "example:errors:key")
	if !errors.Is(err, client.ErrClosed) {
		return fmt.Errorf("a call after Close gave %v, want ErrClosed", err)
	}
	report("ErrClosed", err)
	return nil
}

// closedPort returns an address nothing is listening on, by taking one and
// giving it straight back.
func closedPort() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("reserving a port: %w", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return "", fmt.Errorf("releasing the port: %w", err)
	}
	return addr, nil
}
