// Package ingress turns DockFlare routes into a Cloudflare Tunnel ingress
// list and keeps the tunnel's remote configuration in sync with it.
//
// The destination of a route is always the container's *internal* address —
// http://<container-name>:<container-port> — resolved over the Docker network
// DockFlare has joined. Host-published ports are never used: the point is that
// traffic stays inside the Docker network.
package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/livresolucoes/dockflare/internal/cloudflare"
	"github.com/livresolucoes/dockflare/internal/config"
	"github.com/livresolucoes/dockflare/internal/docker"
)

// catchAllService terminates every ingress list. Cloudflare rejects a list
// without a final rule that has no hostname, and a tunnel whose last rule
// matched a real service would forward unknown hostnames to it.
const catchAllService = "http_status:404"

// ContainerInspector is the slice of *docker.Client this package needs.
type ContainerInspector interface {
	InspectContainer(ctx context.Context, nameOrID string) (*docker.ContainerInfo, error)
}

// NetworkSet reports which Docker networks DockFlare can reach. Implemented by
// *network.Manager.
type NetworkSet interface {
	// Reachable reports whether DockFlare has joined netName.
	Reachable(netName string) bool
	// Tracking is false when DockFlare cannot determine its own container ID,
	// in which case Reachable carries no information and is not enforced.
	Tracking() bool
}

// ValidationError collects everything wrong with the configured routes. It is
// distinguished from transport errors so callers can treat a bad config as
// fatal at startup while tolerating a flaky Cloudflare API.
type ValidationError struct {
	Errs []error
}

func (e *ValidationError) Error() string {
	msgs := make([]string, 0, len(e.Errs))
	for _, err := range e.Errs {
		msgs = append(msgs, err.Error())
	}
	return strings.Join(msgs, "; ")
}

func (e *ValidationError) Unwrap() []error { return e.Errs }

// noTLSVerifyKey is the only originRequest key DockFlare owns. Everything else
// in a rule's originRequest belongs to whoever set it (dashboard, terraform)
// and is carried through untouched.
const noTLSVerifyKey = "noTLSVerify"

// ServiceURL is the internal address cloudflared dials for a target.
func ServiceURL(t config.Target) string {
	scheme := t.Scheme
	if scheme == "" {
		scheme = config.SchemeHTTP
	}
	return fmt.Sprintf("%s://%s:%d", scheme, t.Container, t.Port)
}

// BuildIngress renders routes as an ingress list, always terminated by the
// catch-all rule. Routes are emitted in configuration order: Cloudflare
// evaluates ingress top-down, so the order the user wrote is the order that
// matters.
func BuildIngress(routes []config.Route) []cloudflare.IngressRule {
	rules := make([]cloudflare.IngressRule, 0, len(routes)+1)
	for _, r := range routes {
		// A route always has exactly one target today; Resolve guarantees it
		// before we get here.
		t := r.Targets()[0]
		rule := cloudflare.IngressRule{
			Hostname: r.Hostname,
			Service:  ServiceURL(t),
		}
		if t.Scheme == config.SchemeHTTPS {
			// A container's certificate cannot match its Docker network name,
			// so verification would fail for every https origin. Skipping it
			// costs nothing here: the hop never leaves the Docker network.
			rule.OriginRequest = mustSetOriginKey(nil, noTLSVerifyKey, true)
		}
		rules = append(rules, rule)
	}
	return append(rules, cloudflare.IngressRule{Service: catchAllService})
}

// WithUIRule publishes the web UI through the tunnel, inserted just before the
// catch-all so that rule stays last.
//
// The origin is plain localhost: cloudflared runs as a subprocess beside the UI
// inside the DockFlare container, so it needs no Docker network and no container
// name to reach it.
//
// A hostname with no rules to insert into is ignored — with no routes DockFlare
// does not manage ingress at all, and config validation rejects that pairing
// before it gets here.
func WithUIRule(rules []cloudflare.IngressRule, hostname string, port int) []cloudflare.IngressRule {
	if hostname == "" || len(rules) == 0 {
		return rules
	}
	out := make([]cloudflare.IngressRule, 0, len(rules)+1)
	out = append(out, rules[:len(rules)-1]...)
	out = append(out, cloudflare.IngressRule{
		Hostname: hostname,
		Service:  fmt.Sprintf("http://localhost:%d", port),
	})
	return append(out, rules[len(rules)-1])
}

// CarryOverOriginRequests copies each remote rule's originRequest onto the
// desired rule for the same hostname, then reapplies the one key DockFlare
// owns. Without this, writing the ingress would wipe per-hostname settings
// (connectTimeout, httpHostHeader, …) configured outside DockFlare.
func CarryOverOriginRequests(desired, remote []cloudflare.IngressRule) []cloudflare.IngressRule {
	byHostname := make(map[string]json.RawMessage, len(remote))
	for _, r := range remote {
		if len(r.OriginRequest) > 0 {
			byHostname[r.Hostname] = r.OriginRequest
		}
	}

	out := make([]cloudflare.IngressRule, len(desired))
	copy(out, desired)
	for i := range out {
		base, ok := byHostname[out[i].Hostname]
		if !ok {
			continue
		}
		// Ours wins on the key we own; theirs survives on every other key.
		wantNoTLSVerify := hasOriginKey(out[i].OriginRequest, noTLSVerifyKey)
		merged := mustSetOriginKey(base, noTLSVerifyKey, wantNoTLSVerify)
		if !wantNoTLSVerify {
			merged = mustDeleteOriginKey(base, noTLSVerifyKey)
		}
		out[i].OriginRequest = merged
	}
	return out
}

