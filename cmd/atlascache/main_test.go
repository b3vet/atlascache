package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b3vet/atlascache/internal/config"
	"github.com/b3vet/atlascache/internal/eviction"
)

// testConfig is the configuration the binary would have loaded, with a tick
// short enough to watch. Everything else is the shipped default.
func testConfig() *config.Config {
	cfg := config.Defaults()
	cfg.Storage.ShardCount = 8
	cfg.TTL.CheckInterval = 20 * time.Millisecond

	return cfg
}

func startedCore(t *testing.T, cfg *config.Config) *core {
	t.Helper()

	c, err := newCore(cfg)
	require.NoError(t, err)

	c.start(zerolog.Nop())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		assert.NoError(t, c.stop(ctx))
		c.close()
	})

	return c
}

// eventually polls until cond holds, so a test states the outcome it wants
// rather than a sleep long enough to hope for it.
func eventually(t *testing.T, within time.Duration, cond func() bool) bool {
	t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}

	return cond()
}

// TestWiredCoreReclaimsExpiredKeys is ISSUE-0007 through the wiring the binary
// actually runs: a TTL manager built from configuration, started, and reclaiming
// on its own. Lazy expiration is off, so nothing but the wheel can reclaim
// anything — a unit test of the engine passes with the manager unwired, and this
// one does not.
func TestWiredCoreReclaimsExpiredKeys(t *testing.T) {
	const keys = 5000

	cfg := testConfig()
	cfg.TTL.LazyExpiration = false
	c := startedCore(t, cfg)

	baselineKeys := c.engine.Stats().Keys
	baselineMemory := c.engine.MemoryUsed()

	const ttl = time.Second

	for i := 0; i < keys; i++ {
		require.NoError(t, c.engine.Set([]byte(fmt.Sprintf("key:%d", i)), []byte("value"), ttl))
	}

	// Every key is either still resident or already reclaimed, whichever side of
	// the TTL the write loop finished on. Asserting that all 5000 are still
	// present instead makes the test a race against its own TTL: CI caught this
	// with 4286 resident and 714 already correctly expired.
	written := c.engine.Stats()
	require.Equal(t, uint64(keys), written.Keys+written.Expirations,
		"every key written is either resident or already expired")
	require.Positive(t, written.MemoryUsed+written.Expirations)
	t.Logf("after writing %d keys with a %s TTL: keys=%d expired=%d memory=%dB",
		keys, ttl, written.Keys, written.Expirations, written.MemoryUsed)

	reclaimed := eventually(t, 5*time.Second, func() bool {
		return c.engine.Stats().Keys == baselineKeys && c.engine.MemoryUsed() == baselineMemory
	})

	after := c.engine.Stats()
	t.Logf("after expiry: keys=%d memory=%dB expirations=%d", after.Keys, after.MemoryUsed, after.Expirations)
	t.Logf("ttl manager: %+v", c.ttl.Stats())

	require.True(t, reclaimed, "key count and memory must return to baseline")
	assert.Equal(t, uint64(0), after.KeysWithTTL)
	assert.Equal(t, uint64(keys), after.Expirations, "each key expired exactly once")
	assert.Positive(t, c.ttl.Stats().Ticks, "the wheel actually ran")
	assert.Equal(t, uint64(keys), c.ttl.Stats().Expired)
}

// TestActiveExpirationDisabled covers the configuration flag: the manager is
// built and wired in either way, and simply never started. There is no pause
// control on it by design, so this is what "off" means.
func TestActiveExpirationDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.TTL.ActiveExpiration = false
	cfg.TTL.LazyExpiration = false
	c := startedCore(t, cfg)

	require.NoError(t, c.engine.Set([]byte("doomed"), []byte("value"), 20*time.Millisecond))
	time.Sleep(200 * time.Millisecond)

	assert.False(t, c.ttl.Stats().Running, "the manager was never started")
	assert.Equal(t, uint64(0), c.ttl.Stats().Ticks)
	assert.Equal(t, uint64(1), c.engine.Stats().Keys, "so nothing reclaimed the key")
	assert.Positive(t, c.ttl.Stats().Added, "but the hint was still accepted")

	t.Run("the passive path still works on its own", func(t *testing.T) {
		lazy := testConfig()
		lazy.TTL.ActiveExpiration = false
		lazy.TTL.LazyExpiration = true
		passive := startedCore(t, lazy)

		require.NoError(t, passive.engine.Set([]byte("doomed"), []byte("value"), 20*time.Millisecond))
		time.Sleep(30 * time.Millisecond)

		_, _, exists := passive.engine.Get([]byte("doomed"))

		assert.False(t, exists)
		assert.Equal(t, uint64(0), passive.engine.Stats().Keys, "the read reclaimed it")
		assert.Equal(t, uint64(0), passive.engine.MemoryUsed())
	})
}

