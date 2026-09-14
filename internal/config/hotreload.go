package config

import (
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/rs/zerolog"

	"github.com/b3vet/atlascache/internal/logging"
)

// fileSettleDelay coalesces the burst of events one logical file change
// produces. A certificate rotation writes two files, and some tools write,
// truncate and rename each of them, so a reload fired per event would attempt
// several reloads of a half-written pair and log a failure for each. Waiting
// for the events to stop first makes the common rotation quiet, and costs a
// delay far below the human timescale a rotation happens on.
const fileSettleDelay = 100 * time.Millisecond

// Watcher handles configuration hot-reload
type Watcher struct {
	loader     *Loader
	path       string
	watcher    *fsnotify.Watcher
	callbacks  []func(*Config)
	files      []*fileWatch
	mu         sync.RWMutex
	done       chan struct{}
	stopOnce   sync.Once
	currentCfg *Config
	log        zerolog.Logger
}

// fileWatch is one file outside the config that the watcher reports changes
// for — the TLS certificate and key, today. Its callback decides what a change
// means; the watcher only says that one happened.
type fileWatch struct {
	path  string
	name  string
	onChg func()

	// timer coalesces a burst of events into one callback. It is nil until the
	// first event arrives.
	timer *time.Timer
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

// WatchFile reports changes to a file that is not the config file, calling
// onChange once the writes to it have settled. It may be called before or after
// Start.
//
// The watch goes on the file's *directory*, not on the file. A certificate
// renewal replaces the file — written alongside and renamed over, or a symlink
// repointed — and a watch placed on the inode dies with the file it was placed
// on, which would make hot-reload work in testing and silently stop working the
// first time a real renewal ran.
func (w *Watcher) WatchFile(path string, onChange func()) error {
	if path == "" {
		return errors.New("config: a file watch needs a path")
	}
	if onChange == nil {
		return errors.New("config: a file watch needs a callback")
	}

	clean := filepath.Clean(path)
	dir := filepath.Dir(clean)
	if err := w.watcher.Add(dir); err != nil {
		return err
	}

	w.mu.Lock()
	w.files = append(w.files, &fileWatch{path: clean, name: filepath.Base(clean), onChg: onChange})
	w.mu.Unlock()

	w.log.Info().Str("path", clean).Str("watching", dir).Msg("watching a file for changes")
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
			w.handleEvent(event)

		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			w.log.Error().Err(err).Msg("config watcher error")
		}
	}
}

// handleEvent routes one filesystem event to whatever registered interest in
// that path.
//
// Routing is by path rather than by "something changed": once a directory is
// watched for a certificate, events arrive for every file in it, and reloading
// the config because a neighboring file was written would apply a policy
// change nobody made.
func (w *Watcher) handleEvent(event fsnotify.Event) {
	if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) && !event.Has(fsnotify.Rename) {
		return
	}

	changed := filepath.Clean(event.Name)

	if changed == filepath.Clean(w.path) && !event.Has(fsnotify.Rename) {
		w.log.Info().Str("event", event.Op.String()).Msg("config file changed")
		w.reload()
		return
	}

	for _, watch := range w.matching(changed) {
		w.log.Info().Str("path", watch.path).Str("event", event.Op.String()).Msg("watched file changed")
		w.schedule(watch)
	}
}

// matching returns the file watches interested in a changed path.
func (w *Watcher) matching(changed string) []*fileWatch {
	w.mu.RLock()
	defer w.mu.RUnlock()

	var found []*fileWatch
	for _, watch := range w.files {
		// The base name is compared as well as the full path because a
		// directory watch reports some events by the name they were created
		// under, and a symlink swap reports the link rather than its target.
		if watch.path == changed || watch.name == filepath.Base(changed) {
			found = append(found, watch)
		}
	}
	return found
}

// schedule fires a watch's callback once its events have settled.
func (w *Watcher) schedule(watch *fileWatch) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if watch.timer != nil {
		watch.timer.Reset(fileSettleDelay)
		return
	}
	watch.timer = time.AfterFunc(fileSettleDelay, watch.onChg)
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
			w.reloadFiles()
		}
	}
}

// reloadFiles fires every file watch, which is what SIGHUP means: reload
// everything, not merely the config file. An operator who has just replaced a
// certificate and sent SIGHUP is entitled to expect the new one to be served.
func (w *Watcher) reloadFiles() {
	w.mu.RLock()
	watches := make([]*fileWatch, len(w.files))
	copy(watches, w.files)
	w.mu.RUnlock()

	for _, watch := range watches {
		watch.onChg()
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

// Stop stops the watcher. It is safe to call more than once, which matters
// because a failed Start is cleaned up by calling it.
func (w *Watcher) Stop() error {
	var err error
	w.stopOnce.Do(func() {
		close(w.done)

		w.mu.Lock()
		for _, watch := range w.files {
			if watch.timer != nil {
				watch.timer.Stop()
			}
		}
		w.mu.Unlock()

		err = w.watcher.Close()
	})
	return err
}
