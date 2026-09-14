package client

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestClient wires a client to a fake server and closes it with the test.
func newTestClient(t *testing.T, s *fakeServer, opts ...Option) Client {
	t.Helper()
	all := append([]Option{WithAddr(s.address())}, opts...)
	c, err := New(all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return c
}

func TestValueMethodsRoundTrip(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s)
	ctx := t.Context()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := c.Set(ctx, "greeting", []byte("hello"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	value, found, err := c.Get(ctx, "greeting")
	if err != nil || !found || string(value) != "hello" {
		t.Fatalf("Get gave (%q, %v, %v)", value, found, err)
	}

	text, found, err := c.GetString(ctx, "greeting")
	if err != nil || !found || text != "hello" {
		t.Fatalf("GetString gave (%q, %v, %v)", text, found, err)
	}

	if setErr := c.SetString(ctx, "second", "value", time.Minute); setErr != nil {
		t.Fatalf("SetString: %v", setErr)
	}

	echoed, err := c.Echo(ctx, []byte("probe"))
	if err != nil || string(echoed) != "probe" {
		t.Fatalf("Echo gave (%q, %v)", echoed, err)
	}
}

func TestExistenceMethodsRoundTrip(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s)
	ctx := t.Context()

	if err := c.Set(ctx, "a", []byte("1"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	stored, err := c.SetNX(ctx, "a", []byte("other"))
	if err != nil || stored {
		t.Fatalf("SetNX on an existing key gave (%v, %v)", stored, err)
	}
	stored, err = c.SetNX(ctx, "b", []byte("2"))
	if err != nil || !stored {
		t.Fatalf("SetNX on a free key gave (%v, %v)", stored, err)
	}

	count, err := c.Exists(ctx, "a", "b", "missing")
	if err != nil || count != 2 {
		t.Fatalf("Exists gave (%d, %v)", count, err)
	}

	removed, err := c.Del(ctx, "a", "b", "missing")
	if err != nil || removed != 2 {
		t.Fatalf("Del gave (%d, %v)", removed, err)
	}
}

func TestIterationMethodsRoundTrip(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s)
	ctx := t.Context()

	for _, k := range []string{"greeting", "second", "other"} {
		if err := c.Set(ctx, k, []byte("v"), 0); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}

	keys, err := c.Keys(ctx, "*")
	if err != nil || len(keys) != 3 {
		t.Fatalf("Keys gave (%v, %v)", keys, err)
	}

	page, err := c.Scan(ctx, ScanStart, "gree*", 10)
	if err != nil || !page.Done() || len(page.Keys) != 1 || page.Keys[0] != "greeting" {
		t.Fatalf("Scan gave (%+v, %v)", page, err)
	}

	// An empty cursor is the start, so a caller need not know the sentinel.
	page, err = c.Scan(ctx, "", "", 0)
	if err != nil || len(page.Keys) != 3 {
		t.Fatalf("Scan from an empty cursor gave (%+v, %v)", page, err)
	}
}

func TestIntrospectionMethodsRoundTrip(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s)
	ctx := t.Context()

	if err := c.Set(ctx, "a", []byte("1"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	size, err := c.DBSize(ctx)
	if err != nil || size != 1 {
		t.Fatalf("DBSize gave (%d, %v)", size, err)
	}

	info, err := c.Info(ctx)
	if err != nil || !strings.Contains(info, "atlascache_version") {
		t.Fatalf("Info gave (%q, %v)", info, err)
	}

	stats, err := c.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if hits, ok := stats.Value("hits"); !ok || hits != 7 {
		t.Fatalf("Stats gave %v", stats)
	}
	if _, ok := stats.Value("no-such-counter"); ok {
		t.Fatal("Stats reported a counter the server never sent")
	}
}

// A missing key and a key holding an empty value are different answers, the
// server sends different frames for them, and a client that conflates them is
// wrong in a way the caller cannot detect.
func TestGetDistinguishesAMissFromAnEmptyValue(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s)
	ctx := t.Context()

	value, found, err := c.Get(ctx, "absent")
	if err != nil || found || value != nil {
		t.Fatalf("Get on a missing key gave (%q, %v, %v)", value, found, err)
	}

	if setErr := c.Set(ctx, "empty", []byte{}, 0); setErr != nil {
		t.Fatalf("Set: %v", setErr)
	}
	value, found, err = c.Get(ctx, "empty")
	if err != nil || !found || len(value) != 0 {
		t.Fatalf("Get on an empty value gave (%q, %v, %v)", value, found, err)
	}
}

// RESP is binary safe and so is this SDK. Null bytes, invalid UTF-8 and CRLF
// inside a value are values, not mistakes, and a client that routes them
// through a string-shaped code path corrupts them silently.
func TestBinarySafety(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s)
	ctx := t.Context()

	cases := map[string]struct {
		key   string
		value []byte
	}{
		"null bytes":       {"key\x00with\x00nulls", []byte("value\x00with\x00nulls")},
		"invalid UTF-8":    {"key-\xff\xfe", []byte{0xff, 0xfe, 0xc3, 0x28}},
		"embedded CRLF":    {"key\r\nline", []byte("value\r\n*1\r\n$4\r\nPING\r\n")},
		"every byte value": {"all-bytes", allBytes()},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := c.Set(ctx, tc.key, tc.value, 0); err != nil {
				t.Fatalf("Set: %v", err)
			}
			got, found, err := c.Get(ctx, tc.key)
			if err != nil || !found {
				t.Fatalf("Get gave (%v, %v)", found, err)
			}
			if !bytes.Equal(got, tc.value) {
				t.Fatalf("Get gave %q, want %q", got, tc.value)
			}

			echoed, err := c.Echo(ctx, tc.value)
			if err != nil || !bytes.Equal(echoed, tc.value) {
				t.Fatalf("Echo gave (%q, %v)", echoed, err)
			}
		})
	}
}

