// Package cloudflare is the single place in DockFlare that speaks HTTP to the
// Cloudflare REST API (https://developers.cloudflare.com/api/). Nothing else
// in the codebase builds a Cloudflare request.
//
// Authentication is a Bearer API token supplied via the CLOUDFLARE_API_TOKEN
// environment variable; it is carried in a header and is never logged, never
// placed in a URL, and never included in an error message.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/livresolucoes/dockflare/internal/logger"
)

const (
	defaultBaseURL = "https://api.cloudflare.com/client/v4"
	requestTimeout = 30 * time.Second
	maxBodyBytes   = 4 << 20 // 4 MiB — plenty for any config/zone payload
	zonesPerPage   = 50

	// tunnelDNSSuffix is the CNAME target shape for a Cloudflare Tunnel:
	// <tunnel-id>.cfargotunnel.com
	tunnelDNSSuffix = ".cfargotunnel.com"

	// redirectPhase is the ruleset phase that holds Redirect Rules.
	redirectPhase = "http_request_dynamic_redirect"

	// ruleMarker prefixes the description of every rule DockFlare owns, so a
	// zone's other rules can be told apart and preserved. The version suffix
	// lets a future change to the rule template rewrite existing rules instead
	// of silently leaving stale ones behind.
	ruleMarker  = "dockflare:"
	ruleVersion = "v1"
)

// ErrNotFound reports a 404 from the API. Some endpoints answer 404 for
// "nothing configured yet", which is not an error to the caller.
var ErrNotFound = errors.New("not found")

// IngressRule is one entry of a tunnel's ingress list. The last rule of a
// valid list has no Hostname and acts as the catch-all.
type IngressRule struct {
	Hostname string `json:"hostname,omitempty"`
	Path     string `json:"path,omitempty"`
	Service  string `json:"service"`
	// OriginRequest is kept opaque so per-rule settings configured elsewhere
	// (dashboard, terraform) survive a round-trip through DockFlare.
	OriginRequest json.RawMessage `json:"originRequest,omitempty"`
}

// TunnelConfig is the `config` object of a remotely-managed tunnel. DockFlare
// only ever rewrites Ingress; the other fields are carried through untouched.
type TunnelConfig struct {
	Ingress       []IngressRule   `json:"ingress"`
	WarpRouting   json.RawMessage `json:"warp-routing,omitempty"`
	OriginRequest json.RawMessage `json:"originRequest,omitempty"`
}

// Zone is the subset of a Cloudflare zone DockFlare needs.
type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// DNSRecord is the subset of a DNS record DockFlare reads or writes.
type DNSRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl,omitempty"`
}

type Client struct {
	apiToken string
	baseURL  string
	http     *http.Client
	log      *logger.Logger

	mu    sync.Mutex
	zones []Zone // cached after the first lookup; zone lists rarely change
	// redirectZones remembers which zones we have written redirect rules into,
	// so that removing the last force_https hostname of a zone still cleans
	// that zone's rule up.
	redirectZones map[string]bool
}

func NewClient(apiToken string, log *logger.Logger) *Client {
	return &Client{
		apiToken:      apiToken,
		baseURL:       defaultBaseURL,
		http:          &http.Client{Timeout: requestTimeout},
		log:           log,
		redirectZones: make(map[string]bool),
	}
}

// SetBaseURL points the client at an alternate API endpoint. Used by tests.
func (c *Client) SetBaseURL(u string) {
	c.baseURL = strings.TrimSuffix(u, "/")
}

// GetTunnelConfiguration reads the tunnel's current remotely-managed config.
// Returns a zero config (not an error) for a tunnel that has never been
// configured, since the API answers that with `"config": null`.
//
// GET /accounts/{account_id}/cfd_tunnel/{tunnel_id}/configurations
func (c *Client) GetTunnelConfiguration(ctx context.Context, accountID, tunnelID string) (*TunnelConfig, error) {
	var out struct {
		TunnelID string        `json:"tunnel_id"`
		Version  int           `json:"version"`
		Config   *TunnelConfig `json:"config"`
	}
	path := fmt.Sprintf("/accounts/%s/cfd_tunnel/%s/configurations", accountID, tunnelID)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	if out.Config == nil {
		return &TunnelConfig{}, nil
	}
	return out.Config, nil
}

// PutTunnelConfiguration replaces the tunnel's remotely-managed config. This
// is the same endpoint the Zero Trust dashboard writes when you add a Public
// Hostname.
//
// PUT /accounts/{account_id}/cfd_tunnel/{tunnel_id}/configurations
func (c *Client) PutTunnelConfiguration(ctx context.Context, accountID, tunnelID string, cfg *TunnelConfig) error {
	body := struct {
		Config *TunnelConfig `json:"config"`
	}{Config: cfg}
	path := fmt.Sprintf("/accounts/%s/cfd_tunnel/%s/configurations", accountID, tunnelID)
	return c.do(ctx, http.MethodPut, path, body, nil)
}

