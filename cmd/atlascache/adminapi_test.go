package main

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b3vet/atlascache/internal/config"
	"github.com/b3vet/atlascache/internal/server"
	"github.com/b3vet/atlascache/internal/storage"
)

// TestAdminStatsReadsTheSameSourceAsTheStatsCommand is the anti-drift wiring
// check.
//
// The STATS command reaches the engine through the keyspace seam, and so does
// this. Writing to the engine and seeing the figure move proves the admin API
// is reading the live keyspace and not a copy taken at startup — which is the
// failure that would make /stats disagree with STATS while both looked healthy.
func TestAdminStatsReadsTheSameSourceAsTheStatsCommand(t *testing.T) {
	cfg := testConfig()
	cfg.Server.MaxConnections = 4242

	core := startedCore(t, cfg)
	procmem := storage.NewProcessMemorySampler(10 * time.Millisecond)
	procmem.Start()
	t.Cleanup(procmem.Stop)

	store := keyspace{engine: core.engine, procmem: procmem}

	limits, err := connLimits(cfg)
	require.NoError(t, err)

	srv, err := server.New(context.Background(), "127.0.0.1:0", zerolog.Nop(), store,
		server.WithConnLimits(limits))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		assert.NoError(t, srv.Shutdown(ctx))
	})

	source := adminStats{store: store, srv: srv}

	before := source.Stats()
	require.NoError(t, core.engine.Set([]byte("admin:stats:key"), []byte("value"), 0))

	after := source.Stats()
	assert.Equal(t, before.Keys+1, after.Keys, "the admin API reads the live keyspace")
	assert.Equal(t, before.Sets+1, after.Sets)
	assert.Equal(t, store.Stats().Keys, after.Keys, "and reads it through the same seam STATS does")

	// The limits come from the server rather than from a second copy of the
	// configuration, so /stats cannot report a ceiling the server is not
	// actually enforcing.
	assert.Equal(t, 4242, source.Limits().MaxConnections)
	assert.Equal(t, srv.Limits(), source.Limits())

	assert.True(t, eventually(t, 2*time.Second, func() bool {
		return !source.Stats().Process.SampledAt.IsZero()
	}), "the process figures come from the sampler, never from a read on the request path")
}

func TestEffectiveConfigReportsTheRunningConfiguration(t *testing.T) {
	cfg := config.Defaults()
	cfg.Eviction.Policy = "lfu"
	cfg.Auth.Enabled = true
	cfg.Auth.Token = "CLIENT-TOKEN-cmd-5a1e"
	cfg.Admin.Token = "ADMIN-TOKEN-cmd-9f22"

	source := newEffectiveConfig(cfg)
	rendered := source.EffectiveConfig()

	eviction, ok := rendered["eviction"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "lfu", eviction["policy"])

	// The redaction is internal/config's, and the wiring must not have routed
	// around it.
	auth, ok := rendered["auth"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, config.RedactedValue, auth["token"])

	admin, ok := rendered["admin"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, config.RedactedValue, admin["token"])

	t.Run("a reload replaces what is reported", func(t *testing.T) {
		reloaded := config.Defaults()
		reloaded.Eviction.Policy = "fifo"
		source.set(reloaded)

		eviction, ok := source.EffectiveConfig()["eviction"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "fifo", eviction["policy"])
	})
}

// TestEffectiveConfigBeforeAnythingIsSet covers the window between binding the
// admin port and loading a configuration into it. An empty document is right;
// a nil dereference would take down the process over a probe.
func TestEffectiveConfigBeforeAnythingIsSet(t *testing.T) {
	var source effectiveConfig
	assert.Empty(t, source.EffectiveConfig())
}
