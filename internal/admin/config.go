package admin

import "net/http"

// ConfigSource is where /config gets the document it renders.
//
// The contract is that what comes back is already redacted. That is not this
// package being trusting: redaction belongs where the secrets are declared, in
// internal/config, because that is the only place that can know a newly added
// field holds one. A redactor living here would be a list of paths maintained
// at a distance from the fields it describes, and the failure mode of such a
// list is that it silently falls behind.
//
// internal/config redacts on two independent rules — the field's path, and the
// field's type — so a secret has to escape both to reach this handler. The
// admin-config-redact spec scans the whole response body for the token values
// rather than checking named fields, which is what catches one that does.
type ConfigSource interface {
	// EffectiveConfig returns the configuration the node is running, keyed the
	// way the configuration file keys it, with every secret already replaced.
	EffectiveConfig() map[string]any
}

// handleConfig renders the effective configuration.
//
// Read-only in v0.1.0. PUT /config arrives in P6; until then the way to change
// a setting is the configuration file, which hot-reload picks up for the fields
// that can be applied without a restart.
func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	source := s.configSource()
	if source == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errorResponse{
			Error: "the effective configuration is not available on this node",
		})
		return
	}

	s.writeJSON(w, http.StatusOK, source.EffectiveConfig())
}
