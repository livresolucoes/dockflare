package cloudflared

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/livresolucoes/dockflare/internal/logger"
)

func newTestLogger() *logger.Logger {
	return logger.New(os.Stderr)
}

// fakeBinary returns the path to a shell script (Unix) or batch file (Windows)
// that acts as a stand-in cloudflared binary: it sleeps until signalled.
func fakeBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		p := filepath.Join(dir, "cloudflared.bat")
		os.WriteFile(p, []byte("@echo off\ntimeout /t 30 /nobreak >nul\n"), 0700)
		return p
	}
	p := filepath.Join(dir, "cloudflared")
	os.WriteFile(p, []byte("#!/bin/sh\nsleep 30\n"), 0700)
	return p
}

func TestProcess_StartStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process tests require Unix signal handling")
	}
	log := newTestLogger()

	p := New(fakeBinary(t), "fake-token", log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestProcess_StopIdempotent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process tests require Unix signal handling")
	}
	log := newTestLogger()
	p := New("/bin/true", "fake-token", log)

	// Stop without Start should not panic or error.
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
}
