package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"time"
)

// Defaults every option starts from.
//
// The timeouts are the ones a cache client wants rather than the ones a
// database client wants: a read that has taken three seconds against an
// in-memory store is not slow, it is broken, and a caller waiting on it has
// already lost whatever the cache was saving.
const (
	defaultAddr            = "127.0.0.1:6379"
	defaultPoolSize        = 10
	defaultDialTimeout     = 5 * time.Second
	defaultReadTimeout     = 3 * time.Second
	defaultWriteTimeout    = 3 * time.Second
	defaultMaxIdleTime     = 5 * time.Minute
	defaultReconnectWindow = 5 * time.Second
	defaultBackoffBase     = 20 * time.Millisecond
	defaultBackoffMax      = time.Second
)

// options is the resolved configuration one client runs on. It is built once by
// New and never written again, so every connection and every call reads it
// without a lock.
type options struct {
	addr     string
	username string
	token    string

	tlsConfig *tls.Config

	poolSize    int
	maxIdleTime time.Duration

	dialTimeout  time.Duration
	readTimeout  time.Duration
	writeTimeout time.Duration

	reconnectWindow time.Duration
	backoffBase     time.Duration
	backoffMax      time.Duration

	// dialer is the hook the tests use to stand in for the network. It is
	// unexported and has no option, because an SDK that lets a caller replace
	// its transport has to keep that a stable API forever.
	dialer func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Option configures a client. Options are applied in order, so a later one
// wins over an earlier one, and a nil option is skipped — which makes
// conditional configuration a plain expression rather than a slice to build up:
//
//	client.New(
//		client.WithAddr(addr),
//		tlsOption(),   // returns nil when TLS is off
//	)
//
// An option that does not validate is reported by [New], before anything is
// dialed.
type Option func(*options) error

func defaultOptions() *options {
	return &options{
		addr:            defaultAddr,
		poolSize:        defaultPoolSize,
		maxIdleTime:     defaultMaxIdleTime,
		dialTimeout:     defaultDialTimeout,
		readTimeout:     defaultReadTimeout,
		writeTimeout:    defaultWriteTimeout,
		reconnectWindow: defaultReconnectWindow,
		backoffBase:     defaultBackoffBase,
		backoffMax:      defaultBackoffMax,
	}
}

// WithAddr sets the server's host:port. It defaults to 127.0.0.1:6379.
func WithAddr(addr string) Option {
	return func(o *options) error {
		if addr == "" {
			return configError(opNew, "the address must not be empty")
		}
		o.addr = addr
		return nil
	}
}

// WithAuth authenticates every connection with a token, using the single
// argument form of AUTH.
//
// The token is sent on every new connection, including ones opened later to
// replace a dropped one, so a caller never has to re-authenticate by hand.
//
// Read it from the environment or a secret store rather than writing it into
// the source or passing it on a command line: an argument list is visible in
// `ps` to every other user on the host, which is why atlasctl prefers
// ATLASCACHE_AUTH to its own --auth flag.
//
// Authenticating against a server that has auth disabled fails with [ErrAuth]
// even though the token is not the problem: the server has no password to check
// and refuses every AUTH. That one is told apart from a refused token — it
// wraps [ErrAuthDisabled], and its message names the server's configuration
// rather than the caller's token.
func WithAuth(token string) Option {
	return func(o *options) error {
		if token == "" {
			return configError(opNew, "the auth token must not be empty")
		}
		o.username = ""
		o.token = token
		return nil
	}
}

// WithUserAuth authenticates with the two argument form, `AUTH <user> <token>`.
//
// v0.1.0 has one shared token and one user, "default" (ADR-0020); both forms
// check the same secret. The two argument form exists because clients written
// against Redis 6 ACLs send it unconditionally, and because P6 may grow real
// users — a caller that already sends a username will not have to change.
func WithUserAuth(username, token string) Option {
	return func(o *options) error {
		if username == "" {
			return configError(opNew, "the auth username must not be empty")
		}
		if token == "" {
			return configError(opNew, "the auth token must not be empty")
		}
		o.username = username
		o.token = token
		return nil
	}
}

// WithTLS makes every connection TLS, using the given configuration.
//
// The configuration is cloned, so the caller may keep using its own copy. When
// it names no ServerName, the host from the address is used — without that, a
// certificate check against an IP-less config fails for a reason that reads
// like a certificate problem and is not.
//
// A config of &tls.Config{MinVersion: tls.VersionTLS13} verifies the server
// against the host's trust store, which is what a real certificate wants. For a
// private CA, reach for [WithTLSCAFile] rather than assembling an x509.CertPool
// here: it is the same thing, without the plumbing.
//
// InsecureSkipVerify turns the connection into encryption without
// authentication — anything on the path can present its own certificate — so
// it belongs in a test and nowhere else.
func WithTLS(config *tls.Config) Option {
	return func(o *options) error {
		if config == nil {
			return configError(opNew, "the TLS configuration must not be nil")
		}
		o.tlsConfig = config.Clone()
		return nil
	}
}

// WithTLSCAFile makes every connection TLS, verifying the server against the
// certificate authorities in a PEM file.
//
// It is the private-CA case, which is most of them: a cache is an internal
// service, its certificate is usually issued by an internal authority, and
// without this every caller writes the same dozen lines of os.ReadFile,
// x509.NewCertPool and AppendCertsFromPEM (ISSUE-0023).
//
//	c, err := client.New(
//		client.WithAddr("cache.internal:6379"),
//		client.WithTLSCAFile("/etc/atlascache/ca.crt"),
//	)
//
// The file is read once, here, so that an unreadable path or a file holding no
// certificate is an error from [New] rather than a handshake failure on the
// first call — the difference between a process that refuses to start and one
// that starts and then cannot talk to anything.
//
// The floor is TLS 1.2 rather than the server's own 1.3, so that this client
// keeps working against a server that later chooses to accept 1.2 without ever
// being the reason an older version is negotiated. A caller who wants a
// different floor, a client certificate, or a pinned ServerName composes the
// two options — this one keeps whatever an earlier [WithTLS] set and replaces
// only RootCAs:
//
//	client.WithTLS(&tls.Config{MinVersion: tls.VersionTLS13}),
//	client.WithTLSCAFile("/etc/atlascache/ca.crt"),
//
// Order matters there, and only in that direction: a [WithTLS] applied after
// this one replaces the configuration outright, CA included.
func WithTLSCAFile(path string) Option {
	return func(o *options) error {
		if path == "" {
			return configError(opNew, "the TLS CA file path must not be empty")
		}

		pem, err := os.ReadFile(path)
		if err != nil {
			return configError(opNew, "reading the TLS CA file: "+err.Error())
		}

		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			// AppendCertsFromPEM reports nothing but "none of it parsed", which
			// is almost always one of these two mistakes.
			return configError(opNew, "the TLS CA file "+path+
				" holds no PEM certificate; a DER .crt or a private key would look like this")
		}

		config := o.tlsConfig
		if config == nil {
			config = &tls.Config{MinVersion: tls.VersionTLS12}
		} else {
			config = config.Clone()
		}
		config.RootCAs = roots
		o.tlsConfig = config
		return nil
	}
}

