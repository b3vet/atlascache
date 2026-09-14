package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The connection settings FEAT-0024 adds, and the one combination check among
// them.
//
// Each is a bound on what one client may cost, so the tests below care about
// two things: that the default is a real limit rather than a placeholder, and
// that a value which reads as deliberate but cannot work is refused rather than
// accepted and quietly corrected.

func TestConnectionDefaultsAreRealLimits(t *testing.T) {
	defaults := Defaults()

	assert.Equal(t, 10000, defaults.Server.MaxConnections,
		"max_connections is mandatory, not optional (ADR-0021)")
	assert.Equal(t, 30*time.Second, defaults.Server.ClientIdleTimeout)
	assert.Equal(t, 1024, defaults.Server.MaxPipelineCommands)
	assert.Equal(t, "64MB", defaults.Server.MaxOutputBuffer)
	assert.Equal(t, "0", defaults.Server.MaxRequestSize, "the default budget is derived")

	require.NoError(t, Validate(defaults), "the defaults must be a configuration the server can honor")
}

// TestTheRequestBudgetIsDerivedFromTheValueSize. Leaving it derived is what
// keeps a deployment that raises max_value_size from finding its largest values
// unwritable (ISSUE-0018).
func TestTheRequestBudgetIsDerivedFromTheValueSize(t *testing.T) {
	cfg := Defaults()

	budget, err := cfg.RequestBudget()
	require.NoError(t, err)
	assert.EqualValues(t, 1024*1024+RequestBudgetMargin, budget, "1MB of value plus the framing margin")

	cfg.Storage.MaxValueSize = "16MB"
	budget, err = cfg.RequestBudget()
	require.NoError(t, err)
	assert.EqualValues(t, 16*1024*1024+RequestBudgetMargin, budget, "the budget follows the value size up")

	cfg.Server.MaxRequestSize = "32MB"
	budget, err = cfg.RequestBudget()
	require.NoError(t, err)
	assert.EqualValues(t, 32*1024*1024, budget, "an explicit budget wins")
}

func TestRequestBudgetReportsUnparseableSizes(t *testing.T) {
	cfg := Defaults()
	cfg.Server.MaxRequestSize = "several"
	_, err := cfg.RequestBudget()
	assert.Error(t, err)

	cfg = Defaults()
	cfg.Storage.MaxValueSize = "a bit"
	_, err = cfg.RequestBudget()
	assert.Error(t, err)
}

// TestARequestBudgetBelowTheValueSizeIsRefused is the combination check: each
// field is legal on its own, and together they describe a server whose largest
// permitted value can never be written to it. That is the class of defect a
// per-field pass cannot see, and the one ISSUE-0017 was.
func TestARequestBudgetBelowTheValueSizeIsRefused(t *testing.T) {
	cfg := Defaults()
	cfg.Storage.MaxValueSize = "8MB"
	cfg.Server.MaxRequestSize = "1MB"

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), fieldMaxRequestSize)
	assert.Contains(t, err.Error(), fieldMaxValueSize,
		"a combination error has to name both fields, or it does not say what to change")

	// Raising the budget over the value size plus framing settles it.
	cfg.Server.MaxRequestSize = "9MB"
	assert.NoError(t, Validate(cfg))
}

func TestConnectionSettingsAreValidated(t *testing.T) {
	tests := map[string]struct {
		mutate func(*Config)
		field  string
	}{
		"no connections at all": {
			mutate: func(c *Config) { c.Server.MaxConnections = 0 },
			field:  "server.max_connections",
		},
		"a negative connection limit": {
			mutate: func(c *Config) { c.Server.MaxConnections = -1 },
			field:  "server.max_connections",
		},
		"more connections than a machine has": {
			mutate: func(c *Config) { c.Server.MaxConnections = 2_000_000 },
			field:  "server.max_connections",
		},
		"a negative idle timeout": {
			mutate: func(c *Config) { c.Server.ClientIdleTimeout = -time.Second },
			field:  "server.client_idle_timeout",
		},
		"an idle timeout shorter than a round trip": {
			mutate: func(c *Config) { c.Server.ClientIdleTimeout = 10 * time.Millisecond },
			field:  "server.client_idle_timeout",
		},
		"a pipeline batch of nothing": {
			mutate: func(c *Config) { c.Server.MaxPipelineCommands = 0 },
			field:  "server.max_pipeline_commands",
		},
		"an unbounded pipeline batch": {
			mutate: func(c *Config) { c.Server.MaxPipelineCommands = 2_000_000 },
			field:  "server.max_pipeline_commands",
		},
		"an output buffer that is not a size": {
			mutate: func(c *Config) { c.Server.MaxOutputBuffer = "lots" },
			field:  "server.max_output_buffer",
		},
		"an output buffer below one reply": {
			mutate: func(c *Config) { c.Server.MaxOutputBuffer = "1KB" },
			field:  "server.max_output_buffer",
		},
		"a request size that is not a size": {
			mutate: func(c *Config) { c.Server.MaxRequestSize = "big" },
			field:  fieldMaxRequestSize,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := Defaults()
			tt.mutate(cfg)

			err := Validate(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.field)
		})
	}
}

// TestTheTimeoutsThatMeanOff. Zero is an operator's decision for two of these
// and must survive validation, or "off" becomes unreachable.
func TestTheTimeoutsThatMeanOff(t *testing.T) {
	cfg := Defaults()
	cfg.Server.ClientIdleTimeout = 0
	cfg.Server.MaxOutputBuffer = "0"

	assert.NoError(t, Validate(cfg))
}
