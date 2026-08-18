package ingress

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/livresolucoes/dockflare/internal/config"
	"github.com/livresolucoes/dockflare/internal/docker"
)

// writeConfig writes config.yml and returns its path, overwriting any previous
// content — the shape of a hot reload.
func writeConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestHotReload_ConfigFileToIngress walks the whole reload path a running
// DockFlare takes — parse config.yml, validate, resolve Docker destinations,
// push ingress — across three successive edits of the file.
func TestHotReload_ConfigFileToIngress(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "cfapi")

	dir := t.TempDir()
	insp := &fakeDocker{containers: map[string]*docker.ContainerInfo{
		"api":   {Name: "api", Networks: []string{"app_net"}, Ports: []int{3000}},
		"admin": {Name: "admin", Networks: []string{"app_net"}, Ports: []int{8080}},
	}}
	api := &fakeAPI{}
	m := newTestManager(api, insp, reachableNets("app_net"))
	ctx := context.Background()

	sync := func(t *testing.T, content string) error {
		t.Helper()
		cfg, err := config.Load(writeConfig(t, dir, content))
		if err != nil {
			return err
		}
		return m.Sync(ctx, cfg)
	}

	// 1. Start with one route.
	if err := sync(t, `
containers: [api]
routes:
  - hostname: api.example.com
    container: api
    port: 3000
`); err != nil {
		t.Fatal(err)
	}
	if api.puts != 1 {
		t.Fatalf("puts = %d, want 1 after the first load", api.puts)
	}

	// 2. Same file rewritten with no semantic change — must not hit the API.
	if err := sync(t, `
containers: [api, admin]
routes:
  - hostname: api.example.com
    container: api
    port: 3000
`); err != nil {
		t.Fatal(err)
	}
	if api.puts != 1 {
		t.Errorf("puts = %d, want still 1 — the routes did not change", api.puts)
	}

	// 3. A route is added — the tunnel must be updated to match.
	if err := sync(t, `
containers: [api, admin]
routes:
  - hostname: api.example.com
    container: api
    port: 3000

  - hostname: admin.example.com
    container: admin
    port: 8080
`); err != nil {
		t.Fatal(err)
	}
	if api.puts != 2 {
		t.Fatalf("puts = %d, want 2 after adding a route", api.puts)
	}
	final := api.putBody[1].Ingress
	if len(final) != 3 {
		t.Fatalf("ingress = %+v, want 2 routes + catch-all", final)
	}
	if final[1].Hostname != "admin.example.com" || final[1].Service != "http://admin:8080" {
		t.Errorf("new rule = %+v", final[1])
	}
	if final[2].Service != "http_status:404" {
		t.Errorf("catch-all missing after reload: %+v", final)
	}
}

func TestHotReload_InvalidRouteKeepsPreviousIngress(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "cfapi")

	dir := t.TempDir()
	insp := &fakeDocker{containers: map[string]*docker.ContainerInfo{
		"api": {Name: "api", Networks: []string{"app_net"}, Ports: []int{3000}},
	}}
	api := &fakeAPI{}
	m := newTestManager(api, insp, reachableNets("app_net"))
	ctx := context.Background()

	cfg, err := config.Load(writeConfig(t, dir, `
routes:
  - hostname: api.example.com
    container: api
    port: 3000
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Sync(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	applied := api.puts

	// The user typos a container name. The good route must stay live.
	cfg, err = config.Load(writeConfig(t, dir, `
routes:
  - hostname: api.example.com
    container: api
    port: 3000

  - hostname: typo.example.com
    container: aip
    port: 3000
`))
	if err != nil {
		t.Fatal(err)
	}
	err = m.Sync(ctx, cfg)
	if err == nil {
		t.Fatal("expected a validation error")
	}
	if !strings.Contains(err.Error(), `container "aip" but that container was not found`) {
		t.Errorf("error = %v", err)
	}
	if api.puts != applied {
		t.Errorf("puts = %d, want %d — a bad reload must not rewrite the ingress", api.puts, applied)
	}
}

func TestHotReload_DuplicateHostnameFailsAtLoad(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "cfapi")

	_, err := config.Load(writeConfig(t, t.TempDir(), `
routes:
  - hostname: api.example.com
    container: api
    port: 3000

  - hostname: API.example.com
    container: other
    port: 8080
`))
	if err == nil {
		t.Fatal("expected duplicate hostnames to be rejected (case-insensitively)")
	}
	if !strings.Contains(err.Error(), "declared more than once") {
		t.Errorf("error = %v", err)
	}
}
