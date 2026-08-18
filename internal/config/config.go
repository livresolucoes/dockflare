package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	tunnelTokenEnv = "TUNNEL_TOKEN"
	apiTokenEnv    = "CLOUDFLARE_API_TOKEN"
	uiTokenEnv     = "DOCKFLARE_UI_TOKEN"

	// DefaultWebUIPort is where the UI listens inside the container. Reaching
	// it is a separate decision: publish the port, or set web_ui.hostname.
	DefaultWebUIPort = 8080

	// minUITokenLen keeps the UI from being protected by a guessable string.
	// Once published through the tunnel it is reachable from the internet, and
	// the token is the only thing between a stranger and your routing.
	minUITokenLen = 32
)

// WebUI configures the optional browser interface.
//
// Off by default, deliberately: the UI rewrites production routing and can
// write DNS records, so turning it on has to be a decision, not an accident.
type WebUI struct {
	Enabled bool `yaml:"enabled"`
	// Port the UI listens on inside the container.
	Port int `yaml:"port"`
	// Hostname, when set, publishes the UI through the tunnel itself — a second,
	// separate opt-in on top of Enabled. Because cloudflared runs beside the UI
	// in the same container, the origin is plain localhost: no Docker network,
	// no container name.
	//
	// Leaving it empty keeps the UI reachable only on Port, which is what the
	// example compose file publishes to 127.0.0.1.
	Hostname string `yaml:"hostname"`
}

// Origin schemes accepted by `origin_scheme`.
const (
	SchemeHTTP  = "http"
	SchemeHTTPS = "https"
)

// Target is one origin behind a route: a Docker container and the port it
// listens on *inside* the Docker network.
type Target struct {
	Container string
	Port      int
	// Scheme is how cloudflared talks to the container — the hop from the
	// tunnel into the Docker network, not how the browser talks to Cloudflare.
	// Always populated after Load; defaults to SchemeHTTP.
	Scheme string
}

// Route maps a public hostname to a container. Only the single-target form
// (container/port on the route itself) is accepted today; Targets is the seam
// a future `targets:` list — several origins behind one hostname — plugs into
// without reshaping every consumer downstream.
type Route struct {
	Hostname  string `yaml:"hostname"`
	Container string `yaml:"container"`
	Port      int    `yaml:"port"`
	// OriginScheme is `http` (default) or `https`, and describes what the
	// *container* speaks. Use `https` only for containers that refuse plain
	// HTTP; certificate verification is skipped, since a container's cert
	// cannot match its Docker network name.
	OriginScheme string `yaml:"origin_scheme,omitempty"`
	// ForceHTTPS redirects http→https for this hostname at the Cloudflare
	// edge. Scoped to this hostname alone — other hostnames in the same zone
	// are untouched.
	ForceHTTPS bool `yaml:"force_https,omitempty"`
}

// Targets returns the route's origins. Always exactly one in this version.
func (r Route) Targets() []Target {
	return []Target{{Container: r.Container, Port: r.Port, Scheme: r.OriginScheme}}
}

type Config struct {
	Token      string   `yaml:"token"`
	Containers []string `yaml:"containers"`
	// Routes is optional. When empty, DockFlare never touches the tunnel's
	// ingress and routing stays owned by the Zero Trust dashboard.
	Routes []Route `yaml:"routes"`
	// ManageDNS additionally creates/updates the proxied CNAME each route
	// hostname needs. Off by default: writing DNS records into the user's zone
	// is a bigger blast radius than writing ingress.
	ManageDNS bool `yaml:"manage_dns"`
	// WebUI is the optional browser interface. Absent → off.
	WebUI WebUI `yaml:"web_ui"`

	// APIToken authenticates DockFlare against the Cloudflare REST API, and
	// UIToken authenticates browsers against the web UI. Both are deliberately
	// not YAML fields — they only ever come from the environment, so a secret
	// can never end up in config.yml, not even when the UI rewrites the file.
	APIToken string `yaml:"-"`
	UIToken  string `yaml:"-"`
}

// Hostnames lists every hostname DockFlare publishes: the routes plus, when the
// UI is exposed, its own. Used for DNS and for duplicate detection.
func (c *Config) Hostnames() []string {
	out := make([]string, 0, len(c.Routes)+1)
	for _, r := range c.Routes {
		out = append(out, r.Hostname)
	}
	if c.UIExposed() {
		out = append(out, c.WebUI.Hostname)
	}
	return out
}

