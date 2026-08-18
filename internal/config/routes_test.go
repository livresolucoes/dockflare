package config

import (
	"strings"
	"testing"
)

func TestLoad_NoRoutesIsValid(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "")
	path := writeTemp(t, `
containers:
  - app
  - grafana
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("a config without routes must keep working: %v", err)
	}
	if len(cfg.Routes) != 0 {
		t.Errorf("routes = %v, want none", cfg.Routes)
	}
	if cfg.ManageDNS {
		t.Error("manage_dns must default to false")
	}
}

func TestLoad_ParsesRoutes(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "cfapi")
	path := writeTemp(t, `
containers:
  - meuapp_web
  - meuapp_api

routes:
  - hostname: meuapp.example.com
    container: meuapp_web
    port: 4000

  - hostname: api.meuapp.example.com
    container: meuapp_api
    port: 3000
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routes) != 2 {
		t.Fatalf("routes count = %d, want 2", len(cfg.Routes))
	}
	if cfg.Routes[1].Hostname != "api.meuapp.example.com" {
		t.Errorf("routes[1].Hostname = %q", cfg.Routes[1].Hostname)
	}
	if cfg.Routes[1].Container != "meuapp_api" || cfg.Routes[1].Port != 3000 {
		t.Errorf("routes[1] = %+v", cfg.Routes[1])
	}
}

func TestLoad_NormalizesHostname(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "cfapi")
	path := writeTemp(t, `
routes:
  - hostname: "  API.Example.COM  "
    container: "  api  "
    port: 3000
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routes[0].Hostname != "api.example.com" {
		t.Errorf("hostname = %q, want api.example.com", cfg.Routes[0].Hostname)
	}
	if cfg.Routes[0].Container != "api" {
		t.Errorf("container = %q, want api", cfg.Routes[0].Container)
	}
}

func TestLoad_RoutesRequireAPIToken(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "")
	path := writeTemp(t, `
routes:
  - hostname: api.example.com
    container: api
    port: 3000
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error when routes are set without CLOUDFLARE_API_TOKEN")
	}
	if !strings.Contains(err.Error(), "CLOUDFLARE_API_TOKEN") {
		t.Errorf("error should name the env var, got: %v", err)
	}
}

