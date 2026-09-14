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
// Two habits are worth copying from this program. The CA goes into an
// x509.CertPool and then into RootCAs, which is what verifies a server holding
// a private certificate; and the token is read from the environment rather than
// from a flag, because an argument list is visible in `ps` to every other user
// on the host.
//
// It exits 0 when the connection verified, authenticated and round-tripped a
// value, and 1 otherwise.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
	config, err := tlsConfig(caFile)
	if err != nil {
		return err
	}

	opts := []client.Option{
		client.WithAddr(addr),
		// The configuration is cloned, so the caller may keep using its own
		// copy. With no ServerName set, the host from addr is used — which is
		// why this connects to "localhost" and not to "127.0.0.1" unless the
		// certificate names the address.
		client.WithTLS(config),
	}
	if token != "" {
		// Sent on every connection the pool opens, including replacements for
		// dropped ones, so nothing re-authenticates by hand.
		opts = append(opts, client.WithAuth(token))
	} else {
		fmt.Printf("%s is not set; connecting without authentication\n", envAuth)
	}

	c, err := client.New(opts...)
	if err != nil {
		return fmt.Errorf("configuring the client: %w", err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := c.Ping(ctx); err != nil {
		if errors.Is(err, client.ErrAuth) {
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

// tlsConfig builds the configuration. With no CA file the host's trust store
// is used, which is what a server holding a certificate from a public CA
// wants; with one, only that authority is trusted.
func tlsConfig(caFile string) (*tls.Config, error) {
	// TLS 1.3 is the server's floor and is not configurable there, so naming
	// it here costs nothing and documents the expectation.
	config := &tls.Config{MinVersion: tls.VersionTLS13}
	if caFile == "" {
		return config, nil
	}

	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading the CA file: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("%s holds no PEM certificate", caFile)
	}
	config.RootCAs = roots
	return config, nil
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
