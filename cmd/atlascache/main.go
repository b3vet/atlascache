// Command atlascache runs the AtlasCache server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/b3vet/atlascache/internal/admin"
	"github.com/b3vet/atlascache/internal/config"
	"github.com/b3vet/atlascache/internal/eviction"
	"github.com/b3vet/atlascache/internal/logging"
	"github.com/b3vet/atlascache/internal/server"
	"github.com/b3vet/atlascache/internal/storage"
	"github.com/b3vet/atlascache/internal/ttl"
)

// Build metadata, injected via -ldflags by the Makefile
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// shutdownTimeout bounds the drain; FEAT-0010 requires exit under 10s
const shutdownTimeout = 5 * time.Second

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "", "path to the configuration file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("atlascache %s\ncommit: %s\nbuilt:  %s\n", version, commit, date)
		return 0
	}

	cfg, configFile, err := loadConfig(*configPath)
	if err != nil {
		// Logging is not configured yet, and a config failure must be legible
		fmt.Fprintf(os.Stderr, "atlascache: %v\n", err)
		return 1
	}

	logging.Configure(logging.Config{Level: cfg.Logging.Level, Format: cfg.Logging.Format})
	log := logging.WithComponent("atlascache")

	// HELLO and INFO report the version, and the build stamp lives here rather
	// than in internal/server — which is why the two have to be joined
	// somewhere, and the composition root is that somewhere (FEAT-0021). Before
	// this, a released binary told every monitoring tool it was 0.1.0-dev.
	server.Version = version

	// Established before the listeners bind so a signal arriving during startup
	// is respected rather than racing the handler installed further down.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Before anything binds or starts: a TLS configuration that cannot be
	// honored must stop the process, not degrade it to plaintext (FEAT-0023).
	sec, err := newSecurity(cfg, logging.WithComponent("security"))
	if err != nil {
		log.Error().Err(err).Msg("failed to load the TLS certificate")
		return 1
	}

	cache, err := newCore(cfg)
	if err != nil {
		log.Error().Err(err).Msg("failed to build the storage engine")
		return 1
	}
	defer cache.close()

	// Process memory is read on a timer and served from the last reading, so
	// that INFO — which dashboards poll by the second — never triggers the
	// stop-the-world that runtime.ReadMemStats is (ISSUE-0015).
	procmem := storage.NewProcessMemorySampler(0)
	procmem.Start()
	defer procmem.Stop()

	limits, err := connLimits(cfg)
	if err != nil {
		log.Error().Err(err).Msg("failed to read the connection limits")
		return 1
	}

	// The engine reaches the server through the keyspace seam, so the transport
	// layer holds no storage types (FEAT-0017).
	store := keyspace{engine: cache.engine, procmem: procmem}
	srv, err := server.New(ctx, cfg.ClientAddr(), logging.WithComponent("server"), store,
		append(sec.options(), server.WithConnLimits(limits))...)
	if err != nil {
		log.Error().Err(err).Msg("failed to bind client port")
		return 1
	}

	adm, err := admin.New(ctx, cfg.AdminAddr(), logging.WithComponent("admin"))
	if err != nil {
		log.Error().Err(err).Msg("failed to bind admin port")
		if shutdownErr := srv.Shutdown(context.Background()); shutdownErr != nil {
			log.Debug().Err(shutdownErr).Msg("client listener shutdown failed")
		}
		return 1
	}

	logBanner(log, cfg, configFile, srv.Addr(), adm.Addr())

	cache.start(logging.WithComponent("ttl"))

	// Hot-reload is best-effort: a node that cannot watch its config file still
	// serves, it just needs a restart to pick up a policy change.
	watcher := watchConfig(configFile, cache, sec, log)
	if watcher != nil {
		defer func() {
			if err := watcher.Stop(); err != nil {
				log.Debug().Err(err).Msg("config watcher shutdown failed")
			}
		}()
	}

	serveErr := make(chan error, 2)
	go func() { serveErr <- srv.Serve() }()
	go func() { serveErr <- adm.Serve() }()

	adm.SetReady(true)
	log.Info().Msg("atlascache ready")

	exitCode := 0
	select {
	case <-ctx.Done():
		log.Info().Msg("shutdown signal received")
	case err := <-serveErr:
		if err != nil {
			log.Error().Err(err).Msg("server stopped unexpectedly")
			exitCode = 1
		}
	}

	stop()
	adm.SetReady(false)

	if err := shutdown(srv, adm, cache); err != nil {
		log.Error().Err(err).Msg("graceful shutdown incomplete")
		exitCode = 1
	}

	log.Info().Msg("atlascache stopped")
	return exitCode
}

