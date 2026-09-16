package client

import (
	"crypto/tls"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resolve(t *testing.T, opts ...Option) *options {
	t.Helper()
	resolved := defaultOptions()
	for _, opt := range opts {
		if err := opt(resolved); err != nil {
			t.Fatalf("applying an option: %v", err)
		}
	}
	return resolved
}

func TestDefaultOptions(t *testing.T) {
	o := defaultOptions()

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"addr", o.addr, defaultAddr},
		{"pool size", o.poolSize, defaultPoolSize},
		{"dial timeout", o.dialTimeout, defaultDialTimeout},
		{"read timeout", o.readTimeout, defaultReadTimeout},
		{"write timeout", o.writeTimeout, defaultWriteTimeout},
		{"max idle time", o.maxIdleTime, defaultMaxIdleTime},
		{"reconnect window", o.reconnectWindow, defaultReconnectWindow},
		{"backoff base", o.backoffBase, defaultBackoffBase},
		{"backoff max", o.backoffMax, defaultBackoffMax},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("default %s is %v, want %v", c.name, c.got, c.want)
		}
	}
	if o.token != "" || o.username != "" || o.tlsConfig != nil {
		t.Errorf("a default client is not configured for auth or TLS, got %+v", o)
	}
}

func TestEachOptionTakesEffect(t *testing.T) {
	t.Run("WithAddr", func(t *testing.T) {
		if got := resolve(t, WithAddr("cache:7000")).addr; got != "cache:7000" {
			t.Fatalf("addr is %q", got)
		}
	})

	t.Run("WithAuth", func(t *testing.T) {
		o := resolve(t, WithAuth("s3cret"))
		if o.token != "s3cret" || o.username != "" {
			t.Fatalf("auth is %q/%q", o.username, o.token)
		}
	})

	t.Run("WithUserAuth", func(t *testing.T) {
		o := resolve(t, WithUserAuth("default", "s3cret"))
		if o.username != "default" || o.token != "s3cret" {
			t.Fatalf("auth is %q/%q", o.username, o.token)
		}
	})

	t.Run("WithAuth clears a username set earlier", func(t *testing.T) {
		o := resolve(t, WithUserAuth("default", "old"), WithAuth("new"))
		if o.username != "" || o.token != "new" {
			t.Fatalf("auth is %q/%q, want the single-argument form", o.username, o.token)
		}
	})

	t.Run("WithTLS clones the configuration", func(t *testing.T) {
		config := &tls.Config{ServerName: "cache.example", MinVersion: tls.VersionTLS13}
		o := resolve(t, WithTLS(config))
		if o.tlsConfig == config {
			t.Fatal("the caller's TLS configuration was kept by reference")
		}
		config.ServerName = "changed"
		if o.tlsConfig.ServerName != "cache.example" {
			t.Fatal("changing the caller's configuration changed the client's")
		}
	})

	t.Run("a later option wins", func(t *testing.T) {
		if got := resolve(t, WithAddr("first:1"), WithAddr("second:2")).addr; got != "second:2" {
			t.Fatalf("addr is %q, want the later value", got)
		}
	})
}

func TestPoolAndTimeoutOptions(t *testing.T) {
	t.Run("WithPoolSize", func(t *testing.T) {
		if got := resolve(t, WithPoolSize(3)).poolSize; got != 3 {
			t.Fatalf("pool size is %d", got)
		}
	})

	t.Run("WithMaxIdleTime", func(t *testing.T) {
		if got := resolve(t, WithMaxIdleTime(time.Minute)).maxIdleTime; got != time.Minute {
			t.Fatalf("max idle time is %v", got)
		}
	})

	t.Run("timeouts", func(t *testing.T) {
		o := resolve(t,
			WithDialTimeout(time.Second),
			WithReadTimeout(2*time.Second),
			WithWriteTimeout(3*time.Second),
		)
		if o.dialTimeout != time.Second || o.readTimeout != 2*time.Second || o.writeTimeout != 3*time.Second {
			t.Fatalf("timeouts are %v/%v/%v", o.dialTimeout, o.readTimeout, o.writeTimeout)
		}
	})

	t.Run("WithReconnectWindow", func(t *testing.T) {
		if got := resolve(t, WithReconnectWindow(0)).reconnectWindow; got != 0 {
			t.Fatalf("reconnect window is %v", got)
		}
	})

	t.Run("WithBackoff", func(t *testing.T) {
		o := resolve(t, WithBackoff(time.Millisecond, time.Second))
		if o.backoffBase != time.Millisecond || o.backoffMax != time.Second {
			t.Fatalf("backoff is %v/%v", o.backoffBase, o.backoffMax)
		}
	})
}

