package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// anyInterface is the bind address that means "every interface", and the one
// an operator reaches for when loopback turns out not to be reachable from a
// container's probe. It is the case the guard exists for.
const anyInterface = "0.0.0.0"

// TestAdminExposureGuard is the P0 debt paid.
//
// FEAT-0010 could only log a warning here, because there was no admin.token to
// require; ADR-0023 always meant it to be a startup failure. The refusal is the
// point, but so is the shape of what is refused: the guard must not fire on a
// loopback bind, and must not be satisfied by the *client* token, which is the
// mistake the separate credential exists to prevent.
func TestAdminExposureGuard(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		refused bool
	}{
		{
			name:   "the default is loopback and needs no token",
			mutate: func(*Config) {},
		},
		{
			name:   "localhost by name is loopback",
			mutate: func(c *Config) { c.Admin.BindAddr = "localhost" },
		},
		{
			name:   "any loopback address will do",
			mutate: func(c *Config) { c.Admin.BindAddr = "127.0.0.2" },
		},
		{
			name:   "IPv6 loopback",
			mutate: func(c *Config) { c.Admin.BindAddr = "::1" },
		},
		{
			name:    "every interface, no token",
			mutate:  func(c *Config) { c.Admin.BindAddr = anyInterface },
			refused: true,
		},
		{
			name:    "a routable address, no token",
			mutate:  func(c *Config) { c.Admin.BindAddr = "10.4.2.7" },
			refused: true,
		},
		{
			name:    "IPv6 unspecified, no token",
			mutate:  func(c *Config) { c.Admin.BindAddr = "::" },
			refused: true,
		},
		{
			name:    "a hostname that is not localhost, no token",
			mutate:  func(c *Config) { c.Admin.BindAddr = "cache.internal" },
			refused: true,
		},
		{
			// The whole reason ADR-0023 asks for a second credential: holding
			// the data token must not confer administrative access, so it
			// cannot satisfy the guard either.
			name: "the client token does not satisfy the guard",
			mutate: func(c *Config) {
				c.Admin.BindAddr = anyInterface
				c.Auth.Enabled = true
				c.Auth.Token = "a-client-token"
			},
			refused: true,
		},
		{
			name: "exposed with an admin token is allowed",
			mutate: func(c *Config) {
				c.Admin.BindAddr = anyInterface
				c.Admin.Token = "an-admin-token"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Defaults()
			tt.mutate(cfg)

			err := Validate(cfg)
			if !tt.refused {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assertNamesTheCauseAndBothFixes(t, err.Error())
		})
	}
}

// assertNamesTheCauseAndBothFixes holds the message to the standard the ADR
// sets: a bare refusal of a value the operator typed on purpose reads as a bug
// in the server, and their next move is to look for a way around it rather than
// to pick one of the two ways out.
func assertNamesTheCauseAndBothFixes(t *testing.T, message string) {
	t.Helper()

	// The cause: which field, and why it is a problem.
	assert.Contains(t, message, fieldAdminBindAddr, "the message must name the field at fault")
	assert.Contains(t, message, "reachable from other hosts", "the message must say what is wrong")
	assert.Contains(t, message, "no credential", "the message must say why that is dangerous")

	// Both fixes, because either one resolves it and naming one of them hides
	// the other.
	assert.Contains(t, message, fieldAdminToken, "the message must offer the token")
	assert.Contains(t, message, "127.0.0.1", "the message must offer the loopback bind")

	// And where the rule comes from, so it can be looked up rather than argued
	// with.
	assert.Contains(t, message, "ADR-0023")
}

// TestAdminExposureGuardIsQuietOnAMalformedAddress keeps two errors about the
// same field from arriving together. "not an address" and "that address is
// exposed" would send an operator after the wrong one.
func TestAdminExposureGuardIsQuietOnAMalformedAddress(t *testing.T) {
	cfg := Defaults()
	cfg.Admin.BindAddr = "not a valid address"

	err := Validate(cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "valid IP address or hostname")
	assert.NotContains(t, err.Error(), "reachable from other hosts")
}

// TestAdminTokenMustDifferFromTheClientToken covers the other half of the
// separation. Identical values satisfy the schema and defeat the decision: every
// data client would hold a working admin credential.
func TestAdminTokenMustDifferFromTheClientToken(t *testing.T) {
	cfg := Defaults()
	cfg.Auth.Enabled = true
	cfg.Auth.Token = "one-token-for-everything"
	cfg.Admin.Token = "one-token-for-everything"

	err := Validate(cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), fieldAdminToken)
	assert.Contains(t, err.Error(), fieldAuthToken)
	assert.Contains(t, err.Error(), "administrative access")
	assert.NotContains(t, err.Error(), "one-token-for-everything",
		"an error about a token must not carry it: errors are logged")

	t.Run("different values are fine", func(t *testing.T) {
		cfg := Defaults()
		cfg.Auth.Enabled = true
		cfg.Auth.Token = "a-client-token"
		cfg.Admin.Token = "an-admin-token"
		assert.NoError(t, Validate(cfg))
	})

	t.Run("two unset tokens are not the same token", func(t *testing.T) {
		// Both empty means neither is configured, which is the default and is
		// not a sharing of secrets.
		assert.NoError(t, Validate(Defaults()))
	})
}

// TestLoadRefusesAnExposedAdminAPI runs the guard through the loader, which is
// the path the binary takes. A check that only ever runs against a hand-built
// struct would pass while a mapstructure tag typo left admin.token empty.
func TestLoadRefusesAnExposedAdminAPI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("admin:\n  bind_addr: \""+anyInterface+"\"\n"), 0o600))

	cfg, err := NewLoader().LoadFromFile(path)

	require.Error(t, err)
	assert.Nil(t, cfg)
	assertNamesTheCauseAndBothFixes(t, err.Error())
}

// TestLoadAcceptsAnExposedAdminAPIWithAToken is the same file with the token
// added, which is what proves the token actually reaches the guard from the
// configuration file rather than only from a struct literal.
func TestLoadAcceptsAnExposedAdminAPIWithAToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "admin:\n  bind_addr: \"" + anyInterface + "\"\n  token: \"from-the-file-a71c\"\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	cfg, err := NewLoader().LoadFromFile(path)

	require.NoError(t, err)
	assert.Equal(t, Secret("from-the-file-a71c"), cfg.Admin.Token)
	assert.Equal(t, RedactedValue, cfg.Admin.Token.String())
	assert.Equal(t, RedactedValue, cfg.Admin.Redacted().Token.String())
	assert.False(t, cfg.AdminIsLoopback())
}