// connLimits turns the server section of the configuration into the bounds the
// connection layer enforces (FEAT-0024).
//
// It lives here rather than in internal/server for the same reason the keyspace
// seam does: the server package names what it needs and the composition root
// fills it in, so nothing under internal/server imports the configuration
// package and nothing in the configuration package knows how a connection is
// served.
func connLimits(cfg *config.Config) (server.ConnLimits, error) {
	requestBudget, err := cfg.RequestBudget()
	if err != nil {
		return server.ConnLimits{}, fmt.Errorf("server.max_request_size: %w", err)
	}
	outputBuffer, err := config.ParseSize(cfg.Server.MaxOutputBuffer)
	if err != nil {
		return server.ConnLimits{}, fmt.Errorf("server.max_output_buffer: %w", err)
	}

	return server.ConnLimits{
		MaxConnections:      cfg.Server.MaxConnections,
		IdleTimeout:         cfg.Server.ClientIdleTimeout,
		MaxRequestBytes:     clampToInt(requestBudget),
		MaxPipelineCommands: cfg.Server.MaxPipelineCommands,
		MaxOutputBytes:      clampToInt(outputBuffer),
	}, nil
}

// clampToInt narrows a configured size to the int the limits are expressed in.
// A size past the int range is not a limit anybody meant; it becomes the
// largest one that can be enforced rather than wrapping into a small one, which
// is the failure mode that turns a generous setting into a strict one.
func clampToInt(size uint64) int {
	if size > math.MaxInt {
		return math.MaxInt
	}
	return int(size)
}

// core is the cache itself: the storage engine, the eviction policy it makes
// room with, and the TTL manager that reclaims what expires. It is assembled
// here rather than inside the engine so the dependencies point one way —
// eviction and ttl both know about storage, and storage knows about neither.
type core struct {
	engine   *storage.ShardedEngine
	eviction eviction.Controller
	ttl      ttl.Manager

	// active records whether the TTL manager should run. With
	// ttl.active_expiration off it is still built and still wired in, so
	// hints are accepted and nothing else changes; it simply never ticks.
	active bool
	log    zerolog.Logger
}

// newCore builds the engine and everything hanging off it.
func newCore(cfg *config.Config) (*core, error) {
	maxMemory, err := config.ParseSize(cfg.Storage.MaxMemory)
	if err != nil {
		return nil, fmt.Errorf("storage.max_memory: %w", err)
	}
	maxValueSize, err := config.ParseSize(cfg.Storage.MaxValueSize)
	if err != nil {
		return nil, fmt.Errorf("storage.max_value_size: %w", err)
	}

	controller, err := eviction.New(eviction.Config{
		Policy:     cfg.Eviction.Policy,
		SampleSize: cfg.Eviction.SampleSize,
	})
	if err != nil {
		return nil, err
	}

	engine := storage.NewShardedEngine(storage.EngineConfig{
		ShardCount:            cfg.GetShardCount(),
		MaxMemory:             maxMemory,
		MaxValueSize:          maxValueSize,
		DisableLazyExpiration: !cfg.TTL.LazyExpiration,
	})
	engine.SetEvictionController(controller)

	// The engine is both the manager's keyspace, which it validates hints
	// against, and the source of the hints themselves.
	manager := ttl.New(ttl.Config{
		Tick:            cfg.TTL.CheckInterval,
		MaxHintsPerTick: cfg.TTL.BatchSize,
	}, engine)
	engine.SetExpiryScheduler(manager)

	return &core{
		engine:   engine,
		eviction: controller,
		ttl:      manager,
		active:   cfg.TTL.ActiveExpiration,
		log:      zerolog.Nop(),
	}, nil
}

// start launches active expiration, unless configuration turned it off. There
// is deliberately no pause control on the manager, so "off" means never
// started (FEAT-0012).
func (c *core) start(log zerolog.Logger) {
	c.log = log

	if !c.active {
		log.Info().Msg("active expiration disabled; expired keys are reclaimed on access only")
		return
	}

	if err := c.ttl.Start(); err != nil {
		log.Error().Err(err).Msg("ttl manager failed to start")
		return
	}

	log.Info().Msg("ttl manager started")
}

// stop halts active expiration and reports what it did, which is the only view
// of the wheel from outside the process until the admin API grows one.
func (c *core) stop(ctx context.Context) error {
	err := c.ttl.Stop(ctx)

	stats := c.ttl.Stats()
	c.log.Debug().
		Uint64("ticks", stats.Ticks).
		Uint64("added", stats.Added).
		Uint64("fired", stats.Fired).
		Uint64("expired", stats.Expired).
		Uint64("dropped", stats.Dropped).
		Uint64("pending", stats.Pending).
		Dur("max_lag", stats.MaxLag).
		Msg("ttl manager stopped")

	return err
}

// close releases the keyspace. Closing an already-closed engine is the normal
// path when shutdown ran first, and is not an error worth reporting.
func (c *core) close() {
	_ = c.engine.Close()
}

// applyConfig takes what a reload can change without a restart. The eviction
// policy and the memory ceiling are both read on every write, so switching them
// takes effect on the next one; the shard count and the wheel's tick are not,
// and are ignored here rather than half-applied.
func (c *core) applyConfig(cfg *config.Config, log zerolog.Logger) {
	if err := c.eviction.SetPolicy(cfg.Eviction.Policy); err != nil {
		log.Error().Err(err).Str("policy", cfg.Eviction.Policy).Msg("eviction policy not applied")
	} else {
		log.Info().Str("policy", c.eviction.Policy()).Msg("eviction policy applied")
	}

	maxMemory, err := config.ParseSize(cfg.Storage.MaxMemory)
	if err != nil {
		log.Error().Err(err).Msg("max_memory not applied")
		return
	}
	c.engine.SetMaxMemory(maxMemory)
}

