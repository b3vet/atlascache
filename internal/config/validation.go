package config

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ValidationError represents a configuration validation error
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid config: %s - %s", e.Field, e.Message)
}

// MultiValidationError holds multiple validation errors
type MultiValidationError struct {
	Errors []*ValidationError
}

func (e *MultiValidationError) Error() string {
	if len(e.Errors) == 1 {
		return e.Errors[0].Error()
	}

	var msgs []string
	for _, err := range e.Errors {
		msgs = append(msgs, err.Error())
	}
	return fmt.Sprintf("multiple config errors:\n  - %s", strings.Join(msgs, "\n  - "))
}

// Validate checks if the configuration is valid
func Validate(cfg *Config) error {
	var errs []*ValidationError

	// Validate storage config
	if err := validateStorage(&cfg.Storage); err != nil {
		errs = append(errs, err...)
	}

	// Validate TTL config
	if err := validateTTL(&cfg.TTL); err != nil {
		errs = append(errs, err...)
	}

	// Validate eviction config
	if err := validateEviction(&cfg.Eviction); err != nil {
		errs = append(errs, err...)
	}

	// Validate logging config
	if err := validateLogging(&cfg.Logging); err != nil {
		errs = append(errs, err...)
	}

	if len(errs) > 0 {
		return &MultiValidationError{Errors: errs}
	}

	return nil
}

func validateStorage(cfg *StorageConfig) []*ValidationError {
	var errs []*ValidationError

	// Validate shard count
	if cfg.ShardCount < 0 {
		errs = append(errs, &ValidationError{
			Field:   "storage.shard_count",
			Message: "must be >= 0 (0 = auto-detect)",
		})
	}
	if cfg.ShardCount > 4096 {
		errs = append(errs, &ValidationError{
			Field:   "storage.shard_count",
			Message: "must be <= 4096",
		})
	}

	// Validate max_memory
	if _, err := ParseSize(cfg.MaxMemory); err != nil {
		errs = append(errs, &ValidationError{
			Field:   "storage.max_memory",
			Message: fmt.Sprintf("invalid size format: %v", err),
		})
	}

	// Validate max_value_size
	maxValSize, err := ParseSize(cfg.MaxValueSize)
	if err != nil {
		errs = append(errs, &ValidationError{
			Field:   "storage.max_value_size",
			Message: fmt.Sprintf("invalid size format: %v", err),
		})
	} else if maxValSize > 16*1024*1024 { // 16MB max
		errs = append(errs, &ValidationError{
			Field:   "storage.max_value_size",
			Message: "must be <= 16MB",
		})
	}

	return errs
}

func validateTTL(cfg *TTLConfig) []*ValidationError {
	var errs []*ValidationError

	if cfg.CheckInterval < 0 {
		errs = append(errs, &ValidationError{
			Field:   "ttl.check_interval",
			Message: "must be >= 0",
		})
	}
	if cfg.CheckInterval > 0 && cfg.CheckInterval < 10*1e6 { // 10ms minimum
		errs = append(errs, &ValidationError{
			Field:   "ttl.check_interval",
			Message: "must be >= 10ms for practical use",
		})
	}

	return errs
}

func validateEviction(cfg *EvictionConfig) []*ValidationError {
	var errs []*ValidationError

	// Validate policy
	validPolicies := map[string]bool{
		"lru":  true,
		"lfu":  true,
		"fifo": true,
		"none": true,
	}
	if !validPolicies[strings.ToLower(cfg.Policy)] {
		errs = append(errs, &ValidationError{
			Field:   "eviction.policy",
			Message: "must be one of: lru, lfu, fifo, none",
		})
	}

	// Validate sample size
	if cfg.SampleSize < 1 {
		errs = append(errs, &ValidationError{
			Field:   "eviction.sample_size",
			Message: "must be >= 1",
		})
	}
	if cfg.SampleSize > 100 {
		errs = append(errs, &ValidationError{
			Field:   "eviction.sample_size",
			Message: "must be <= 100",
		})
	}

	return errs
}

func validateLogging(cfg *LoggingConfig) []*ValidationError {
	var errs []*ValidationError

	// Validate level
	validLevels := map[string]bool{
		"debug":    true,
		"info":     true,
		"warn":     true,
		"warning":  true,
		"error":    true,
		"fatal":    true,
		"panic":    true,
		"disabled": true,
		"off":      true,
	}
	if !validLevels[strings.ToLower(cfg.Level)] {
		errs = append(errs, &ValidationError{
			Field:   "logging.level",
			Message: "must be one of: debug, info, warn, error, fatal, panic, disabled",
		})
	}

	// Validate format
	validFormats := map[string]bool{
		"json":    true,
		"console": true,
	}
	if !validFormats[strings.ToLower(cfg.Format)] {
		errs = append(errs, &ValidationError{
			Field:   "logging.format",
			Message: "must be one of: json, console",
		})
	}

	return errs
}

// sizeRegex matches size strings like "1GB", "512MB", "100KB", "1024", "0"
var sizeRegex = regexp.MustCompile(`^(\d+)\s*(B|KB|MB|GB|TB)?$`)

// ParseSize parses a size string (e.g., "1GB", "512MB") into bytes
func ParseSize(s string) (uint64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))

	if s == "" || s == "0" {
		return 0, nil
	}

	matches := sizeRegex.FindStringSubmatch(s)
	if matches == nil {
		return 0, errors.New("invalid size format, expected format like '1GB', '512MB', or '0'")
	}

	value, err := strconv.ParseUint(matches[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number: %w", err)
	}

	unit := matches[2]
	if unit == "" {
		unit = "B"
	}

	multipliers := map[string]uint64{
		"B":  1,
		"KB": 1024,
		"MB": 1024 * 1024,
		"GB": 1024 * 1024 * 1024,
		"TB": 1024 * 1024 * 1024 * 1024,
	}

	multiplier, ok := multipliers[unit]
	if !ok {
		return 0, fmt.Errorf("unknown unit: %s", unit)
	}

	return value * multiplier, nil
}

// FormatSize formats bytes into a human-readable size string
func FormatSize(bytes uint64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%dB", bytes)
	}
	div, exp := uint64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