func TestLoad_APITokenNeverComesFromFile(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "")
	// A user putting the secret in the file must not have it honoured.
	path := writeTemp(t, `
api_token: shouldbeignored
apiToken: shouldbeignored
routes:
  - hostname: api.example.com
    container: api
    port: 3000
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("api token read from config.yml — it must only come from the env var")
	}
}

func TestValidateRoutes(t *testing.T) {
	cases := []struct {
		name   string
		routes []Route
		want   string // substring expected in the error; "" means valid
	}{
		{
			name:   "valid single route",
			routes: []Route{{Hostname: "api.example.com", Container: "api", Port: 3000}},
		},
		{
			name:   "no routes at all",
			routes: nil,
		},
		{
			name:   "missing hostname",
			routes: []Route{{Container: "api", Port: 3000}},
			want:   "route #1 is missing a hostname",
		},
		{
			name:   "missing container",
			routes: []Route{{Hostname: "api.example.com", Port: 3000}},
			want:   "route api.example.com is missing a container",
		},
		{
			name:   "missing port",
			routes: []Route{{Hostname: "api.example.com", Container: "api"}},
			want:   "route api.example.com is missing a port",
		},
		{
			name:   "port above range",
			routes: []Route{{Hostname: "api.example.com", Container: "api", Port: 70000}},
			want:   "invalid port 70000",
		},
		{
			name:   "negative port",
			routes: []Route{{Hostname: "api.example.com", Container: "api", Port: -1}},
			want:   "invalid port -1",
		},
		{
			name: "duplicate hostname",
			routes: []Route{
				{Hostname: "api.example.com", Container: "api", Port: 3000},
				{Hostname: "api.example.com", Container: "other", Port: 8080},
			},
			want: "route api.example.com is declared more than once",
		},
		{
			name:   "hostname with a scheme",
			routes: []Route{{Hostname: "https://api.example.com", Container: "api", Port: 3000}},
			want:   "invalid hostname",
		},
		{
			name:   "single label hostname",
			routes: []Route{{Hostname: "localhost", Container: "api", Port: 3000}},
			want:   "invalid hostname",
		},
		{
			name:   "wildcard hostname is allowed",
			routes: []Route{{Hostname: "*.example.com", Container: "api", Port: 3000}},
		},
		{
			name:   "wildcard not in first label",
			routes: []Route{{Hostname: "api.*.example.com", Container: "api", Port: 3000}},
			want:   "invalid hostname",
		},
		{
			name:   "port 1 and 65535 are in range",
			routes: []Route{{Hostname: "a.example.com", Container: "a", Port: 65535}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRoutes(tc.routes)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("expected valid, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestValidateRoutes_ReportsEveryProblemAtOnce(t *testing.T) {
	err := ValidateRoutes([]Route{
		{Hostname: "a.example.com"},                              // no container, no port
		{Hostname: "a.example.com", Container: "x", Port: 3000},  // duplicate
		{Hostname: "b.example.com", Container: "y", Port: 99999}, // bad port
	})
	if err == nil {
		t.Fatal("expected errors")
	}
	msg := err.Error()
	for _, want := range []string{
		"is missing a container",
		"is missing a port",
		"declared more than once",
		"invalid port 99999",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q; got:\n%s", want, msg)
		}
	}
}

func TestRoute_TargetsIsSingleOrigin(t *testing.T) {
	r := Route{Hostname: "api.example.com", Container: "api", Port: 3000, OriginScheme: SchemeHTTPS}
	got := r.Targets()
	if len(got) != 1 {
		t.Fatalf("Targets() length = %d, want 1", len(got))
	}
	if got[0] != (Target{Container: "api", Port: 3000, Scheme: SchemeHTTPS}) {
		t.Errorf("Targets()[0] = %+v", got[0])
	}
}

func TestLoad_OriginSchemeDefaultsToHTTP(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "cfapi")
	path := writeTemp(t, `
routes:
  - hostname: api.example.com
    container: api
    port: 3000
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routes[0].OriginScheme != SchemeHTTP {
		t.Errorf("origin_scheme = %q, want %q", cfg.Routes[0].OriginScheme, SchemeHTTP)
	}
	if cfg.Routes[0].ForceHTTPS {
		t.Error("force_https must default to false")
	}
}

func TestLoad_OriginSchemeAndForceHTTPS(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "cfapi")
	path := writeTemp(t, `
routes:
  - hostname: vault.example.com
    container: vault
    port: 8200
    origin_scheme: HTTPS
    force_https: yes
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// Case is normalised, so HTTPS and https are the same thing.
	if cfg.Routes[0].OriginScheme != SchemeHTTPS {
		t.Errorf("origin_scheme = %q, want %q", cfg.Routes[0].OriginScheme, SchemeHTTPS)
	}
	// `yes` is the spelling most people reach for; it must work.
	if !cfg.Routes[0].ForceHTTPS {
		t.Error("force_https: yes should parse as true")
	}
}

func TestLoad_ForceHTTPSSpellings(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "cfapi")
	for _, spelling := range []string{"yes", "true", "on", "Yes", "TRUE"} {
		t.Run(spelling, func(t *testing.T) {
			path := writeTemp(t, `
routes:
  - hostname: a.example.com
    container: a
    port: 80
    force_https: `+spelling+`
`)
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.Routes[0].ForceHTTPS {
				t.Errorf("force_https: %s did not parse as true", spelling)
			}
		})
	}
}

func TestValidateRoutes_OriginScheme(t *testing.T) {
	cases := []struct {
		scheme string
		want   string // "" means valid
	}{
		{"", ""},
		{SchemeHTTP, ""},
		{SchemeHTTPS, ""},
		{"ftp", `has an invalid origin_scheme "ftp"`},
		{"tcp", `has an invalid origin_scheme "tcp"`},
		{"https://", `has an invalid origin_scheme`},
	}
	for _, tc := range cases {
		t.Run(tc.scheme, func(t *testing.T) {
			err := ValidateRoutes([]Route{
				{Hostname: "a.example.com", Container: "a", Port: 80, OriginScheme: tc.scheme},
			})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("expected valid, got: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestLoad_InvalidOriginSchemeIsRejected(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "cfapi")
	path := writeTemp(t, `
routes:
  - hostname: a.example.com
    container: a
    port: 80
    origin_scheme: ftp
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "origin_scheme") {
		t.Fatalf("want a clear origin_scheme error, got: %v", err)
	}
}
