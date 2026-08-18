package ingress

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/livresolucoes/dockflare/internal/cloudflare"
	"github.com/livresolucoes/dockflare/internal/config"
	"github.com/livresolucoes/dockflare/internal/docker"
	"github.com/livresolucoes/dockflare/internal/logger"
)

// fakeAPI records every call so tests can assert on how often the Cloudflare
// API was hit, not just on the end state.
type fakeAPI struct {
	remote  *cloudflare.TunnelConfig
	gets    int
	puts    int
	putBody []*cloudflare.TunnelConfig
	dns     []string
	getErr  error
	putErr  error
	dnsErr  error

	// redirectCalls records the hostname list passed to each
	// SyncHTTPSRedirects call, so tests can assert that the feature stays
	// silent when unused.
	redirectCalls [][]string
	redirectZones []string
	redirectErr   error
}

func (f *fakeAPI) GetTunnelConfiguration(_ context.Context, _, _ string) (*cloudflare.TunnelConfig, error) {
	f.gets++
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.remote == nil {
		return &cloudflare.TunnelConfig{}, nil
	}
	return f.remote, nil
}

func (f *fakeAPI) PutTunnelConfiguration(_ context.Context, _, _ string, cfg *cloudflare.TunnelConfig) error {
	f.puts++
	if f.putErr != nil {
		return f.putErr
	}
	f.putBody = append(f.putBody, cfg)
	f.remote = cfg
	return nil
}

func (f *fakeAPI) EnsureTunnelCNAME(_ context.Context, hostname, _ string) (bool, error) {
	f.dns = append(f.dns, hostname)
	if f.dnsErr != nil {
		return false, f.dnsErr
	}
	return true, nil
}

// SyncHTTPSRedirects returns redirectZones and redirectErr together, because
// that is what the real client does: one zone can be written while another is
// denied, so a non-nil error does not mean nothing happened.
func (f *fakeAPI) SyncHTTPSRedirects(_ context.Context, hostnames []string) ([]string, error) {
	f.redirectCalls = append(f.redirectCalls, hostnames)
	return f.redirectZones, f.redirectErr
}

func newTestManager(api API, insp ContainerInspector, nets NetworkSet) *Manager {
	return New(api, insp, nets, "acct123", "tunnel456", logger.New(io.Discard))
}

func twoContainers() *fakeDocker {
	return &fakeDocker{containers: map[string]*docker.ContainerInfo{
		"meuapp_web": {Name: "meuapp_web", Networks: []string{"rpg_net"}, Ports: []int{4000}},
		"meuapp_api": {Name: "meuapp_api", Networks: []string{"rpg_net"}, Ports: []int{3000}},
		"admin":      {Name: "admin", Networks: []string{"rpg_net"}, Ports: []int{8080}},
	}}
}

func cfgWith(routes ...config.Route) *config.Config {
	return &config.Config{Token: "tok", APIToken: "api", Routes: routes}
}

func TestSync_NoRoutesNeverTouchesCloudflare(t *testing.T) {
	api := &fakeAPI{}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))

	cfg := &config.Config{Token: "tok", Containers: []string{"meuapp_web"}}
	if err := m.Sync(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if api.gets != 0 || api.puts != 0 {
		t.Errorf("gets=%d puts=%d, want 0/0 — a config without routes must leave ingress alone",
			api.gets, api.puts)
	}
}

func TestSync_AppliesIngress(t *testing.T) {
	api := &fakeAPI{}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))

	cfg := cfgWith(
		config.Route{Hostname: "meuapp.example.com", Container: "meuapp_web", Port: 4000},
		config.Route{Hostname: "api.meuapp.example.com", Container: "meuapp_api", Port: 3000},
	)
	if err := m.Sync(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if api.puts != 1 {
		t.Fatalf("puts = %d, want 1", api.puts)
	}
	got := api.putBody[0].Ingress
	want := []cloudflare.IngressRule{
		{Hostname: "meuapp.example.com", Service: "http://meuapp_web:4000"},
		{Hostname: "api.meuapp.example.com", Service: "http://meuapp_api:3000"},
		{Service: "http_status:404"},
	}
	if !SameRules(got, want) {
		t.Errorf("ingress = %+v\nwant %+v", got, want)
	}
}