// TestWiredCoreEvicts checks the other half of the wiring: the configured policy
// is installed on the engine, so max_memory is a limit the binary lives within.
func TestWiredCoreEvicts(t *testing.T) {
	cfg := testConfig()
	cfg.Storage.ShardCount = 1
	cfg.Storage.MaxMemory = "8KB"
	c := startedCore(t, cfg)

	for i := 0; i < 500; i++ {
		require.NoError(t, c.engine.Set([]byte(fmt.Sprintf("key:%04d", i)), make([]byte, 64), 0))
		require.LessOrEqual(t, c.engine.MemoryUsed(), uint64(8*1024))
	}

	assert.Positive(t, c.engine.Stats().Evictions)
	assert.Equal(t, uint64(0), c.engine.Stats().OOMRejected)
}

// TestApplyConfigSwitchesPolicyAtRuntime is the hot-reload path: what the
// watcher's callback does when the file changes.
func TestApplyConfigSwitchesPolicyAtRuntime(t *testing.T) {
	c := startedCore(t, testConfig())
	require.Equal(t, eviction.PolicyLRU, c.eviction.Policy())

	reloaded := testConfig()
	reloaded.Eviction.Policy = eviction.PolicyLFU
	reloaded.Storage.MaxMemory = "16MB"
	c.applyConfig(reloaded, zerolog.Nop())

	assert.Equal(t, eviction.PolicyLFU, c.eviction.Policy())
	assert.Equal(t, uint64(16*1024*1024), c.engine.MaxMemory())

	t.Run("a bad reload leaves the running configuration alone", func(t *testing.T) {
		broken := testConfig()
		broken.Eviction.Policy = "random"
		broken.Storage.MaxMemory = "not-a-size"
		c.applyConfig(broken, zerolog.Nop())

		assert.Equal(t, eviction.PolicyLFU, c.eviction.Policy())
		assert.Equal(t, uint64(16*1024*1024), c.engine.MaxMemory())
	})
}

func TestNewCoreRejectsBadConfiguration(t *testing.T) {
	cases := map[string]func(*config.Config){
		"max_memory":     func(cfg *config.Config) { cfg.Storage.MaxMemory = "lots" },
		"max_value_size": func(cfg *config.Config) { cfg.Storage.MaxValueSize = "some" },
		"policy":         func(cfg *config.Config) { cfg.Eviction.Policy = "random" },
	}

	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			breakIt(cfg)

			c, err := newCore(cfg)

			require.Error(t, err)
			assert.Nil(t, c)
		})
	}
}

func TestWatchConfig(t *testing.T) {
	t.Run("no config file, no watcher", func(t *testing.T) {
		assert.Nil(t, watchConfig("", nil, zerolog.Nop()))
	})

	t.Run("an unreadable path is not fatal", func(t *testing.T) {
		assert.Nil(t, watchConfig(filepath.Join(t.TempDir(), "absent.yaml"), nil, zerolog.Nop()))
	})

	t.Run("a real file is watched and applied", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		require.NoError(t, os.WriteFile(path, []byte("eviction:\n  policy: \"lru\"\n"), 0o600))

		c := startedCore(t, testConfig())
		watcher := watchConfig(path, c, zerolog.Nop())
		require.NotNil(t, watcher)
		defer func() { assert.NoError(t, watcher.Stop()) }()

		require.NoError(t, os.WriteFile(path, []byte("eviction:\n  policy: \"fifo\"\n"), 0o600))

		assert.True(t, eventually(t, 5*time.Second, func() bool {
			return c.eviction.Policy() == eviction.PolicyFIFO
		}), "the policy follows the file")
	})
}

func TestLoadConfig(t *testing.T) {
	t.Run("no path falls back to the defaults", func(t *testing.T) {
		cfg, path, err := loadConfig("")

		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, 10000, cfg.TTL.BatchSize, "the new ttl.batch_size default is plumbed through")
		_ = path
	})

	t.Run("a named file that is not there is an error", func(t *testing.T) {
		_, _, err := loadConfig(filepath.Join(t.TempDir(), "absent.yaml"))

		require.Error(t, err)
	})

	t.Run("a named file is loaded", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		require.NoError(t, os.WriteFile(path, []byte("ttl:\n  batch_size: 42\n"), 0o600))

		cfg, used, err := loadConfig(path)

		require.NoError(t, err)
		assert.Equal(t, path, used)
		assert.Equal(t, 42, cfg.TTL.BatchSize)
	})
}
