package network

import (
	"io"
	"testing"

	"github.com/livresolucoes/dockflare/internal/logger"
)

func TestTracking(t *testing.T) {
	log := logger.New(io.Discard)

	// Running inside Docker: DockFlare knows its own container and manages
	// its own network membership.
	if !New(nil, "abc123456789", log).Tracking() {
		t.Error("Tracking() = false with a known self ID, want true")
	}
	// Running outside Docker: membership is unknown, so reachability must not
	// be enforced against it.
	if New(nil, "", log).Tracking() {
		t.Error("Tracking() = true without a self ID, want false")
	}
}

func TestReachable(t *testing.T) {
	m := New(nil, "abc123456789", logger.New(io.Discard))

	if m.Reachable("app_net") {
		t.Error("a network we never joined must not be reachable")
	}

	// Sync marks networks as joined; emulate that bookkeeping directly since
	// joining needs a live Docker daemon.
	m.mu.Lock()
	m.joinedNets["app_net"] = true
	m.mu.Unlock()

	if !m.Reachable("app_net") {
		t.Error("a joined network must be reachable")
	}
	if m.Reachable("other_net") {
		t.Error("an unrelated network must not be reachable")
	}
}
