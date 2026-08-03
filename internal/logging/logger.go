// Package logging provides structured logging using zerolog.
package logging

import (
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

var (
	// Logger is the global logger instance
	Logger zerolog.Logger

	// mu protects logger reconfiguration
	mu sync.RWMutex
)

// Config holds logging configuration
type Config struct {
	Level  string
	Format string
}

func init() {
	// Initialize with default settings
	Configure(Config{
		Level:  "info",
		Format: "json",
	})
}

// Configure sets up the global logger with the given configuration
func Configure(cfg Config) {
	mu.Lock()
	defer mu.Unlock()

	var output io.Writer = os.Stdout

	// Configure output format
	if strings.ToLower(cfg.Format) == "console" {
		output = zerolog.ConsoleWriter{
			Out:        os.Stdout,
			TimeFormat: time.RFC3339,
		}
	}

	// Parse log level
	level := parseLevel(cfg.Level)

	// Create logger
	Logger = zerolog.New(output).
		Level(level).
		With().
		Timestamp().
		Caller().
		Logger()
}

// parseLevel converts a string level to zerolog.Level
func parseLevel(level string) zerolog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return zerolog.DebugLevel
	case "info":
		return zerolog.InfoLevel
	case "warn", "warning":
		return zerolog.WarnLevel
	case "error":
		return zerolog.ErrorLevel
	case "fatal":
		return zerolog.FatalLevel
	case "panic":
		return zerolog.PanicLevel
	case "disabled", "off":
		return zerolog.Disabled
	default:
		return zerolog.InfoLevel
	}
}

// SetLevel changes the log level at runtime
func SetLevel(level string) {
	mu.Lock()
	defer mu.Unlock()
	Logger = Logger.Level(parseLevel(level))
}

// WithComponent returns a logger with a component field
func WithComponent(component string) zerolog.Logger {
	mu.RLock()
	defer mu.RUnlock()
	return Logger.With().Str("component", component).Logger()
}

// WithNodeID returns a logger with a node_id field
func WithNodeID(nodeID string) zerolog.Logger {
	mu.RLock()
	defer mu.RUnlock()
	return Logger.With().Str("node_id", nodeID).Logger()
}
