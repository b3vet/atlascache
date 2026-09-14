package admin

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b3vet/atlascache/internal/config"
)

// fakeConfig is a ConfigSource that renders a real configuration, so the
// endpoint is tested against the redaction that ships rather than against a map
// written to pass.
type fakeConfig struct {
	cfg *config.Config
}

func (f fakeConfig) EffectiveConfig() map[string]any {
	cfg := f.cfg
	if cfg == nil {
		cfg = config.Defaults()
	}
	return cfg.Effective()
}

// TestConfigRendersTheEffectiveConfiguration checks that the endpoint answers
// with something an operator can diff against their file — the failure mode
// being an endpoint that returns `{}` and passes every redaction test there is.
func TestConfigRendersTheEffectiveConfiguration(t *testing.T) {
	srv := newTestServer(t, WithConfig(fakeConfig{}))
	srv.SetReady(true)

	resp := get(t, srv, "/config")
	require.Equal(t, http.StatusOK, resp.status)

	fields := resp.fields(t)
	for _, section := range []string{"node", "server", "admin", "storage", "ttl", "eviction", "logging", "tls", "auth"} {
		assert.Contains(t, fields, section)
	}

	eviction, ok := fields["eviction"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "lru", eviction["policy"], "a non-secret value is reported as it is")
}

// TestConfigReturnsNoTokenAnywhereInTheBody is the check FEAT-0030 names as the
// one that matters.
//
// It scans the whole response rather than reading auth.token and admin.token,
// because a field-by-field check passes against a body that leaked the same
// secret through a path nobody thought to look at — a section added later, a
// nested duplicate, a debug field. The tokens are distinctive strings so that a
// substring match cannot be satisfied by chance.
//
// The E2E spec admin-config-redact runs the same scan against a real process;
// this one runs it against every handler path in a unit test, which is where it
// is cheap enough to keep.
func TestConfigReturnsNoTokenAnywhereInTheBody(t *testing.T) {
	const (
		authSecret  = "CLIENT-TOKEN-must-never-appear-3f9c1e"
		adminSecret = "ADMIN-TOKEN-must-never-appear-7b2d40"
	)

	cfg := config.Defaults()
	cfg.Auth.Enabled = true
	cfg.Auth.Token = authSecret
	cfg.Admin.Token = adminSecret
	require.NoError(t, config.Validate(cfg), "the fixture must be a configuration the server would accept")

	srv := newTestServer(t, WithToken(adminSecret), WithConfig(fakeConfig{cfg: cfg}))
	srv.SetReady(true)

	resp := request(t, srv, http.MethodGet, "/config", map[string]string{"Authorization": "Bearer " + adminSecret})
	require.Equal(t, http.StatusOK, resp.status)

	assert.NotContains(t, resp.body, authSecret,
		"the client token leaked to a caller who held only the admin one")
	assert.NotContains(t, resp.body, adminSecret,
		"the admin token was returned by the endpoint it protects")

	// And the fields are present and marked, rather than dropped: an operator
	// has to be able to see that a token is configured.
	fields := resp.fields(t)
	auth, ok := fields["auth"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, config.RedactedValue, auth["token"])

	admin, ok := fields["admin"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, config.RedactedValue, admin["token"])
}

// TestConfigShowsAnUnsetTokenAsEmpty keeps the redaction from claiming a secret
// that is not there: "<redacted>" against an unconfigured token would tell an
// operator they had set one.
func TestConfigShowsAnUnsetTokenAsEmpty(t *testing.T) {
	srv := newTestServer(t, WithConfig(fakeConfig{}))
	srv.SetReady(true)

	fields := get(t, srv, "/config").fields(t)

	auth, ok := fields["auth"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "", auth["token"])
}

func TestConfigWithoutASourceIsUnavailable(t *testing.T) {
	srv := newTestServer(t)
	srv.SetReady(true)

	resp := get(t, srv, "/config")
	assert.Equal(t, http.StatusServiceUnavailable, resp.status)
	assert.Contains(t, resp.fields(t)["error"], "not available")
}

// TestConfigSourceCanBeReplaced covers a hot reload: /config has to follow the
// configuration the node applied, not the one it was launched with.
func TestConfigSourceCanBeReplaced(t *testing.T) {
	srv := newTestServer(t, WithConfig(fakeConfig{}))
	srv.SetReady(true)

	reloaded := config.Defaults()
	reloaded.Eviction.Policy = "fifo"
	srv.SetConfig(fakeConfig{cfg: reloaded})

	eviction, ok := get(t, srv, "/config").fields(t)["eviction"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "fifo", eviction["policy"])
}
