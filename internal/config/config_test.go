package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaults(t *testing.T) {
	cfg := Defaults()

	assert.Equal(t, "", cfg.Node.ID)
	assert.Equal(t, "/var/lib/atlascache", cfg.Node.DataDir)
	assert.Equal(t, 0, cfg.Storage.ShardCount)
	assert.Equal(t, "0", cfg.Storage.MaxMemory)
	assert.Equal(t, "1MB", cfg.Storage.MaxValueSize)
	assert.Equal(t, 100*time.Millisecond, cfg.TTL.CheckInterval)
	assert.True(t, cfg.TTL.LazyExpiration)
	assert.True(t, cfg.TTL.ActiveExpiration)
	assert.Equal(t, "lru", cfg.Eviction.Policy)
	assert.Equal(t, 5, cfg.Eviction.SampleSize)
	assert.Equal(t, "info", cfg.Logging.Level)
	assert.Equal(t, "json", cfg.Logging.Format)
}

func TestGetShardCount(t *testing.T) {
	tests := []struct {
		name       string
		shardCount int
		wantMin    int
		wantMax    int
	}{
		{
			name:       "auto-detect",
			shardCount: 0,
			wantMin:    16,
			wantMax:    1024,
		},
		{
			name:       "explicit value",
			shardCount: 64,
			wantMin:    64,
			wantMax:    64,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Storage.ShardCount = tt.shardCount
			count := cfg.GetShardCount()
			assert.GreaterOrEqual(t, count, tt.wantMin)
			assert.LessOrEqual(t, count, tt.wantMax)
		})
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		input    string
		expected uint64
		wantErr  bool
	}{
		{"0", 0, false},
		{"", 0, false},
		{"1024", 1024, false},
		{"1KB", 1024, false},
		{"1kb", 1024, false},
		{"1MB", 1024 * 1024, false},
		{"1GB", 1024 * 1024 * 1024, false},
		{"1TB", 1024 * 1024 * 1024 * 1024, false},
		{"512MB", 512 * 1024 * 1024, false},
		{"2GB", 2 * 1024 * 1024 * 1024, false},
		{"invalid", 0, true},
		{"-1", 0, true},
		{"1XB", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := ParseSize(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestFormatSize(t *testing.T) {
	tests := []struct {
		bytes    uint64
		expected string
	}{
		{0, "0B"},
		{512, "512B"},
		{1024, "1.0KB"},
		{1024 * 1024, "1.0MB"},
		{1024 * 1024 * 1024, "1.0GB"},
		{1536 * 1024 * 1024, "1.5GB"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := FormatSize(tt.bytes)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestValidation(t *testing.T) {
	t.Run("valid config", func(t *testing.T) {
		cfg := Defaults()
		err := Validate(cfg)
		assert.NoError(t, err)
	})

	t.Run("invalid shard count", func(t *testing.T) {
		cfg := Defaults()
		cfg.Storage.ShardCount = -1
		err := Validate(cfg)
		assert.Error(t, err)
	})

	t.Run("invalid max memory", func(t *testing.T) {
		cfg := Defaults()
		cfg.Storage.MaxMemory = "invalid"
		err := Validate(cfg)
		assert.Error(t, err)
	})

	t.Run("invalid eviction policy", func(t *testing.T) {
		cfg := Defaults()
		cfg.Eviction.Policy = "random"
		err := Validate(cfg)
		assert.Error(t, err)
	})

	t.Run("invalid log level", func(t *testing.T) {
		cfg := Defaults()
		cfg.Logging.Level = "verbose"
		err := Validate(cfg)
		assert.Error(t, err)
	})

	t.Run("invalid log format", func(t *testing.T) {
		cfg := Defaults()
		cfg.Logging.Format = "xml"
		err := Validate(cfg)
		assert.Error(t, err)
	})
}

func TestLoader(t *testing.T) {
	// Create temp config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	configContent := `
node:
  id: "test-node"
  data_dir: "/tmp/test"
storage:
  shard_count: 32
  max_memory: "512MB"
eviction:
  policy: "lfu"
logging:
  level: "debug"
`
	err := os.WriteFile(configPath, []byte(configContent), 0644)
	require.NoError(t, err)

	t.Run("load from file", func(t *testing.T) {
		loader := NewLoader()
		cfg, err := loader.LoadFromFile(configPath)
		require.NoError(t, err)

		assert.Equal(t, "test-node", cfg.Node.ID)
		assert.Equal(t, "/tmp/test", cfg.Node.DataDir)
		assert.Equal(t, 32, cfg.Storage.ShardCount)
		assert.Equal(t, "512MB", cfg.Storage.MaxMemory)
		assert.Equal(t, "lfu", cfg.Eviction.Policy)
		assert.Equal(t, "debug", cfg.Logging.Level)
	})

	t.Run("load defaults when no file", func(t *testing.T) {
		loader := NewLoader()
		cfg, err := loader.LoadFromFile(filepath.Join(tmpDir, "nonexistent.yaml"))
		require.NoError(t, err)

		// Should have defaults
		assert.Equal(t, 0, cfg.Storage.ShardCount)
		assert.Equal(t, "lru", cfg.Eviction.Policy)
	})
}

func TestEnvOverrides(t *testing.T) {
	// Set environment variables
	os.Setenv("ATLAS_STORAGE_SHARD_COUNT", "128")
	os.Setenv("ATLAS_EVICTION_POLICY", "fifo")
	defer func() {
		os.Unsetenv("ATLAS_STORAGE_SHARD_COUNT")
		os.Unsetenv("ATLAS_EVICTION_POLICY")
	}()

	loader := NewLoader()
	cfg, err := loader.Load()
	require.NoError(t, err)

	assert.Equal(t, 128, cfg.Storage.ShardCount)
	assert.Equal(t, "fifo", cfg.Eviction.Policy)
}
