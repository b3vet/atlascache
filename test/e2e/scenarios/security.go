package scenarios

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/b3vet/atlascache/test/e2e/certs"
	"github.com/b3vet/atlascache/test/e2e/client"
	"github.com/b3vet/atlascache/test/e2e/runner"
)

func init() {
	runner.RegisterScenario("tls_connection_is_encrypted", tlsConnectionIsEncrypted)
	runner.RegisterScenario("tls_rejects_plaintext", tlsRejectsPlaintext)
	runner.RegisterScenario("tls_refuses_old_versions", tlsRefusesOldVersions)
	runner.RegisterScenario("tls_rotates_certificates", tlsRotatesCertificates)
	runner.RegisterScenario("tls_missing_certificate_fails_fast", tlsMissingCertificateFailsFast)
	runner.RegisterScenario("auth_gates_every_command", authGatesEveryCommand)
	runner.RegisterScenario("auth_token_never_logged", authTokenNeverLogged)
}

// How long a security scenario waits for something that should already have
// happened. Each is a bound on a hang, not a guess at a duration: every wait
// below polls for a positive signal and returns the moment it appears.
const (
	rotationBudget = 20 * time.Second
	logBudget      = 15 * time.Second
	pollInterval   = 50 * time.Millisecond

	// plaintextBudget is how long a plaintext client dialing a TLS port may
	// take to find out. A hang here is the worst outcome: it looks like the
	// server is wedged rather than like the client is talking the wrong
	// protocol.
	plaintextBudget = 5 * time.Second
)

// tlsHarness is the part of the harness a TLS scenario needs: where the
// certificate lives, and what to trust when dialing.
//
// It is a type assertion rather than an import, like serverBinary above, so the
// scenarios stay independent of any one harness implementation and say so
// clearly when handed one that cannot do this.
type tlsHarness interface {
	TLSEnabled() bool
	CertPaths() (certFile, keyFile string)
	ClientTLSConfig() (*tls.Config, error)
}

func tlsOf(c *runner.Ctx) (tlsHarness, error) {
	harness, ok := c.Harness.(tlsHarness)
	if !ok {
		return nil, fmt.Errorf(
			"this scenario needs a harness that owns the server's TLS material, and %T does not; run it against --harness process",
			c.Harness)
	}
	if !harness.TLSEnabled() {
		return nil, errors.New("this scenario needs a spec with tls.enabled: true")
	}
	return harness, nil
}

// dialSecure opens a verified TLS connection of the scenario's own, separate
// from the one the harness holds. Scenarios need their own because they assert
// on what the handshake produced, and because a rotation test has to keep one
// connection open across the rotation.
func dialSecure(c *runner.Ctx, harness tlsHarness) (*client.Client, error) {
	cfg, err := harness.ClientTLSConfig()
	if err != nil {
		return nil, err
	}

	conn, err := client.DialTLS(c.Context(), c.Info().ClientAddr, cfg)
	if err != nil {
		return nil, fmt.Errorf("dialing %s over TLS: %w", c.Info().ClientAddr, err)
	}
	return conn, nil
}

// pingOver checks that a connection serves, which is the whole point of
// encrypting it.
func pingOver(c *runner.Ctx, conn *client.Client) error {
	reply, err := conn.Call(c.Context(), "PING")
	if err != nil {
		return fmt.Errorf("PING over TLS: %w", err)
	}
	if reply.Kind == runner.KindError {
		return fmt.Errorf("PING answered %s", reply.Text)
	}
	if got := reply.String(); got != pong {
		return fmt.Errorf("PING answered %q, want PONG", got)
	}
	return nil
}

// presentedName reports the common name of the certificate a connection was
// given, which is how one certificate is told from the next.
func presentedName(conn *client.Client) (string, error) {
	state, ok := conn.ConnectionState()
	if !ok {
		return "", errors.New("the connection is not encrypted")
	}
	name := certs.CommonName(state)
	if name == "" {
		return "", errors.New("the server presented no certificate")
	}
	return name, nil
}

