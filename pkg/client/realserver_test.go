package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The SDK against the real server.
//
// Everything else in this package tests the client against a server written for
// the purpose, which proves the client and proves nothing about the two agreeing
// on the wire. These tests build and run cmd/atlascache and talk to it — the
// only place the SDK's independently written RESP implementation meets the
// server's own.
//
// They cannot be an E2E spec: the SDK is a separate module and the E2E suite
// belongs to a third (ADR-0015). They reach the server by building it, not by
// importing it, so the SDK's go.mod stays empty.

// serverBinary builds cmd/atlascache once per test run and returns its path.
// A checkout without the server's source — the SDK downloaded on its own —
// skips rather than fails, because there is nothing there to be wrong.
var serverBinary = sync.OnceValues(func() (string, error) {
	root, err := filepath.Abs("../..")
	if err != nil {
		return "", err
	}
	if _, statErr := os.Stat(filepath.Join(root, "cmd", "atlascache")); statErr != nil {
		return "", os.ErrNotExist
	}

	dir, err := os.MkdirTemp("", "atlascache-sdk-test")
	if err != nil {
		return "", err
	}
	binary := filepath.Join(dir, "atlascache")

	cmd := exec.CommandContext(context.Background(), "go", "build", "-o", binary, "./cmd/atlascache")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", errors.New("building the server: " + err.Error() + "\n" + string(out))
	}
	return binary, nil
})

// realServer is one atlascache process, started on a port the test chose.
type realServer struct {
	t      *testing.T
	binary string
	addr   string
	config string
	cmd    *exec.Cmd
	log    *syncBuffer
	mu     sync.Mutex
}

// syncBuffer collects the server's output. exec copies stdout on a goroutine of
// its own, so the buffer needs a lock: without one the race detector is right
// to complain, and it would complain about the test rather than the SDK.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type realServerConfig struct {
	authToken string
	certFile  string
	keyFile   string
}

func startRealServer(t *testing.T, cfg realServerConfig) *realServer {
	t.Helper()

	binary, err := serverBinary()
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("the server's source is not in this checkout; nothing to run the SDK against")
	}
	if err != nil {
		t.Fatalf("%v", err)
	}

	addr := freePort(t)
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("splitting %q: %v", addr, err)
	}
	_, adminPort, err := net.SplitHostPort(freePort(t))
	if err != nil {
		t.Fatalf("splitting an admin address: %v", err)
	}

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(renderConfig(port, adminPort, dir, cfg)), 0o600); err != nil {
		t.Fatalf("writing the server config: %v", err)
	}

	s := &realServer{t: t, binary: binary, addr: addr, config: configPath, log: &syncBuffer{}}
	s.launch()
	t.Cleanup(s.stop)
	return s
}

// renderConfig writes the YAML by hand rather than through a marshaller: the
// SDK module has no dependencies, and a dozen lines of static configuration is
// not a reason for its first one.
func renderConfig(port, adminPort, dataDir string, cfg realServerConfig) string {
	var b strings.Builder
	b.WriteString("node:\n  data_dir: \"" + dataDir + "\"\n")
	b.WriteString("server:\n  bind_addr: \"127.0.0.1\"\n  client_port: " + port + "\n")
	b.WriteString("admin:\n  bind_addr: \"127.0.0.1\"\n  port: " + adminPort + "\n")
	b.WriteString("logging:\n  level: \"error\"\n  format: \"json\"\n")

	if cfg.authToken != "" {
		b.WriteString("auth:\n  enabled: true\n  token: \"" + cfg.authToken + "\"\n")
	}
	if cfg.certFile != "" {
		b.WriteString("tls:\n  enabled: true\n  cert_file: \"" + cfg.certFile + "\"\n  key_file: \"" + cfg.keyFile + "\"\n")
	}
	return b.String()
}

func (s *realServer) launch() {
	s.t.Helper()

	cmd := exec.CommandContext(context.Background(), s.binary, "--config", s.config)
	s.mu.Lock()
	s.log = &syncBuffer{}
	cmd.Stdout = s.log
	cmd.Stderr = s.log
	s.cmd = cmd
	s.mu.Unlock()

	if err := cmd.Start(); err != nil {
		s.t.Fatalf("starting the server: %v", err)
	}
	s.waitUntilListening()
}

