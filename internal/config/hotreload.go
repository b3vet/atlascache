package config

import (
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/fsnotify/fsnotify"
	"github.com/rs/zerolog"

	"github.com/b3vet/atlascache/internal/logging"
)

// Watcher handles configuration hot-reload
type Watcher struct {
	loader     *Loader
	path       string
	watcher    *fsnotify.Watcher
	callbacks  []func(*Config)
	mu         sync.RWMutex
	done       chan struct{}
	currentCfg *Config
	log        zerolog.Logger
}

// NewWatcher creates a new configuration watcher
func NewWatcher(loader *Loader, path string) (*Watcher, error) {
	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	// Load initial config
	cfg, err := loader.LoadFromFile(path)
	if err != nil {
		fsWatcher.Close()
		return nil, err
	}

	w := &Watcher{
		loader:     loader,
		path:       path,
		watcher:    fsWatcher,
		callbacks:  make([]func(*Config), 0),
		done:       make(chan struct{}),
		currentCfg: cfg,
		log:        logging.WithComponent("config-watcher"),
	}

	return w, nil
}

// Start begins watching for configuration changes
func (w *Watcher) Start() error {
	// Watch the config file
	if err := w.watcher.Add(w.path); err != nil {
		return err
	}

	// Start the file watcher goroutine
	go w.watchFile()

	// Start the signal handler goroutine
	go w.watchSignals()

	w.log.Info().Str("path", w.path).Msg("started config watcher")
	return nil
}

// watchFile handles file system events
func (w *Watcher) watchFile() {
	for {
		select {
		case <-w.done:
			return
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}

			// Handle write and create events
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				w.log.Info().Str("event", event.Op.String()).Msg("config file changed")
				w.reload()
			}

		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			w.log.Error().Err(err).Msg("config watcher error")
		}
	}
}

// watchSignals handles SIGHUP for manual reload
func (w *Watcher) watchSignals() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP)

	for {
		select {
		case <-w.done:
			signal.Stop(sigCh)
			return
		case <-sigCh:
			w.log.Info().Msg("received SIGHUP, reloading config")
			w.reload()
		}
	}
}

// reload attempts to reload the configuration
func (w *Watcher) reload() {
	cfg, err := w.loader.LoadFromFile(w.path)
	if err != nil {
		w.log.Error().Err(err).Msg("failed to reload config")
		return
	}

	w.mu.Lock()
	w.currentCfg = cfg
	callbacks := make([]func(*Config), len(w.callbacks))
	copy(callbacks, w.callbacks)
	w.mu.Unlock()

	// Notify all callbacks
	for _, cb := range callbacks {
		go cb(cfg)
	}

	w.log.Info().Msg("config reloaded successfully")
}

// OnChange registers a callback for configuration changes
func (w *Watcher) OnChange(callback func(*Config)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.callbacks = append(w.callbacks, callback)
}

// Config returns the current configuration
func (w *Watcher) Config() *Config {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.currentCfg
}

// Stop stops the watcher
func (w *Watcher) Stop() error {
	close(w.done)
	return w.watcher.Close()
}
