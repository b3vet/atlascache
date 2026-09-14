// Package config provides configuration management for AtlasCache.
package config

import (
	"net"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Config represents the AtlasCache configuration
type Config struct {
	Node     NodeConfig     `mapstructure:"node"`
	Server   ServerConfig   `mapstructure:"server"`
	Admin    AdminConfig    `mapstructure:"admin"`
	Storage  StorageConfig  `mapstructure:"storage"`
	TTL      TTLConfig      `mapstructure:"ttl"`
	Eviction EvictionConfig `mapstructure:"eviction"`
	Logging  LoggingConfig  `mapstructure:"logging"`
	TLS      TLSConfig      `mapstructure:"tls"`
	Auth     AuthConfig     `mapstructure:"auth"`
}

// NodeConfig contains node identification settings
type NodeConfig struct {
	ID      string `mapstructure:"id"`       // Auto-generated if empty
	Name    string `mapstructure:"name"`     // Human-readable name
	DataDir string `mapstructure:"data_dir"` // Data directory path
}

// ServerConfig contains client-facing listener settings
type ServerConfig struct {
	BindAddr   string `mapstructure:"bind_addr"`   // Interface to bind, e.g. "0.0.0.0"
	ClientPort int    `mapstructure:"client_port"` // RESP client port
}

// AdminConfig contains admin HTTP API settings
type AdminConfig struct {
	BindAddr string `mapstructure:"bind_addr"` // Loopback by default (ADR-0023)
	Port     int    `mapstructure:"port"`      // Admin HTTP port
}

// TLSConfig contains client-facing TLS settings.
//
// Disabled by default (ADR-0009): the first run needs no certificates, and the
// startup warning names what that costs. There is deliberately no minimum
// version setting — the floor is TLS 1.3 and offering 1.2 would invite a
// deployment that quietly used it (FEAT-0023).
type TLSConfig struct {
	Enabled  bool   `mapstructure:"enabled"`   // false = plaintext, with a startup warning
	CertFile string `mapstructure:"cert_file"` // PEM certificate chain, leaf first
	KeyFile  string `mapstructure:"key_file"`  // PEM private key for cert_file
}

// AuthConfig contains client authentication settings.
//
// One shared token (ADR-0020), disabled by default (ADR-0009). The token is
// never logged, at any level, so nothing here is safe to print by reflection —
// see Redacted.
type AuthConfig struct {
	Enabled bool   `mapstructure:"enabled"` // false = open port, with a startup warning
	Token   string `mapstructure:"token"`   // the shared secret AUTH compares against
}

// Redacted renders the auth configuration without its secret, so a config dump
// or a debug log cannot leak the token by accident.
func (c AuthConfig) Redacted() AuthConfig {
	if c.Token != "" {
		c.Token = "<redacted>"
	}
	return c
}

// StorageConfig contains storage engine settings
type StorageConfig struct {
	ShardCount   int    `mapstructure:"shard_count"`    // 0 = auto (CPU × 4)
	MaxMemory    string `mapstructure:"max_memory"`     // e.g., "1GB", "512MB", "0" = unlimited
	MaxValueSize string `mapstructure:"max_value_size"` // e.g., "1MB"
}

// TTLConfig contains TTL management settings
type TTLConfig struct {
	CheckInterval    time.Duration `mapstructure:"check_interval"`    // Time wheel tick
	LazyExpiration   bool          `mapstructure:"lazy_expiration"`   // Check on access
	ActiveExpiration bool          `mapstructure:"active_expiration"` // Background cleanup
	BatchSize        int           `mapstructure:"batch_size"`        // Max keys expired per tick
}

// EvictionConfig contains eviction policy settings
type EvictionConfig struct {
	Policy     string `mapstructure:"policy"`      // lru, lfu, fifo, none
	SampleSize int    `mapstructure:"sample_size"` // Keys to sample for eviction
}

// LoggingConfig contains logging settings
type LoggingConfig struct {
	Level  string `mapstructure:"level"`  // debug, info, warn, error
	Format string `mapstructure:"format"` // json, console
}

// Defaults returns a Config with default values
func Defaults() *Config {
	return &Config{
		Node: NodeConfig{
			ID:      "",
			Name:    "",
			DataDir: "/var/lib/atlascache",
		},
		Server: ServerConfig{
			BindAddr:   "0.0.0.0",
			ClientPort: 6379,
		},
		Admin: AdminConfig{
			BindAddr: "127.0.0.1",
			Port:     8080,
		},
		Storage: StorageConfig{
			ShardCount:   0,   // Auto-detect
			MaxMemory:    "0", // Unlimited
			MaxValueSize: "1MB",
		},
		TTL: TTLConfig{
			CheckInterval:    100 * time.Millisecond,
			LazyExpiration:   true,
			ActiveExpiration: true,
			// Matches ttl.DefaultMaxHintsPerTick, the wheel's own bound on how
			// much one tick may do.
			BatchSize: 10000,
		},
		Eviction: EvictionConfig{
			Policy:     "lru",
			SampleSize: 5,
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "json",
		},
		// Both security features ship disabled (ADR-0009) so the first run
		// needs no setup. The cost is named in a startup warning, not hidden.
		TLS: TLSConfig{
			Enabled:  false,
			CertFile: "",
			KeyFile:  "",
		},
		Auth: AuthConfig{
			Enabled: false,
			Token:   "",
		},
	}
}

// GetShardCount returns the effective shard count
// If ShardCount is 0, it auto-detects based on CPU cores
func (c *Config) GetShardCount() int {
	if c.Storage.ShardCount > 0 {
		return c.Storage.ShardCount
	}

	// Auto-detect: CPU cores × 4
	count := runtime.NumCPU() * 4

	// Minimum 16, maximum 1024
	if count < 16 {
		count = 16
	}
	if count > 1024 {
		count = 1024
	}

	return count
}

// GetNodeID returns the node ID, generating one if not set
func (c *Config) GetNodeID() string {
	if c.Node.ID != "" {
		return c.Node.ID
	}
	return uuid.New().String()
}

// ClientAddr returns the host:port the client listener binds to
func (c *Config) ClientAddr() string {
	return net.JoinHostPort(c.Server.BindAddr, strconv.Itoa(c.Server.ClientPort))
}

// AdminAddr returns the host:port the admin HTTP server binds to
func (c *Config) AdminAddr() string {
	return net.JoinHostPort(c.Admin.BindAddr, strconv.Itoa(c.Admin.Port))
}

// AdminIsLoopback reports whether the admin API is bound to a loopback address
func (c *Config) AdminIsLoopback() bool {
	if ip := net.ParseIP(c.Admin.BindAddr); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(c.Admin.BindAddr, "localhost")
}
