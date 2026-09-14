package config

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
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

// Field names used in more than one message. A combination check has to name
// every field involved, since naming one of them does not say what to change.
const (
	fieldAdminPort     = "admin.port"
	fieldAdminBindAddr = "admin.bind_addr"
	fieldAdminToken    = "admin.token"
	fieldAuthToken     = "auth.token"
	fieldTTLActive     = "ttl.active_expiration"
	fieldTTLLazy       = "ttl.lazy_expiration"
)

// Validate checks whether the configuration is one the server can honor.
//
// Most checks are per-field, but not all: admin.port against server.client_port
// and the two TTL reclamation flags are both defects only in combination, and a
// per-field pass cannot see them. When adding a check, ask whether the field is
// wrong on its own or only alongside another — the second kind is the one that
// escapes review, which is how ISSUE-0017 reached a release.
func Validate(cfg *Config) error {
	var errs []*ValidationError

	// Validate server config
	if err := validateServer(&cfg.Server, &cfg.Storage); err != nil {
		errs = append(errs, err...)
	}

	// Validate admin config
	if err := validateAdmin(&cfg.Admin, &cfg.Server, &cfg.Auth); err != nil {
		errs = append(errs, err...)
	}

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

	// Validate TLS config
	if err := validateTLS(&cfg.TLS); err != nil {
		errs = append(errs, err...)
	}

	// Validate auth config
	if err := validateAuth(&cfg.Auth); err != nil {
		errs = append(errs, err...)
	}

	if len(errs) > 0 {
		return &MultiValidationError{Errors: errs}
	}

	return nil
}

func validateServer(cfg *ServerConfig, storage *StorageConfig) []*ValidationError {
	var errs []*ValidationError

	if err := validateBindAddr("server.bind_addr", cfg.BindAddr); err != nil {
		errs = append(errs, err)
	}
	if err := validatePort("server.client_port", cfg.ClientPort); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, validateConnLimits(cfg, storage)...)

	return errs
}

// Field names used in more than one connection-limit message.
const (
	fieldMaxConnections      = "server.max_connections"
	fieldClientIdleTimeout   = "server.client_idle_timeout"
	fieldMaxPipelineCommands = "server.max_pipeline_commands"
	fieldMaxOutputBuffer     = "server.max_output_buffer"
	fieldMaxRequestSize      = "server.max_request_size"
	fieldMaxValueSize        = "storage.max_value_size"
)

// tooLarge is the ceiling message shared by the counted limits. Each has a
// different floor and a different reason for it; the ceiling is the same
// sanity bound in every case.
const tooLarge = "must be <= 1000000"

// validateConnLimits checks the bounds one client is held to (FEAT-0024).
//
// Every one of these has a floor rather than merely a type, because the
// dangerous value is not a malformed one — it is a small one that looks
// deliberate. A max_connections of 0 is a server nobody can reach; a
// client_idle_timeout of 10ms disconnects clients mid-round-trip and reads as a
// network fault; a max_request_size below max_value_size makes the largest
// value the engine accepts unwritable, which surfaces as a protocol error and
// sends an operator looking at the wrong layer.
func validateConnLimits(cfg *ServerConfig, storage *StorageConfig) []*ValidationError {
	var errs []*ValidationError

	if cfg.MaxConnections < 1 {
		errs = append(errs, &ValidationError{
			Field:   fieldMaxConnections,
			Message: "must be >= 1; a server that accepts no connections serves nobody",
		})
	}
	if cfg.MaxConnections > 1_000_000 {
		errs = append(errs, &ValidationError{
			Field:   fieldMaxConnections,
			Message: tooLarge,
		})
	}

	if cfg.ClientIdleTimeout < 0 {
		errs = append(errs, &ValidationError{
			Field:   fieldClientIdleTimeout,
			Message: "must be >= 0 (0 disables the timeout)",
		})
	}
	if cfg.ClientIdleTimeout > 0 && cfg.ClientIdleTimeout < time.Second {
		errs = append(errs, &ValidationError{
			Field:   fieldClientIdleTimeout,
			Message: "must be >= 1s when set; anything shorter disconnects clients between commands",
		})
	}

	if cfg.MaxPipelineCommands < 1 {
		errs = append(errs, &ValidationError{
			Field:   fieldMaxPipelineCommands,
			Message: "must be >= 1; a batch of zero commands never flushes a reply",
		})
	}
	if cfg.MaxPipelineCommands > 1_000_000 {
		errs = append(errs, &ValidationError{
			Field:   fieldMaxPipelineCommands,
			Message: tooLarge,
		})
	}

	if outputBuffer, err := ParseSize(cfg.MaxOutputBuffer); err != nil {
		errs = append(errs, &ValidationError{
			Field:   fieldMaxOutputBuffer,
			Message: fmt.Sprintf("invalid size format: %v", err),
		})
	} else if outputBuffer > 0 && outputBuffer < minOutputBuffer {
		errs = append(errs, &ValidationError{
			Field:   fieldMaxOutputBuffer,
			Message: "must be >= 64KB when set, or 0 to leave output uncapped",
		})
	}

	return append(errs, validateRequestSize(cfg, storage)...)
}

