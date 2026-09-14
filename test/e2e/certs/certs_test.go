package certs

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGenerate(t *testing.T) {
	pair, err := Generate("atlascache-test")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	block, _ := pem.Decode(pair.CertPEM)
	if block == nil {
		t.Fatal("the certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parsing the certificate: %v", err)
	}

	if cert.Subject.CommonName != "atlascache-test" {
		t.Errorf("common name = %q, want %q", cert.Subject.CommonName, "atlascache-test")
	}
	// The suite dials 127.0.0.1 and verifies the name "localhost", so the
	// certificate has to carry both or the verification is theater.
	if hostErr := cert.VerifyHostname(ServerName); hostErr != nil {
		t.Errorf("the certificate is not valid for %s: %v", ServerName, hostErr)
	}
	if !cert.IPAddresses[0].Equal(net.IPv4(127, 0, 0, 1)) {
		t.Errorf("IP SANs = %v, want 127.0.0.1 first", cert.IPAddresses)
	}
	if time.Until(cert.NotAfter) <= 0 {
		t.Error("the certificate is already expired")
	}

	// Two calls must not produce the same certificate: a rotation test tells
	// them apart by their serial and their name.
	other, err := Generate("atlascache-test")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if string(other.CertPEM) == string(pair.CertPEM) {
		t.Error("two generated certificates are identical")
	}

	// And the pair matches, which is what the server checks before swapping.
	if _, err := tls.X509KeyPair(pair.CertPEM, pair.KeyPEM); err != nil {
		t.Errorf("the generated pair does not load: %v", err)
	}
}

func TestWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "certs")

	certFile, keyFile, err := Write(dir, "written")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if filepath.Base(certFile) != CertFileName || filepath.Base(keyFile) != KeyFileName {
		t.Errorf("wrote %s and %s, want the standard names", certFile, keyFile)
	}
	if _, loadErr := tls.LoadX509KeyPair(certFile, keyFile); loadErr != nil {
		t.Errorf("the written pair does not load: %v", loadErr)
	}

	info, err := os.Stat(keyFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key permissions = %o, want 600: a private key is not a public file", perm)
	}

	t.Run("WriteTo replaces in place", func(t *testing.T) {
		if err := WriteTo(certFile, keyFile, "replaced"); err != nil {
			t.Fatalf("WriteTo: %v", err)
		}
		pemBytes, err := ReadCertificate(certFile)
		if err != nil {
			t.Fatalf("ReadCertificate: %v", err)
		}
		block, _ := pem.Decode(pemBytes)
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("parsing: %v", err)
		}
		if cert.Subject.CommonName != "replaced" {
			t.Errorf("common name = %q, want %q", cert.Subject.CommonName, "replaced")
		}
	})

	t.Run("WriteInvalid leaves something unusable", func(t *testing.T) {
		if err := WriteInvalid(certFile); err != nil {
			t.Fatalf("WriteInvalid: %v", err)
		}
		if _, err := ReadCertificate(certFile); err == nil {
			t.Error("the invalid certificate parsed; it is meant to be rejected")
		}
		if _, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
			t.Error("the invalid pair loaded; it is meant to be rejected")
		}
	})
}

func TestWriteReportsBadPaths(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir", "server.crt")

	if err := WriteTo(missing, missing, "x"); err == nil {
		t.Error("writing into a directory that does not exist should fail")
	}
	if err := WriteInvalid(missing); err == nil {
		t.Error("writing junk into a directory that does not exist should fail")
	}
	if _, err := ReadCertificate(missing); err == nil {
		t.Error("reading a certificate that does not exist should fail")
	}
}

func TestClientConfig(t *testing.T) {
	pair, err := Generate("trusted")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cfg, err := ClientConfig(pair.CertPEM)
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	if cfg.ServerName != ServerName {
		t.Errorf("ServerName = %q, want %q", cfg.ServerName, ServerName)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %#04x, want TLS 1.3", cfg.MinVersion)
	}
	if cfg.InsecureSkipVerify {
		t.Error("verification is disabled; a client that skips it would accept the wrong certificate after a botched rotation")
	}
	if cfg.RootCAs == nil {
		t.Fatal("no trust anchors")
	}

	t.Run("a second certificate can be trusted alongside the first", func(t *testing.T) {
		// What a client needs across a rotation: the old certificate and the
		// new one are both acceptable while connections are being made.
		second, err := Generate("also-trusted")
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if _, err := ClientConfig(pair.CertPEM, second.CertPEM); err != nil {
			t.Errorf("ClientConfig with two anchors: %v", err)
		}
	})

	t.Run("junk is refused rather than silently ignored", func(t *testing.T) {
		_, err := ClientConfig([]byte("not a certificate"))
		if err == nil {
			t.Fatal("junk was accepted as a trust anchor")
		}
		if !strings.Contains(err.Error(), "trust anchor") {
			t.Errorf("error = %q, want it to say what was wrong", err)
		}
	})
}

func TestCommonName(t *testing.T) {
	if name := CommonName(tls.ConnectionState{}); name != "" {
		t.Errorf("CommonName of a state with no peer = %q, want empty", name)
	}

	pair, err := Generate("named")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	block, _ := pem.Decode(pair.CertPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	if name := CommonName(state); name != "named" {
		t.Errorf("CommonName = %q, want %q", name, "named")
	}
}
