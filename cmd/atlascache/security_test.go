package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b3vet/atlascache/internal/config"
	"github.com/b3vet/atlascache/internal/server"
)

// writeCertPair writes a self-signed localhost pair into dir and returns the
// paths. Generated per run rather than committed: a committed certificate is
// one that will expire, in a suite nobody will connect the failure to.
func writeCertPair(t *testing.T, dir, commonName string) (certFile, keyFile string) {
	t.Helper()

	certFile = filepath.Join(dir, "server.crt")
	keyFile = filepath.Join(dir, "server.key")

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: commonName},
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
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(certFile,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyFile,
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))

	return certFile, keyFile
}

func TestNewSecurityDefaultsToNeither(t *testing.T) {
	sec, err := newSecurity(config.Defaults(), zerolog.Nop())

	require.NoError(t, err)
	assert.Nil(t, sec.certs, "no TLS by default")
	assert.False(t, sec.auth.Required(), "no auth by default")
	assert.Len(t, sec.options(), 1, "the authenticator is always wired; TLS is not")
}

func TestNewSecurityLoadsTLS(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeCertPair(t, dir, "atlascache")

	cfg := config.Defaults()
	cfg.TLS = config.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile}
	cfg.Auth = config.AuthConfig{Enabled: true, Token: "a-token"}

	sec, err := newSecurity(cfg, zerolog.Nop())

	require.NoError(t, err)
	require.NotNil(t, sec.certs)
	assert.True(t, sec.auth.Required())
	assert.Len(t, sec.options(), 2)

	tlsConfig := sec.certs.TLSConfig()
	assert.Equal(t, server.MinTLSVersion, tlsConfig.MinVersion, "the floor is TLS 1.3 and is not configurable")
	assert.NotNil(t, tlsConfig.GetCertificate, "the certificate is fetched per handshake, so a swap needs no restart")
}

// TestNewSecurityFailsFastOnAMissingCertificate is the one that keeps an
// operator from believing traffic is encrypted when it is not.
func TestNewSecurityFailsFastOnAMissingCertificate(t *testing.T) {
	dir := t.TempDir()
	_, keyFile := writeCertPair(t, dir, "atlascache")
	missing := filepath.Join(dir, "not-here.crt")

	cfg := config.Defaults()
	cfg.TLS = config.TLSConfig{Enabled: true, CertFile: missing, KeyFile: keyFile}

	sec, err := newSecurity(cfg, zerolog.Nop())

	require.Error(t, err, "a server that cannot load its certificate must not start")
	assert.Nil(t, sec)
	assert.Contains(t, err.Error(), missing, "the message names the file to fix")
}

func TestSecurityAppliesATokenRotation(t *testing.T) {
	sec, err := newSecurity(config.Defaults(), zerolog.Nop())
	require.NoError(t, err)
	require.False(t, sec.auth.Required())

	rotated := config.Defaults()
	rotated.Auth = config.AuthConfig{Enabled: true, Token: "rotated"}
	sec.applyConfig(rotated, zerolog.Nop())

	assert.True(t, sec.auth.Required())
	assert.NoError(t, sec.auth.Verify(nil, []byte("rotated")))
}

// TestCertificateHotReloadThroughTheWatcher is the wiring test: the config
// watcher notices the certificate files, and the keeper validates before it
// swaps. Both halves exist elsewhere; this is the only place they are joined.
func TestCertificateHotReloadThroughTheWatcher(t *testing.T) {
	dir := t.TempDir()
	certDir := filepath.Join(dir, "certs")
	require.NoError(t, os.MkdirAll(certDir, 0o750))
	certFile, keyFile := writeCertPair(t, certDir, "before-rotation")

	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("eviction:\n  policy: lru\n"), 0o600))

	cfg := config.Defaults()
	cfg.TLS = config.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile}
	sec, err := newSecurity(cfg, zerolog.Nop())
	require.NoError(t, err)

	core := startedCore(t, testConfig())
	watcher := watchConfig(configPath, core, sec, nil, zerolog.Nop())
	require.NotNil(t, watcher)
	defer func() { assert.NoError(t, watcher.Stop()) }()

	leaf := func() string {
		cert, certErr := sec.certs.TLSConfig().GetCertificate(nil)
		require.NoError(t, certErr)
		return cert.Leaf.Subject.CommonName
	}
	require.Equal(t, "before-rotation", leaf())

	t.Run("a valid rotation is picked up without a restart", func(t *testing.T) {
		writeCertPair(t, certDir, "after-rotation")

		assert.True(t, eventually(t, 10*time.Second, func() bool { return leaf() == "after-rotation" }),
			"the certificate on disk changed and the server never noticed")
	})

	t.Run("an invalid replacement leaves the running certificate in place", func(t *testing.T) {
		require.NoError(t, os.WriteFile(certFile, []byte("this is not a certificate"), 0o600))

		_, failuresBefore := sec.certs.Stats()
		assert.True(t, eventually(t, 10*time.Second, func() bool {
			_, failures := sec.certs.Stats()
			return failures > failuresBefore
		}), "the bad pair was never even attempted")

		assert.Equal(t, "after-rotation", leaf(),
			"a broken certificate on disk must not take the listener down with it")
	})
}