func TestSync_PreservesRemoteSettingsItDoesNotOwn(t *testing.T) {
	api := &fakeAPI{remote: &cloudflare.TunnelConfig{
		Ingress:       []cloudflare.IngressRule{{Service: "http_status:404"}},
		WarpRouting:   []byte(`{"enabled":true}`),
		OriginRequest: []byte(`{"connectTimeout":30}`),
	}}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))

	err := m.Sync(context.Background(), cfgWith(
		config.Route{Hostname: "api.meuapp.example.com", Container: "meuapp_api", Port: 3000},
	))
	if err != nil {
		t.Fatal(err)
	}
	sent := api.putBody[0]
	if string(sent.WarpRouting) != `{"enabled":true}` {
		t.Errorf("warp-routing was clobbered: %s", sent.WarpRouting)
	}
	if string(sent.OriginRequest) != `{"connectTimeout":30}` {
		t.Errorf("originRequest was clobbered: %s", sent.OriginRequest)
	}
}

func TestSync_SkipsWriteWhenRemoteAlreadyMatches(t *testing.T) {
	// e.g. DockFlare restarted with an unchanged config: it should read once
	// and write nothing.
	api := &fakeAPI{remote: &cloudflare.TunnelConfig{Ingress: []cloudflare.IngressRule{
		{Hostname: "api.meuapp.example.com", Service: "http://meuapp_api:3000"},
		{Service: "http_status:404"},
	}}}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))

	err := m.Sync(context.Background(), cfgWith(
		config.Route{Hostname: "api.meuapp.example.com", Container: "meuapp_api", Port: 3000},
	))
	if err != nil {
		t.Fatal(err)
	}
	if api.puts != 0 {
		t.Errorf("puts = %d, want 0 — remote already matched", api.puts)
	}
	if api.gets != 1 {
		t.Errorf("gets = %d, want 1", api.gets)
	}
}

func TestSync_HotReload_UnchangedRoutesMakeNoAPICalls(t *testing.T) {
	api := &fakeAPI{}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))
	cfg := cfgWith(config.Route{Hostname: "api.meuapp.example.com", Container: "meuapp_api", Port: 3000})

	if err := m.Sync(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	gets, puts := api.gets, api.puts

	// Two more reloads with the same routes — a watcher firing on an unrelated
	// edit to config.yml.
	for i := 0; i < 2; i++ {
		if err := m.Sync(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	if api.gets != gets || api.puts != puts {
		t.Errorf("gets %d→%d, puts %d→%d; unchanged routes must not hit the API again",
			gets, api.gets, puts, api.puts)
	}
}

func TestSync_HotReload_AddedRouteIsPushed(t *testing.T) {
	api := &fakeAPI{}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))
	ctx := context.Background()

	first := cfgWith(config.Route{Hostname: "api.example.com", Container: "meuapp_api", Port: 3000})
	if err := m.Sync(ctx, first); err != nil {
		t.Fatal(err)
	}

	second := cfgWith(
		config.Route{Hostname: "api.example.com", Container: "meuapp_api", Port: 3000},
		config.Route{Hostname: "admin.example.com", Container: "admin", Port: 8080},
	)
	if err := m.Sync(ctx, second); err != nil {
		t.Fatal(err)
	}

	if api.puts != 2 {
		t.Fatalf("puts = %d, want 2", api.puts)
	}
	got := api.putBody[1].Ingress
	want := []cloudflare.IngressRule{
		{Hostname: "api.example.com", Service: "http://meuapp_api:3000"},
		{Hostname: "admin.example.com", Service: "http://admin:8080"},
		{Service: "http_status:404"},
	}
	if !SameRules(got, want) {
		t.Errorf("ingress after reload = %+v\nwant %+v", got, want)
	}
}

func TestSync_HotReload_RemovedRouteIsPushed(t *testing.T) {
	api := &fakeAPI{}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))
	ctx := context.Background()

	both := cfgWith(
		config.Route{Hostname: "api.example.com", Container: "meuapp_api", Port: 3000},
		config.Route{Hostname: "admin.example.com", Container: "admin", Port: 8080},
	)
	if err := m.Sync(ctx, both); err != nil {
		t.Fatal(err)
	}
	one := cfgWith(config.Route{Hostname: "api.example.com", Container: "meuapp_api", Port: 3000})
	if err := m.Sync(ctx, one); err != nil {
		t.Fatal(err)
	}

	last := api.putBody[len(api.putBody)-1].Ingress
	if len(last) != 2 {
		t.Fatalf("ingress = %+v, want 1 route + catch-all", last)
	}
	if last[0].Hostname != "api.example.com" {
		t.Errorf("remaining route = %q", last[0].Hostname)
	}
}

