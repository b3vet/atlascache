package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validConfig is a configuration that passes validation, for tests that change
// one thing about it and expect that one thing to be reported.
func validConfig() *Config {
	return Defaults()
}

func TestSecurityDefaultsAreOff(t *testing.T) {
	// ADR-0009: both features ship in v0.1.0 and both are disabled, so the
	// first run needs no certificates and no token. The cost of that default is
	// paid by the startup warnings, not by the default itself.
	cfg := Defaults()

	assert.False(t, cfg.TLS.Enabled)
	assert.Empty(t, cfg.TLS.CertFile)
	assert.Empty(t, cfg.TLS.KeyFile)
	assert.False(t, cfg.Auth.Enabled)
	assert.Empty(t, cfg.Auth.Token)

	require.NoError(t, Validate(cfg), "the default configuration must be valid")
}

func TestValidateTLS(t *testing.T) {
	t.Run("paths may be set while TLS is off", func(t *testing.T) {
		// So that turning TLS on is one flag rather than three.
		cfg := validConfig()
		cfg.TLS = TLSConfig{Enabled: false, CertFile: "/etc/atlascache/server.crt"}
		assert.NoError(t, Validate(cfg))
	})

	t.Run("enabled with no certificate names both files", func(t *testing.T) {
		cfg := validConfig()
		cfg.TLS.Enabled = true

		err := Validate(cfg)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "tls.cert_file")
		assert.Contains(t, err.Error(), "tls.key_file")
	})

	t.Run("enabled with only a certificate names the key", func(t *testing.T) {
		cfg := validConfig()
		cfg.TLS = TLSConfig{Enabled: true, CertFile: "server.crt"}

		err := Validate(cfg)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "tls.key_file")
		assert.NotContains(t, err.Error(), "tls.cert_file")
	})

	t.Run("enabled with both is valid here", func(t *testing.T) {
		// Whether the files exist and hold a usable pair is settled at startup,
		// by loading them — validation does no I/O, because hot-reload runs it
		// on every config change.
		cfg := validConfig()
		cfg.TLS = TLSConfig{Enabled: true, CertFile: "server.crt", KeyFile: "server.key"}
		assert.NoError(t, Validate(cfg))
	})

	t.Run("whitespace is not a path", func(t *testing.T) {
		cfg := validConfig()
		cfg.TLS = TLSConfig{Enabled: true, CertFile: "   ", KeyFile: "\t"}

		err := Validate(cfg)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "tls.cert_file")
		assert.Contains(t, err.Error(), "tls.key_file")
	})
}

func TestValidateAuth(t *testing.T) {
	t.Run("a token may sit in the file with auth off", func(t *testing.T) {
		cfg := validConfig()
		cfg.Auth = AuthConfig{Enabled: false, Token: "unused"}
		assert.NoError(t, Validate(cfg))
	})

	t.Run("enabled with no token is refused", func(t *testing.T) {
		// An enabled auth with an empty token accepts `AUTH ""` from anyone,
		// which is no protection wearing the costume of some.
		cfg := validConfig()
		cfg.Auth.Enabled = true

		err := Validate(cfg)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "auth.token")
	})

	t.Run("enabled with a token is valid", func(t *testing.T) {
		cfg := validConfig()
		cfg.Auth = AuthConfig{Enabled: true, Token: "s3cret"}
		assert.NoError(t, Validate(cfg))
	})
}

// TestRedactedAuth guards the type that holds the secret against being printed
// whole by a debug dump.
func TestRedactedAuth(t *testing.T) {
	redacted := AuthConfig{Enabled: true, Token: "s3cret"}.Redacted()

	assert.True(t, redacted.Enabled)
	assert.NotContains(t, redacted.Token, "s3cret")
	assert.Empty(t, AuthConfig{}.Redacted().Token, "nothing to redact when there is no token")
}

// TestTTLBothDisabledIsRefused covers ISSUE-0017.
//
// Each flag is individually legitimate and individually tested. Only the
// combination is broken, and it is broken in the way that is hardest to
// diagnose: nothing fails, memory simply grows, and it looks like a leak in
// AtlasCache rather than a configuration mistake. Validation checked fields
// independently before this, which is exactly why the combination got through.
func TestTTLBothDisabledIsRefused(t *testing.T) {
	cfg := validConfig()
	cfg.TTL.ActiveExpiration = false
	cfg.TTL.LazyExpiration = false

	err := Validate(cfg)

	require.Error(t, err)
	// Both fields, because naming one of them does not say what to change.
	assert.Contains(t, err.Error(), "ttl.active_expiration")
	assert.Contains(t, err.Error(), "ttl.lazy_expiration")

	t.Run("each one alone is still allowed", func(t *testing.T) {
		activeOnly := validConfig()
		activeOnly.TTL.LazyExpiration = false
		assert.NoError(t, Validate(activeOnly), "active expiration alone still reclaims")

		lazyOnly := validConfig()
		lazyOnly.TTL.ActiveExpiration = false
		assert.NoError(t, Validate(lazyOnly), "lazy expiration alone still reclaims")
	})
}