// UIExposed reports whether the UI is published through the tunnel.
func (c *Config) UIExposed() bool {
	return c.WebUI.Enabled && c.WebUI.Hostname != ""
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	// Environment variable takes precedence over config file token.
	if t := os.Getenv(tunnelTokenEnv); t != "" {
		cfg.Token = t
	}
	cfg.APIToken = os.Getenv(apiTokenEnv)
	cfg.UIToken = os.Getenv(uiTokenEnv)
	normalizeRoutes(cfg.Routes)
	normalizeWebUI(&cfg.WebUI)
	if err := Validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Normalize applies the same canonicalisation Load applies, for a Config built
// in memory — from the web UI, or from a test.
func Normalize(c *Config) {
	normalizeRoutes(c.Routes)
	normalizeWebUI(&c.WebUI)
}

func normalizeWebUI(w *WebUI) {
	w.Hostname = strings.ToLower(strings.TrimSpace(w.Hostname))
	if w.Port == 0 {
		w.Port = DefaultWebUIPort
	}
}

// normalizeRoutes trims whitespace, lowercases hostnames and fills in the
// default origin scheme, in place. Doing it here means duplicate detection,
// comparison against the remote ingress and every consumer downstream all see
// one canonical form.
func normalizeRoutes(routes []Route) {
	for i := range routes {
		routes[i].Hostname = strings.ToLower(strings.TrimSpace(routes[i].Hostname))
		routes[i].Container = strings.TrimSpace(routes[i].Container)
		routes[i].OriginScheme = strings.ToLower(strings.TrimSpace(routes[i].OriginScheme))
		if routes[i].OriginScheme == "" {
			routes[i].OriginScheme = SchemeHTTP
		}
	}
}

func Validate(c *Config) error {
	if c.Token == "" {
		return fmt.Errorf("config: token is required — set TUNNEL_TOKEN env var or token field in config.yml")
	}
	if err := ValidateRoutes(c.Routes); err != nil {
		return err
	}
	if len(c.Routes) > 0 && c.APIToken == "" {
		return fmt.Errorf("config: %d route(s) configured but %s is not set — "+
			"automatic ingress needs a Cloudflare API token with the "+
			"\"Account · Cloudflare Tunnel · Edit\" permission", len(c.Routes), apiTokenEnv)
	}
	return validateWebUI(c)
}

func validateWebUI(c *Config) error {
	w := c.WebUI
	if !w.Enabled {
		// A hostname with the UI off is a config that does not do what it looks
		// like it does, so say so rather than silently ignoring it.
		if w.Hostname != "" {
			return fmt.Errorf("config: web_ui.hostname is set but web_ui.enabled is false — " +
				"set enabled: true to serve the UI, or remove the hostname")
		}
		return nil
	}

	if w.Port < 1 || w.Port > 65535 {
		return fmt.Errorf("config: web_ui.port %d is invalid (must be 1-65535)", w.Port)
	}
	if c.UIToken == "" {
		return fmt.Errorf("config: web_ui.enabled is true but %s is not set — "+
			"the UI edits production routing and must not run unauthenticated. "+
			"Generate one with: openssl rand -hex 32", uiTokenEnv)
	}
	if len(c.UIToken) < minUITokenLen {
		return fmt.Errorf("config: %s is only %d characters; at least %d are required "+
			"because the UI can be reached from the internet. "+
			"Generate one with: openssl rand -hex 32", uiTokenEnv, len(c.UIToken), minUITokenLen)
	}

	if w.Hostname == "" {
		return nil
	}
	if !validHostname(w.Hostname) {
		return fmt.Errorf("config: web_ui.hostname %q is not a valid hostname", w.Hostname)
	}
	// Publishing the UI means adding an ingress rule. With no routes DockFlare
	// never touches ingress at all, and starting to would wipe whatever the Zero
	// Trust dashboard is serving — so require the opt-in to be explicit.
	if len(c.Routes) == 0 {
		return fmt.Errorf("config: web_ui.hostname needs at least one entry under routes — " +
			"publishing the UI writes the tunnel ingress, and with no routes DockFlare " +
			"leaves ingress to the Zero Trust dashboard")
	}
	for _, r := range c.Routes {
		if r.Hostname == w.Hostname {
			return fmt.Errorf("config: web_ui.hostname %s is also used by a route", w.Hostname)
		}
	}
	return nil
}

// ValidateRoutes checks everything about the routes that can be known without
// talking to Docker: required fields, port range and duplicate hostnames.
// Container existence, network reachability and port availability are checked
// later by the ingress package, which has a Docker client.
//
// All problems are reported at once rather than stopping at the first, so a
// user fixing a config sees the whole list.
func ValidateRoutes(routes []Route) error {
	var errs []error
	seen := make(map[string]bool, len(routes))

	for i, r := range routes {
		// label identifies the route in messages; routes without a hostname
		// fall back to their position in the list.
		label := r.Hostname
		switch {
		case r.Hostname == "":
			label = fmt.Sprintf("#%d", i+1)
			errs = append(errs, fmt.Errorf("route %s is missing a hostname", label))
		case !validHostname(r.Hostname):
			errs = append(errs, fmt.Errorf("route %s has an invalid hostname", r.Hostname))
		case seen[r.Hostname]:
			errs = append(errs, fmt.Errorf("route %s is declared more than once", r.Hostname))
		}
		seen[r.Hostname] = true

		for _, t := range r.Targets() {
			if t.Container == "" {
				errs = append(errs, fmt.Errorf("route %s is missing a container", label))
			}
			switch {
			case t.Port == 0:
				errs = append(errs, fmt.Errorf("route %s is missing a port", label))
			case t.Port < 1 || t.Port > 65535:
				errs = append(errs, fmt.Errorf("route %s has an invalid port %d (must be 1-65535)", label, t.Port))
			}
			// Empty is valid: normalizeRoutes fills the default. It stays
			// reachable here so a hand-built Config is validated too.
			switch t.Scheme {
			case "", SchemeHTTP, SchemeHTTPS:
			default:
				errs = append(errs, fmt.Errorf(
					"route %s has an invalid origin_scheme %q (must be %s or %s)",
					label, t.Scheme, SchemeHTTP, SchemeHTTPS))
			}
		}
	}
	return errors.Join(errs...)
}

// validHostname accepts DNS hostnames of at least two labels, optionally with
// a leading wildcard label (Cloudflare accepts `*.example.com` in ingress).
func validHostname(h string) bool {
	if strings.ContainsAny(h, " /:\\") {
		return false
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return false
	}
	for i, l := range labels {
		if l == "" {
			return false
		}
		if l == "*" {
			if i != 0 {
				return false
			}
			continue
		}
		if strings.HasPrefix(l, "-") || strings.HasSuffix(l, "-") {
			return false
		}
		for _, c := range l {
			isAlnum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
			if !isAlnum && c != '-' {
				return false
			}
		}
	}
	return true
}