// watchConfig starts the config watcher, returning nil when there is nothing to
// watch or the watch could not be established. Neither is fatal: hot-reload is
// a convenience, and the node runs the configuration it started with.
func watchConfig(path string, c *core, sec *security, log zerolog.Logger) *config.Watcher {
	if path == "" {
		return nil
	}

	watcher, err := config.NewWatcher(config.NewLoader(), path)
	if err != nil {
		log.Warn().Err(err).Msg("config hot-reload unavailable")
		return nil
	}

	watcher.OnChange(func(cfg *config.Config) {
		c.applyConfig(cfg, log)
		if sec != nil {
			sec.applyConfig(cfg, log)
		}
	})

	// The certificate files are watched separately from the config file: they
	// change on their own schedule — every 90 days with Let's Encrypt, more
	// often elsewhere — and nothing in the config file changes when they do.
	if sec != nil {
		sec.watchCertificates(watcher, log)
	}

	if err := watcher.Start(); err != nil {
		log.Warn().Err(err).Msg("config hot-reload unavailable")
		if stopErr := watcher.Stop(); stopErr != nil {
			log.Debug().Err(stopErr).Msg("config watcher shutdown failed")
		}
		return nil
	}

	return watcher
}

// loadConfig loads and validates configuration, reporting the file used
func loadConfig(path string) (*config.Config, string, error) {
	loader := config.NewLoader()

	if path == "" {
		cfg, err := loader.Load()
		if err != nil {
			return nil, "", err
		}
		return cfg, loader.GetPath(), nil
	}

	// An explicitly requested file that does not exist is an error, not a
	// silent fallback to defaults
	if _, err := os.Stat(path); err != nil {
		return nil, "", fmt.Errorf("config file %s: %w", path, err)
	}

	cfg, err := loader.LoadFromFile(path)
	if err != nil {
		return nil, "", err
	}
	return cfg, path, nil
}

func shutdown(srv *server.Server, adm *admin.Server, c *core) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	admErr := adm.Shutdown(ctx)
	srvErr := srv.Shutdown(ctx)

	// Expiry stops after the listeners drain, so an in-flight command never
	// finds the keyspace half-managed.
	ttlErr := c.stop(ctx)

	return errors.Join(admErr, srvErr, ttlErr)
}

func logBanner(log zerolog.Logger, cfg *config.Config, configFile, clientAddr, adminAddr string) {
	if configFile == "" {
		configFile = "(defaults)"
	}

	log.Info().
		Str("version", version).
		Str("commit", commit).
		Str("built", date).
		Str("node_id", cfg.GetNodeID()).
		Str("config_file", configFile).
		Str("client_addr", clientAddr).
		Str("admin_addr", adminAddr).
		Int("shard_count", cfg.GetShardCount()).
		Str("max_memory", cfg.Storage.MaxMemory).
		Str("max_value_size", cfg.Storage.MaxValueSize).
		Str("eviction_policy", cfg.Eviction.Policy).
		Int("eviction_sample_size", cfg.Eviction.SampleSize).
		Dur("ttl_check_interval", cfg.TTL.CheckInterval).
		Int("ttl_batch_size", cfg.TTL.BatchSize).
		Bool("ttl_active_expiration", cfg.TTL.ActiveExpiration).
		Bool("ttl_lazy_expiration", cfg.TTL.LazyExpiration).
		Int("max_connections", cfg.Server.MaxConnections).
		Dur("client_idle_timeout", cfg.Server.ClientIdleTimeout).
		Str("max_request_size", cfg.Server.MaxRequestSize).
		Int("max_pipeline_commands", cfg.Server.MaxPipelineCommands).
		Str("max_output_buffer", cfg.Server.MaxOutputBuffer).
		Bool("tls_enabled", cfg.TLS.Enabled).
		Bool("auth_enabled", cfg.Auth.Enabled).
		Str("log_level", cfg.Logging.Level).
		Msg("atlascache starting")

	logExposure(log, cfg)

	if !cfg.AdminIsLoopback() {
		log.Warn().
			Str("admin_addr", adminAddr).
			Msg("admin API is not bound to loopback — it is unauthenticated and reachable from other hosts")
	}
}

// logExposure names what is exposed by the defaults, which ADR-0009 requires
// and P0 could not do because neither config section existed yet.
//
// The wording names the consequence rather than the setting. "tls.enabled is
// false" tells an operator what they already typed; "traffic is unencrypted"
// tells them what it costs, and that is the difference between a warning that
// is acted on and one that is scrolled past.
func logExposure(log zerolog.Logger, cfg *config.Config) {
	if !cfg.TLS.Enabled {
		log.Warn().Msg("TLS disabled — traffic is unencrypted. Do not use in production.")
	}
	if !cfg.Auth.Enabled {
		log.Warn().Msg("auth disabled — any client that can reach this port has full access.")
	}
}