// SameRules reports whether two ingress lists are equivalent, originRequest
// included — DockFlare now owns a key inside it, so a difference there is a
// real difference.
func SameRules(a, b []cloudflare.IngressRule) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Hostname != b[i].Hostname ||
			a[i].Path != b[i].Path ||
			a[i].Service != b[i].Service {
			return false
		}
		if !sameOriginRequest(a[i].OriginRequest, b[i].OriginRequest) {
			return false
		}
	}
	return true
}

// sameOriginRequest compares two originRequest blobs by content rather than by
// bytes: JSON key order is not meaningful, and absent/null/{} all mean "unset".
func sameOriginRequest(a, b json.RawMessage) bool {
	return reflect.DeepEqual(decodeOriginRequest(a), decodeOriginRequest(b))
}

func decodeOriginRequest(raw json.RawMessage) map[string]any {
	out := map[string]any{}
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	if out == nil { // literal null
		return map[string]any{}
	}
	return out
}

func hasOriginKey(raw json.RawMessage, key string) bool {
	_, ok := decodeOriginRequest(raw)[key]
	return ok
}

// mustSetOriginKey and mustDeleteOriginKey never fail in practice: the input is
// either nil or a blob we already decoded, and the output is a plain map. On the
// impossible error they fall back to leaving the blob alone.
func mustSetOriginKey(raw json.RawMessage, key string, value any) json.RawMessage {
	fields := decodeOriginRequest(raw)
	fields[key] = value
	encoded, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return encoded
}

func mustDeleteOriginKey(raw json.RawMessage, key string) json.RawMessage {
	fields := decodeOriginRequest(raw)
	delete(fields, key)
	if len(fields) == 0 {
		return nil
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return encoded
}

// Resolve checks every route against live Docker state: the container must
// exist, DockFlare must be able to reach it on a Docker network, and the port
// must be one the container actually exposes.
//
// Returns a *ValidationError listing every problem found, or nil.
func Resolve(ctx context.Context, routes []config.Route, insp ContainerInspector, nets NetworkSet) error {
	var errs []error

	// Cache inspections: several routes commonly point at the same container.
	inspected := make(map[string]*docker.ContainerInfo, len(routes))

	for _, r := range routes {
		targets := r.Targets()
		if len(targets) != 1 {
			errs = append(errs, fmt.Errorf(
				"Route %s declares %d targets; this version supports exactly one.", r.Hostname, len(targets)))
			continue
		}
		t := targets[0]

		info, seen := inspected[t.Container]
		if !seen {
			var err error
			info, err = insp.InspectContainer(ctx, t.Container)
			if err != nil {
				errs = append(errs, fmt.Errorf(
					"Route %s references container %q but inspecting it failed: %v", r.Hostname, t.Container, err))
				continue
			}
			inspected[t.Container] = info
		}

		if info == nil {
			errs = append(errs, fmt.Errorf(
				"Route %s references container %q but that container was not found.", r.Hostname, t.Container))
			continue
		}

		if err := checkReachable(r.Hostname, t.Container, info, nets); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := checkPort(r.Hostname, t, info); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return &ValidationError{Errs: errs}
	}
	return nil
}

func checkReachable(hostname, container string, info *docker.ContainerInfo, nets NetworkSet) error {
	// Outside Docker we have no network membership to compare against, so the
	// check would only ever produce false negatives.
	if nets == nil || !nets.Tracking() {
		return nil
	}
	if len(info.Networks) == 0 {
		return fmt.Errorf(
			"Route %s references container %q, but that container is not attached to any Docker network.",
			hostname, container)
	}
	for _, n := range info.Networks {
		if nets.Reachable(n) {
			return nil
		}
	}
	return fmt.Errorf(
		"Route %s references container %q, but DockFlare is not connected to any of its Docker networks (%s). "+
			"Add %q to the containers list so DockFlare joins them.",
		hostname, container, strings.Join(info.Networks, ", "), container)
}

func checkPort(hostname string, t config.Target, info *docker.ContainerInfo) error {
	// A container that declares no exposed ports is normal (plenty of images
	// skip EXPOSE). We cannot disprove the port, so we accept it.
	if len(info.Ports) == 0 {
		return nil
	}
	for _, p := range info.Ports {
		if p == t.Port {
			return nil
		}
	}
	return fmt.Errorf(
		"Route %s references container %q, but port %d is not available. Exposed ports: %s.",
		hostname, t.Container, t.Port, formatPorts(info.Ports))
}

func formatPorts(ports []int) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, fmt.Sprintf("%d", p))
	}
	return strings.Join(parts, ", ")
}

// IsValidationError reports whether err came from route validation rather than
// from talking to Cloudflare.
func IsValidationError(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve)
}