// WithPoolSize sets how many connections the client may hold open at once. It
// defaults to 10, and must be at least 1.
//
// It is a ceiling and not a target: connections are opened on demand, and an
// idle client holds none.
//
// # Exhaustion
//
// A call that arrives when every connection is busy waits for one. It does not
// open an eleventh, and it does not fail fast: it blocks until a connection is
// returned or its own context expires, and an expired context surfaces as
// [ErrTimeout] with Op "pool". That is the designed behavior and not a hang —
// a pool that grew under pressure would turn one leaked connection into
// exhausted memory, while one that blocks turns the same leak into timeouts
// that name the callers waiting on it (FEAT-0027).
//
// Two consequences worth designing around. Always give a call a context with a
// deadline, or an exhausted pool is an unbounded wait. And size the pool for
// concurrent in-flight calls rather than for goroutines: a request path that
// makes one cache call at a time needs far fewer connections than it has
// workers, because each connection is held only for one round trip.
//
// A pool of one serializes every call through a single connection, which is
// what a CLI running one command wants and what a busy service does not. It is
// no longer what a multi-page scan needs: [Client.ScanIter] pins a connection
// for the length of a walk itself (ISSUE-0023). Note the other side of that —
// a walk in progress is holding one of these connections, so a pool of one has
// nothing left for a call made from inside the loop.
func WithPoolSize(size int) Option {
	return func(o *options) error {
		if size < 1 {
			return configError(opNew, "the pool size must be at least 1")
		}
		o.poolSize = size
		return nil
	}
}

