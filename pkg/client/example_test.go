package client_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/b3vet/atlascache/pkg/client"
)

// The examples are compiled by `go test` and therefore cannot go stale: a
// signature that changes breaks them the way it breaks a caller. They do not
// run, because running them would need a server.

func ExampleNew() {
	c, err := client.New(
		client.WithAddr("localhost:6379"),
		client.WithPoolSize(10),
		client.WithDialTimeout(5*time.Second),
	)
	if err != nil {
		fmt.Println("misconfigured:", err)
		return
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// New opens nothing. Ping is how you ask whether the server is there.
	if err := c.Ping(ctx); err != nil {
		fmt.Println("unreachable:", err)
		return
	}
	fmt.Println("connected")
}

func ExampleClient_Get() {
	c, err := client.New(client.WithAddr("localhost:6379"))
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	if setErr := c.Set(ctx, "session:abc", []byte("payload"), 5*time.Minute); setErr != nil {
		fmt.Println(setErr)
		return
	}

	// found tells a missing key from a key holding an empty value. Both are
	// legal, and the server answers them differently.
	value, found, err := c.Get(ctx, "session:abc")
	switch {
	case err != nil:
		fmt.Println(err)
	case !found:
		fmt.Println("cache miss")
	default:
		fmt.Printf("%d bytes\n", len(value))
	}
}

// Errors carry a category, so a caller decides what to do with errors.Is rather
// than by matching on strings.
func ExampleRetryable() {
	c, err := client.New(client.WithAddr("localhost:6379"))
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	_, _, err = c.Get(context.Background(), "key")
	switch {
	case err == nil:
		fmt.Println("served from the cache")
	case errors.Is(err, client.ErrAuth):
		fmt.Println("the client is misconfigured:", err)
	case client.Retryable(err):
		// The cache is unreachable; serve from the source instead.
		fmt.Println("degraded:", err)
	default:
		fmt.Println(err)
	}
}

// Retries are opt-in per call site, and only for commands whose reply is
// idempotent. Del, SetNX, Scan and Do are never retried.
func ExampleClient_WithRetries() {
	c, err := client.New(client.WithAddr("localhost:6379"))
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	value, found, err := c.WithRetries(2).Get(context.Background(), "key")
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(found, len(value))
}

// Do reaches commands the SDK does not wrap, so an SDK release is never what
// stands between a caller and a new server command.
func ExampleClient_Do() {
	c, err := client.New(client.WithAddr("localhost:6379"))
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	reply, err := c.Do(context.Background(), "DBSIZE")
	if err != nil {
		fmt.Println(err)
		return
	}

	keys, err := reply.Int64()
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("%d keys\n", keys)
}

// Scan pages through the keyspace. The scan is over when the cursor comes back
// as ScanStart, and only then — an empty page says nothing.
func ExampleClient_Scan() {
	c, err := client.New(client.WithAddr("localhost:6379"))
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	var total int
	for cursor := client.ScanStart; ; {
		page, pageErr := c.Scan(ctx, cursor, "session:*", 100)
		if pageErr != nil {
			fmt.Println(pageErr)
			return
		}
		total += len(page.Keys)
		if page.Done() {
			break
		}
		cursor = page.Cursor
	}
	fmt.Println(total)
}

func ExampleWithTLS() {
	pemBytes, err := os.ReadFile("/etc/atlascache/ca.crt")
	if err != nil {
		fmt.Println(err)
		return
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pemBytes) {
		fmt.Println("no certificates in the CA file")
		return
	}

	c, err := client.New(
		client.WithAddr("cache.internal:6379"),
		client.WithTLS(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}),
		client.WithAuth(os.Getenv("ATLASCACHE_TOKEN")),
	)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = c.Close() }()

	fmt.Println(c.Ping(context.Background()))
}
