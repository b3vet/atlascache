package server

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writePair writes a self-signed certificate and key for localhost, and returns
// their paths.
//
// The certificates are generated per test rather than committed as fixtures:
// committed test certificates get copied into production, and they expire — at
// which point a suite that was green for a year starts failing for reasons that
// have nothing to do with the code (FEAT-0023).
func writePair(t *testing.T, dir, commonName string) (certFile, keyFile string) {
	t.Helper()

	certFile = filepath.Join(dir, "server.crt")
	keyFile = filepath.Join(dir, "server.key")
	writePairAt(t, certFile, keyFile, commonName, time.Now().Add(time.Hour))
	return certFile, keyFile
}

// writePairAt writes a pair to exact paths, with an expiry the caller chooses.
func writePairAt(t *testing.T, certFile, keyFile, commonName string, notAfter time.Time) {
	t.Helper()

	certPEM, keyPEM := makePair(t, commonName, notAfter)
	require.NoError(t, os.WriteFile(certFile, certPEM, 0o600))
	require.NoError(t, os.WriteFile(keyFile, keyPEM, 0o600))
}

func makePair(t *testing.T, commonName string, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)

	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              notAfter,
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

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// newTLSServer starts a server serving TLS from a freshly generated pair.
func newTLSServer(t *testing.T) (*Server, *CertificateKeeper) {
	t.Helper()

	dir := t.TempDir()
	certFile, keyFile := writePair(t, dir, "atlascache-v1")

	keeper, err := NewCertificateKeeper(certFile, keyFile, zerolog.Nop())
	require.NoError(t, err)

	srv, err := New(context.Background(), "127.0.0.1:0", zerolog.Nop(), newFakeStore(),
		WithTLS(keeper.TLSConfig()))
	require.NoError(t, err)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()
	t.Cleanup(func() { shutdownServer(t, srv, serveErr) })

	return srv, keeper
}

// dialTLS opens a verified TLS connection, trusting whatever certificate the
// keeper is currently serving.
func dialTLS(t *testing.T, srv *Server, keeper *CertificateKeeper) *tls.Conn {
	t.Helper()

	conn, err := dialTLSWith(t, srv.Addr(), clientConfig(t, keeper))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// dialTLSWith dials with an exact client configuration, and returns the
// handshake error rather than failing, for the tests whose subject is the
// failure.
func dialTLSWith(t *testing.T, addr string, cfg *tls.Config) (*tls.Conn, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dialer := &tls.Dialer{Config: cfg}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	tlsConn, ok := conn.(*tls.Conn)
	require.True(t, ok, "a tls.Dialer returned something that is not a tls.Conn")
	return tlsConn, nil
}

// dialPlaintext opens an ordinary TCP connection, for the tests that check what
// happens to a client that has not been told the port is encrypted.
func dialPlaintext(t *testing.T, addr string) (net.Conn, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dialer := net.Dialer{Timeout: 2 * time.Second}
	return dialer.DialContext(ctx, "tcp", addr)
}

// clientConfig trusts the keeper's current certificate, so verification is real
// rather than skipped. A test that dialed with InsecureSkipVerify would pass
// against a server presenting no certificate at all.
func clientConfig(t *testing.T, keeper *CertificateKeeper) *tls.Config {
	t.Helper()

	certFile, _ := keeper.Files()
	pemBytes, err := os.ReadFile(certFile)
	require.NoError(t, err)

	return clientConfigFor(t, pemBytes)
}

// clientConfigFor trusts exactly the certificate given, for the cases where the
// file on disk is deliberately not the one being served.
func clientConfigFor(t *testing.T, certPEM []byte) *tls.Config {
	t.Helper()

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(certPEM))

	return &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS13}
}

// leafName reads the common name of the certificate the keeper would serve now.
func leafName(t *testing.T, keeper *CertificateKeeper) string {
	t.Helper()

	cert, err := keeper.getCertificate(nil)
	require.NoError(t, err)
	require.NotNil(t, cert.Leaf)

	return cert.Leaf.Subject.CommonName
}