// TestLoadSecuritySections checks the sections survive the file, which is the
// part a struct-only test would miss: a mapstructure tag typo would leave every
// value at its default and every unit test above still green.
func TestLoadSecuritySections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "" +
		"tls:\n" +
		"  enabled: true\n" +
		"  cert_file: \"/etc/atlascache/server.crt\"\n" +
		"  key_file: \"/etc/atlascache/server.key\"\n" +
		"auth:\n" +
		"  enabled: true\n" +
		"  token: \"from-the-file\"\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	cfg, err := NewLoader().LoadFromFile(path)

	require.NoError(t, err)
	assert.True(t, cfg.TLS.Enabled)
	assert.Equal(t, "/etc/atlascache/server.crt", cfg.TLS.CertFile)
	assert.Equal(t, "/etc/atlascache/server.key", cfg.TLS.KeyFile)
	assert.True(t, cfg.Auth.Enabled)
	assert.Equal(t, Secret("from-the-file"), cfg.Auth.Token)
	// Reading the real value took a conversion. Every other way of getting at
	// it — printing it, encoding it, logging it — yields the redaction.
	assert.Equal(t, RedactedValue, cfg.Auth.Token.String())
}

func TestLoadRefusesTTLBothDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "ttl:\n  active_expiration: false\n  lazy_expiration: false\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	cfg, err := NewLoader().LoadFromFile(path)

	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "ttl.active_expiration")
	assert.Contains(t, err.Error(), "ttl.lazy_expiration")
}

// TestWatchFile covers the watch the certificate reload hangs off.
func TestWatchFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("eviction:\n  policy: lru\n"), 0o600))

	// The watched file lives in its own directory, as certificates do: the
	// watch is on the directory, so a file replaced by rename is still seen.
	watched := filepath.Join(dir, "certs", "server.crt")
	require.NoError(t, os.MkdirAll(filepath.Dir(watched), 0o750))
	require.NoError(t, os.WriteFile(watched, []byte("v1"), 0o600))

	watcher, err := NewWatcher(NewLoader(), configPath)
	require.NoError(t, err)
	defer func() { assert.NoError(t, watcher.Stop()) }()

	var fired atomic.Int64
	require.NoError(t, watcher.WatchFile(watched, func() { fired.Add(1) }))
	require.NoError(t, watcher.Start())

	t.Run("a write is reported", func(t *testing.T) {
		require.NoError(t, os.WriteFile(watched, []byte("v2"), 0o600))
		assert.Eventually(t, func() bool { return fired.Load() > 0 }, 5*time.Second, 20*time.Millisecond)
	})

	t.Run("a replacement by rename is reported", func(t *testing.T) {
		// How a real renewal lands: written alongside, then moved over. A watch
		// placed on the file itself would have died with the old inode.
		before := fired.Load()
		staged := watched + ".new"
		require.NoError(t, os.WriteFile(staged, []byte("v3"), 0o600))
		require.NoError(t, os.Rename(staged, watched))

		assert.Eventually(t, func() bool { return fired.Load() > before }, 5*time.Second, 20*time.Millisecond)
	})

	t.Run("a neighboring file does not reload the config", func(t *testing.T) {
		// Routing is by path: once a directory is watched, events arrive for
		// every file in it, and a config reload triggered by an unrelated write
		// would apply a policy change nobody made.
		before := fired.Load()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "certs", "unrelated.txt"), []byte("x"), 0o600))

		time.Sleep(2 * fileSettleDelay)
		assert.Equal(t, before, fired.Load())
	})

	t.Run("a watch needs a path and a callback", func(t *testing.T) {
		assert.Error(t, watcher.WatchFile("", func() {}))
		assert.Error(t, watcher.WatchFile(watched, nil))
	})

	t.Run("stopping twice is not an error", func(t *testing.T) {
		// Start's own failure path calls Stop, and so does the caller's defer.
		require.NoError(t, watcher.Stop())
		assert.NoError(t, watcher.Stop())
	})
}

// TestValidationNamesTheField is a small guard on the error text every one of
// these messages is read through.
func TestValidationNamesTheField(t *testing.T) {
	err := &ValidationError{Field: "tls.cert_file", Message: "must be set"}
	assert.Equal(t, "invalid config: tls.cert_file - must be set", err.Error())

	multi := &MultiValidationError{Errors: []*ValidationError{
		{Field: "a", Message: "one"},
		{Field: "b", Message: "two"},
	}}
	assert.True(t, strings.Contains(multi.Error(), "a") && strings.Contains(multi.Error(), "b"))
}
