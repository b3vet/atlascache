package main

import (
	"sync/atomic"

	"github.com/b3vet/atlascache/internal/config"
	"github.com/b3vet/atlascache/internal/server"
)

// adminStats is what the admin API's /stats and /stats/memory read.
//
// It joins the two halves the figures come from — the keyspace, which is the
// same object the STATS command reads through, and the server, which owns the
// limits those figures are measured against — and adds nothing. That is the
// whole design: FEAT-0030 requires the admin API and the STATS command to agree,
// and the only way to guarantee agreement is for there to be one set of
// counters with one reader. A struct here that computed anything would be a
// second path, and two paths drift.
//
// It lives in the composition root for the same reason the keyspace seam does:
// this is the only place entitled to know about both internal/server and the
// storage engine behind it.
type adminStats struct {
	store keyspace
	srv   *server.Server
}

func (a adminStats) Stats() server.Stats         { return a.store.Stats() }
func (a adminStats) Limits() server.ConnLimits   { return a.srv.Limits() }
func (a adminStats) ConnStats() server.ConnStats { return a.srv.ConnStats() }

// effectiveConfig is the configuration the admin API reports.
//
// It holds a pointer rather than a rendered document so that a hot reload is a
// single store: the watcher swaps in the configuration it just applied and the
// next /config request renders that. Rendering at request time also means the
// redaction runs at request time, which is where it has to run — a document
// redacted once at startup and cached would be a copy of the secrets sitting in
// memory waiting for someone to forget why it was safe.
//
// The caveat, and it is P6's to remove: a reload applies only the settings that
// can change without a restart (the eviction policy and the memory ceiling),
// while this reports the whole file. A field that needs a restart therefore
// reads as configured rather than as running until the node is restarted.
type effectiveConfig struct {
	current atomic.Pointer[config.Config]
}

// newEffectiveConfig returns a source reporting cfg.
func newEffectiveConfig(cfg *config.Config) *effectiveConfig {
	source := &effectiveConfig{}
	source.set(cfg)
	return source
}

// set replaces the configuration reported by /config.
func (e *effectiveConfig) set(cfg *config.Config) { e.current.Store(cfg) }

// EffectiveConfig renders the configuration with every secret redacted.
//
// The redaction is internal/config's, not this function's, and deliberately so:
// it is driven by the field's declared type and by a registered path, both of
// which live beside the fields themselves. A renderer here would be a list of
// secret names maintained at a distance from the struct it describes, and that
// list is the thing that falls behind.
func (e *effectiveConfig) EffectiveConfig() map[string]any {
	cfg := e.current.Load()
	if cfg == nil {
		return map[string]any{}
	}
	return cfg.Effective()
}