func TestSync_HotReload_ChangedPortIsPushed(t *testing.T) {
	api := &fakeAPI{}
	m := newTestManager(api, &fakeDocker{containers: map[string]*docker.ContainerInfo{
		// No declared ports, so either port validates.
		"api": {Name: "api", Networks: []string{"rpg_net"}},
	}}, reachableNets("rpg_net"))
	ctx := context.Background()

	if err := m.Sync(ctx, cfgWith(config.Route{Hostname: "a.example.com", Container: "api", Port: 3000})); err != nil {
		t.Fatal(err)
	}
	if err := m.Sync(ctx, cfgWith(config.Route{Hostname: "a.example.com", Container: "api", Port: 4000})); err != nil {
		t.Fatal(err)
	}
	if api.puts != 2 {
		t.Fatalf("puts = %d, want 2 — the port change must be pushed", api.puts)
	}
	if api.putBody[1].Ingress[0].Service != "http://api:4000" {
		t.Errorf("service = %q, want http://api:4000", api.putBody[1].Ingress[0].Service)
	}
}

func TestSync_InvalidRouteAbortsWithoutWriting(t *testing.T) {
	// Ingress is replaced wholesale, so applying the valid subset would take
	// the other hostnames offline.
	api := &fakeAPI{}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))

	err := m.Sync(context.Background(), cfgWith(
		config.Route{Hostname: "ok.example.com", Container: "meuapp_api", Port: 3000},
		config.Route{Hostname: "bad.example.com", Container: "does_not_exist", Port: 80},
	))
	if err == nil {
		t.Fatal("expected a validation error")
	}
	if !IsValidationError(err) {
		t.Errorf("want a ValidationError, got %T: %v", err, err)
	}
	if api.gets != 0 || api.puts != 0 {
		t.Errorf("gets=%d puts=%d, want 0/0 — nothing may be applied when validation fails",
			api.gets, api.puts)
	}
}

func TestSync_APIFailureIsNotAValidationError(t *testing.T) {
	api := &fakeAPI{putErr: errors.New("503 service unavailable")}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))

	err := m.Sync(context.Background(), cfgWith(
		config.Route{Hostname: "api.example.com", Container: "meuapp_api", Port: 3000},
	))
	if err == nil {
		t.Fatal("expected the API error to surface")
	}
	if IsValidationError(err) {
		t.Error("a transport failure must not be classified as a config problem")
	}
}

func TestSync_FailedPutIsRetriedOnNextReload(t *testing.T) {
	api := &fakeAPI{putErr: errors.New("boom")}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))
	cfg := cfgWith(config.Route{Hostname: "api.example.com", Container: "meuapp_api", Port: 3000})
	ctx := context.Background()

	if err := m.Sync(ctx, cfg); err == nil {
		t.Fatal("expected the first sync to fail")
	}
	api.putErr = nil
	if err := m.Sync(ctx, cfg); err != nil {
		t.Fatalf("second sync should retry and succeed, got: %v", err)
	}
	if api.puts != 2 {
		t.Errorf("puts = %d, want 2 — a failed apply must not be remembered as applied", api.puts)
	}
}

func TestSync_ExposedUIGetsAnIngressRuleAndDNS(t *testing.T) {
	api := &fakeAPI{}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))

	cfg := cfgWith(config.Route{Hostname: "api.example.com", Container: "meuapp_api", Port: 3000})
	cfg.ManageDNS = true
	cfg.WebUI = config.WebUI{Enabled: true, Port: 8080, Hostname: "ui.example.com"}

	if err := m.Sync(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	got := api.putBody[0].Ingress
	want := []cloudflare.IngressRule{
		{Hostname: "api.example.com", Service: "http://meuapp_api:3000"},
		{Hostname: "ui.example.com", Service: "http://localhost:8080"},
		{Service: "http_status:404"},
	}
	if !SameRules(got, want) {
		t.Errorf("ingress = %+v\nwant %+v", got, want)
	}
	// The UI hostname needs a CNAME just like a route's.
	if len(api.dns) != 2 || api.dns[1] != "ui.example.com" {
		t.Errorf("dns = %v, want the UI hostname included", api.dns)
	}
}

