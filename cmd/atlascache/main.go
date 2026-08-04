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
	"github.com/b3vet/atlascache/internal/logging"
	"github.com/b3vet/atlascache/internal/server"
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

	srv, err := server.New(ctx, cfg.ClientAddr(), logging.WithComponent("server"))
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

	if err := shutdown(srv, adm); err != nil {
		log.Error().Err(err).Msg("graceful shutdown incomplete")
		exitCode = 1
	}

	log.Info().Msg("atlascache stopped")
	return exitCode
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

func shutdown(srv *server.Server, adm *admin.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	admErr := adm.Shutdown(ctx)
	srvErr := srv.Shutdown(ctx)

	return errors.Join(admErr, srvErr)
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
		Dur("ttl_check_interval", cfg.TTL.CheckInterval).
		Str("log_level", cfg.Logging.Level).
		Msg("atlascache starting")

	if !cfg.AdminIsLoopback() {
		log.Warn().
			Str("admin_addr", adminAddr).
			Msg("admin API is not bound to loopback — it is unauthenticated and reachable from other hosts")
	}
}
