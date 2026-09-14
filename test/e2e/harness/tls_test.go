package harness

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/b3vet/atlascache/test/e2e/certs"
)

func TestTLSRequested(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]any
		want   bool
	}{
		{name: "no config at all"},
		{name: "no tls block", config: map[string]any{"storage": map[string]any{"shard_count": 1}}},
		{name: "tls off", config: map[string]any{"tls": map[string]any{"enabled": false}}},
		{name: "tls on", config: map[string]any{"tls": map[string]any{"enabled": true}}, want: true},
		{
			name: "tls on, decoded as a YAML mapping with any keys",
			// yaml.v3 produces map[any]any for some shapes, and a spec could
			// legitimately arrive as either.
			config: map[string]any{"tls": map[any]any{"enabled": true}},
			want:   true,
		},
		{name: "enabled is not a bool", config: map[string]any{"tls": map[string]any{"enabled": "yes"}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tlsRequested(tc.config); got != tc.want {
				t.Errorf("tlsRequested = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWithTLSPaths(t *testing.T) {
	original := map[string]any{
		"tls":     map[string]any{"enabled": true, "cert_file": "/etc/somewhere/else.crt"},
		"storage": map[string]any{"shard_count": 4},
	}

	updated := withTLSPaths(original, "/tmp/spec/server.crt", "/tmp/spec/server.key")

	section, ok := asMap(updated["tls"])
	if !ok {
		t.Fatal("the tls block went missing")
	}
	if section["cert_file"] != "/tmp/spec/server.crt" || section["key_file"] != "/tmp/spec/server.key" {
		t.Errorf("paths = %v, want the harness's own", section)
	}
	if section["enabled"] != true {
		t.Error("enabled was lost")
	}
	if storage, _ := asMap(updated["storage"]); storage["shard_count"] != 4 {
		t.Error("an unrelated block was lost")
	}

	// The spec's own value is replaced rather than honored: the certificate
	// lives in the directory the harness cleans up, and a spec pointing
	// elsewhere would leave files behind.
	if before, _ := asMap(original["tls"]); before["cert_file"] != "/etc/somewhere/else.crt" {
		t.Error("the spec's config map was mutated; it belongs to the spec")
	}
}

// tlsHarnessFor builds a harness that has generated its own certificate.
func tlsHarnessFor(t *testing.T) *Process {
	t.Helper()
	return newHarness(t, map[string]any{"tls": map[string]any{"enabled": true}})
}

func TestHarnessGeneratesItsOwnCertificate(t *testing.T) {
	h := tlsHarnessFor(t)

	if !h.TLSEnabled() {
		t.Fatal("the harness did not notice that the spec asked for TLS")
	}

	certFile, keyFile := h.CertPaths()
	if certFile == "" || keyFile == "" {
		t.Fatal("no certificate paths")
	}
	// Inside the harness's own directory, so cleanup takes them with
	// everything else and no fixture is ever committed.
	if filepath.Dir(filepath.Dir(certFile)) != h.Root() {
		t.Errorf("the certificate is at %s, outside the harness root %s", certFile, h.Root())
	}
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		t.Fatalf("the generated pair does not load: %v", err)
	}

	// And the config the server is given names them.
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeConfig(path, h.dataDir, "spec", 1234, 1235, h.config); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	rendered, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the config: %v", err)
	}
	for _, want := range []string{certFile, keyFile} {
		if !strings.Contains(string(rendered), want) {
			t.Errorf("the config does not name %s:\n%s", want, rendered)
		}
	}
}

func TestHarnessWithoutTLS(t *testing.T) {
	h := newHarness(t, nil)

	if h.TLSEnabled() {
		t.Error("TLS is on for a spec that did not ask for it")
	}
	if certFile, keyFile := h.CertPaths(); certFile != "" || keyFile != "" {
		t.Errorf("certificates were generated for a plaintext spec: %s %s", certFile, keyFile)
	}
	if _, err := h.ClientTLSConfig(); err == nil {
		t.Error("a plaintext harness produced a TLS client config")
	}
}

func TestClientTLSConfigFollowsRotations(t *testing.T) {
	h := tlsHarnessFor(t)
	certFile, keyFile := h.CertPaths()

	first, err := h.ClientTLSConfig()
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	if first.ServerName != certs.ServerName || first.MinVersion != tls.VersionTLS13 {
		t.Errorf("client config = %+v, want a verified TLS 1.3 config", first)
	}

	// After a rotation both certificates are trusted: during a rotation a
	// connection may be made against either, and a client that trusted only the
	// newest would fail for reasons that are the rig's rather than the server's.
	if err := certs.WriteTo(certFile, keyFile, "rotated"); err != nil {
		t.Fatalf("rotating: %v", err)
	}
	if _, err := h.ClientTLSConfig(); err != nil {
		t.Fatalf("ClientTLSConfig after a rotation: %v", err)
	}
	if len(h.trusted) != 2 {
		t.Errorf("trusted %d certificates after one rotation, want 2", len(h.trusted))
	}

	// Reading the same file again adds nothing.
	if _, err := h.ClientTLSConfig(); err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	if len(h.trusted) != 2 {
		t.Errorf("trusted %d certificates, want the duplicate to be ignored", len(h.trusted))
	}

	// And a certificate file that is no longer a certificate does not cost the
	// client the anchors it already has — which is the state the server is left
	// in when a renewal writes something broken.
	if err := certs.WriteInvalid(certFile); err != nil {
		t.Fatalf("writing an invalid certificate: %v", err)
	}
	if _, err := h.ClientTLSConfig(); err != nil {
		t.Fatalf("ClientTLSConfig with a broken file on disk: %v", err)
	}
}

// TestDialSpeaksWhateverTheServerSpeaks checks the branch a spec never sees:
// the harness dials TLS when the server is serving TLS, and plain TCP
// otherwise, and the steps are identical either way.
func TestDialSpeaksWhateverTheServerSpeaks(t *testing.T) {
	h := tlsHarnessFor(t)
	certFile, keyFile := h.CertPaths()

	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("loading the pair: %v", err)
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{pair},
	})
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	defer func() { _ = listener.Close() }()

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		// Complete the handshake and answer nothing; the dial is the assertion.
		if _, readErr := conn.Read(make([]byte, 1)); readErr != nil {
			_ = conn.Close()
			return
		}
		_ = conn.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h.clientAddr = listener.Addr().String()
	conn, err := h.dial(ctx)
	if err != nil {
		t.Fatalf("dialing a TLS server: %v", err)
	}
	defer func() { _ = conn.Close() }()

	state, ok := conn.ConnectionState()
	if !ok {
		t.Fatal("the harness opened a plaintext connection to a TLS server")
	}
	if state.Version != tls.VersionTLS13 {
		t.Errorf("negotiated %#04x, want TLS 1.3", state.Version)
	}

	t.Run("a plaintext harness dials plaintext", func(t *testing.T) {
		var listenConfig net.ListenConfig
		plain, listenErr := listenConfig.Listen(ctx, "tcp", "127.0.0.1:0")
		if listenErr != nil {
			t.Fatalf("listening: %v", listenErr)
		}
		defer func() { _ = plain.Close() }()

		other := newHarness(t, nil)
		other.clientAddr = plain.Addr().String()

		plainConn, dialErr := other.dial(ctx)
		if dialErr != nil {
			t.Fatalf("dialing a plaintext server: %v", dialErr)
		}
		defer func() { _ = plainConn.Close() }()

		if _, encrypted := plainConn.ConnectionState(); encrypted {
			t.Error("a plaintext connection reports TLS state")
		}
	})
}
