package ingress

import (
	"context"
	"fmt"
	"sync"

	"github.com/livresolucoes/dockflare/internal/cloudflare"
	"github.com/livresolucoes/dockflare/internal/config"
	"github.com/livresolucoes/dockflare/internal/logger"
)

// API is the slice of *cloudflare.Client this package needs.
type API interface {
	GetTunnelConfiguration(ctx context.Context, accountID, tunnelID string) (*cloudflare.TunnelConfig, error)
	PutTunnelConfiguration(ctx context.Context, accountID, tunnelID string, cfg *cloudflare.TunnelConfig) error
	EnsureTunnelCNAME(ctx context.Context, hostname, tunnelID string) (bool, error)
	SyncHTTPSRedirects(ctx context.Context, hostnames []string) ([]string, error)
}

// Manager reconciles the tunnel's remote ingress with the routes in
// config.yml. It is safe for concurrent use, though in practice Sync is called
// serially from the reload pipeline.
type Manager struct {
	cf        API
	docker    ContainerInspector
	nets      NetworkSet
	accountID string
	tunnelID  string
	log       *logger.Logger

	mu sync.Mutex
	// lastApplied is what we last pushed. Holding it lets a hot reload that
	// changed nothing relevant skip the Cloudflare API entirely.
	lastApplied []cloudflare.IngressRule
	// dnsEnsured remembers hostnames whose CNAME we already verified this
	// process, so repeated reloads do not re-hit the DNS endpoints.
	dnsEnsured map[string]bool
	// redirectsInUse is true once force_https has been synced with at least one
	// hostname. It keeps a config that never uses the feature from making any
	// redirect-rule call at all, while still allowing the last hostname to be
	// removed and cleaned up.
	redirectsInUse bool
}

func New(
	cf API,
	insp ContainerInspector,
	nets NetworkSet,
	accountID, tunnelID string,
	log *logger.Logger,
) *Manager {
	return &Manager{
		cf:         cf,
		docker:     insp,
		nets:       nets,
		accountID:  accountID,
		tunnelID:   tunnelID,
		log:        log,
		dnsEnsured: make(map[string]bool),
	}
}

// Sync brings the tunnel's ingress in line with cfg.
//
// With no routes configured it is a no-op: DockFlare leaves routing entirely
// to the Zero Trust dashboard, exactly as it did before this feature existed.
//
// A validation failure aborts the whole update rather than applying the valid
// subset — ingress is replaced wholesale, so a partial apply would silently
// take working hostnames offline.
func (m *Manager) Sync(ctx context.Context, cfg *config.Config) error {
	if len(cfg.Routes) == 0 {
		return nil
	}

	if err := Resolve(ctx, cfg.Routes, m.docker, m.nets); err != nil {
		return err
	}

	desired := BuildIngress(cfg.Routes)
	if cfg.UIExposed() {
		desired = WithUIRule(desired, cfg.WebUI.Hostname, cfg.WebUI.Port)
	}
	if err := m.applyIngress(ctx, desired); err != nil {
		return err
	}

	if cfg.ManageDNS {
		// The UI's own hostname needs a CNAME just like any route's.
		m.ensureDNS(ctx, cfg.Hostnames())
	}
	m.syncRedirects(ctx, cfg.Routes)
	return nil
}

