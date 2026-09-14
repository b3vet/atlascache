package main

import (
	"github.com/rs/zerolog"

	"github.com/b3vet/atlascache/internal/config"
	"github.com/b3vet/atlascache/internal/server"
)

// security is the two P2 protections as one wiring unit: the certificate the
// listener serves, and the token clients authenticate against.
//
// Both ship in v0.1.0 and both are off by default (ADR-0009), so the common
// path through this file builds a keeper of nil and an authenticator that
// requires nothing — and logs a warning for each, naming what is exposed.
type security struct {
	// certs is nil when tls.enabled is false. It is the only thing in the
	// process that holds the private key.
	certs *server.CertificateKeeper

	// auth is never nil: with auth.enabled false it is an authenticator that
	// requires no authentication, so the dispatch gate has no special case.
	auth *server.Authenticator
}

// newSecurity loads what the configuration asks for.
//
// A TLS configuration that cannot be honored is an error and not a warning:
// falling back to plaintext would leave the operator believing traffic was
// encrypted when it was not, and they would have no way to find out short of a
// packet capture. The error names the file at fault.
func newSecurity(cfg *config.Config, log zerolog.Logger) (*security, error) {
	// The conversion is deliberate and is the only way to read a
	// config.Secret: every use of a real secret value is visible at the call
	// site, and nothing can reach one by printing a struct.
	sec := &security{auth: server.NewAuthenticator(cfg.Auth.Enabled, string(cfg.Auth.Token))}

	if cfg.TLS.Enabled {
		keeper, err := server.NewCertificateKeeper(cfg.TLS.CertFile, cfg.TLS.KeyFile, log)
		if err != nil {
			return nil, err
		}
		sec.certs = keeper
	}

	return sec, nil
}

// options is what the server is built with. TLS reaches the listener as a
// tls.Config and goes no further: no handler is given any way to ask whether
// its connection is encrypted (ADR-0007).
func (s *security) options() []server.Option {
	opts := []server.Option{server.WithAuth(s.auth)}
	if s.certs != nil {
		opts = append(opts, server.WithTLS(s.certs.TLSConfig()))
	}
	return opts
}

// applyConfig takes what a config reload can change.
//
// The token can: a rotation changes what later AUTH calls are checked against
// and leaves authenticated connections alone, which is what keeps a rotation
// from disconnecting every client at once. Turning TLS or auth on or off cannot
// — one needs the listener rebuilt and the other would change the meaning of
// every connection already open — so those are reported and ignored rather than
// half-applied.
func (s *security) applyConfig(cfg *config.Config, log zerolog.Logger) {
	s.auth.Set(cfg.Auth.Enabled, string(cfg.Auth.Token))
	// Deliberately no token, no length, no prefix: the point of logging a
	// rotation is to record that one happened.
	log.Info().Bool("auth_enabled", cfg.Auth.Enabled).Msg("auth configuration applied")
}

// watchCertificates asks the config watcher to report changes to the
// certificate and key, and reloads on each.
//
// The reload validates before it swaps, so a certificate written to disk half
// way through a copy — or a renewal that produced a broken pair — leaves the
// running certificate in place with an error logged. Rotation is the routine
// operation here; an outage caused by rotation is the failure worth engineering
// against.
func (s *security) watchCertificates(watcher *config.Watcher, log zerolog.Logger) {
	if s.certs == nil || watcher == nil {
		return
	}

	certFile, keyFile := s.certs.Files()
	reload := func() {
		if err := s.certs.Reload(); err != nil {
			// The keeper has already logged this at error level with the file
			// named. What matters here is that nothing else happens: the
			// certificate in use is unchanged and the listener is untouched.
			log.Debug().Err(err).Msg("certificate reload rejected")
		}
	}

	for _, path := range []string{certFile, keyFile} {
		if err := watcher.WatchFile(path, reload); err != nil {
			log.Warn().Err(err).Str("path", path).
				Msg("certificate hot-reload unavailable; a renewal will need a restart")
		}
	}
}