func allBytes() []byte {
	value := make([]byte, 256)
	for i := range value {
		value[i] = byte(i)
	}
	return value
}

func TestDoSendsArbitraryCommands(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s)
	ctx := t.Context()

	if _, err := c.Do(ctx, "SET", "n", 42); err != nil {
		t.Fatalf("Do SET: %v", err)
	}
	reply, err := c.Do(ctx, "GET", []byte("n"))
	if err != nil {
		t.Fatalf("Do GET: %v", err)
	}
	n, convErr := reply.Int64()
	if convErr != nil || n != 42 {
		t.Fatalf("the reply read as (%d, %v)", n, convErr)
	}

	// An unknown command reaches the server and comes back as its error, which
	// is what makes Do an escape hatch rather than a second command table.
	_, err = c.Do(ctx, "NOSUCHCOMMAND")
	if !errors.Is(err, ErrServer) {
		t.Fatalf("an unknown command gave %v, want ErrServer", err)
	}
	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != "ERR" {
		t.Fatalf("the error is %#v, want one carrying the server's kind", err)
	}
}

func TestDoArgumentEncoding(t *testing.T) {
	cases := map[string]struct {
		arg  any
		want string
	}{
		"string":   {"text", "text"},
		"bytes":    {[]byte{0x00, 0x01}, "\x00\x01"},
		"int":      {-5, "-5"},
		"int8":     {int8(-8), "-8"},
		"int16":    {int16(-16), "-16"},
		"int32":    {int32(-32), "-32"},
		"int64":    {int64(-64), "-64"},
		"uint":     {uint(5), "5"},
		"uint8":    {uint8(8), "8"},
		"uint16":   {uint16(16), "16"},
		"uint32":   {uint32(32), "32"},
		"uint64":   {uint64(64), "64"},
		"float32":  {float32(1.5), "1.5"},
		"float64":  {2.25, "2.25"},
		"true":     {true, "1"},
		"false":    {false, "0"},
		"duration": {90 * time.Second, "90"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := encodeArg(tc.arg)
			if err != nil {
				t.Fatalf("encodeArg(%v): %v", tc.arg, err)
			}
			if string(got) != tc.want {
				t.Fatalf("encodeArg(%v) gave %q, want %q", tc.arg, got, tc.want)
			}
		})
	}

	t.Run("an unsupported type is refused", func(t *testing.T) {
		if _, err := encodeArg(struct{ A int }{1}); err == nil {
			t.Fatal("a struct was accepted as a command argument")
		}
		if _, err := encodeArg(nil); err == nil {
			t.Fatal("nil was accepted as a command argument")
		}
	})
}

