package scenarios

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/b3vet/atlascache/pkg/client"
	"github.com/b3vet/atlascache/test/e2e/certs"
	"github.com/b3vet/atlascache/test/e2e/runner"
)

// How a client is configured, and what it is told when the configuration is
// wrong — the three parts of it that ISSUE-0023 found by building atlasctl.
//
// None of these is a bug in what the SDK does. They are all about what it tells
// the caller, which is why they are worth a spec: an error that sends its reader
// to the wrong place costs more than the failure it describes.

func init() {
	runner.RegisterScenario("sdk_config_errors_are_classified", sdkConfigErrorsAreClassified)
	runner.RegisterScenario("sdk_tls_ca_file_verifies_the_server", sdkTLSCAFileVerifiesTheServer)
	runner.RegisterScenario("sdk_auth_against_a_passwordless_server", sdkAuthAgainstAPasswordlessServer)
}

// sdkConfigErrorsAreClassified checks that an option the SDK refuses is a
// category a caller can test for.
//
// Without it the only way to classify one is to know that New's single possible
// failure is a bad option — a fact about the implementation rather than an API,
// and the assumption atlasctl was built on before ErrConfig existed.
func sdkConfigErrorsAreClassified(c *runner.Ctx) error {
	binary, err := binaryOf(c)
	if err != nil {
		return err
	}

	bad := map[string]client.Option{
		"an empty address":         client.WithAddr(""),
		"a pool size of zero":      client.WithPoolSize(0),
		"a negative timeout":       client.WithReadTimeout(-1),
		"a CA file that is absent": client.WithTLSCAFile(filepath.Join(binary.Root(), "no-such-ca.crt")),
	}

	for name, option := range bad {
		sdk, err := client.New(client.WithAddr(c.Info().ClientAddr), option)
		if err == nil {
			_ = sdk.Close()
			return fmt.Errorf("client.New accepted %s", name)
		}
		if !errors.Is(err, client.ErrConfig) {
			return fmt.Errorf("client.New refused %s with %v, which is not an ErrConfig", name, err)
		}
		if client.Retryable(err) {
			return fmt.Errorf("client.New refused %s with an error that reports as retryable: %v", name, err)
		}
		// Exclusive, like every other category: an error that matched several
		// would make errors.Is useless to the caller it exists for.
		for _, other := range []error{client.ErrNetwork, client.ErrTimeout, client.ErrProtocol,
			client.ErrServer, client.ErrAuth, client.ErrClosed} {
			if errors.Is(err, other) {
				return fmt.Errorf("client.New refused %s with an error that is also %v", name, other)
			}
		}
	}
	c.Logf("every refused option is an ErrConfig and nothing else")
	return nil
}

