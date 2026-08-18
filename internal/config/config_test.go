package config

import (
	"os"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config*.yml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

func TestLoad_TokenFromEnv(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhenv")
	path := writeTemp(t, `containers: [app, grafana]`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "eyJhenv" {
		t.Errorf("token = %q, want eyJhenv", cfg.Token)
	}
	if len(cfg.Containers) != 2 {
		t.Errorf("containers = %v, want [app grafana]", cfg.Containers)
	}
}

func TestLoad_TokenFromFile(t *testing.T) {
	os.Unsetenv("TUNNEL_TOKEN")
	path := writeTemp(t, `
token: eyJhfile
containers:
  - app
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "eyJhfile" {
		t.Errorf("token = %q, want eyJhfile", cfg.Token)
	}
}

func TestLoad_EnvOverridesFileToken(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "fromenv")
	path := writeTemp(t, `token: fromfile`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "fromenv" {
		t.Errorf("token = %q, want fromenv (env should win)", cfg.Token)
	}
}

func TestLoad_MissingToken(t *testing.T) {
	os.Unsetenv("TUNNEL_TOKEN")
	path := writeTemp(t, `containers: [app]`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing token")
	}
}

func TestLoad_EmptyContainersIsValid(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	path := writeTemp(t, `token: eyJhtest`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Containers) != 0 {
		t.Errorf("expected 0 containers, got %d", len(cfg.Containers))
	}
}

func TestLoad_ContainersList(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	path := writeTemp(t, `
token: eyJhtest
containers:
  - app
  - grafana
  - api
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Containers) != 3 {
		t.Fatalf("containers count = %d, want 3", len(cfg.Containers))
	}
	if cfg.Containers[1] != "grafana" {
		t.Errorf("containers[1] = %q, want grafana", cfg.Containers[1])
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}
