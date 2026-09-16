//go:build ignore

// This file carries the `ignore` build tag, and it is not a mistake.
//
// These programs live in the root module, so without the tag they would be
// swept up by `go test ./...` — as packages with no test files, contributing
// several hundred uncovered statements each and pulling the module below the
// coverage floor the phase gate enforces. The tag keeps them out of `./...`
// while leaving them buildable, runnable and vettable by name:
//
//	go run  ./examples/tls/main.go -addr 127.0.0.1:6379
//	go vet  ./examples/tls/main.go
//
// examples/run.sh builds and runs every one of them against a real server on
// every CI run, which is the check that actually matters here.

// Command tls connects to an AtlasCache server over TLS, authenticating with a
// token taken from the environment.
//
//	ATLASCACHE_AUTH=… go run ./examples/tls/main.go \
//	    -addr localhost:6380 -ca certs/server.crt
//
// Two habits are worth copying from this program. A private certificate
// authority is named by file with WithTLSCAFile, which reads and parses it
// inside New — so a path that is wrong stops the process starting instead of
// failing the first call; and the token is read from the environment rather
// than from a flag, because an argument list is visible in `ps` to every other
// user on the host.
//
// It exits 0 when the connection verified, authenticated and round-tripped a
// value, and 1 otherwise.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/b3vet/atlascache/pkg/client"
)

// envAuth is the same variable atlasctl reads, for the same reason.
const envAuth = "ATLASCACHE_AUTH"

const key = "example:tls:key"

func main() {
	addr := flag.String("addr", "localhost:6380", "AtlasCache server, as host:port")
	caFile := flag.String("ca", "", "PEM file of the certificate authority that signed the server (optional)")
	flag.Parse()

	if err := run(*addr, *caFile, os.Getenv(envAuth)); err != nil {
		fmt.Fprintln(os.Stderr, "tls:", err)
		os.Exit(1)
	}
}

func run(addr, caFile, token string) error {
	opts := []client.Option{
		client.WithAddr(addr),
		// With no ServerName set, the host from addr is used — which is why
		// this connects to "localhost" and not to "127.0.0.1" unless the
		// certificate names the address.
		tlsOption(caFile),
	}
	if token != "" {
		// Sent on every connection the pool opens, including replacements for
		// dropped ones, so nothing re-authenticates by hand.
		opts = append(opts, client.WithAuth(token))
	} else {
		fmt.Printf("%s is not set; connecting without authentication\n", envAuth)
	}

	c, err := client.New(opts...)
	if errors.Is(err, client.ErrConfig) {
		// A CA file that is missing, unreadable, or holds no certificate. It is
		// a configuration error rather than a network one, and no connection
		// was attempted — so this is a reason to exit, not to retry.
		return fmt.Errorf("the client is misconfigured: %w", err)
	}
	if err != nil {
		return fmt.Errorf("configuring the client: %w", err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := c.Ping(ctx); err != nil {
		switch {
		case errors.Is(err, client.ErrAuthDisabled):
			// The one authentication failure the token did not cause: this
			// server has no password set, so it refuses every AUTH. Blaming
			// the token here would send its owner to rotate a working secret.
			return fmt.Errorf("%s is set, but this server has authentication disabled: %w", envAuth, err)
		case errors.Is(err, client.ErrAuth):
			return fmt.Errorf("the server refused the token in %s: %w", envAuth, err)
		}
		return fmt.Errorf("connecting to %s over TLS: %w", addr, err)
	}
	fmt.Printf("connected to %s over TLS\n", addr)

	if err := roundTrip(ctx, c); err != nil {
		return err
	}
	return verificationIsNotOptional(ctx, addr)
}

// tlsOption picks how the server is verified. With no CA file the host's trust
// store is used, which is what a server holding a certificate from a public CA
// wants; with one, only that authority is trusted.
//
// Neither branch reads a file or builds a certificate pool. WithTLSCAFile does
// both, inside New, which is a dozen lines of x509 this program does not carry
// and cannot get subtly wrong.
func tlsOption(caFile string) client.Option {
	if caFile == "" {
		// TLS 1.3 is the server's floor and is not configurable there, so
		// naming it here costs nothing and documents the expectation.
		return client.WithTLS(&tls.Config{MinVersion: tls.VersionTLS13})
	}
	return client.WithTLSCAFile(caFile)
}

// roundTrip proves the connection carries commands, not just a handshake.
func roundTrip(ctx context.Context, c client.Client) error {
	if err := c.Set(ctx, key, []byte("over TLS"), time.Minute); err != nil {
		return fmt.Errorf("writing %s: %w", key, err)
	}
	defer func() { _, _ = c.Del(context.Background(), key) }()

	value, found, err := c.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("reading %s: %w", key, err)
	}
	if !found {
		return fmt.Errorf("%s was written and then missing", key)
	}
	fmt.Printf("%s = %q\n", key, string(value))
	return nil
}

// verificationIsNotOptional: the same address without the CA fails, because
// the certificate is signed by an authority the host does not trust. The
// failure arrives as ErrNetwork — the connection never came up — which is
// worth knowing, since it reads at first like the server being down.
func verificationIsNotOptional(ctx context.Context, addr string) error {
	c, err := client.New(
		client.WithAddr(addr),
		client.WithTLS(&tls.Config{MinVersion: tls.VersionTLS13}), // host trust store only
		client.WithReconnectWindow(0),
	)
	if err != nil {
		return fmt.Errorf("configuring the client: %w", err)
	}
	defer func() { _ = c.Close() }()

	switch err := c.Ping(ctx); {
	case err == nil:
		fmt.Println("the host already trusts this server's certificate; no CA file needed")
	case errors.Is(err, client.ErrNetwork):
		fmt.Println("without the CA: rejected at the handshake, reported as ErrNetwork")
	default:
		return fmt.Errorf("an untrusted certificate gave %v, want ErrNetwork", err)
	}
	return nil
}