func TestDoRejectsAnEmptyCommand(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s)

	if _, err := c.Do(t.Context()); err == nil {
		t.Fatal("Do with no arguments was accepted")
	}
	if _, err := c.Do(t.Context(), "SET", struct{}{}); err == nil {
		t.Fatal("Do with an unrenderable argument was accepted")
	}
}

func TestDelAndExistsNeedAKey(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s)

	if _, err := c.Del(t.Context()); err == nil {
		t.Fatal("Del with no keys was accepted")
	}
	if _, err := c.Exists(t.Context()); err == nil {
		t.Fatal("Exists with no keys was accepted")
	}
}

func TestSetRejectsANegativeTTL(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s)

	if err := c.Set(t.Context(), "k", []byte("v"), -time.Second); err == nil {
		t.Fatal("a negative TTL was accepted")
	}
}

func TestSubSecondTTLIsSentAsMilliseconds(t *testing.T) {
	args, err := expiryArgs(1500 * time.Millisecond)
	if err != nil || len(args) != 2 || string(args[0]) != "PX" || string(args[1]) != "1500" {
		t.Fatalf("a 1.5s TTL rendered as %q (%v)", args, err)
	}

	args, err = expiryArgs(2 * time.Second)
	if err != nil || string(args[0]) != "EX" || string(args[1]) != "2" {
		t.Fatalf("a 2s TTL rendered as %q (%v)", args, err)
	}

	// Below a millisecond the expiry still has to be positive, or the server
	// reads it as a syntax error and the key is never written.
	args, err = expiryArgs(100 * time.Microsecond)
	if err != nil || string(args[1]) != "1" {
		t.Fatalf("a sub-millisecond TTL rendered as %q (%v)", args, err)
	}
}

