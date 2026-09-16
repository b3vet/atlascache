package main

import (
	"crypto/tls"

	"github.com/b3vet/atlascache/pkg/client"
)

// tlsOptions turns --tls and --tls-ca into the SDK options that carry them, or
// nothing at all for a plaintext connection.
//
// --tls-ca implies --tls. Reading it any other way would mean a caller who
// named a CA and forgot the flag connects in plaintext while believing
// otherwise, which is the failure mode worth designing against: nothing in the
// output would say so.
//
// Neither branch reads a file or builds a certificate pool. The SDK's
// WithTLSCAFile does both, and it does them in New — so an unreadable CA file
// is refused before a connection is attempted, with a message that names the
// path, and this program keeps no x509 code of its own (ISSUE-0023).
func (g *globals) tlsOptions() []client.Option {
	switch {
	case g.tlsCA != "":
		return []client.Option{client.WithTLSCAFile(g.tlsCA)}

	case g.useTLS:
		// The host's trust store, which is what a server with a real
		// certificate wants and what a self-signed one will fail against —
		// with a message naming the certificate, which is the truth.
		//
		// The server floor is TLS 1.3; naming 1.2 here lets this client keep
		// working against a server that later chooses to accept 1.2, without
		// ever being the reason an older version is negotiated.
		return []client.Option{client.WithTLS(&tls.Config{MinVersion: tls.VersionTLS12})}

	default:
		return nil
	}
}
