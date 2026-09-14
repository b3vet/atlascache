package server

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// MinTLSVersion is the floor for every client connection, and it is not
// configurable (FEAT-0023).
//
// Offering TLS 1.2 as an option invites a deployment that quietly negotiates
// it: nothing fails, nothing warns, and the operator believes they are on 1.3.
// A client that cannot speak 1.3 is a client worth finding out about, and every
// current Redis client library can.
const MinTLSVersion = uint16(tls.VersionTLS13)

// handshakeTimeout bounds how long a connection may take to complete its TLS
// handshake. Without it a peer that connects and then says nothing holds a
// goroutine and a file descriptor indefinitely — including the ordinary case of
// a plaintext client that has dialed a TLS port and is waiting for a banner
// that will never come.
const handshakeTimeout = 10 * time.Second

// certificateExpiryWarning is how far ahead of expiry the keeper starts saying
// so on every load. A week is enough notice to act on and short enough that the
// warning still means something when it appears.
const certificateExpiryWarning = 7 * 24 * time.Hour

// CertificateKeeper holds the certificate the listener serves, and swaps it for
// a new one when the files on disk change.
//
// The swap is validated before it happens: a replacement is parsed and checked
// in full, and only a usable pair ever reaches the live pointer. A certificate
// that is half-written, mismatched with its key, or plain corrupt therefore
// leaves the running one in place with an error logged. That is the whole point
// of the type — the failure that turns a routine 90-day renewal into an outage
// is a server that swapped in a broken certificate and can no longer accept a
// connection.
//
// The live certificate is read through tls.Config.GetCertificate, which is
// consulted per handshake, so a swap needs no listener restart and does not
// disturb connections that are already established.
type CertificateKeeper struct {
	certFile string
	keyFile  string
	log      zerolog.Logger

	current  atomic.Pointer[tls.Certificate]
	reloads  atomic.Uint64
	failures atomic.Uint64
}

// NewCertificateKeeper loads the pair and returns a keeper serving it.
//
// A pair that cannot be loaded is an error, never a fallback: a server that
// quietly served plaintext because its certificate was missing would leave the
// operator believing traffic was encrypted when it was not, which is the worst
// outcome available here. The error names the file at fault so the fix is
// obvious from the exit message alone.
func NewCertificateKeeper(certFile, keyFile string, log zerolog.Logger) (*CertificateKeeper, error) {
	if certFile == "" || keyFile == "" {
		return nil, errors.New("tls: both tls.cert_file and tls.key_file are required when tls.enabled is true")
	}

	keeper := &CertificateKeeper{certFile: certFile, keyFile: keyFile, log: log}

	cert, err := keeper.read()
	if err != nil {
		return nil, err
	}
	keeper.current.Store(cert)
	keeper.report("certificate loaded", cert)

	return keeper, nil
}

// Reload re-reads the pair and swaps it in, if and only if it is usable.
//
// A rejected reload is reported and otherwise has no effect: the previous
// certificate stays live, established connections are untouched, and new
// handshakes keep succeeding against the old certificate. The error is returned
// as well as logged, for callers that want to fail a test on it.
func (k *CertificateKeeper) Reload() error {
	cert, err := k.read()
	if err != nil {
		k.failures.Add(1)
		k.log.Error().Err(err).
			Str("cert_file", k.certFile).
			Str("key_file", k.keyFile).
			Msg("certificate reload rejected; still serving the certificate already loaded")
		return err
	}

	k.current.Store(cert)
	k.reloads.Add(1)
	k.report("certificate reloaded", cert)
	return nil
}

// Stats reports how many reloads succeeded and how many were rejected, which is
// the only view of certificate rotation from outside the process.
func (k *CertificateKeeper) Stats() (reloads, failures uint64) {
	return k.reloads.Load(), k.failures.Load()
}

// Files returns the certificate and key paths the keeper watches.
func (k *CertificateKeeper) Files() (certFile, keyFile string) {
	return k.certFile, k.keyFile
}

// TLSConfig is the configuration the listener is wrapped with. The certificate
// is fetched per handshake rather than fixed at construction, which is what
// makes a rotation invisible to the listener.
func (k *CertificateKeeper) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     MinTLSVersion,
		GetCertificate: k.getCertificate,
	}
}

// getCertificate is the tls.Config hook, consulted once per handshake.
func (k *CertificateKeeper) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert := k.current.Load()
	if cert == nil {
		// Unreachable while the keeper is built by NewCertificateKeeper, which
		// refuses to return one without a certificate. Answering an error is
		// still better than a nil dereference inside the handshake.
		return nil, errors.New("tls: no certificate is loaded")
	}
	return cert, nil
}

// read loads and validates the pair without touching the live certificate.
//
// Everything that could fail happens here, before the swap: reading each file,
// pairing the key with the certificate, and parsing the leaf. A caller that
// gets an error from this has a keeper in exactly the state it was in before.
func (k *CertificateKeeper) read() (*tls.Certificate, error) {
	// The paths come from the operator's own config file, which is as trusted
	// as the binary it configures.
	certPEM, err := os.ReadFile(k.certFile)
	if err != nil {
		return nil, fmt.Errorf("tls.cert_file %s: %w", k.certFile, err)
	}
	keyPEM, err := os.ReadFile(k.keyFile)
	if err != nil {
		return nil, fmt.Errorf("tls.key_file %s: %w", k.keyFile, err)
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("tls: %s and %s are not a usable certificate and key pair: %w",
			k.certFile, k.keyFile, err)
	}

	// X509KeyPair has already parsed the leaf, but saying so explicitly keeps
	// the guarantee local: everything the handshake needs is resolved here,
	// where a failure is still recoverable, rather than per connection.
	leaf := cert.Leaf
	if leaf == nil {
		leaf, err = x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return nil, fmt.Errorf("tls.cert_file %s: %w", k.certFile, err)
		}
		cert.Leaf = leaf
	}

	return &cert, nil
}

// report logs what was loaded, and says so loudly when it is expired or close
// to it. An expired certificate is not refused: refusing would take the server
// down over a clock problem, which is worse than serving a certificate clients
// will complain about.
func (k *CertificateKeeper) report(msg string, cert *tls.Certificate) {
	event := k.log.Info().
		Str("cert_file", k.certFile).
		Str("key_file", k.keyFile)

	if cert.Leaf != nil {
		event = event.
			Str("subject", cert.Leaf.Subject.String()).
			Strs("dns_names", cert.Leaf.DNSNames).
			Time("not_after", cert.Leaf.NotAfter)
	}
	event.Msg(msg)

	if cert.Leaf == nil {
		return
	}
	switch remaining := time.Until(cert.Leaf.NotAfter); {
	case remaining <= 0:
		k.log.Warn().
			Str("cert_file", k.certFile).
			Time("not_after", cert.Leaf.NotAfter).
			Msg("TLS certificate has expired — clients will refuse to connect until it is renewed")
	case remaining <= certificateExpiryWarning:
		k.log.Warn().
			Str("cert_file", k.certFile).
			Time("not_after", cert.Leaf.NotAfter).
			Dur("expires_in", remaining).
			Msg("TLS certificate expires soon")
	}
}