func TestTTLSentinels(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s)
	ctx := t.Context()

	if err := c.Set(ctx, "permanent", []byte("v"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	ttl, err := c.TTL(ctx, "permanent")
	if err != nil || ttl != TTLNoExpiry {
		t.Fatalf("TTL on a key with no expiry gave (%v, %v), want TTLNoExpiry", ttl, err)
	}

	ttl, err = c.TTL(ctx, "missing")
	if err != nil || ttl != TTLNoKey {
		t.Fatalf("TTL on a missing key gave (%v, %v), want TTLNoKey", ttl, err)
	}
}

func TestExpireWithANonPositiveTTLDeletesTheKey(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s)
	ctx := t.Context()

	if err := c.Set(ctx, "doomed", []byte("v"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	applied, err := c.Expire(ctx, "doomed", 0)
	if err != nil || !applied {
		t.Fatalf("Expire gave (%v, %v)", applied, err)
	}
	if _, found, err := c.Get(ctx, "doomed"); err != nil || found {
		t.Fatalf("the key survived EXPIRE 0: (%v, %v)", found, err)
	}
}

// A sub-second TTL must not round down to EXPIRE 0, which deletes the key.
func TestExpireRoundsSubSecondTTLsUp(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s)
	ctx := t.Context()

	if err := c.Set(ctx, "brief", []byte("v"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := c.Expire(ctx, "brief", 200*time.Millisecond); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if _, found, err := c.Get(ctx, "brief"); err != nil || !found {
		t.Fatalf("a 200ms expiry deleted the key: (%v, %v)", found, err)
	}
}

// The SDK sends HELLO 3 and carries on in RESP2 when the server answers
// -NOPROTO, which is what a v0.1.0 server does (ADR-0028). The fallback has to
// be silent: a probe that fails the connection is worse than no probe.
func TestHelloNegotiation(t *testing.T) {
	t.Run("a -NOPROTO server is used in RESP2", func(t *testing.T) {
		noLeaks(t)
		s := newFakeServer(t)

		var (
			mu    sync.Mutex
			asked [][]byte
		)
		s.setHelloReply(func(args [][]byte) string {
			mu.Lock()
			asked = append(asked, bytes.Join(args, []byte(" ")))
			mu.Unlock()
			return "-NOPROTO unsupported protocol version\r\n"
		})

		c := newTestClient(t, s)
		if err := c.Ping(t.Context()); err != nil {
			t.Fatalf("the connection did not survive -NOPROTO: %v", err)
		}

		mu.Lock()
		defer mu.Unlock()
		if len(asked) != 1 || string(asked[0]) != "3" {
			t.Fatalf("the SDK sent HELLO %q, want HELLO 3", asked)
		}
	})

	t.Run("an unknown command also falls back", func(t *testing.T) {
		noLeaks(t)
		s := newFakeServer(t)
		s.setHelloReply(func([][]byte) string { return "-ERR unknown command 'HELLO'\r\n" })

		c := newTestClient(t, s)
		if err := c.Ping(t.Context()); err != nil {
			t.Fatalf("the connection did not survive an unknown HELLO: %v", err)
		}
	})

	t.Run("a server that accepts HELLO 3 is used in RESP3", func(t *testing.T) {
		noLeaks(t)
		s := newFakeServer(t)
		s.setHelloReply(func([][]byte) string {
			return "%2\r\n$6\r\nserver\r\n$10\r\natlascache\r\n$5\r\nproto\r\n:3\r\n"
		})

		c := newTestClient(t, s)
		if err := c.Ping(t.Context()); err != nil {
			t.Fatalf("Ping: %v", err)
		}
	})
}

func TestAuthentication(t *testing.T) {
	t.Run("the single-argument form", func(t *testing.T) {
		noLeaks(t)
		s := newFakeServer(t)
		s.requireAuth("s3cret")

		c := newTestClient(t, s, WithAuth("s3cret"))
		if err := c.Ping(t.Context()); err != nil {
			t.Fatalf("Ping: %v", err)
		}
		if _, err := c.DBSize(t.Context()); err != nil {
			t.Fatalf("a gated command failed after AUTH: %v", err)
		}
	})

	t.Run("the two-argument form", func(t *testing.T) {
		noLeaks(t)
		s := newFakeServer(t)
		s.requireAuth("s3cret")

		c := newTestClient(t, s, WithUserAuth("default", "s3cret"))
		if _, err := c.DBSize(t.Context()); err != nil {
			t.Fatalf("a gated command failed after AUTH default: %v", err)
		}
	})

	t.Run("a wrong token is ErrAuth", func(t *testing.T) {
		noLeaks(t)
		s := newFakeServer(t)
		s.requireAuth("s3cret")

		c := newTestClient(t, s, WithAuth("wrong"))
		err := c.Ping(t.Context())
		if !errors.Is(err, ErrAuth) {
			t.Fatalf("a wrong token gave %v, want ErrAuth", err)
		}
		if Retryable(err) {
			t.Fatalf("a wrong token was reported as retryable: %v", err)
		}
	})

	t.Run("no token against a server that wants one is ErrAuth", func(t *testing.T) {
		noLeaks(t)
		s := newFakeServer(t)
		s.requireAuth("s3cret")

		c := newTestClient(t, s)
		err := c.Ping(t.Context())
		if err == nil {
			// PING is on the server's pre-auth allowlist, so the failure has to
			// come from a gated command instead.
			_, err = c.DBSize(t.Context())
		}
		if !errors.Is(err, ErrAuth) {
			t.Fatalf("an unauthenticated command gave %v, want ErrAuth", err)
		}
	})

	t.Run("a token against a server with none is ErrAuth", func(t *testing.T) {
		noLeaks(t)
		s := newFakeServer(t)

		c := newTestClient(t, s, WithAuth("unnecessary"))
		if err := c.Ping(t.Context()); !errors.Is(err, ErrAuth) {
			t.Fatalf("got %v, want ErrAuth naming the configuration mistake", err)
		}
	})
}

func TestTLS(t *testing.T) {
	noLeaks(t)
	cert, pool := selfSignedCert(t)

	s := &fakeServer{
		t:         t,
		data:      make(map[string]fakeEntry),
		conns:     make(map[net.Conn]struct{}),
		tlsConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13},
	}
	s.start(t, "127.0.0.1:0")
	t.Cleanup(s.stop)

	c := newTestClient(t, s, WithTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}))
	if err := c.Set(t.Context(), "k", []byte("v"), 0); err != nil {
		t.Fatalf("Set over TLS: %v", err)
	}
	value, found, err := c.Get(t.Context(), "k")
	if err != nil || !found || string(value) != "v" {
		t.Fatalf("Get over TLS gave (%q, %v, %v)", value, found, err)
	}
}

