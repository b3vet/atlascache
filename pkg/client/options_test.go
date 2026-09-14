package client

import (
	"crypto/tls"
	"errors"
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
