package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/livresolucoes/dockflare/internal/logger"
)

func TestWatcher_SingleFire(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	os.WriteFile(path, []byte("initial"), 0600)

	var count atomic.Int32
	log := logger.New(os.Stderr)
	debounce := 100 * time.Millisecond

	w, err := New(path, debounce, func() { count.Add(1) }, log)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go w.Start(ctx)
	time.Sleep(20 * time.Millisecond)

	os.WriteFile(path, []byte("updated"), 0600)
	time.Sleep(debounce + 100*time.Millisecond)

	cancel()
	time.Sleep(20 * time.Millisecond)

	if n := count.Load(); n != 1 {
		t.Errorf("onChange called %d times, want 1", n)
	}
}

func TestWatcher_DebounceCoalescing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	os.WriteFile(path, []byte("initial"), 0600)

	var count atomic.Int32
	log := logger.New(os.Stderr)
	debounce := 150 * time.Millisecond

	w, err := New(path, debounce, func() { count.Add(1) }, log)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go w.Start(ctx)
	time.Sleep(20 * time.Millisecond)

	// Rapid successive writes within the debounce window.
	for i := 0; i < 5; i++ {
		os.WriteFile(path, []byte("update"), 0600)
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(debounce + 100*time.Millisecond)

	cancel()
	time.Sleep(20 * time.Millisecond)

	if n := count.Load(); n != 1 {
		t.Errorf("onChange called %d times after rapid writes, want 1", n)
	}
}

func TestWatcher_CancelStopsCallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	os.WriteFile(path, []byte("initial"), 0600)

	var count atomic.Int32
	log := logger.New(os.Stderr)

	w, err := New(path, 50*time.Millisecond, func() { count.Add(1) }, log)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Start(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher did not stop after ctx cancel")
	}

	os.WriteFile(path, []byte("after cancel"), 0600)
	time.Sleep(200 * time.Millisecond)

	if n := count.Load(); n != 0 {
		t.Errorf("onChange called %d times after cancel, want 0", n)
	}
}