func TestSync_UIEnabledButNotExposedStaysOutOfIngress(t *testing.T) {
	api := &fakeAPI{}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))

	cfg := cfgWith(config.Route{Hostname: "api.example.com", Container: "meuapp_api", Port: 3000})
	cfg.ManageDNS = true
	cfg.WebUI = config.WebUI{Enabled: true, Port: 8080} // no hostname

	if err := m.Sync(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(api.putBody[0].Ingress) != 2 {
		t.Errorf("ingress = %+v, want only the route and the catch-all", api.putBody[0].Ingress)
	}
	if len(api.dns) != 1 {
		t.Errorf("dns = %v, want only the route hostname", api.dns)
	}
}

func TestSync_ManageDNSOffMakesNoDNSCalls(t *testing.T) {
	api := &fakeAPI{}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))

	cfg := cfgWith(config.Route{Hostname: "api.example.com", Container: "meuapp_api", Port: 3000})
	if err := m.Sync(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(api.dns) != 0 {
		t.Errorf("dns calls = %v, want none when manage_dns is off", api.dns)
	}
}

func TestSync_ManageDNSEnsuresEachHostnameOnce(t *testing.T) {
	api := &fakeAPI{}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))

	cfg := cfgWith(config.Route{Hostname: "api.example.com", Container: "meuapp_api", Port: 3000})
	cfg.ManageDNS = true
	ctx := context.Background()

	if err := m.Sync(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := m.Sync(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if len(api.dns) != 1 || api.dns[0] != "api.example.com" {
		t.Errorf("dns calls = %v, want exactly one for api.example.com", api.dns)
	}
}

func TestSync_NoForceHTTPSMakesNoRedirectCall(t *testing.T) {
	api := &fakeAPI{}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))

	cfg := cfgWith(config.Route{Hostname: "api.example.com", Container: "meuapp_api", Port: 3000})
	if err := m.Sync(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(api.redirectCalls) != 0 {
		t.Errorf("redirect calls = %v, want none when no route sets force_https", api.redirectCalls)
	}
}

func TestSync_ForceHTTPSPassesOnlyTaggedHostnames(t *testing.T) {
	api := &fakeAPI{}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))

	cfg := cfgWith(
		config.Route{Hostname: "api.example.com", Container: "meuapp_api", Port: 3000, ForceHTTPS: true},
		config.Route{Hostname: "plain.example.com", Container: "admin", Port: 8080},
	)
	if err := m.Sync(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(api.redirectCalls) != 1 {
		t.Fatalf("redirect calls = %d, want 1", len(api.redirectCalls))
	}
	got := api.redirectCalls[0]
	if len(got) != 1 || got[0] != "api.example.com" {
		t.Errorf("hostnames = %v, want only api.example.com", got)
	}
}

func TestSync_ForceHTTPSRemovedStillSyncsToClearTheRule(t *testing.T) {
	// Turning the flag off must reach the API with an empty list, otherwise the
	// redirect rule would linger forever.
	api := &fakeAPI{}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))
	ctx := context.Background()

	on := cfgWith(config.Route{Hostname: "api.example.com", Container: "meuapp_api", Port: 3000, ForceHTTPS: true})
	if err := m.Sync(ctx, on); err != nil {
		t.Fatal(err)
	}
	off := cfgWith(config.Route{Hostname: "api.example.com", Container: "meuapp_api", Port: 3000})
	if err := m.Sync(ctx, off); err != nil {
		t.Fatal(err)
	}

	if len(api.redirectCalls) != 2 {
		t.Fatalf("redirect calls = %d, want 2", len(api.redirectCalls))
	}
	if len(api.redirectCalls[1]) != 0 {
		t.Errorf("second call = %v, want an empty list to clear the rule", api.redirectCalls[1])
	}

	// And once cleared, a third sync must go quiet again.
	if err := m.Sync(ctx, off); err != nil {
		t.Fatal(err)
	}
	if len(api.redirectCalls) != 2 {
		t.Errorf("redirect calls = %d, want still 2 — nothing left to clear", len(api.redirectCalls))
	}
}

func TestSync_ForceHTTPSFailureDoesNotFailTheSync(t *testing.T) {
	// Redirect rules need a zone permission the tunnel does not. Missing it
	// must not take down routing that is already working.
	api := &fakeAPI{redirectErr: errors.New("status 403: 10000: Authentication error")}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))

	cfg := cfgWith(config.Route{Hostname: "api.example.com", Container: "meuapp_api", Port: 3000, ForceHTTPS: true})
	if err := m.Sync(context.Background(), cfg); err != nil {
		t.Fatalf("a redirect failure must not fail the sync, got: %v", err)
	}
	if api.puts != 1 {
		t.Errorf("ingress should still have been applied; puts = %d", api.puts)
	}
}