// WithMaxIdleTime sets how long a connection may sit unused in the pool before
// it is closed rather than reused. It defaults to five minutes, and zero
// disables the check.
//
// It exists because the far end has its own idea: a server with
// client_idle_timeout set, or any load balancer in between, will drop an idle
// connection without telling anyone. Closing first turns a surfaced error into
// a dial.
func WithMaxIdleTime(d time.Duration) Option {
	return func(o *options) error {
		if d < 0 {
			return configError(opNew, "the maximum idle time must not be negative")
		}
		o.maxIdleTime = d
		return nil
	}
}

// WithDialTimeout bounds one connection attempt, including the TLS handshake
// and the HELLO/AUTH exchange. Zero means no bound beyond the caller's context.
func WithDialTimeout(d time.Duration) Option {
	return func(o *options) error {
		if d < 0 {
			return configError(opNew, "the dial timeout must not be negative")
		}
		o.dialTimeout = d
		return nil
	}
}

// WithReadTimeout bounds how long one reply may take to arrive. Zero means no
// bound beyond the caller's context.
func WithReadTimeout(d time.Duration) Option {
	return func(o *options) error {
		if d < 0 {
			return configError(opNew, "the read timeout must not be negative")
		}
		o.readTimeout = d
		return nil
	}
}

// WithWriteTimeout bounds how long one request may take to write. Zero means no
// bound beyond the caller's context.
func WithWriteTimeout(d time.Duration) Option {
	return func(o *options) error {
		if d < 0 {
			return configError(opNew, "the write timeout must not be negative")
		}
		o.writeTimeout = d
		return nil
	}
}

// WithReconnectWindow sets how long a failed connection attempt keeps being
// retried, with backoff, before the failure is surfaced to the caller. It
// defaults to five seconds.
//
// This is what makes a server restart a pause rather than an outage: a call
// that arrives while the server is coming back waits and then succeeds. Zero
// surfaces the first dial failure immediately, which is the right setting for a
// caller that would rather serve a cache miss than wait.
//
// The caller's own context always wins: the window never extends a deadline,
// it only bounds the wait for a caller who set none.
func WithReconnectWindow(d time.Duration) Option {
	return func(o *options) error {
		if d < 0 {
			return configError(opNew, "the reconnect window must not be negative")
		}
		o.reconnectWindow = d
		return nil
	}
}

// WithBackoff sets the exponential backoff the reconnect window spends.
//
// The delay before attempt n is drawn uniformly from [0, min(base·2ⁿ, max)) —
// full jitter. The jitter is not a refinement: without it every client that
// lost its connection to the same server retries in lockstep, and the restart
// they are all waiting for is held down by their synchronized retries.
func WithBackoff(base, maximum time.Duration) Option {
	return func(o *options) error {
		if base < 0 || maximum < 0 {
			return configError(opNew, "the backoff durations must not be negative")
		}
		if maximum > 0 && base > maximum {
			return configError(opNew, "the backoff base must not exceed the maximum")
		}
		o.backoffBase = base
		o.backoffMax = maximum
		return nil
	}
}