// minOutputBuffer is the floor on a set server.max_output_buffer. One INFO
// reply is a few kilobytes and one bulk reply may be a whole value, so a cap
// below this disconnects clients running ordinary commands.
const minOutputBuffer = 64 * 1024

// validateRequestSize is the combination check: the request budget is only
// meaningful against the value size it has to carry.
//
// It is the kind of defect a per-field pass cannot see — each field is
// individually legal and the pair is not — which is the class ISSUE-0017 was.
func validateRequestSize(cfg *ServerConfig, storage *StorageConfig) []*ValidationError {
	requestSize, err := ParseSize(cfg.MaxRequestSize)
	if err != nil {
		return []*ValidationError{{
			Field:   fieldMaxRequestSize,
			Message: fmt.Sprintf("invalid size format: %v", err),
		}}
	}
	if requestSize == 0 {
		// Derived from storage.max_value_size at startup, so there is no pair
		// to disagree.
		return nil
	}

	maxValueSize, valueErr := ParseSize(storage.MaxValueSize)
	if valueErr != nil {
		// storage.max_value_size is reported by its own check; nothing here can
		// add to it.
		return nil
	}

	if requestSize < maxValueSize+protocolFraming {
		return []*ValidationError{{
			Field: fieldMaxRequestSize,
			Message: fmt.Sprintf(
				"must be at least %s -- %s plus %s of protocol framing -- or the largest value "+
					"%s allows can never be written; raise %s or lower %s",
				FormatSize(maxValueSize+protocolFraming), fieldMaxValueSize,
				FormatSize(protocolFraming), fieldMaxValueSize,
				fieldMaxRequestSize, fieldMaxValueSize),
		}}
	}
	return nil
}

// protocolFraming is the wire overhead a SET of a maximum-sized value carries
// besides the value: the command name, the key, and the length headers.
const protocolFraming = 64 * 1024

func validateAdmin(cfg *AdminConfig, server *ServerConfig, auth *AuthConfig) []*ValidationError {
	var errs []*ValidationError

	bindErr := validateBindAddr(fieldAdminBindAddr, cfg.BindAddr)
	if bindErr != nil {
		errs = append(errs, bindErr)
	}
	if err := validatePort(fieldAdminPort, cfg.Port); err != nil {
		errs = append(errs, err)
	}

	// The two listeners cannot share a port
	if cfg.Port > 0 && cfg.Port == server.ClientPort {
		errs = append(errs, &ValidationError{
			Field:   fieldAdminPort,
			Message: "must differ from server.client_port",
		})
	}

	// Only when the address parses: "is it loopback" has no answer for an
	// address that is not an address, and reporting the exposure guard on top
	// of the malformed-address error would send the operator after the wrong
	// field.
	if bindErr == nil {
		if err := validateAdminExposure(cfg); err != nil {
			errs = append(errs, err)
		}
	}
	if err := validateAdminTokenSeparation(cfg, auth); err != nil {
		errs = append(errs, err)
	}

	return errs
}

// validateAdminExposure is the guard ADR-0023 requires: binding the admin API
// to a non-loopback address without an admin token is refused at startup.
//
// P0 could not enforce it. FEAT-0010 excluded auth and there was no
// admin.token to require, so the skeleton logged a WARN naming the exposure and
// carried on — which is how a warning that was always meant to be an error
// stays a warning. FEAT-0030 adds the token, so the guard becomes real.
//
// The message spends its length on the two ways out rather than on the refusal.
// A bare "invalid config: admin.bind_addr" against a value the operator typed
// on purpose reads as a bug in the server, and the operator's next move is to
// look for a way around it rather than to pick one of the two fixes.
func validateAdminExposure(cfg *AdminConfig) *ValidationError {
	if cfg.IsLoopback() || cfg.Token.IsSet() {
		return nil
	}

	return &ValidationError{
		Field: fieldAdminBindAddr,
		Message: "is " + cfg.BindAddr + ", which is reachable from other hosts, while " + fieldAdminToken +
			" is not set: the admin API would serve statistics and the effective configuration " +
			"to anyone who can reach that address, with no credential (ADR-0023). " +
			"Set " + fieldAdminToken + " to require one, or set " + fieldAdminBindAddr +
			" to 127.0.0.1 so the API is reachable only from this host.",
	}
}