func TestSync_ForceHTTPSPartialFailureStillTracksWhatWasWritten(t *testing.T) {
	// One zone denied, another written: removing force_https later must still
	// reach the API to clean up the zone that did get a rule.
	api := &fakeAPI{
		redirectZones: []string{"allowed.com"},
		redirectErr:   errors.New("zone denied.com: status 403"),
	}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))
	ctx := context.Background()

	on := cfgWith(config.Route{Hostname: "api.example.com", Container: "meuapp_api", Port: 3000, ForceHTTPS: true})
	if err := m.Sync(ctx, on); err != nil {
		t.Fatalf("a partial redirect failure must not fail the sync: %v", err)
	}

	api.redirectErr = nil
	api.redirectZones = nil
	off := cfgWith(config.Route{Hostname: "api.example.com", Container: "meuapp_api", Port: 3000})
	if err := m.Sync(ctx, off); err != nil {
		t.Fatal(err)
	}
	if len(api.redirectCalls) != 2 {
		t.Fatalf("redirect calls = %d, want 2 — the written zone still needs cleaning", len(api.redirectCalls))
	}
	if len(api.redirectCalls[1]) != 0 {
		t.Errorf("second call = %v, want an empty list", api.redirectCalls[1])
	}
}

func TestSync_ForceHTTPSTotalFailureNeedsNoCleanup(t *testing.T) {
	// Nothing was written anywhere, so a later removal has nothing to undo and
	// must not spend an API call.
	api := &fakeAPI{redirectErr: errors.New("status 403")}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))
	ctx := context.Background()

	on := cfgWith(config.Route{Hostname: "api.example.com", Container: "meuapp_api", Port: 3000, ForceHTTPS: true})
	if err := m.Sync(ctx, on); err != nil {
		t.Fatal(err)
	}
	api.redirectErr = nil
	off := cfgWith(config.Route{Hostname: "api.example.com", Container: "meuapp_api", Port: 3000})
	if err := m.Sync(ctx, off); err != nil {
		t.Fatal(err)
	}
	if len(api.redirectCalls) != 1 {
		t.Errorf("redirect calls = %d, want 1 — nothing was ever written", len(api.redirectCalls))
	}
}

func TestSync_ForceHTTPSRetriedAfterFailure(t *testing.T) {
	api := &fakeAPI{redirectErr: errors.New("boom")}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))
	cfg := cfgWith(config.Route{Hostname: "api.example.com", Container: "meuapp_api", Port: 3000, ForceHTTPS: true})
	ctx := context.Background()

	if err := m.Sync(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	api.redirectErr = nil
	if err := m.Sync(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if len(api.redirectCalls) != 2 {
		t.Errorf("redirect calls = %d, want 2 — a failure must not be remembered as done", len(api.redirectCalls))
	}
}

func TestSync_OriginSchemeHTTPSReachesTheIngress(t *testing.T) {
	api := &fakeAPI{}
	m := newTestManager(api, &fakeDocker{containers: map[string]*docker.ContainerInfo{
		"vault": {Name: "vault", Networks: []string{"rpg_net"}, Ports: []int{8200}},
	}}, reachableNets("rpg_net"))

	cfg := cfgWith(config.Route{
		Hostname: "vault.example.com", Container: "vault", Port: 8200,
		OriginScheme: config.SchemeHTTPS,
	})
	if err := m.Sync(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := api.putBody[0].Ingress[0].Service; got != "https://vault:8200" {
		t.Errorf("service = %q, want https://vault:8200", got)
	}
}

func TestSync_DNSFailureDoesNotFailTheSync(t *testing.T) {
	// The ingress is already live; a missing zone permission is fixable
	// without restarting DockFlare.
	api := &fakeAPI{dnsErr: errors.New("no zone permission")}
	m := newTestManager(api, twoContainers(), reachableNets("rpg_net"))

	cfg := cfgWith(config.Route{Hostname: "api.example.com", Container: "meuapp_api", Port: 3000})
	cfg.ManageDNS = true

	if err := m.Sync(context.Background(), cfg); err != nil {
		t.Fatalf("a DNS failure must not fail the sync, got: %v", err)
	}
	if api.puts != 1 {
		t.Errorf("ingress should still have been applied; puts = %d", api.puts)
	}
}
