// Package certs generates the TLS material the E2E suite runs against.
//
// Certificates are generated per run into a temp directory, never committed.
// Committed test certificates get copied into production with depressing
// regularity, and they expire — at which point a suite that has been green for
// a year starts failing for a reason nobody will connect to the calendar
// ([[FEAT-0023]]).
//
// Like the rest of the suite, this shares no code with the server under test
// ([[ADR-0003]]): it is written against crypto/x509 directly, so a mistake in
// the server's certificate handling cannot be canceled out by the same mistake
// here.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// File names inside a certificate directory. They are fixed so that a scenario
// rotating a certificate and the harness serving it agree on where it lives.
const (
	CertFileName = "server.crt"
	KeyFileName  = "server.key"
)

// ServerName is the name the certificates are issued for and the name clients
// verify against. Servers under test bind loopback, and the certificate carries
// 127.0.0.1 as well, so either form of address works.
const ServerName = "localhost"

// Lifetime is how long a generated certificate is valid. Long enough that no
// run can outlive one, short enough that a leaked copy is worthless.
const Lifetime = 24 * time.Hour

// Pair is a generated certificate and its key, in PEM.
type Pair struct {
	CertPEM []byte
	KeyPEM  []byte
}

// Generate builds a self-signed certificate for localhost, valid from now.
//
// The common name is the caller's, and is what a rotation test tells one
// certificate from another by.
func Generate(commonName string) (Pair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Pair{}, fmt.Errorf("generating a key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return Pair{}, fmt.Errorf("generating a serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"AtlasCache E2E"}},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(Lifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{ServerName},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return Pair{}, fmt.Errorf("creating the certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return Pair{}, fmt.Errorf("encoding the key: %w", err)
	}

	return Pair{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// Write generates a pair and writes it into dir under the standard names,
// returning the two paths.
func Write(dir, commonName string) (certFile, keyFile string, err error) {
	if err = os.MkdirAll(dir, 0o750); err != nil {
		return "", "", fmt.Errorf("creating the certificate directory %s: %w", dir, err)
	}

	certFile = filepath.Join(dir, CertFileName)
	keyFile = filepath.Join(dir, KeyFileName)
	if err = WriteTo(certFile, keyFile, commonName); err != nil {
		return "", "", err
	}
	return certFile, keyFile, nil
}

// WriteTo generates a pair and writes it to exact paths, replacing whatever was
// there. This is what a rotation looks like to the server: the files it is
// watching change underneath it.
func WriteTo(certFile, keyFile, commonName string) error {
	pair, err := Generate(commonName)
	if err != nil {
		return err
	}
	return WritePair(certFile, keyFile, pair)
}

// WritePair writes an already-generated pair, for a caller that needs to keep
// the PEM — a client that must go on trusting a certificate after the file has
// been replaced, for instance.
func WritePair(certFile, keyFile string, pair Pair) error {
	if err := os.WriteFile(certFile, pair.CertPEM, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", certFile, err)
	}
	if err := os.WriteFile(keyFile, pair.KeyPEM, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", keyFile, err)
	}
	return nil
}

// WriteInvalid replaces the certificate with something that is not one,
// leaving the key alone.
//
// This is the failure a renewal actually produces: a file that was copied
// half way, or written by a tool that failed part way through. The server must
// reject it and go on serving the certificate it already has.
func WriteInvalid(certFile string) error {
	junk := []byte("-----BEGIN CERTIFICATE-----\nthis is not a certificate\n-----END CERTIFICATE-----\n")
	if err := os.WriteFile(certFile, junk, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", certFile, err)
	}
	return nil
}

// ClientConfig trusts exactly the certificates given, and nothing else.
//
// Verification is real rather than skipped: a client dialing with
// InsecureSkipVerify would connect happily to a server presenting any
// certificate at all, including the wrong one after a botched rotation, which
// is precisely what these tests exist to catch.
func ClientConfig(certPEMs ...[]byte) (*tls.Config, error) {
	pool := x509.NewCertPool()
	for _, pemBytes := range certPEMs {
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("the certificate is not usable as a trust anchor: %.40q", pemBytes)
		}
	}

	return &tls.Config{
		RootCAs:    pool,
		ServerName: ServerName,
		MinVersion: tls.VersionTLS13,
	}, nil
}

// ReadCertificate reads a PEM certificate from disk and checks it parses, so a
// caller that is about to trust it fails on the file rather than on the
// handshake.
func ReadCertificate(certFile string) ([]byte, error) {
	pemBytes, err := os.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("%s does not hold a PEM block", certFile)
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return nil, fmt.Errorf("%s: %w", certFile, err)
	}
	return pemBytes, nil
}

// CommonName reports the common name of the leaf certificate a peer presented,
// which is how a rotation test tells one certificate from the next.
func CommonName(state tls.ConnectionState) string {
	if len(state.PeerCertificates) == 0 {
		return ""
	}
	return state.PeerCertificates[0].Subject.CommonName
}
