package config

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatcherFiresOnRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var fires atomic.Int32
	w := NewWatcher(path, func() { fires.Add(1) }, 20*time.Millisecond, 30*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	time.Sleep(40 * time.Millisecond)
	if fires.Load() != 0 {
		t.Fatalf("unexpected fire before change: %d", fires.Load())
	}

	// Atomic-ish rewrite: write temp then rename (common editor pattern).
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if fires.Load() >= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("watcher did not fire after rewrite, fires=%d", fires.Load())
}

func TestWatcherIgnoresUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("same\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var fires atomic.Int32
	w := NewWatcher(path, func() { fires.Add(1) }, 15*time.Millisecond, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	time.Sleep(80 * time.Millisecond)
	if n := fires.Load(); n != 0 {
		t.Fatalf("fires=%d want 0", n)
	}
}
