package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

// LoadError represents a configuration loading error
type LoadError struct {
	Path string
	Err  error
}

func (e *LoadError) Error() string {
	return fmt.Sprintf("failed to load config from %s: %v", e.Path, e.Err)
}

func (e *LoadError) Unwrap() error {
	return e.Err
}

// Loader handles configuration loading and management
type Loader struct {
	v    *viper.Viper
	path string
}

// NewLoader creates a new configuration loader
func NewLoader() *Loader {
	v := viper.New()

	// Set config name and type
	v.SetConfigName("config")
	v.SetConfigType("yaml")

	// Add search paths
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/atlascache")
	v.AddConfigPath("$HOME/.atlascache")

	// Environment variable support
	v.SetEnvPrefix("ATLAS")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	return &Loader{
		v: v,
	}
}

// LoadFromFile loads configuration from a specific file path
func (l *Loader) LoadFromFile(path string) (*Config, error) {
	l.path = path

	// Set the config file
	l.v.SetConfigFile(path)

	// Set defaults first
	l.setDefaults()

	// Try to read the config file
	if err := l.v.ReadInConfig(); err != nil {
		// Check if file doesn't exist - that's okay, we'll use defaults
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			return l.buildConfig()
		}
		return nil, &LoadError{Path: path, Err: err}
	}

	return l.buildConfig()
}

// Load loads configuration from the default search paths
func (l *Loader) Load() (*Config, error) {
	// Set defaults first
	l.setDefaults()

	// Try to read the config file
	if err := l.v.ReadInConfig(); err != nil {
		// If no config file found, just use defaults + env vars
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			return l.buildConfig()
		}
		return nil, &LoadError{Path: "default paths", Err: err}
	}

	l.path = l.v.ConfigFileUsed()
	return l.buildConfig()
}

// setDefaults sets the default configuration values
func (l *Loader) setDefaults() {
	defaults := Defaults()

	// Node defaults
	l.v.SetDefault("node.id", defaults.Node.ID)
	l.v.SetDefault("node.name", defaults.Node.Name)
	l.v.SetDefault("node.data_dir", defaults.Node.DataDir)

	// Server defaults
	l.v.SetDefault("server.bind_addr", defaults.Server.BindAddr)
	l.v.SetDefault("server.client_port", defaults.Server.ClientPort)

	// Admin defaults
	l.v.SetDefault("admin.bind_addr", defaults.Admin.BindAddr)
	l.v.SetDefault("admin.port", defaults.Admin.Port)

	// Storage defaults
	l.v.SetDefault("storage.shard_count", defaults.Storage.ShardCount)
	l.v.SetDefault("storage.max_memory", defaults.Storage.MaxMemory)
	l.v.SetDefault("storage.max_value_size", defaults.Storage.MaxValueSize)

	// TTL defaults
	l.v.SetDefault("ttl.check_interval", defaults.TTL.CheckInterval)
	l.v.SetDefault("ttl.lazy_expiration", defaults.TTL.LazyExpiration)
	l.v.SetDefault("ttl.active_expiration", defaults.TTL.ActiveExpiration)
	l.v.SetDefault("ttl.batch_size", defaults.TTL.BatchSize)

	// Eviction defaults
	l.v.SetDefault("eviction.policy", defaults.Eviction.Policy)
	l.v.SetDefault("eviction.sample_size", defaults.Eviction.SampleSize)

	// Logging defaults
	l.v.SetDefault("logging.level", defaults.Logging.Level)
	l.v.SetDefault("logging.format", defaults.Logging.Format)
}

// buildConfig creates a Config from viper values
func (l *Loader) buildConfig() (*Config, error) {
	cfg := &Config{}

	if err := l.v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Validate the configuration
	if err := Validate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// GetPath returns the path of the loaded config file
func (l *Loader) GetPath() string {
	return l.path
}

// GetViper returns the underlying viper instance
func (l *Loader) GetViper() *viper.Viper {
	return l.v
}

// MustLoad loads configuration and panics on error
func MustLoad() *Config {
	loader := NewLoader()
	cfg, err := loader.Load()
	if err != nil {
		panic(fmt.Sprintf("failed to load configuration: %v", err))
	}
	return cfg
}

// MustLoadFromFile loads configuration from file and panics on error
func MustLoadFromFile(path string) *Config {
	loader := NewLoader()
	cfg, err := loader.LoadFromFile(path)
	if err != nil {
		panic(fmt.Sprintf("failed to load configuration: %v", err))
	}
	return cfg
}

// ResolveConfigPath finds a config file in standard locations
func ResolveConfigPath() string {
	locations := []string{
		"config.yaml",
		"config.yml",
		filepath.Join(os.Getenv("HOME"), ".atlascache", "config.yaml"),
		"/etc/atlascache/config.yaml",
	}

	for _, loc := range locations {
		// The only non-literal element is built from $HOME, and this probes a
		// fixed set of well-known locations rather than acting on user input.
		if _, err := os.Stat(loc); err == nil { //nolint:gosec // fixed candidate list, not attacker-controlled
			return loc
		}
	}

	return ""
}
