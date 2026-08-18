package config

import (
	"strings"
	"testing"
)

const longToken = "0123456789abcdef0123456789abcdef" // exactly minUITokenLen

func TestLoad_WebUIOffByDefault(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "")
	t.Setenv("DOCKFLARE_UI_TOKEN", "")

	cfg, err := Load(writeTemp(t, "containers: [web]\n"))
	if err != nil {
		t.Fatalf("a config without web_ui must keep working: %v", err)
	}
	if cfg.WebUI.Enabled {
		t.Error("web_ui.enabled must default to false")
	}
	if cfg.UIExposed() {
		t.Error("UIExposed must be false by default")
	}
}

func TestLoad_WebUIRequiresToken(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("DOCKFLARE_UI_TOKEN", "")

	_, err := Load(writeTemp(t, "web_ui:\n  enabled: true\n"))
	if err == nil {
		t.Fatal("the UI must not run unauthenticated")
	}
	if !strings.Contains(err.Error(), "DOCKFLARE_UI_TOKEN") {
		t.Errorf("error should name the env var: %v", err)
	}
}

func TestLoad_WebUIRejectsShortToken(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("DOCKFLARE_UI_TOKEN", "short")

	_, err := Load(writeTemp(t, "web_ui:\n  enabled: true\n"))
	if err == nil {
		t.Fatal("a guessable token must be rejected")
	}
	if !strings.Contains(err.Error(), "characters") {
		t.Errorf("error should explain the length requirement: %v", err)
	}
}

func TestLoad_WebUIDefaultPort(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("DOCKFLARE_UI_TOKEN", longToken)

	cfg, err := Load(writeTemp(t, "web_ui:\n  enabled: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebUI.Port != DefaultWebUIPort {
		t.Errorf("port = %d, want %d", cfg.WebUI.Port, DefaultWebUIPort)
	}
}

func TestLoad_WebUIInvalidPort(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("DOCKFLARE_UI_TOKEN", longToken)

	_, err := Load(writeTemp(t, "web_ui:\n  enabled: true\n  port: 70000\n"))
	if err == nil || !strings.Contains(err.Error(), "web_ui.port") {
		t.Fatalf("want a port error, got: %v", err)
	}
}

func TestLoad_WebUIHostnameWithoutEnabledIsAnError(t *testing.T) {
	// A hostname with the UI off is a config that does not do what it looks
	// like it does.
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("DOCKFLARE_UI_TOKEN", longToken)

	_, err := Load(writeTemp(t, "web_ui:\n  hostname: ui.example.com\n"))
	if err == nil || !strings.Contains(err.Error(), "enabled") {
		t.Fatalf("want an enabled/hostname mismatch error, got: %v", err)
	}
}

func TestLoad_WebUIHostnameRequiresRoutes(t *testing.T) {
	// Publishing the UI writes ingress. With no routes DockFlare does not manage
	// ingress at all, and starting to would wipe the dashboard's routing.
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "cfapi")
	t.Setenv("DOCKFLARE_UI_TOKEN", longToken)

	_, err := Load(writeTemp(t, `
containers: [web]
web_ui:
  enabled: true
  hostname: ui.example.com
`))
	if err == nil || !strings.Contains(err.Error(), "routes") {
		t.Fatalf("want an error requiring routes, got: %v", err)
	}
}

func TestLoad_WebUIHostnameCannotClashWithARoute(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "cfapi")
	t.Setenv("DOCKFLARE_UI_TOKEN", longToken)

	_, err := Load(writeTemp(t, `
routes:
  - hostname: ui.example.com
    container: web
    port: 80
web_ui:
  enabled: true
  hostname: ui.example.com
`))
	if err == nil || !strings.Contains(err.Error(), "also used by a route") {
		t.Fatalf("want a clash error, got: %v", err)
	}
}

func TestLoad_WebUIExposed(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "cfapi")
	t.Setenv("DOCKFLARE_UI_TOKEN", longToken)

	cfg, err := Load(writeTemp(t, `
routes:
  - hostname: a.example.com
    container: web
    port: 80
web_ui:
  enabled: true
  hostname: UI.Example.com
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.UIExposed() {
		t.Error("UIExposed should be true")
	}
	if cfg.WebUI.Hostname != "ui.example.com" {
		t.Errorf("hostname = %q, want it lowercased", cfg.WebUI.Hostname)
	}
}

func TestLoad_WebUIEnabledWithoutHostnameIsNotExposed(t *testing.T) {
	// Enabled but not published: reachable only on the port, which is the safe
	// default the example compose file publishes to 127.0.0.1.
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("DOCKFLARE_UI_TOKEN", longToken)

	cfg, err := Load(writeTemp(t, "web_ui:\n  enabled: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UIExposed() {
		t.Error("UIExposed must be false without a hostname")
	}
}

func TestHostnames(t *testing.T) {
	cfg := &Config{
		Routes: []Route{
			{Hostname: "a.example.com"},
			{Hostname: "b.example.com"},
		},
	}
	if got := cfg.Hostnames(); len(got) != 2 {
		t.Fatalf("Hostnames = %v, want the two routes", got)
	}

	// The UI's own hostname needs DNS just like a route's.
	cfg.WebUI = WebUI{Enabled: true, Hostname: "ui.example.com"}
	got := cfg.Hostnames()
	if len(got) != 3 || got[2] != "ui.example.com" {
		t.Errorf("Hostnames = %v, want the UI hostname appended", got)
	}

	// Not exposed → not published, so not listed.
	cfg.WebUI.Hostname = ""
	if got := cfg.Hostnames(); len(got) != 2 {
		t.Errorf("Hostnames = %v, want only the routes", got)
	}
}