// A malformed option is refused by New rather than carried into the first call,
// where it would surface as something that looks like a server problem.
func TestOptionsAreValidated(t *testing.T) {
	bad := map[string]Option{
		"empty address":          WithAddr(""),
		"empty token":            WithAuth(""),
		"empty username":         WithUserAuth("", "token"),
		"empty token with user":  WithUserAuth("default", ""),
		"nil TLS configuration":  WithTLS(nil),
		"pool size below one":    WithPoolSize(0),
		"negative idle time":     WithMaxIdleTime(-time.Second),
		"negative dial timeout":  WithDialTimeout(-1),
		"negative read timeout":  WithReadTimeout(-1),
		"negative write timeout": WithWriteTimeout(-1),
		"negative window":        WithReconnectWindow(-1),
		"negative backoff":       WithBackoff(-1, time.Second),
		"base above maximum":     WithBackoff(time.Minute, time.Second),
	}

	for name, opt := range bad {
		t.Run(name, func(t *testing.T) {
			c, err := New(opt)
			if err == nil {
				_ = c.Close()
				t.Fatalf("New accepted the %s option", name)
			}

			// A configuration mistake is not retryable and is not a failure of
			// the server, the network or the protocol. Categorizing it as one
			// would send a caller looking in the wrong place.
			if Retryable(err) {
				t.Fatalf("a configuration error was reported as retryable: %v", err)
			}
			var typed *Error
			if !errors.As(err, &typed) || typed.Op != "New" {
				t.Fatalf("New returned %#v, want an *Error from New", err)
			}

			// The category is what a caller classifies on. Without it they have
			// to know that New's only possible failure is a bad option, which
			// is a fact about today's implementation rather than an API
			// (ISSUE-0023).
			if !errors.Is(err, ErrConfig) {
				t.Fatalf("New returned %v, which is not an ErrConfig", err)
			}
			for _, other := range []error{ErrNetwork, ErrTimeout, ErrProtocol, ErrServer, ErrAuth, ErrClosed} {
				if errors.Is(err, other) {
					t.Fatalf("a configuration error also matches %v; the categories are meant to be exclusive", other)
				}
			}
		})
	}
}

func TestNewIgnoresNilOptions(t *testing.T) {
	c, err := New(nil, WithAddr("127.0.0.1:1"), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()
}

// An argument a command cannot be built from is the same category as a bad
// option, and names the command rather than New: "atlascache: New: DEL needs at
// least one key" points at the wrong call.
func TestArgumentErrorsAreConfigErrors(t *testing.T) {
	s := newFakeServer(t)
	c := newTestClient(t, s)
	ctx := t.Context()

	cases := map[string]struct {
		op   string
		call func() error
	}{
		"DEL with no keys": {op: "DEL", call: func() error {
			_, err := c.Del(ctx)
			return err
		}},
		"EXISTS with no keys": {op: "EXISTS", call: func() error {
			_, err := c.Exists(ctx)
			return err
		}},
		"Do with no command": {op: "DO", call: func() error {
			_, err := c.Do(ctx)
			return err
		}},
		"Do with an argument it will not guess at": {op: "DO", call: func() error {
			_, err := c.Do(ctx, "SET", "k", struct{ A int }{1})
			return err
		}},
		"a negative TTL": {op: "SET", call: func() error {
			return c.Set(ctx, "k", []byte("v"), -time.Second)
		}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.call()
			if !errors.Is(err, ErrConfig) {
				t.Fatalf("got %v, want an ErrConfig", err)
			}
			var typed *Error
			if !errors.As(err, &typed) || typed.Op != tc.op {
				t.Fatalf("got %v with Op %q, want one naming %s", err, typed.Op, tc.op)
			}
		})
	}
}

