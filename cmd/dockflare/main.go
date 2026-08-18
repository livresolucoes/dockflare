package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/livresolucoes/dockflare/internal/cloudflare"
	"github.com/livresolucoes/dockflare/internal/cloudflared"
	"github.com/livresolucoes/dockflare/internal/config"
	"github.com/livresolucoes/dockflare/internal/docker"
	"github.com/livresolucoes/dockflare/internal/ingress"
	"github.com/livresolucoes/dockflare/internal/logger"
	"github.com/livresolucoes/dockflare/internal/network"
	"github.com/livresolucoes/dockflare/internal/watcher"
	"github.com/livresolucoes/dockflare/internal/web"
)

const (
	cloudflaredBin = "/usr/local/bin/cloudflared"
	watchDebounce  = 500 * time.Millisecond
)

func main() {
	configPath := flag.String("config", "/config/config.yml", "path to dockflare config file")
	flag.Parse()

	log := logger.New(os.Stderr)

	dockerClient, err := docker.New(log)
	if err != nil {
		log.Fatal("connecting to Docker: %v", err)
	}

	selfID, err := docker.SelfContainerID()
	if err != nil {
		log.Warn("cannot determine own container ID, network auto-connect disabled: %v", err)
	}

	netManager := network.New(dockerClient, selfID, log)

	initialCfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal("loading config: %v", err)
	}

	cfProcess := cloudflared.New(cloudflaredBin, initialCfg.Token, log)

	// The connector token carries the account tag and tunnel ID, so automatic
	// ingress needs no extra identifiers in config.yml. A token we cannot
	// decode is not fatal — the tunnel still runs with dashboard-managed
	// routing, which is the pre-existing behaviour.
	var ingressManager *ingress.Manager
	if tok, err := cloudflare.ParseConnectorToken(initialCfg.Token); err != nil {
		log.Warn("cannot read tunnel metadata from token, automatic ingress routing disabled: %v", err)
	} else {
		cfAPI := cloudflare.NewClient(initialCfg.APIToken, log)
		ingressManager = ingress.New(cfAPI, dockerClient, netManager, tok.AccountTag, tok.TunnelID, log)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reload := &reloader{
		configPath: *configPath,
		nets:       netManager,
		ingress:    ingressManager,
		proc:       cfProcess,
		log:        log,
		// Seeded so the first reload does not mistake startup for a token
		// change and bounce a process that is about to be started anyway.
		lastToken: initialCfg.Token,
	}

	if err := reload.apply(ctx, initialCfg); err != nil {
		// A bad config is the user's to fix and should stop startup. A
		// Cloudflare API hiccup should not: the tunnel can still serve the
		// routing it already has.
		if ingress.IsValidationError(err) {
			log.Fatal("startup: %v", err)
		}
		log.Error("startup: %v", err)
	}
	if err := cfProcess.Start(ctx); err != nil {
		log.Fatal("starting cloudflared: %v", err)
	}

	if initialCfg.WebUI.Enabled {
		startWebUI(ctx, initialCfg, *configPath, dockerClient, netManager, cfProcess, reload, log)
	}

	w, err := watcher.New(*configPath, watchDebounce, func() {
		if err := reload.Reload(ctx); err != nil {
			log.Error("reload pipeline failed: %v", err)
		}
	}, log)
	if err != nil {
		log.Fatal("starting file watcher: %v", err)
	}
	defer w.Close()

	go w.Start(ctx)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Info("Received signal %s, shutting down", sig)
	case <-cfProcess.CrashCh():
		log.Error("cloudflared exited unexpectedly after max retries")
	}

	cancel()
	cfProcess.Stop()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	netManager.Disconnect(shutdownCtx)

	log.Info("DockFlare stopped")
}

// startWebUI launches the optional browser interface. Config validation has
// already guaranteed a token of sufficient length by the time we get here.
func startWebUI(
	ctx context.Context,
	cfg *config.Config,
	configPath string,
	dockerClient *docker.Client,
	netManager *network.Manager,
	cfProcess *cloudflared.Process,
	reload *reloader,
	log *logger.Logger,
) {
	// Saving is the whole point of the UI, so a read-only config is worth
	// saying out loud at startup rather than at the user's first click.
	if err := config.CheckWritable(configPath); err != nil {
		log.Warn("Web UI: config is not writable, saving will fail: %v", err)
		log.Warn("Web UI: mount the config directory read-write (./config:/config), not the file")
	}
	if cfg.UIExposed() {
		log.Info("Web UI published through the tunnel at %s", cfg.WebUI.Hostname)
		log.Warn("Web UI is reachable from the internet — protect %s with Cloudflare Access", cfg.WebUI.Hostname)
	}

	server := web.New(web.Options{
		ConfigPath: configPath,
		Token:      cfg.UIToken,
		Port:       cfg.WebUI.Port,
		Exposed:    cfg.UIExposed(),
		Docker:     dockerClient,
		Nets:       netManager,
		Proc:       cfProcess,
		Reloader:   reload,
		Log:        log,
	})

	go func() {
		if err := server.Start(ctx); err != nil {
			log.Error("web UI: %v", err)
		}
	}()
}