// tlsConnectionIsEncrypted is the positive control: a TLS 1.3 client connects,
// verifies the certificate properly, and runs commands.
func tlsConnectionIsEncrypted(c *runner.Ctx) error {
	harness, err := tlsOf(c)
	if err != nil {
		return err
	}

	conn, err := dialSecure(c, harness)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	state, ok := conn.ConnectionState()
	if !ok {
		return errors.New("the connection reports no TLS state, so it is not encrypted")
	}
	if state.Version != tls.VersionTLS13 {
		return fmt.Errorf("the connection negotiated TLS version %#04x, want TLS 1.3 (%#04x)",
			state.Version, tls.VersionTLS13)
	}
	c.Logf("negotiated TLS 1.3 with %s, cipher %#04x", certs.CommonName(state), state.CipherSuite)

	if pingErr := pingOver(c, conn); pingErr != nil {
		return pingErr
	}

	// And the data commands, because an encrypted connection that can only
	// answer PING is not a working server.
	if _, setErr := conn.Call(c.Context(), "SET", "tls:key", "encrypted"); setErr != nil {
		return fmt.Errorf("SET over TLS: %w", setErr)
	}
	reply, err := conn.Call(c.Context(), "GET", "tls:key")
	if err != nil {
		return fmt.Errorf("GET over TLS: %w", err)
	}
	if reply.String() != "encrypted" {
		return fmt.Errorf("GET over TLS answered %q, want %q", reply.String(), "encrypted")
	}
	return nil
}

// tlsRejectsPlaintext checks the failure a misconfigured client actually hits.
//
// The requirement is not merely that it fails — it is that it fails *quickly*.
// A plaintext client left hanging on a TLS port looks like a broken server, and
// that is the diagnosis the operator will pursue.
func tlsRejectsPlaintext(c *runner.Ctx) error {
	if _, err := tlsOf(c); err != nil {
		return err
	}

	conn, err := dialRaw(c)
	if err != nil {
		return err
	}
	defer conn.close()

	started := time.Now()
	if _, err = conn.write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		// A write that fails outright is a clean refusal too: the server has
		// already given up on the connection.
		c.Logf("the plaintext write failed after %s: %v", time.Since(started).Round(time.Millisecond), err)
		return nil
	}

	line, err := conn.readLine()
	elapsed := time.Since(started)

	switch {
	case err == nil:
		return fmt.Errorf("a plaintext client got %q from a TLS port; it must not be served", line)
	case isTimeout(err):
		return fmt.Errorf("a plaintext client hung for %s against a TLS port, rather than being refused", elapsed)
	case elapsed > plaintextBudget:
		return fmt.Errorf("a plaintext client took %s to be refused, over the %s budget", elapsed, plaintextBudget)
	}

	c.Logf("plaintext refused in %s: %v", elapsed.Round(time.Millisecond), err)
	return nil
}

// isTimeout reports whether an error is a read that never came back, as opposed
// to a connection that was closed. The difference is the whole assertion in
// tlsRejectsPlaintext.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// tlsRefusesOldVersions checks the floor. There is no configuration for it, so
// this is checking that the floor exists rather than that it was set correctly.
func tlsRefusesOldVersions(c *runner.Ctx) error {
	harness, err := tlsOf(c)
	if err != nil {
		return err
	}

	for _, version := range []struct {
		name string
		max  uint16
	}{
		{"TLS 1.2", tls.VersionTLS12},
		{"TLS 1.1", tls.VersionTLS11},
		{"TLS 1.0", tls.VersionTLS10},
	} {
		cfg, cfgErr := harness.ClientTLSConfig()
		if cfgErr != nil {
			return cfgErr
		}
		cfg.MinVersion = tls.VersionTLS10
		cfg.MaxVersion = version.max

		conn, dialErr := client.DialTLS(c.Context(), c.Info().ClientAddr, cfg)
		if dialErr == nil {
			_ = conn.Close()
			return fmt.Errorf("a %s client completed a handshake; the floor is TLS 1.3", version.name)
		}
		c.Logf("%s refused: %v", version.name, dialErr)
	}
	return nil
}

