// Command atlascache runs the AtlasCache server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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

	// Established before the listeners bind so a signal arriving during startup
	// is respected rather than racing the handler installed further down.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	cache, err := newCore(cfg)
	if err != nil {
		log.Error().Err(err).Msg("failed to build the storage engine")
		return 1
	}
	defer cache.close()

	// The engine reaches the server through the keyspace seam, so the transport
	// layer holds no storage types (FEAT-0017).
	srv, err := server.New(ctx, cfg.ClientAddr(), logging.WithComponent("server"), keyspace{engine: cache.engine})
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
	watcher := watchConfig(configFile, cache, log)
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
func watchConfig(path string, c *core, log zerolog.Logger) *config.Watcher {
	if path == "" {
		return nil
	}

	watcher, err := config.NewWatcher(config.NewLoader(), path)
	if err != nil {
		log.Warn().Err(err).Msg("config hot-reload unavailable")
		return nil
	}

	watcher.OnChange(func(cfg *config.Config) { c.applyConfig(cfg, log) })

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
		Str("log_level", cfg.Logging.Level).
		Msg("atlascache starting")

	if !cfg.AdminIsLoopback() {
		log.Warn().
			Str("admin_addr", adminAddr).
			Msg("admin API is not bound to loopback — it is unauthenticated and reachable from other hosts")
	}
}
