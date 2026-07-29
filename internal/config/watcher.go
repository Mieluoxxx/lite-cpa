package config

import (
	"context"
	"log"
	"os"
	"sync"
	"time"
)

// Watcher polls a config file's mtime and invokes onChange after debounce.
// It is intentionally dependency-free (no fsnotify): reliable across Docker
// bind mounts and editors that rewrite via temp+rename.
type Watcher struct {
	path     string
	interval time.Duration
	debounce time.Duration
	onChange func()

	mu       sync.Mutex
	lastMod  time.Time
	lastSize int64
}

// NewWatcher builds a poller. interval defaults to 1s; debounce to 300ms.
func NewWatcher(path string, onChange func(), interval, debounce time.Duration) *Watcher {
	if interval <= 0 {
		interval = time.Second
	}
	if debounce <= 0 {
		debounce = 300 * time.Millisecond
	}
	w := &Watcher{
		path:     path,
		interval: interval,
		debounce: debounce,
		onChange: onChange,
	}
	if fi, err := os.Stat(path); err == nil {
		w.lastMod = fi.ModTime()
		w.lastSize = fi.Size()
	}
	return w
}

// Run polls until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	var pending bool
	var fireAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			changed, ok := w.poll()
			if !ok {
				continue
			}
			if changed {
				pending = true
				fireAt = now.Add(w.debounce)
			}
			if pending && !now.Before(fireAt) {
				pending = false
				if w.onChange != nil {
					w.onChange()
				}
			}
		}
	}
}

func (w *Watcher) poll() (changed, ok bool) {
	fi, err := os.Stat(w.path)
	if err != nil {
		// Missing file is common during atomic renames; keep last known stamp.
		return false, true
	}
	mod := fi.ModTime()
	size := fi.Size()
	w.mu.Lock()
	defer w.mu.Unlock()
	if mod.Equal(w.lastMod) && size == w.lastSize {
		return false, true
	}
	w.lastMod = mod
	w.lastSize = size
	return true, true
}

// Snapshot stamps the current file so a subsequent Run does not immediately
// re-fire for the same content (call after a successful manual reload).
func (w *Watcher) Snapshot() {
	fi, err := os.Stat(w.path)
	if err != nil {
		return
	}
	w.mu.Lock()
	w.lastMod = fi.ModTime()
	w.lastSize = fi.Size()
	w.mu.Unlock()
}

// LogReloadError is a small helper so callers share wording.
func LogReloadError(err error) {
	log.Printf("config reload: %v (keeping previous config)", err)
}