func TestCertificateKeeperFailsFast(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writePair(t, dir, "atlascache")

	t.Run("no paths at all", func(t *testing.T) {
		_, err := NewCertificateKeeper("", "", zerolog.Nop())
		require.Error(t, err)
	})

	t.Run("a missing certificate names the certificate", func(t *testing.T) {
		missing := filepath.Join(dir, "absent.crt")
		_, err := NewCertificateKeeper(missing, keyFile, zerolog.Nop())

		require.Error(t, err)
		// The file, by name. An operator reading the exit message must not have
		// to guess which of the two paths was wrong.
		assert.Contains(t, err.Error(), missing)
		assert.Contains(t, err.Error(), "tls.cert_file")
	})

	t.Run("a missing key names the key", func(t *testing.T) {
		missing := filepath.Join(dir, "absent.key")
		_, err := NewCertificateKeeper(certFile, missing, zerolog.Nop())

		require.Error(t, err)
		assert.Contains(t, err.Error(), missing)
		assert.Contains(t, err.Error(), "tls.key_file")
	})

	t.Run("a key that does not match its certificate", func(t *testing.T) {
		otherCert, otherKey := filepath.Join(dir, "other.crt"), filepath.Join(dir, "other.key")
		writePairAt(t, otherCert, otherKey, "someone-else", time.Now().Add(time.Hour))

		_, err := NewCertificateKeeper(certFile, otherKey, zerolog.Nop())

		require.Error(t, err)
		assert.Contains(t, err.Error(), certFile)
		assert.Contains(t, err.Error(), otherKey)
	})

	t.Run("a certificate that is not a certificate", func(t *testing.T) {
		junk := filepath.Join(dir, "junk.crt")
		require.NoError(t, os.WriteFile(junk, []byte("-----BEGIN CERTIFICATE-----\nnope\n"), 0o600))

		_, err := NewCertificateKeeper(junk, keyFile, zerolog.Nop())
		require.Error(t, err)
	})
}

// TestCertificateKeeperValidatesBeforeSwap is the subtle one.
//
// A renewal that writes a broken pair — interrupted mid-copy, a key that does
// not match, a file the process cannot read — must leave the running
// certificate in place. The alternative is a server that cannot complete a
// handshake, which turns a routine 90-day rotation into an outage, and does so
// at the moment nobody is watching.
func TestCertificateKeeperValidatesBeforeSwap(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writePair(t, dir, "first")

	keeper, err := NewCertificateKeeper(certFile, keyFile, zerolog.Nop())
	require.NoError(t, err)
	require.Equal(t, "first", leafName(t, keeper))

	t.Run("garbage on disk is rejected and changes nothing", func(t *testing.T) {
		require.NoError(t, os.WriteFile(certFile, []byte("not a certificate at all"), 0o600))

		require.Error(t, keeper.Reload())
		assert.Equal(t, "first", leafName(t, keeper), "the running certificate must survive a bad replacement")

		reloads, failures := keeper.Stats()
		assert.Zero(t, reloads)
		assert.Equal(t, uint64(1), failures)
	})

	t.Run("a half-written pair is rejected", func(t *testing.T) {
		// The realistic shape of the failure: the certificate has been replaced
		// and the key has not yet, so the two do not match.
		newCert, newKey := makePair(t, "second", time.Now().Add(time.Hour))
		require.NoError(t, os.WriteFile(certFile, newCert, 0o600))

		require.Error(t, keeper.Reload())
		assert.Equal(t, "first", leafName(t, keeper))

		t.Run("and the completed pair is accepted", func(t *testing.T) {
			require.NoError(t, os.WriteFile(keyFile, newKey, 0o600))

			require.NoError(t, keeper.Reload())
			assert.Equal(t, "second", leafName(t, keeper))

			reloads, _ := keeper.Stats()
			assert.Equal(t, uint64(1), reloads)
		})
	})

	t.Run("a certificate that disappears is rejected", func(t *testing.T) {
		require.NoError(t, os.Remove(certFile))

		err := keeper.Reload()
		require.Error(t, err)
		assert.Contains(t, err.Error(), certFile)
		assert.Equal(t, "second", leafName(t, keeper), "still serving what it had")
	})
}