func (m *Manager) applyIngress(ctx context.Context, desired []cloudflare.IngressRule) error {
	routeCount := len(desired) - 1 // the catch-all is not a user route

	m.mu.Lock()
	unchanged := m.lastApplied != nil && SameRules(m.lastApplied, desired)
	m.mu.Unlock()
	if unchanged {
		m.log.Info("Ingress unchanged, skipping Cloudflare API (%d routes)", routeCount)
		return nil
	}

	// Read the remote config before writing: it tells us whether a write is
	// needed at all (e.g. after a restart), and it carries the warp-routing
	// and originRequest settings we must not clobber.
	remote, err := m.cf.GetTunnelConfiguration(ctx, m.accountID, m.tunnelID)
	if err != nil {
		return fmt.Errorf("reading tunnel configuration: %w", err)
	}
	if remote == nil {
		remote = &cloudflare.TunnelConfig{}
	}

	// Keep per-hostname originRequest settings DockFlare does not own before
	// comparing, otherwise every sync would look like a change and then wipe
	// them.
	desired = CarryOverOriginRequests(desired, remote.Ingress)

	if SameRules(remote.Ingress, desired) {
		m.remember(desired)
		m.log.Info("Ingress already up to date (%d routes)", routeCount)
		return nil
	}

	next := *remote
	next.Ingress = desired
	if err := m.cf.PutTunnelConfiguration(ctx, m.accountID, m.tunnelID, &next); err != nil {
		return fmt.Errorf("updating tunnel ingress: %w", err)
	}

	m.remember(desired)
	for _, r := range desired {
		if r.Hostname != "" {
			m.log.Info("Ingress route %s → %s", r.Hostname, r.Service)
		}
	}
	m.log.Info("Ingress updated: %d routes", routeCount)
	return nil
}

func (m *Manager) remember(rules []cloudflare.IngressRule) {
	m.mu.Lock()
	m.lastApplied = rules
	m.mu.Unlock()
}

// ensureDNS creates the proxied CNAME each hostname needs. Failures are warned
// about, not returned: the ingress is already in place and a DNS problem is
// usually a missing zone permission the user can fix without a restart.
func (m *Manager) ensureDNS(ctx context.Context, hostnames []string) {
	for _, hostname := range hostnames {
		m.mu.Lock()
		done := m.dnsEnsured[hostname]
		m.mu.Unlock()
		if done {
			continue
		}

		written, err := m.cf.EnsureTunnelCNAME(ctx, hostname, m.tunnelID)
		if err != nil {
			m.log.Warn("DNS for %s: %v", hostname, err)
			continue
		}
		if written {
			m.log.Info("DNS record %s → %s.cfargotunnel.com (proxied)", hostname, m.tunnelID)
		}

		m.mu.Lock()
		m.dnsEnsured[hostname] = true
		m.mu.Unlock()
	}
}

// syncRedirects reconciles the http→https Redirect Rules for the hostnames that
// asked for them. Like DNS, failures are warned about rather than returned: the
// ingress is already live, and a missing zone permission is fixable without a
// restart.
func (m *Manager) syncRedirects(ctx context.Context, routes []config.Route) {
	var hostnames []string
	for _, r := range routes {
		if r.ForceHTTPS {
			hostnames = append(hostnames, r.Hostname)
		}
	}

	m.mu.Lock()
	inUse := m.redirectsInUse
	m.mu.Unlock()
	// Never used and still not used: make no call at all.
	if len(hostnames) == 0 && !inUse {
		return
	}

	zones, err := m.cf.SyncHTTPSRedirects(ctx, hostnames)

	// Report the zones that worked even when another one failed: a permission
	// is often granted per zone, so partial success is the normal case.
	for _, zone := range zones {
		m.log.Info("HTTPS redirect rules updated in zone %s", zone)
	}

	m.mu.Lock()
	switch {
	case err == nil:
		m.redirectsInUse = len(hostnames) > 0
	case len(zones) > 0:
		// Something was written somewhere, so a later removal must still be
		// able to clean it up.
		m.redirectsInUse = true
	}
	m.mu.Unlock()

	if err != nil {
		// ASCII only: the middle dot renders as mojibake on terminals that are
		// not UTF-8, which is exactly where people read container logs.
		m.log.Warn("force_https: %v", err)
		// "Single Redirect" is the current dashboard label for the permission
		// group covering the http_request_dynamic_redirect phase.
		m.log.Warn(`force_https: the API token needs the zone permission ` +
			`"Single Redirect > Edit" for each zone listed above`)
	}
}