// validateAdminTokenSeparation keeps the two credentials apart.
//
// ADR-0023 asks for a separate admin.token so that a client holding a data
// token does not thereby gain administrative access. Setting both to the same
// string satisfies the schema and defeats the decision: every data client would
// hold a working admin credential, and rotating one would silently rotate the
// other. It is refused rather than warned about, because the configuration has
// no use that setting them to different values does not serve better.
func validateAdminTokenSeparation(cfg *AdminConfig, auth *AuthConfig) *ValidationError {
	if !cfg.Token.IsSet() || cfg.Token != auth.Token {
		return nil
	}

	return &ValidationError{
		Field: fieldAdminToken,
		Message: "must not be the same value as " + fieldAuthToken + ": the admin API is more powerful " +
			"than the data port, and sharing the secret gives every data client administrative access " +
			"and makes either token impossible to rotate on its own (ADR-0023). " +
			"Give " + fieldAdminToken + " a value of its own.",
	}
}

func validateBindAddr(field, addr string) *ValidationError {
	if strings.TrimSpace(addr) == "" {
		return &ValidationError{
			Field:   field,
			Message: "must not be empty (use \"0.0.0.0\" for all interfaces)",
		}
	}
	if net.ParseIP(addr) != nil || hostnameRegex.MatchString(addr) {
		return nil
	}
	return &ValidationError{
		Field:   field,
		Message: "must be a valid IP address or hostname",
	}
}

func validatePort(field string, port int) *ValidationError {
	if port < 1 || port > 65535 {
		return &ValidationError{
			Field:   field,
			Message: "must be between 1 and 65535",
		}
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

	// The batch bounds how much work one tick may do. A tick that expired
	// everything it found could stall the wheel behind a mass expiry, and a
	// batch of zero would never reclaim anything.
	if cfg.BatchSize < 1 {
		errs = append(errs, &ValidationError{
			Field:   "ttl.batch_size",
			Message: "must be >= 1",
		})
	}
	if cfg.BatchSize > 1_000_000 {
		errs = append(errs, &ValidationError{
			Field:   "ttl.batch_size",
			Message: "must be <= 1000000",
		})
	}

	// The combination check. Each flag is individually legitimate — active-only
	// and lazy-only are both supported and both tested — but with both off
	// nothing reclaims an expired key: the wheel never runs and the read path
	// never deletes. That is ISSUE-0007, the unbounded leak P1 was built to
	// fix, reachable again through two flags that each look harmless
	// (ISSUE-0017). The configuration has no valid use, so refusing it costs
	// nothing and a warning would leave the leak reachable.
	if !cfg.ActiveExpiration && !cfg.LazyExpiration {
		errs = append(errs, &ValidationError{
			Field: fieldTTLActive,
			Message: "must not be false while " + fieldTTLLazy + " is also false: " +
				"with both disabled nothing reclaims an expired key and memory grows without bound; " +
				"leave " + fieldTTLActive + " or " + fieldTTLLazy + " enabled",
		})
	}

	return errs
}

// validateTLS checks the client-facing TLS settings.
//
// Only the shape is checked here: whether the files exist and hold a usable
// pair is settled at startup, by loading them, so the message can name the file
// and the exact failure. Validation stays free of I/O, which matters because
// hot-reload runs it on every config change.
func validateTLS(cfg *TLSConfig) []*ValidationError {
	if !cfg.Enabled {
		// The paths are allowed to be set while TLS is off, so turning it on is
		// one flag rather than three.
		return nil
	}

	var errs []*ValidationError
	if strings.TrimSpace(cfg.CertFile) == "" {
		errs = append(errs, &ValidationError{
			Field:   "tls.cert_file",
			Message: "must be set when tls.enabled is true",
		})
	}
	if strings.TrimSpace(cfg.KeyFile) == "" {
		errs = append(errs, &ValidationError{
			Field:   "tls.key_file",
			Message: "must be set when tls.enabled is true",
		})
	}
	return errs
}

// validateAuth checks the authentication settings.
//
// An enabled auth with no token would accept `AUTH ""` from anyone, which is
// indistinguishable from no auth at all while looking like protection.
func validateAuth(cfg *AuthConfig) []*ValidationError {
	if cfg.Enabled && cfg.Token == "" {
		return []*ValidationError{{
			Field:   fieldAuthToken,
			Message: "must be set when auth.enabled is true",
		}}
	}
	return nil
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

// hostnameRegex matches DNS hostnames like "localhost" or "cache.internal"
var hostnameRegex = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*$`)

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