func (s *realServer) waitUntilListening() {
	s.t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for {
		dialer := net.Dialer{Timeout: 200 * time.Millisecond}
		conn, err := dialer.DialContext(context.Background(), "tcp", s.addr)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			s.mu.Lock()
			output := s.log
			s.mu.Unlock()
			s.t.Fatalf("the server never accepted a connection on %s:\n%s", s.addr, output.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (s *realServer) stop() {
	s.mu.Lock()
	cmd := s.cmd
	s.cmd = nil
	s.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}
	// Both errors are dropped deliberately: the process is being killed, so
	// "already finished" is the expected answer to each. Wait rather than
	// Process.Wait, because it also waits for the goroutines copying the
	// process's output — which would otherwise still be writing into a buffer
	// the next launch has replaced.
	_ = cmd.Process.Kill() //nolint:errcheck // see above
	_ = cmd.Wait()         //nolint:errcheck // see above
}

func (s *realServer) restart() {
	s.stop()
	s.launch()
}

// The end-to-end binary safety claim: bytes the SDK writes come back byte for
// byte from a server that never agreed to anything but the specification.
func TestRealServerIsBinarySafe(t *testing.T) {
	noLeaks(t)
	s := startRealServer(t, realServerConfig{})
	c := newRealClient(t, s.addr)
	ctx := t.Context()

	cases := map[string]struct {
		key   string
		value []byte
	}{
		"null bytes":        {"key\x00with\x00nulls", []byte("value\x00with\x00nulls")},
		"invalid UTF-8":     {"key-\xff\xfe", []byte{0xff, 0xfe, 0xc3, 0x28, 0x80}},
		"CRLF in the value": {"crlf", []byte("value\r\n*1\r\n$4\r\nPING\r\n")},
		"every byte":        {"all-bytes", allBytes()},
		"empty value":       {"empty", []byte{}},
		// Comfortably under the server's 1MB default for storage.max_value_size:
		// the claim here is that a large binary value survives the trip, not that
		// the engine's limit can be exceeded.
		"a large value": {"large", bytes.Repeat([]byte{0x00, 0x01, 0xff}, 100_000)},
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
				t.Fatalf("the value came back changed: %d bytes, want %d", len(got), len(tc.value))
			}

			echoed, err := c.Echo(ctx, tc.value)
			if err != nil || !bytes.Equal(echoed, tc.value) {
				t.Fatalf("ECHO gave %d bytes (%v), want %d", len(echoed), err, len(tc.value))
			}
		})
	}
}

// Every typed method against the real command surface, which is the check that
// the SDK's idea of each reply matches the server's.
//
// It is one server and a sequence of helpers rather than independent tests,
// because the keyspace each step leaves behind is what the next one reads —
// which is also how a caller uses the SDK.
func TestRealServerTypedMethods(t *testing.T) {
	noLeaks(t)
	s := startRealServer(t, realServerConfig{})
	c := newRealClient(t, s.addr)
	ctx := t.Context()

	checkRealWrites(t, ctx, c)
	checkRealExistence(t, ctx, c)
	checkRealExpiry(t, ctx, c)
	checkRealIteration(t, ctx, c)
	checkRealIntrospection(t, ctx, c)
	checkRealEscapeHatch(t, ctx, c)
}