// tlsRotatesCertificates is the subtle one, and the reason FEAT-0023 exists in
// the shape it does.
//
// A certificate renewal is routine — every 90 days with Let's Encrypt, more
// often in some infrastructures — and there are three ways it can go wrong that
// all end in an outage: the server does not notice the new certificate, the
// swap drops connections that were already open, or the server swaps in a
// broken file and can no longer complete a handshake. This exercises all three.
func tlsRotatesCertificates(c *runner.Ctx) error {
	harness, err := tlsOf(c)
	if err != nil {
		return err
	}
	certFile, keyFile := harness.CertPaths()

	established, original, err := openBeforeRotation(c, harness)
	if err != nil {
		return err
	}
	defer func() { _ = established.Close() }()

	// 1. A valid rotation is picked up, without a restart.
	if err = rotateTo(c, harness, certFile, keyFile, "atlascache-rotated"); err != nil {
		return err
	}
	c.Logf("new connections are served the rotated certificate")

	// 2. The connection that predates the rotation is untouched. This is what
	// GetCertificate buys: the swap is read per handshake, and this handshake
	// is long finished.
	if err = stillTheSameConnection(c, established, original); err != nil {
		return fmt.Errorf("the rotation disturbed a connection that was already open: %w", err)
	}

	// 3. An invalid replacement is refused, and the server keeps serving what
	// it already had. This is the failure that turns a renewal into an outage.
	if err = certs.WriteInvalid(certFile); err != nil {
		return err
	}
	if err = waitForLog(c, "certificate reload rejected"); err != nil {
		return fmt.Errorf("the server never reported rejecting the broken certificate: %w", err)
	}
	c.Logf("the broken pair was rejected")

	if err = stillServing(c, harness, "atlascache-rotated"); err != nil {
		return err
	}
	if err = stillTheSameConnection(c, established, original); err != nil {
		return fmt.Errorf("a broken certificate on disk took an established connection down: %w", err)
	}

	// 4. And the renewal that follows a failed one is still picked up: a
	// rejected certificate must not leave the watcher wedged.
	if err = rotateTo(c, harness, certFile, keyFile, "atlascache-recovered"); err != nil {
		return fmt.Errorf("after a rejected certificate, a valid one was not picked up: %w", err)
	}

	return pingOver(c, established)
}

// openBeforeRotation opens the connection that has to survive everything that
// follows, and reports which certificate it was given.
func openBeforeRotation(c *runner.Ctx, harness tlsHarness) (*client.Client, string, error) {
	conn, err := dialSecure(c, harness)
	if err != nil {
		return nil, "", err
	}

	original, err := presentedName(conn)
	if err != nil {
		_ = conn.Close()
		return nil, "", err
	}
	if err := pingOver(c, conn); err != nil {
		_ = conn.Close()
		return nil, "", err
	}

	c.Logf("established a connection against certificate %q", original)
	return conn, original, nil
}

// rotateTo writes a fresh pair and waits for new connections to be served it.
func rotateTo(c *runner.Ctx, harness tlsHarness, certFile, keyFile, name string) error {
	pair, err := certs.Generate(name)
	if err != nil {
		return err
	}
	if err = certs.WritePair(certFile, keyFile, pair); err != nil {
		return err
	}
	return waitForPresentedName(c, harness, name)
}

// stillTheSameConnection checks that a connection opened before a rotation is
// serving, and is still on the certificate it handshook with.
func stillTheSameConnection(c *runner.Ctx, conn *client.Client, original string) error {
	if err := pingOver(c, conn); err != nil {
		return err
	}

	current, err := presentedName(conn)
	if err != nil {
		return err
	}
	if current != original {
		return fmt.Errorf("an established connection changed certificate from %q to %q mid-connection",
			original, current)
	}
	return nil
}