// sdkTLSCAFileVerifiesTheServer checks the option that replaced a dozen lines
// of x509 in every caller.
//
// The negative case is what makes it worth running: an option that "loads a CA"
// by trusting everything would pass the positive test and protect nobody.
func sdkTLSCAFileVerifiesTheServer(c *runner.Ctx) error {
	harness, err := tlsOf(c)
	if err != nil {
		return err
	}
	certFile, _ := harness.CertPaths()

	sdk, err := client.New(client.WithAddr(c.Info().ClientAddr), client.WithTLSCAFile(certFile))
	if err != nil {
		return fmt.Errorf("building a client from a CA file: %w", err)
	}
	defer func() { _ = sdk.Close() }()

	if setErr := sdk.Set(c.Context(), "sdk:ca", []byte("v"), 0); setErr != nil {
		return fmt.Errorf("SET over a connection verified by a CA file: %w", setErr)
	}
	c.Logf("a CA file alone is enough to verify the server")

	// A file that parses as PEM and names a different authority. The handshake
	// must fail: this is the assertion that says the CA was used rather than
	// merely read.
	otherCA, err := otherAuthority(c)
	if err != nil {
		return err
	}
	untrusting, err := client.New(
		client.WithAddr(c.Info().ClientAddr),
		client.WithTLSCAFile(otherCA),
		client.WithReconnectWindow(0),
	)
	if err != nil {
		return fmt.Errorf("building a client from an unrelated CA file: %w", err)
	}
	defer func() { _ = untrusting.Close() }()

	if pingErr := untrusting.Ping(c.Context()); pingErr == nil {
		return errors.New("a client given an unrelated certificate authority completed the handshake")
	}

	// And the two ways to get the file wrong are refused by New, before
	// anything is dialed: a process that cannot reach its cache should fail to
	// start rather than start and fail every request.
	binary, err := binaryOf(c)
	if err != nil {
		return err
	}
	notPEM := filepath.Join(binary.Root(), "not-a-certificate.pem")
	if err := os.WriteFile(notPEM, []byte("nothing here is a certificate\n"), 0o600); err != nil {
		return fmt.Errorf("writing a file that is not PEM: %w", err)
	}
	if _, err := client.New(client.WithTLSCAFile(notPEM)); !errors.Is(err, client.ErrConfig) {
		return fmt.Errorf("a CA file holding no certificate gave %v, want an ErrConfig", err)
	}
	c.Logf("a CA file that is missing or empty is refused by New rather than by the first call")
	return nil
}

// otherAuthority writes out a certificate authority that signed nothing here,
// for the negative half of the check above.
func otherAuthority(c *runner.Ctx) (string, error) {
	binary, err := binaryOf(c)
	if err != nil {
		return "", err
	}
	certFile, _, err := certs.Write(filepath.Join(binary.Root(), "other-ca"), "atlascache-unrelated-authority")
	if err != nil {
		return "", fmt.Errorf("generating an unrelated authority: %w", err)
	}
	return certFile, nil
}

// sdkAuthAgainstAPasswordlessServer checks the one authentication failure the
// caller's credential did not cause.
//
// A server with authentication disabled refuses every AUTH, because it has no
// password to check. Reporting that as "authentication failed" sends an
// operator to rotate a token that is fine, when what is wrong is which server
// they are pointed at — or that the server was started without its config
// (ISSUE-0023).
//
// It runs against the real server on purpose. The SDK recognizes the case by
// the start of what the server says, so this is what notices if that wording
// moves out from under the match.
func sdkAuthAgainstAPasswordlessServer(c *runner.Ctx) error {
	token, err := specToken(c)
	if err != nil {
		return err
	}
	if token != "" {
		return errors.New("this scenario needs a spec with authentication disabled")
	}

	sdk, err := sdkClient(c, client.WithAuth("a-token-this-server-never-asked-for"))
	if err != nil {
		return err
	}
	defer func() { _ = sdk.Close() }()

	err = sdk.Ping(c.Context())
	if !errors.Is(err, client.ErrAuth) {
		return fmt.Errorf("authenticating against a passwordless server answered %v, want an ErrAuth", err)
	}
	if !errors.Is(err, client.ErrAuthDisabled) {
		return fmt.Errorf("authenticating against a passwordless server answered %v, which does not identify "+
			"a server with authentication disabled; the server's wording may have moved out from under the match", err)
	}
	if client.Retryable(err) {
		return fmt.Errorf("a server with authentication disabled reports as retryable: %v", err)
	}
	if !strings.Contains(err.Error(), "disabled") {
		return fmt.Errorf("the failure reads %q and does not point at the server's configuration", err)
	}
	c.Logf("a token against a passwordless server names the server's configuration, not the token")

	// A client with no token at all is served normally, which is what makes the
	// diagnosis above actionable: dropping the token is the fix.
	plain, err := sdkClient(c)
	if err != nil {
		return err
	}
	defer func() { _ = plain.Close() }()
	if err := plain.Ping(c.Context()); err != nil {
		return fmt.Errorf("a client with no token was refused by a passwordless server: %w", err)
	}
	return nil
}
