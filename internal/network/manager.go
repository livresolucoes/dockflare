package network

import (
	"context"
	"sort"
	"sync"

	"github.com/livresolucoes/dockflare/internal/docker"
	"github.com/livresolucoes/dockflare/internal/logger"
)

// Manager tracks which networks DockFlare has joined and orchestrates
// connecting to networks needed by the configured containers.
type Manager struct {
	docker     *docker.Client
	selfID     string // empty means network operations are skipped
	joinedNets map[string]bool
	mu         sync.Mutex
	log        *logger.Logger
}

func New(dockerClient *docker.Client, selfID string, log *logger.Logger) *Manager {
	return &Manager{
		docker:     dockerClient,
		selfID:     selfID,
		joinedNets: make(map[string]bool),
		log:        log,
	}
}

// Sync connects DockFlare to the Docker networks of each listed container so
// that cloudflared can resolve container hostnames. Containers that are not
// found are logged as warnings and skipped.
func (m *Manager) Sync(ctx context.Context, containers []string) error {
	for _, name := range containers {
		info, err := m.docker.InspectContainer(ctx, name)
		if err != nil {
			m.log.Error("inspecting container %q: %v", name, err)
			continue
		}
		if info == nil {
			m.log.Warn("Container %s not found, skipping", name)
			continue
		}

		if m.selfID == "" {
			continue
		}
		for _, netName := range info.Networks {
			m.mu.Lock()
			alreadyJoined := m.joinedNets[netName]
			m.mu.Unlock()

			if alreadyJoined {
				continue
			}
			if err := m.docker.ConnectNetwork(ctx, netName, m.selfID); err != nil {
				m.log.Error("connecting to Docker network %s: %v", netName, err)
				continue
			}
			m.log.Info("Connected container %s to network %s", name, netName)
			m.mu.Lock()
			m.joinedNets[netName] = true
			m.mu.Unlock()
		}
	}
	return nil
}

// Tracking reports whether DockFlare manages its own network membership at
// all. It is false when the process could not determine its own container ID
// (running outside Docker), in which case Reachable carries no information.
func (m *Manager) Tracking() bool {
	return m.selfID != ""
}

// Reachable reports whether DockFlare has joined netName and can therefore
// resolve container names on it.
func (m *Manager) Reachable(netName string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.joinedNets[netName]
}

// JoinedNetworks returns the networks DockFlare has joined, sorted. Used by the
// web UI's status panel.
func (m *Manager) JoinedNetworks() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.joinedNets))
	for n := range m.joinedNets {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Disconnect removes DockFlare from all networks it joined during this session.
// Called on graceful shutdown. Errors are logged but do not halt the process.
func (m *Manager) Disconnect(ctx context.Context) {
	if m.selfID == "" {
		return
	}
	m.mu.Lock()
	nets := make([]string, 0, len(m.joinedNets))
	for n := range m.joinedNets {
		nets = append(nets, n)
	}
	m.mu.Unlock()

	for _, netName := range nets {
		if err := m.docker.DisconnectNetwork(ctx, netName, m.selfID); err != nil {
			m.log.Error("disconnecting from network %s: %v", netName, err)
		}
	}
}