// EnsureTunnelCNAME makes hostname resolve to the tunnel via a proxied CNAME.
// Reports whether it had to write anything.
//
// A pre-existing record that does not already point at a tunnel is left alone
// and reported as an error: silently repointing someone's A record at our
// tunnel would be the wrong kind of automatic.
func (c *Client) EnsureTunnelCNAME(ctx context.Context, hostname, tunnelID string) (bool, error) {
	zone, err := c.zoneFor(ctx, hostname)
	if err != nil {
		return false, err
	}
	content := tunnelID + tunnelDNSSuffix

	existing, err := c.findDNSRecord(ctx, zone.ID, hostname)
	if err != nil {
		return false, err
	}
	want := DNSRecord{Type: "CNAME", Name: hostname, Content: content, Proxied: true, TTL: 1}

	switch {
	case existing == nil:
		if err := c.do(ctx, http.MethodPost, "/zones/"+zone.ID+"/dns_records", want, nil); err != nil {
			return false, err
		}
		return true, nil

	case existing.Type == "CNAME" && existing.Content == content && existing.Proxied:
		return false, nil

	case existing.Type == "CNAME" && strings.HasSuffix(existing.Content, tunnelDNSSuffix):
		path := "/zones/" + zone.ID + "/dns_records/" + existing.ID
		if err := c.do(ctx, http.MethodPatch, path, want, nil); err != nil {
			return false, err
		}
		return true, nil

	default:
		return false, fmt.Errorf(
			"DNS record for %s already exists as %s → %s and does not point at a tunnel; leaving it untouched",
			hostname, existing.Type, existing.Content)
	}
}

// zoneFor finds the zone that owns hostname, preferring the longest match so
// that a delegated subdomain zone wins over its parent.
func (c *Client) zoneFor(ctx context.Context, hostname string) (*Zone, error) {
	zones, err := c.listZones(ctx)
	if err != nil {
		return nil, err
	}
	var best *Zone
	for i := range zones {
		z := &zones[i]
		if hostname != z.Name && !strings.HasSuffix(hostname, "."+z.Name) {
			continue
		}
		if best == nil || len(z.Name) > len(best.Name) {
			best = z
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no Cloudflare zone in this account owns %s", hostname)
	}
	return best, nil
}

func (c *Client) listZones(ctx context.Context) ([]Zone, error) {
	c.mu.Lock()
	cached := c.zones
	c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}

	var all []Zone
	for page := 1; ; page++ {
		var out []Zone
		path := fmt.Sprintf("/zones?per_page=%d&page=%d", zonesPerPage, page)
		if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out...)
		if len(out) < zonesPerPage {
			break
		}
	}

	c.mu.Lock()
	c.zones = all
	c.mu.Unlock()
	return all, nil
}

// findDNSRecord returns the record for name, or (nil, nil) if none exists.
func (c *Client) findDNSRecord(ctx context.Context, zoneID, name string) (*DNSRecord, error) {
	var out []DNSRecord
	path := "/zones/" + zoneID + "/dns_records?name=" + url.QueryEscape(name)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

// apiEnvelope is the response shape every Cloudflare v4 endpoint shares.
type apiEnvelope struct {
	Success bool            `json:"success"`
	Errors  []apiError      `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e apiEnvelope) errorText() string {
	if len(e.Errors) == 0 {
		return "no error detail returned"
	}
	parts := make([]string, 0, len(e.Errors))
	for _, err := range e.Errors {
		parts = append(parts, fmt.Sprintf("%d: %s", err.Code, err.Message))
	}
	return strings.Join(parts, "; ")
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	if c.apiToken == "" {
		return fmt.Errorf("cloudflare api: no API token configured")
	}

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("cloudflare api %s %s: encoding request: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("cloudflare api %s %s: building request: %w", method, path, err)
	}
	// The token lives only in this header — never in the URL, never in a log.
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare api %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("cloudflare api %s %s: reading response: %w", method, path, err)
	}

	var env apiEnvelope
	if jsonErr := json.Unmarshal(data, &env); jsonErr != nil {
		return fmt.Errorf("cloudflare api %s %s: status %d with an unreadable response body",
			method, path, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNotFound {
		// Callers that treat "nothing configured yet" as empty check for this.
		return fmt.Errorf("cloudflare api %s %s: %w: %s", method, path, ErrNotFound, env.errorText())
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 || !env.Success {
		return fmt.Errorf("cloudflare api %s %s: status %d: %s",
			method, path, resp.StatusCode, env.errorText())
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("cloudflare api %s %s: decoding result: %w", method, path, err)
		}
	}
	return nil
}