func checkRealWrites(t *testing.T, ctx context.Context, c Client) {
	t.Helper()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := c.Set(ctx, "a", []byte("1"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.SetString(ctx, "b", "2", time.Hour); err != nil {
		t.Fatalf("SetString: %v", err)
	}
	if value, found, err := c.GetString(ctx, "a"); err != nil || !found || value != "1" {
		t.Fatalf("GetString gave (%q, %v, %v)", value, found, err)
	}
	if _, found, err := c.Get(ctx, "missing"); err != nil || found {
		t.Fatalf("Get on a missing key gave (%v, %v)", found, err)
	}
}

func checkRealExistence(t *testing.T, ctx context.Context, c Client) {
	t.Helper()

	if stored, err := c.SetNX(ctx, "a", []byte("other")); err != nil || stored {
		t.Fatalf("SetNX on a live key gave (%v, %v)", stored, err)
	}
	if stored, err := c.SetNX(ctx, "c", []byte("3")); err != nil || !stored {
		t.Fatalf("SetNX on a free key gave (%v, %v)", stored, err)
	}
	if count, err := c.Exists(ctx, "a", "b", "missing"); err != nil || count != 2 {
		t.Fatalf("Exists gave (%d, %v)", count, err)
	}
}

func checkRealExpiry(t *testing.T, ctx context.Context, c Client) {
	t.Helper()

	if ttl, err := c.TTL(ctx, "a"); err != nil || ttl != TTLNoExpiry {
		t.Fatalf("TTL on a key with no expiry gave (%v, %v)", ttl, err)
	}
	if ttl, err := c.TTL(ctx, "missing"); err != nil || ttl != TTLNoKey {
		t.Fatalf("TTL on a missing key gave (%v, %v)", ttl, err)
	}
	if applied, err := c.Expire(ctx, "a", time.Hour); err != nil || !applied {
		t.Fatalf("Expire gave (%v, %v)", applied, err)
	}
	if ttl, err := c.TTL(ctx, "a"); err != nil || ttl < 59*time.Minute {
		t.Fatalf("TTL after Expire gave (%v, %v)", ttl, err)
	}
}

func checkRealIteration(t *testing.T, ctx context.Context, c Client) {
	t.Helper()

	keys, err := c.Keys(ctx, "*")
	if err != nil || len(keys) != 3 {
		t.Fatalf("Keys gave (%v, %v)", keys, err)
	}

	seen := map[string]bool{}
	for cursor := ScanStart; ; {
		page, pageErr := c.Scan(ctx, cursor, "*", 10)
		if pageErr != nil {
			t.Fatalf("Scan: %v", pageErr)
		}
		for _, key := range page.Keys {
			seen[key] = true
		}
		if page.Done() {
			break
		}
		cursor = page.Cursor
	}
	if len(seen) != 3 {
		t.Fatalf("a full scan saw %d keys, want 3", len(seen))
	}
}

func checkRealIntrospection(t *testing.T, ctx context.Context, c Client) {
	t.Helper()

	if size, err := c.DBSize(ctx); err != nil || size != 3 {
		t.Fatalf("DBSize gave (%d, %v)", size, err)
	}

	info, err := c.Info(ctx)
	if err != nil || !strings.Contains(info, "atlascache_version") {
		t.Fatalf("Info gave (%d bytes, %v)", len(info), err)
	}
	if section, sectionErr := c.Info(ctx, "server"); sectionErr != nil || !strings.Contains(section, "# Server") {
		t.Fatalf("Info server gave (%d bytes, %v)", len(section), sectionErr)
	}

	stats, err := c.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if keys, ok := stats.Value("keys"); !ok || keys != 3 {
		t.Fatalf("Stats reported %v", stats)
	}

	if removed, err := c.Del(ctx, "a", "b", "missing"); err != nil || removed != 2 {
		t.Fatalf("Del gave (%d, %v)", removed, err)
	}
}

// The escape hatch, against a command the typed methods do not wrap.
func checkRealEscapeHatch(t *testing.T, ctx context.Context, c Client) {
	t.Helper()

	reply, err := c.Do(ctx, "COMMAND", "DOCS")
	if err != nil {
		t.Fatalf("Do COMMAND DOCS: %v", err)
	}
	if reply.Type != TypeArray {
		t.Fatalf("COMMAND DOCS came back as %v", reply.Type)
	}
}

// The server answers HELLO 3 with -NOPROTO (ADR-0028) and the SDK has to carry
// on regardless. This asserts it against the server that actually sends it.
func TestRealServerProtocolNegotiation(t *testing.T) {
	noLeaks(t)
	s := startRealServer(t, realServerConfig{})
	c := newRealClient(t, s.addr)
	ctx := t.Context()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("the connection did not survive HELLO 3: %v", err)
	}

	// And the fallback really was to RESP2, not an accepted HELLO 3.
	reply, err := c.Do(ctx, "HELLO", "3")
	if err == nil {
		t.Fatalf("the server accepted HELLO 3, returning %+v", reply)
	}
	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != "NOPROTO" {
		t.Fatalf("HELLO 3 gave %v, want -NOPROTO", err)
	}
}

func TestRealServerAuth(t *testing.T) {
	noLeaks(t)
	s := startRealServer(t, realServerConfig{authToken: "s3cret-token"})

	t.Run("the single-argument form", func(t *testing.T) {
		c := newRealClient(t, s.addr, WithAuth("s3cret-token"))
		if err := c.Set(t.Context(), "k", []byte("v"), 0); err != nil {
			t.Fatalf("Set after AUTH: %v", err)
		}
	})

	t.Run("the two-argument form", func(t *testing.T) {
		c := newRealClient(t, s.addr, WithUserAuth("default", "s3cret-token"))
		if _, err := c.DBSize(t.Context()); err != nil {
			t.Fatalf("DBSIZE after AUTH default: %v", err)
		}
	})

	t.Run("a wrong token", func(t *testing.T) {
		c := newRealClient(t, s.addr, WithAuth("wrong"))
		if err := c.Ping(t.Context()); !errors.Is(err, ErrAuth) {
			t.Fatalf("got %v, want ErrAuth", err)
		}
	})

	t.Run("no token at all", func(t *testing.T) {
		c := newRealClient(t, s.addr)
		if _, err := c.DBSize(t.Context()); !errors.Is(err, ErrAuth) {
			t.Fatalf("an unauthenticated command gave %v, want ErrAuth", err)
		}
	})
}

func TestRealServerTLS(t *testing.T) {
	noLeaks(t)

	dir := t.TempDir()
	certFile, keyFile, pool := writeCertificate(t, dir)
	s := startRealServer(t, realServerConfig{certFile: certFile, keyFile: keyFile, authToken: "s3cret-token"})

	c := newRealClient(t, s.addr,
		WithTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}),
		WithAuth("s3cret-token"),
	)

	if err := c.Set(t.Context(), "over-tls", []byte{0x00, 0xff}, 0); err != nil {
		t.Fatalf("Set over TLS: %v", err)
	}
	value, found, err := c.Get(t.Context(), "over-tls")
	if err != nil || !found || !bytes.Equal(value, []byte{0x00, 0xff}) {
		t.Fatalf("Get over TLS gave (%q, %v, %v)", value, found, err)
	}
}

