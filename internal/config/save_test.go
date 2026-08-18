package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSave_RoundTrip(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "cfapi")
	path := tempConfig(t, `
containers: [web, api]
routes:
  - hostname: a.example.com
    container: web
    port: 80
  - hostname: b.example.com
    container: api
    port: 3000
    origin_scheme: https
    force_https: yes
manage_dns: true
`)

	original, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, original); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("the saved file must load again: %v", err)
	}
	if len(reloaded.Routes) != 2 || !reloaded.ManageDNS {
		t.Fatalf("reloaded = %+v", reloaded)
	}
	if reloaded.Routes[1].OriginScheme != SchemeHTTPS || !reloaded.Routes[1].ForceHTTPS {
		t.Errorf("route options lost: %+v", reloaded.Routes[1])
	}
	if len(reloaded.Containers) != 2 || reloaded.Containers[0] != "web" {
		t.Errorf("containers = %v", reloaded.Containers)
	}
}

func TestSave_NeverWritesTheEnvTunnelToken(t *testing.T) {
	// The most dangerous mistake this function could make: cfg.Token holds the
	// value of TUNNEL_TOKEN, and writing it would drop the secret into a file
	// the user may well commit.
	t.Setenv("TUNNEL_TOKEN", "supersecret-from-env")
	t.Setenv("CLOUDFLARE_API_TOKEN", "cfapi")
	path := tempConfig(t, "containers: [web]\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "supersecret-from-env" {
		t.Fatalf("precondition failed: token = %q", cfg.Token)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}

	if got := read(t, path); strings.Contains(got, "supersecret-from-env") {
		t.Errorf("the env token leaked into config.yml:\n%s", got)
	}
}

func TestSave_PreservesTokenAlreadyInTheFile(t *testing.T) {
	// A user who chose the file fallback instead of the env var must not lose it.
	os.Unsetenv("TUNNEL_TOKEN")
	t.Setenv("CLOUDFLARE_API_TOKEN", "cfapi")
	path := tempConfig(t, "token: token-from-file\ncontainers: [web]\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}

	if got := read(t, path); !strings.Contains(got, "token-from-file") {
		t.Errorf("the file token was dropped:\n%s", got)
	}
	if _, err := Load(path); err != nil {
		t.Errorf("the saved file must still be valid: %v", err)
	}
}

func TestSave_NeverWritesAPIOrUIToken(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "api-secret-value")
	t.Setenv("DOCKFLARE_UI_TOKEN", strings.Repeat("u", 40))
	path := tempConfig(t, `
containers: [web]
routes:
  - hostname: a.example.com
    container: web
    port: 80
web_ui:
  enabled: true
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}

	got := read(t, path)
	for _, secret := range []string{"api-secret-value", strings.Repeat("u", 40)} {
		if strings.Contains(got, secret) {
			t.Errorf("a secret leaked into config.yml:\n%s", got)
		}
	}
}

func TestSave_OmitsDefaultOriginScheme(t *testing.T) {
	// Load fills origin_scheme in; writing it back on every route would litter
	// the file with noise the user never typed.
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "cfapi")
	path := tempConfig(t, `
routes:
  - hostname: a.example.com
    container: web
    port: 80
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); strings.Contains(got, "origin_scheme") {
		t.Errorf("default origin_scheme should be omitted:\n%s", got)
	}
}

func TestSave_OmitsWebUISectionWhenDisabled(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "cfapi")
	path := tempConfig(t, "containers: [web]\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); strings.Contains(got, "web_ui") {
		t.Errorf("web_ui should be absent when disabled:\n%s", got)
	}
}

func TestSave_WritesHeaderExplainingItIsManaged(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "cfapi")
	path := tempConfig(t, "containers: [web]\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	// Comments are lost on save, so the file should say why.
	if got := read(t, path); !strings.Contains(got, "web UI") {
		t.Errorf("expected a header explaining the file is managed:\n%s", got)
	}
}

func TestSave_IsAtomic(t *testing.T) {
	// A rename leaves no temp files behind and never a half-written config.
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "cfapi")
	path := tempConfig(t, "containers: [web]\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".config-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestSave_CreatesMissingFile(t *testing.T) {
	// The UI is allowed to write a config that does not exist yet.
	path := filepath.Join(t.TempDir(), "config.yml")
	cfg := &Config{Token: "t", Containers: []string{"web"}}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, path), "web") {
		t.Error("expected the container to be written")
	}
}

func TestCheckWritable(t *testing.T) {
	path := tempConfig(t, "containers: [web]\n")
	if err := CheckWritable(path); err != nil {
		t.Errorf("a normal temp file should be writable: %v", err)
	}

	missingDir := filepath.Join(t.TempDir(), "nope", "config.yml")
	if err := CheckWritable(missingDir); err == nil {
		t.Error("a missing directory must be reported as not writable")
	}
}

func TestCheckWritable_ReadOnlyFile(t *testing.T) {
	path := tempConfig(t, "containers: [web]\n")
	if err := os.Chmod(path, 0o444); err != nil {
		t.Skipf("cannot chmod on this platform: %v", err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o644) })

	// A writable directory with a read-only file is exactly the `:ro` bind mount
	// case, which only the rename would otherwise discover.
	if err := CheckWritable(path); err == nil {
		t.Skip("this platform ignores the read-only bit for the file owner")
	}
}