// waitForPresentedName polls new connections until the server presents the
// certificate named, or the budget runs out.
//
// Polling a real handshake, rather than a log line, is deliberate: the
// assertion is about what a client is served, and a log line saying a
// certificate was loaded would not prove a client ever saw it.
func waitForPresentedName(c *runner.Ctx, harness tlsHarness, want string) error {
	deadline := time.Now().Add(rotationBudget)
	var last string

	for {
		conn, err := dialSecure(c, harness)
		if err == nil {
			last, err = presentedName(conn)
			if err == nil && last == want {
				pingErr := pingOver(c, conn)
				_ = conn.Close()
				return pingErr
			}
			_ = conn.Close()
		}
		if err != nil {
			last = err.Error()
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("new connections were still not served certificate %q after %s (last: %s)",
				want, rotationBudget, last)
		}
		if err := c.Sleep(pollInterval); err != nil {
			return err
		}
	}
}

// stillServing checks, repeatedly over a short window, that new connections are
// served the certificate named. Repeating matters: a swap that happens a moment
// after the check would otherwise pass.
func stillServing(c *runner.Ctx, harness tlsHarness, want string) error {
	const attempts = 5

	for i := 0; i < attempts; i++ {
		conn, err := dialSecure(c, harness)
		if err != nil {
			return fmt.Errorf("the server stopped accepting connections after a broken certificate was written: %w", err)
		}

		name, err := presentedName(conn)
		if err == nil {
			err = pingOver(c, conn)
		}
		_ = conn.Close()
		if err != nil {
			return err
		}
		if name != want {
			return fmt.Errorf("the server swapped in a broken certificate: it now presents %q, want %q", name, want)
		}

		if err := c.Sleep(100 * time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

// waitForLog blocks until the server's log contains want.
func waitForLog(c *runner.Ctx, want string) error {
	deadline := time.Now().Add(logBudget)
	for {
		if strings.Contains(c.Harness.Logs(), want) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the server did not log %q within %s", want, logBudget)
		}
		if err := c.Sleep(pollInterval); err != nil {
			return err
		}
	}
}

// tlsMissingCertificateFailsFast checks the startup refusal.
//
// Falling back to plaintext here would be the worst behavior available: the
// operator believes traffic is encrypted, nothing in the log contradicts them,
// and the only way to find out is a packet capture.
func tlsMissingCertificateFailsFast(c *runner.Ctx) error {
	binary, err := binaryOf(c)
	if err != nil {
		return err
	}

	missing := filepath.Join(binary.Root(), "no-such-certificate.pem")
	path := filepath.Join(binary.Root(), "tls-missing-cert.yaml")
	content := fmt.Sprintf("tls:\n  enabled: true\n  cert_file: %q\n  key_file: %q\n", missing, missing)
	if err = os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	started := time.Now()
	code, output, err := binary.RunBinary(c.Context(), "--config", path)
	elapsed := time.Since(started)
	if err != nil {
		return fmt.Errorf("running the server with a missing certificate: %w", err)
	}
	c.Logf("missing certificate: exit %d in %s", code, elapsed.Round(time.Millisecond))

	if code == 0 {
		return errors.New("the server started with tls.enabled and no certificate; it must never fall back to plaintext")
	}
	if elapsed > startupBudget {
		return fmt.Errorf("the server took %s to refuse a missing certificate, over the %s budget", elapsed, startupBudget)
	}
	if !strings.Contains(output, missing) {
		return fmt.Errorf("the message does not name the missing file, so it does not say what to fix: %s",
			strings.TrimSpace(output))
	}

	// An enabled TLS with no paths at all must be refused by validation, and
	// must name the fields rather than the files.
	blank := filepath.Join(binary.Root(), "tls-no-paths.yaml")
	if err = os.WriteFile(blank, []byte("tls:\n  enabled: true\n"), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", blank, err)
	}
	code, output, err = binary.RunBinary(c.Context(), "--config", blank)
	if err != nil {
		return fmt.Errorf("running the server with no certificate paths: %w", err)
	}
	if code == 0 {
		return errors.New("the server started with tls.enabled and no cert_file or key_file")
	}
	for _, want := range []string{"tls.cert_file", "tls.key_file"} {
		if !strings.Contains(output, want) {
			return fmt.Errorf("the message does not mention %q: %s", want, strings.TrimSpace(output))
		}
	}
	return nil
}

// gatedCommands is every command this server answers that is not on the
// pre-auth allowlist, as of P2.
//
// The authoritative check is the unit test in internal/server, which walks the
// dispatch table itself and therefore cannot fall behind it. This list is the
// same assertion made through a real socket against a real process, where the
// gate could also be defeated by something the unit test cannot see — a
// connection that arrives pre-authenticated, say.
var gatedCommands = []string{
	"GET k", "SET k v", "SETNX k v", "DEL k", "EXISTS k", "KEYS *", "SCAN 0",
	"TTL k", "EXPIRE k 10", "STATS", "INFO", "DBSIZE", "ECHO hi", "COMMAND",
}

// allowedBeforeAuth is the allowlist FEAT-0022 names.
var allowedBeforeAuth = []string{"PING", "HELLO", "AUTH wrong-token", "HELLO 2"}

// authGatesEveryCommand drives the gate from outside the process.
func authGatesEveryCommand(c *runner.Ctx) error {
	const noAuth = "NOAUTH Authentication required"

	for _, cmd := range gatedCommands {
		reply, err := c.Send(cmd)
		if err != nil {
			return fmt.Errorf("%s: %w", cmd, err)
		}
		if reply.Kind != runner.KindError {
			return fmt.Errorf("%s answered %s %q on an unauthenticated connection; it must be gated",
				cmd, reply.Kind, reply.String())
		}
		if reply.Text != noAuth {
			return fmt.Errorf("%s answered %q, want %q", cmd, reply.Text, noAuth)
		}
	}
	c.Logf("%d commands are gated before authentication", len(gatedCommands))

	// An unknown command is refused the same way, so the command table is not
	// readable by an unauthenticated client.
	reply, err := c.Send("NOSUCHCOMMAND")
	if err != nil {
		return err
	}
	if reply.Kind != runner.KindError || reply.Text != noAuth {
		return fmt.Errorf("an unknown command answered %q; an unauthenticated client must not learn the command table",
			reply.String())
	}

	for _, cmd := range allowedBeforeAuth {
		reply, err := c.Send(cmd)
		if err != nil {
			return fmt.Errorf("%s: %w", cmd, err)
		}
		if reply.Kind == runner.KindError && reply.Text == noAuth {
			return fmt.Errorf("%s was refused with NOAUTH; it is on the pre-auth allowlist", cmd)
		}
	}
	c.Logf("the pre-auth allowlist still answers")
	return nil
}

// authTokenNeverLogged checks that the secret is not in the artifact everyone
// copies into a ticket.
func authTokenNeverLogged(c *runner.Ctx) error {
	token, err := configuredToken(c)
	if err != nil {
		return err
	}

	// Produce every path that touches a token: a failure, a near-miss, a
	// success, and the HELLO form.
	for _, cmd := range []string{
		"AUTH " + token + "-wrong",
		"AUTH nobody " + token,
		"AUTH " + token,
		"HELLO 2 AUTH default " + token,
	} {
		if _, err := c.Send(cmd); err != nil {
			return fmt.Errorf("%s: %w", cmd, err)
		}
	}

	logs := c.Harness.Logs()
	if strings.Contains(logs, token) {
		return errors.New("the configured token appears in the server log; a log is the most-copied artifact in an outage")
	}
	if !strings.Contains(logs, "authentication failed") {
		return errors.New("a failed authentication was not logged at all; the outcome and the source address must be")
	}
	c.Logf("the token is absent from %d bytes of log, and failures are still recorded", len(logs))
	return nil
}

// configuredToken reads auth.token out of the config the harness wrote, so the
// scenario and the server cannot disagree about what the secret is.
func configuredToken(c *runner.Ctx) (string, error) {
	binary, err := binaryOf(c)
	if err != nil {
		return "", err
	}

	path := filepath.Join(binary.Root(), "config.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading the server's config at %s: %w", path, err)
	}

	var config struct {
		Auth struct {
			Token string `yaml:"token"`
		} `yaml:"auth"`
	}
	if err := yaml.Unmarshal(raw, &config); err != nil {
		return "", fmt.Errorf("parsing the server's config: %w", err)
	}
	if config.Auth.Token == "" {
		return "", fmt.Errorf("the spec's config sets no auth.token: %s", raw)
	}
	return config.Auth.Token, nil
}
