package main

import (
	"context"
	"sync"

	"github.com/livresolucoes/dockflare/internal/cloudflared"
	"github.com/livresolucoes/dockflare/internal/config"
	"github.com/livresolucoes/dockflare/internal/ingress"
	"github.com/livresolucoes/dockflare/internal/logger"
	"github.com/livresolucoes/dockflare/internal/network"
)

// reloader re-reads the config and reapplies it. The file watcher and the web UI
// both go through here, so an edit on disk and a save in the browser take
// exactly the same path — and the mutex keeps two of them from interleaving.
type reloader struct {
	configPath string
	nets       *network.Manager
	ingress    *ingress.Manager
	proc       *cloudflared.Process
	log        *logger.Logger

	mu sync.Mutex
	// lastToken is the tunnel token currently running. cloudflared is only
	// bounced when this changes.
	lastToken string
}

// Reload applies the config on disk.
//
// cloudflared is restarted only when its token changed. Everything else —
// routes, DNS, redirects — reaches the connector through Cloudflare's remote
// config, so a routing edit needs no restart and causes no downtime. That is
// what makes saving from the UI safe to do repeatedly.
func (r *reloader) Reload(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	cfg, err := config.Load(r.configPath)
	if err != nil {
		return err
	}
	if err := r.apply(ctx, cfg); err != nil {
		return err
	}

	if cfg.Token == r.lastToken {
		return nil
	}
	r.log.Info("Tunnel token changed, restarting cloudflared")
	r.lastToken = cfg.Token
	return r.proc.Reload(ctx)
}

// apply runs the parts that never need a subprocess restart. Used by Reload and,
// at startup, before cloudflared has even been started.
func (r *reloader) apply(ctx context.Context, cfg *config.Config) error {
	if len(cfg.Containers) == 0 {
		r.log.Warn("No containers configured — DockFlare is running but not joined to any networks")
	}
	// Networks first: route validation asks whether DockFlare can reach each
	// container, which is only true after the join.
	if err := r.nets.Sync(ctx, cfg.Containers); err != nil {
		return err
	}
	r.log.Info("Network sync complete: %d containers", len(cfg.Containers))

	if len(cfg.Routes) == 0 {
		return nil
	}
	if r.ingress == nil {
		r.log.Warn("%d route(s) configured but automatic ingress is disabled; routing stays dashboard-managed",
			len(cfg.Routes))
		return nil
	}
	return r.ingress.Sync(ctx, cfg)
}