// WithTLSCAFile is the private-CA case without the x509 plumbing every caller
// was writing identically (ISSUE-0023).
func TestWithTLSCAFile(t *testing.T) {
	cert, _ := selfSignedCert(t)
	caFile := writePEM(t, "ca.crt", cert.Certificate[0])

	t.Run("verifies a server signed by the named authority", func(t *testing.T) {
		noLeaks(t)
		s := newTLSFakeServer(t, cert)

		c := newTestClient(t, s, WithTLSCAFile(caFile))
		if err := c.Ping(t.Context()); err != nil {
			t.Fatalf("Ping over TLS verified by a CA file: %v", err)
		}
	})

	t.Run("still refuses a server it was given no authority for", func(t *testing.T) {
		noLeaks(t)
		s := newTLSFakeServer(t, cert)

		other, _ := selfSignedCert(t)
		c := newTestClient(t, s,
			WithTLSCAFile(writePEM(t, "other.crt", other.Certificate[0])),
			WithReconnectWindow(0),
		)
		if err := c.Ping(t.Context()); err == nil {
			t.Fatal("a CA file naming another authority completed the handshake")
		}
	})

	t.Run("keeps what an earlier WithTLS set", func(t *testing.T) {
		o := resolve(t,
			WithTLS(&tls.Config{MinVersion: tls.VersionTLS13, ServerName: "cache.example"}),
			WithTLSCAFile(caFile),
		)
		if o.tlsConfig.MinVersion != tls.VersionTLS13 || o.tlsConfig.ServerName != "cache.example" {
			t.Fatalf("the earlier configuration was discarded: %+v", o.tlsConfig)
		}
		if o.tlsConfig.RootCAs == nil {
			t.Fatal("the CA file was not loaded")
		}
	})

	t.Run("on its own it sets a floor of TLS 1.2", func(t *testing.T) {
		o := resolve(t, WithTLSCAFile(caFile))
		if o.tlsConfig.MinVersion != tls.VersionTLS12 {
			t.Fatalf("the minimum version is %#x, want TLS 1.2", o.tlsConfig.MinVersion)
		}
	})

	// The two ways to get it wrong, both refused by New rather than by the
	// first call: a process that cannot reach its cache should fail to start
	// rather than start and then fail every request.
	t.Run("a path that is not there", func(t *testing.T) {
		_, err := New(WithTLSCAFile(filepath.Join(t.TempDir(), "absent.crt")))
		if !errors.Is(err, ErrConfig) {
			t.Fatalf("got %v, want an ErrConfig", err)
		}
	})

	t.Run("a file holding no certificate", func(t *testing.T) {
		notPEM := filepath.Join(t.TempDir(), "notes.txt")
		if err := os.WriteFile(notPEM, []byte("this is not a certificate\n"), 0o600); err != nil {
			t.Fatalf("writing the file: %v", err)
		}
		_, err := New(WithTLSCAFile(notPEM))
		if !errors.Is(err, ErrConfig) {
			t.Fatalf("got %v, want an ErrConfig", err)
		}
		if !strings.Contains(err.Error(), notPEM) {
			t.Errorf("the error reads %q and does not name the file", err)
		}
	})

	t.Run("an empty path", func(t *testing.T) {
		if _, err := New(WithTLSCAFile("")); !errors.Is(err, ErrConfig) {
			t.Fatalf("got %v, want an ErrConfig", err)
		}
	})
}

// writePEM writes one DER certificate out as the PEM file a CA option reads.
func writePEM(t *testing.T, name string, der []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}
