package main

import (
	"crypto/tls"
	"crypto/x509"
	"os"
)

// tlsConfig builds the TLS configuration, or nil for a plaintext connection.
//
// --tls-ca implies --tls. Reading it any other way would mean a caller who
// named a CA and forgot the flag connects in plaintext while believing
// otherwise, which is the failure mode worth designing against: nothing in the
// output would say so.
func (g *globals) tlsConfig() (*tls.Config, error) {
	if !g.useTLS && g.tlsCA == "" {
		return nil, nil
	}

	// The server floor is TLS 1.3; naming 1.2 here lets this client keep
	// working against a server that later chooses to accept 1.2, without ever
	// being the reason an older version is negotiated.
	config := &tls.Config{MinVersion: tls.VersionTLS12}

	if g.tlsCA == "" {
		// The host's trust store, which is what a server with a real
		// certificate wants and what a self-signed one will fail against —
		// with a message naming the certificate, which is the truth.
		return config, nil
	}

	pemBytes, err := os.ReadFile(g.tlsCA)
	if err != nil {
		return nil, usageErrorf("reading --tls-ca: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pemBytes) {
		return nil, usageErrorf("--tls-ca %s holds no PEM certificate", g.tlsCA)
	}
	config.RootCAs = roots
	return config, nil
}