func TestTLSRejectsAnUntrustedCertificate(t *testing.T) {
	noLeaks(t)
	cert, _ := selfSignedCert(t)

	s := &fakeServer{
		t:         t,
		data:      make(map[string]fakeEntry),
		conns:     make(map[net.Conn]struct{}),
		tlsConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13},
	}
	s.start(t, "127.0.0.1:0")
	t.Cleanup(s.stop)

	c := newTestClient(t, s, WithTLS(&tls.Config{MinVersion: tls.VersionTLS13}), WithReconnectWindow(0))
	if err := c.Ping(t.Context()); err == nil {
		t.Fatal("an untrusted certificate was accepted")
	}
}

// selfSignedCert mints a localhost certificate for the TLS tests. It is
// generated per run rather than committed: a checked-in test certificate gets
// copied into production with depressing regularity, and it expires.
func selfSignedCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating a certificate: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing the certificate: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: parsed}, pool
}

func TestCloseIsIdempotentAndReleasesConnections(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)

	c, err := New(WithAddr(s.address()), WithPoolSize(2))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}

	waitFor(t, 2*time.Second, "the server to see the connections close", func() bool {
		return s.liveConnections() == 0
	})
}

func TestCallsAfterCloseAreErrClosed(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)

	c, err := New(WithAddr(s.address()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if pingErr := c.Ping(t.Context()); pingErr != nil {
		t.Fatalf("Ping: %v", pingErr)
	}
	if closeErr := c.Close(); closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}

	err = c.Ping(t.Context())
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("a call after Close gave %v, want ErrClosed", err)
	}
	if Retryable(err) {
		t.Fatalf("ErrClosed was reported as retryable: %v", err)
	}
}

// Closing the client a view was derived from closes the view too: they share
// one pool, which is what makes WithRetries free to call per call site.
func TestWithRetriesSharesThePool(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s)

	retrying := c.WithRetries(3)
	if err := retrying.Ping(t.Context()); err != nil {
		t.Fatalf("Ping through the derived client: %v", err)
	}
	if s.connectionsOpened() != 1 {
		t.Fatalf("the derived client opened its own connection: %d in total", s.connectionsOpened())
	}

	// A negative count is clamped rather than refused: it is a caller slip, not
	// a reason to fail a call.
	if err := c.WithRetries(-1).Ping(t.Context()); err != nil {
		t.Fatalf("Ping with a negative retry count: %v", err)
	}
}

// The concurrency the pool exists for: many callers, one client, no crossed
// replies. Each caller checks that it got its own value back.
func TestConcurrentCallersGetTheirOwnReplies(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	c := newTestClient(t, s, WithPoolSize(4))
	ctx := t.Context()

	const callers = 16
	for i := range callers {
		if err := c.Set(ctx, key(i), []byte(key(i)+"-value"), 0); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, callers*8)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 8 {
				value, found, err := c.Get(ctx, key(i))
				switch {
				case err != nil:
					errs <- err
				case !found:
					errs <- errors.New("key " + key(i) + " went missing")
				case string(value) != key(i)+"-value":
					errs <- errors.New("caller " + key(i) + " got " + string(value))
				}
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
}

func key(i int) string { return "key-" + string(rune('a'+i%26)) + string(rune('0'+i/26)) }

// A hook that writes nothing at all leaves the client waiting for a reply that
// will never come; the read timeout is what stops it being forever.
func TestReadTimeout(t *testing.T) {
	noLeaks(t)
	s := newFakeServer(t)
	s.setHook(func(_ *bufio.Writer, _ net.Conn, name string, _ [][]byte) bool {
		return name == "GET" // swallowed: no reply is written
	})

	c := newTestClient(t, s, WithReadTimeout(100*time.Millisecond), WithReconnectWindow(0))

	start := time.Now()
	_, _, err := c.Get(t.Context(), "k")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("a swallowed command gave %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the read timeout took %v to fire", elapsed)
	}
}