func TestLogExposureNamesWhatIsExposed(t *testing.T) {
	t.Run("all three disabled, all three warned about", func(t *testing.T) {
		var out bytes.Buffer
		logExposure(zerolog.New(&out), config.Defaults(), "127.0.0.1:8080")

		written := out.String()
		// The wording has to name the consequence, not the setting: an operator
		// who reads "tls.enabled is false" learns only what they typed.
		assert.Contains(t, written, "traffic is unencrypted")
		assert.Contains(t, written, "full access")
		assert.Contains(t, written, "admin.token is not set")
		assert.Contains(t, written, "warn")
	})

	t.Run("all three set, nothing warned about", func(t *testing.T) {
		var out bytes.Buffer
		cfg := config.Defaults()
		cfg.TLS.Enabled = true
		cfg.Auth.Enabled = true
		cfg.Admin.Token = "an-admin-token"

		logExposure(zerolog.New(&out), cfg, "127.0.0.1:8080")

		assert.Empty(t, out.String())
	})

	// What is left of P0's admin warning. The combination it named — exposed
	// and unauthenticated — is now refused by validation, so the warning that
	// remains covers the case that is merely worth saying out loud.
	t.Run("a non-loopback bind with a token is exposure worth naming", func(t *testing.T) {
		var out bytes.Buffer
		cfg := config.Defaults()
		cfg.TLS.Enabled = true
		cfg.Auth.Enabled = true
		cfg.Admin.BindAddr = "0.0.0.0"
		cfg.Admin.Token = "an-admin-token"

		logExposure(zerolog.New(&out), cfg, "0.0.0.0:8080")

		written := out.String()
		assert.Contains(t, written, "reachable from other hosts")
		assert.NotContains(t, written, "an-admin-token", "a warning about a token must not carry it")
	})
}

// TestWatchCertificatesIsOptional checks the paths that must not panic: no TLS,
// and no watcher.
func TestWatchCertificatesIsOptional(t *testing.T) {
	sec, err := newSecurity(config.Defaults(), zerolog.Nop())
	require.NoError(t, err)

	assert.NotPanics(t, func() { sec.watchCertificates(nil, zerolog.Nop()) })
}

// TestSecurityServerAcceptsTLS proves the options actually reach a listener.
func TestSecurityServerAcceptsTLS(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeCertPair(t, dir, "wired")

	cfg := config.Defaults()
	cfg.TLS = config.TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile}
	sec, err := newSecurity(cfg, zerolog.Nop())
	require.NoError(t, err)

	c := startedCore(t, testConfig())
	srv, err := server.New(context.Background(), "127.0.0.1:0", zerolog.Nop(),
		keyspace{engine: c.engine}, sec.options()...)
	require.NoError(t, err)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		assert.NoError(t, srv.Shutdown(ctx))
		assert.NoError(t, <-serveErr)
	}()

	// A plaintext dial gets no reply: the handshake fails and the transport
	// closes the connection rather than serving it.
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDial()
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(dialCtx, "tcp", srv.Addr())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = conn.Write([]byte("*1\r\n$4\r\nPING\r\n"))
	require.NoError(t, err)

	buf := make([]byte, 32)
	n, readErr := conn.Read(buf)
	require.Error(t, readErr)
	assert.NotContains(t, string(buf[:n]), "PONG")
}