// The acceptance criterion against a real process: kill the server, start it
// again, and the client carries on having surfaced at most one error.
func TestRealServerRestart(t *testing.T) {
	noLeaks(t)
	s := startRealServer(t, realServerConfig{})
	c := newRealClient(t, s.addr, WithPoolSize(1))
	ctx := t.Context()

	if err := c.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	var (
		mu       sync.Mutex
		failures []error
		calls    int
		wg       sync.WaitGroup
	)

	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			err := c.Ping(ctx)
			mu.Lock()
			calls++
			if err != nil {
				failures = append(failures, err)
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
		}
	}()

	time.Sleep(100 * time.Millisecond)
	s.restart()
	time.Sleep(200 * time.Millisecond)
	close(done)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls < 10 {
		t.Fatalf("the workload managed only %d calls", calls)
	}
	if len(failures) > 1 {
		t.Fatalf("a restart surfaced %d errors, want at most 1: %v", len(failures), failures)
	}

	// The keyspace is gone — there is no persistence in v0.1.0 — but the
	// client is not, which is the claim.
	if _, _, err := c.Get(ctx, "k"); err != nil {
		t.Fatalf("the client did not recover from the restart: %v", err)
	}
}

// Cancellation against the real server, where the reply really is in flight.
func TestRealServerCancellation(t *testing.T) {
	noLeaks(t)
	s := startRealServer(t, realServerConfig{})
	c := newRealClient(t, s.addr, WithPoolSize(2))
	ctx := t.Context()

	for i := range 2000 {
		if err := c.Set(ctx, "key-"+strings.Repeat("x", i%40), []byte("v"), 0); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if _, err := c.Keys(canceled, "*"); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled KEYS gave %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the canceled call took %v", elapsed)
	}

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("the client was unusable after a cancellation: %v", err)
	}
}

func newRealClient(t *testing.T, addr string, opts ...Option) Client {
	t.Helper()
	c, err := New(append([]Option{WithAddr(addr)}, opts...)...)
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

// writeCertificate puts a freshly minted localhost certificate on disk, which
// is the only form the server takes one in.
func writeCertificate(t *testing.T, dir string) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()

	cert, pool := selfSignedCert(t)

	certFile = filepath.Join(dir, "server.crt")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("writing the certificate: %v", err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatalf("marshaling the key: %v", err)
	}
	keyFile = filepath.Join(dir, "server.key")
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("writing the key: %v", err)
	}

	return certFile, keyFile, pool
}
