package admin

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// protectedPaths is every endpoint ADR-0023 puts behind the admin token. The
// list is walked rather than sampled so that an endpoint added without its
// guard fails here instead of in production.
var protectedPaths = []string{"/stats", "/stats/memory", "/config"}

const (
	adminToken  = "admin-token-4f1c9a72"
	clientToken = "client-token-8b30de41"
)

// TestProtectedEndpointsRequireTheAdminToken walks the ways a request can fail
// to carry the credential, including the one the ADR exists for: a caller
// holding the *client* token.
func TestProtectedEndpointsRequireTheAdminToken(t *testing.T) {
	srv := newTestServer(t, WithToken(adminToken), WithStats(fakeStats{}), WithConfig(fakeConfig{}))
	srv.SetReady(true)

	refused := map[string]map[string]string{
		"no credential at all":          nil,
		"an empty bearer":               {"Authorization": "Bearer "},
		"a wrong token":                 {"Authorization": "Bearer not-the-token"},
		"the wrong scheme":              {"Authorization": "Basic " + adminToken},
		"the token as a bare header":    {"Authorization": adminToken},
		"a prefix of the right token":   {"Authorization": "Bearer " + adminToken[:8]},
		"the client token (ADR-0023)":   {"Authorization": "Bearer " + clientToken},
		"the client token as X-Admin":   {tokenHeader: clientToken},
		"the right token, wrong header": {"X-Admin-Auth": adminToken},
	}

	for name, headers := range refused {
		for _, path := range protectedPaths {
			resp := request(t, srv, http.MethodGet, path, headers)
			assert.Equal(t, http.StatusUnauthorized, resp.status, "GET %s with %s", path, name)
			assert.Equal(t, `Bearer realm="atlascache admin"`, resp.headers.Get("WWW-Authenticate"),
				"a 401 has to say how to authenticate")
		}
	}
}

// TestTheAdminTokenOpensTheProtectedEndpoints covers both header forms. Two
// forms exist because an operator's first move is curl and the second is a
// monitoring agent, and neither should have to look up the other's convention.
func TestTheAdminTokenOpensTheProtectedEndpoints(t *testing.T) {
	srv := newTestServer(t, WithToken(adminToken), WithStats(fakeStats{}), WithConfig(fakeConfig{}))
	srv.SetReady(true)

	accepted := []map[string]string{
		{"Authorization": "Bearer " + adminToken},
		{"Authorization": "bearer " + adminToken},
		{tokenHeader: adminToken},
	}

	for _, headers := range accepted {
		for _, path := range protectedPaths {
			resp := request(t, srv, http.MethodGet, path, headers)
			assert.Equal(t, http.StatusOK, resp.status, "GET %s with %v", path, headers)
		}
	}
}

// TestUnauthorizedResponseNeverEchoesWhatWasPresented guards a mistake that is
// easy to make while writing a helpful error: repeating the rejected token puts
// it into the caller's terminal, their shell history, their proxy's access log
// and any screenshot of the failure.
func TestUnauthorizedResponseNeverEchoesWhatWasPresented(t *testing.T) {
	const presented = "a-token-that-must-not-come-back-e91a7"

	srv := newTestServer(t, WithToken(adminToken), WithStats(fakeStats{}), WithConfig(fakeConfig{}))

	for _, path := range protectedPaths {
		resp := request(t, srv, http.MethodGet, path, map[string]string{"Authorization": "Bearer " + presented})

		require.Equal(t, http.StatusUnauthorized, resp.status)
		assert.NotContains(t, resp.body, presented, "GET %s echoed the rejected token", path)
		assert.NotContains(t, resp.body, adminToken, "GET %s disclosed the configured token", path)
	}
}

// TestProtectedEndpointsAreOpenWithoutAConfiguredToken records the deliberate
// default, so that it is a decision rather than an oversight.
//
// With no admin.token the protected endpoints answer anyone who can reach the
// port — and config validation only allows that while the API is bound to
// loopback (ADR-0023), so "anyone" means a process on this host. The guard is
// in configuration rather than here because a node that would serve statistics
// to the network should refuse to start, not start and refuse every request.
func TestProtectedEndpointsAreOpenWithoutAConfiguredToken(t *testing.T) {
	srv := newTestServer(t, WithStats(fakeStats{}), WithConfig(fakeConfig{}))
	srv.SetReady(true)

	for _, path := range protectedPaths {
		assert.Equal(t, http.StatusOK, get(t, srv, path).status, "GET %s", path)
	}
}