// TestCertificateKeeperAcceptsAnExpiredCertificate checks that expiry is a
// warning and not a refusal. Refusing would take a server down over a clock
// problem or a late renewal, which is worse than serving a certificate clients
// will complain about — and the operator can see both in the log.
func TestCertificateKeeperAcceptsAnExpiredCertificate(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "expired.crt")
	keyFile := filepath.Join(dir, "expired.key")
	writePairAt(t, certFile, keyFile, "expired", time.Now().Add(-time.Minute))

	logs := &syncBuffer{}
	keeper, err := NewCertificateKeeper(certFile, keyFile, zerolog.New(logs))

	require.NoError(t, err)
	assert.Equal(t, "expired", leafName(t, keeper))
	assert.Contains(t, logs.String(), "has expired")
}

func TestTLSServesCommands(t *testing.T) {
	srv, keeper := newTLSServer(t)

	conn := dialTLS(t, srv, keeper)
	reader := bufio.NewReader(conn)

	state := conn.ConnectionState()
	assert.Equal(t, uint16(tls.VersionTLS13), state.Version, "the floor is 1.3 and there is nothing below it")

	send(t, conn, "*1\r\n$4\r\nPING\r\n")
	assert.Equal(t, "+PONG\r\n", readReply(t, conn, reader))

	send(t, conn, "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n")
	assert.Equal(t, "+OK\r\n", readReply(t, conn, reader))

	send(t, conn, "*2\r\n$3\r\nGET\r\n$1\r\nk\r\n")
	assert.Equal(t, "$1\r\n", readReply(t, conn, reader))
	assert.Equal(t, "v\r\n", readReply(t, conn, reader))
}

// TestTLSRefusesOlderVersions checks the floor. Offering 1.2 as a configurable
// option is how a deployment ends up quietly using it.
func TestTLSRefusesOlderVersions(t *testing.T) {
	srv, keeper := newTLSServer(t)

	for _, version := range []struct {
		name string
		max  uint16
	}{
		{"TLS 1.2", tls.VersionTLS12},
		{"TLS 1.1", tls.VersionTLS11},
		{"TLS 1.0", tls.VersionTLS10},
	} {
		t.Run(version.name+" is refused", func(t *testing.T) {
			cfg := clientConfig(t, keeper)
			cfg.MinVersion = tls.VersionTLS10
			cfg.MaxVersion = version.max

			conn, err := dialTLSWith(t, srv.Addr(), cfg)
			if err == nil {
				_ = conn.Close()
			}
			require.Error(t, err)
		})
	}
}

// TestPlaintextToATLSPortFailsCleanly checks the failure a misconfigured client
// actually hits. It must be an error, and it must arrive quickly: a hang is the
// worst outcome, because it looks like the server rather than the client.
func TestPlaintextToATLSPortFailsCleanly(t *testing.T) {
	srv, _ := newTLSServer(t)

	conn, err := dialPlaintext(t, srv.Addr())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err = conn.Write([]byte("*1\r\n$4\r\nPING\r\n"))
	require.NoError(t, err)

	// The server reads the RESP frame as a TLS record, fails the handshake and
	// hangs up. What arrives here is an alert or an EOF — never a PONG, and
	// never a wait for the deadline.
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	require.Error(t, err, "a plaintext client must not be served")
	assert.NotContains(t, string(buf[:n]), "PONG")
}

