package watcher

import (
	"context"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/livresolucoes/dockflare/internal/logger"
)

// Watcher watches a single file and calls onChange after a debounce window.
// It watches the parent directory to correctly handle atomic-rename writes
// (the pattern used by vim, VS Code, and similar editors).
type Watcher struct {
	path     string
	filename string
	debounce time.Duration
	onChange func()
	log      *logger.Logger
	fw       *fsnotify.Watcher
}

func New(path string, debounce time.Duration, onChange func(), log *logger.Logger) (*Watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := fw.Add(dir); err != nil {
		fw.Close()
		return nil, err
	}
	return &Watcher{
		path:     path,
		filename: filepath.Base(path),
		debounce: debounce,
		onChange: onChange,
		log:      log,
		fw:       fw,
	}, nil
}

// Start blocks until ctx is cancelled, calling onChange after each debounce window.
func (w *Watcher) Start(ctx context.Context) {
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	armed := false

	for {
		select {
		case <-ctx.Done():
			timer.Stop()
			return

		case event, ok := <-w.fw.Events:
			if !ok {
				return
			}
			if filepath.Base(event.Name) != w.filename {
				continue
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				if !armed {
					timer.Reset(w.debounce)
					armed = true
				} else {
					timer.Reset(w.debounce)
				}
			}

		case err, ok := <-w.fw.Errors:
			if !ok {
				return
			}
			w.log.Error("watcher error: %v", err)

		case <-timer.C:
			armed = false
			w.log.Info("Config changed, reloading")
			w.onChange()
		}
	}
}

func (w *Watcher) Close() error {
	return w.fw.Close()
}