// TestCertificateRotationLeavesConnectionsAlone is the rotation contract:
// GetCertificate is consulted per handshake, so a swap reaches new connections
// and cannot disturb established ones.
func TestCertificateRotationLeavesConnectionsAlone(t *testing.T) {
	srv, keeper := newTLSServer(t)
	certFile, keyFile := keeper.Files()

	established := dialTLS(t, srv, keeper)
	establishedReader := bufio.NewReader(established)
	send(t, established, "*1\r\n$4\r\nPING\r\n")
	require.Equal(t, "+PONG\r\n", readReply(t, established, establishedReader))
	require.Equal(t, "atlascache-v1", established.ConnectionState().PeerCertificates[0].Subject.CommonName)

	writePairAt(t, certFile, keyFile, "atlascache-v2", time.Now().Add(time.Hour))
	require.NoError(t, keeper.Reload())

	t.Run("a new connection gets the new certificate", func(t *testing.T) {
		fresh := dialTLS(t, srv, keeper)
		assert.Equal(t, "atlascache-v2", fresh.ConnectionState().PeerCertificates[0].Subject.CommonName)

		reader := bufio.NewReader(fresh)
		send(t, fresh, "*1\r\n$4\r\nPING\r\n")
		assert.Equal(t, "+PONG\r\n", readReply(t, fresh, reader))
	})

	t.Run("the established connection is undisturbed", func(t *testing.T) {
		// No listener restart, no dropped connection: the swap happened behind
		// a pointer the handshake reads, and this connection finished its
		// handshake before the swap.
		send(t, established, "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n")
		assert.Equal(t, "+OK\r\n", readReply(t, established, establishedReader))
		assert.Equal(t, "atlascache-v1", established.ConnectionState().PeerCertificates[0].Subject.CommonName)
	})

	t.Run("an invalid replacement leaves the server serving", func(t *testing.T) {
		// The trust anchor is captured before the file is corrupted: what the
		// server should still be presenting is the pair that is no longer on
		// disk, so reading the file to build it would defeat the test.
		lastGood, err := os.ReadFile(certFile)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(certFile, []byte("corrupted"), 0o600))
		require.Error(t, keeper.Reload())

		fresh, err := dialTLSWith(t, srv.Addr(), clientConfigFor(t, lastGood))
		require.NoError(t, err, "the server stopped accepting connections after a bad replacement")
		defer func() { _ = fresh.Close() }()

		assert.Equal(t, "atlascache-v2", fresh.ConnectionState().PeerCertificates[0].Subject.CommonName,
			"new connections keep getting the last good certificate")

		reader := bufio.NewReader(fresh)
		send(t, fresh, "*1\r\n$4\r\nPING\r\n")
		assert.Equal(t, "+PONG\r\n", readReply(t, fresh, reader))

		send(t, established, "*1\r\n$4\r\nPING\r\n")
		assert.Equal(t, "+PONG\r\n", readReply(t, established, establishedReader))
	})
}

// TestConnHidesTLS is a structural guard, not a behavioral one.
//
// FEAT-0023 requires that a handler cannot tell whether its connection is
// encrypted. The way that requirement gets broken is not by a handler asking —
// it is by someone adding a TLSState or IsTLS method to Conn because one
// handler needed it, at which point P8's gnet transport has to reproduce it.
// Pinning the method set makes that an explicit decision rather than a drive-by.
func TestConnHidesTLS(t *testing.T) {
	iface := reflect.TypeOf((*Conn)(nil)).Elem()

	methods := make([]string, iface.NumMethod())
	for i := range methods {
		methods[i] = iface.Method(i).Name
	}
	sort.Strings(methods)

	assert.Equal(t, []string{"Close", "Flush", "Reader", "RemoteAddr", "Writer"}, methods,
		"the connection a handler sees must expose nothing about its transport")
}

// TestTLSAndPlaintextAnswerIdentically is the behavioral half of the same
// requirement: the same command over the two transports produces the same
// bytes, because the handler is the same code and cannot branch on the
// difference.
func TestTLSAndPlaintextAnswerIdentically(t *testing.T) {
	plain, plainErr := newTestServer(t)
	defer shutdownServer(t, plain, plainErr)
	secure, keeper := newTLSServer(t)

	encrypted := dialTLS(t, secure, keeper)
	encryptedReader := bufio.NewReader(encrypted)
	plaintext, plaintextReader := dial(t, plain)

	for _, request := range []string{
		"*1\r\n$4\r\nPING\r\n",
		"*1\r\n$5\r\nHELLO\r\n",
		"*2\r\n$3\r\nGET\r\n$7\r\nabsent1\r\n",
		"*1\r\n$3\r\nFOO\r\n",
	} {
		send(t, encrypted, request)
		send(t, plaintext, request)
		assert.Equal(t,
			readReply(t, plaintext, plaintextReader),
			readReply(t, encrypted, encryptedReader),
			"a handler answered differently over TLS")
	}
}
