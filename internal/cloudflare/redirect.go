package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
)

// A Redirect Rule lives in a zone's `http_request_dynamic_redirect` ruleset,
// which is a single ordered list per zone. Writing it means replacing the whole
// list, so DockFlare has the same whole-state problem it has with ingress —
// except here the list is shared with rules it does not own.
//
// Two rules make that safe:
//
//   - Rules DockFlare created are tagged in their description with ruleMarker.
//     Everything untagged is read, preserved and written back verbatim.
//   - DockFlare's rules are appended last, so an existing rule that matches the
//     same request keeps winning. We never shadow someone else's redirect.

// redirectRule is the rule DockFlare writes. Foreign rules are never decoded
// into this type — they stay opaque.
type redirectRule struct {
	Action           string         `json:"action"`
	ActionParameters redirectParams `json:"action_parameters"`
	Expression       string         `json:"expression"`
	Description      string         `json:"description"`
	Enabled          bool           `json:"enabled"`
}

type redirectParams struct {
	FromValue redirectFromValue `json:"from_value"`
}

type redirectFromValue struct {
	StatusCode int               `json:"status_code"`
	TargetURL  redirectTargetURL `json:"target_url"`
	// PreserveQueryString is false because the target expression already uses
	// http.request.uri, which carries the query string. Leaving it on would
	// append it twice.
	PreserveQueryString bool `json:"preserve_query_string"`
}

type redirectTargetURL struct {
	Expression string `json:"expression"`
}

// httpsRedirectRule builds the rule that sends plain-HTTP requests for one
// hostname to the same URL over HTTPS.
func httpsRedirectRule(hostname string) redirectRule {
	return redirectRule{
		Action:     "redirect",
		Expression: fmt.Sprintf("(http.host eq %q and not ssl)", hostname),
		// The description is the identity of the rule: it is how a later sync
		// recognises its own work and how ruleVersion forces a rewrite.
		Description: ruleDescription(hostname),
		Enabled:     true,
		ActionParameters: redirectParams{
			FromValue: redirectFromValue{
				StatusCode:          301,
				TargetURL:           redirectTargetURL{Expression: `concat("https://", http.host, http.request.uri)`},
				PreserveQueryString: false,
			},
		},
	}
}

func ruleDescription(hostname string) string {
	return fmt.Sprintf("%s%s force-https %s", ruleMarker, ruleVersion, hostname)
}

// SyncHTTPSRedirects makes exactly the given hostnames redirect http→https, and
// removes DockFlare's rules for hostnames no longer listed. Returns the names
// of the zones it had to write to; an empty slice means everything already
// matched and no request was made.
//
// Hostnames are grouped by zone because the ruleset is per-zone: two hostnames
// in the same zone cost one read and one write, not two of each.
func (c *Client) SyncHTTPSRedirects(ctx context.Context, hostnames []string) ([]string, error) {
	byZone := make(map[string][]string)
	zoneNames := make(map[string]string)
	for _, h := range hostnames {
		zone, err := c.zoneFor(ctx, h)
		if err != nil {
			return nil, err
		}
		byZone[zone.ID] = append(byZone[zone.ID], h)
		zoneNames[zone.ID] = zone.Name
	}

	// Revisit zones we wrote to before, even with no hostnames left: that is
	// what deletes the rule when force_https is switched off.
	c.mu.Lock()
	for id := range c.redirectZones {
		if _, ok := byZone[id]; !ok {
			byZone[id] = nil
			zoneNames[id] = id
		}
	}
	c.mu.Unlock()

	zoneIDs := make([]string, 0, len(byZone))
	for id := range byZone {
		zoneIDs = append(zoneIDs, id)
	}
	sort.Strings(zoneIDs) // deterministic order across runs

	var (
		written []string
		errs    []error
	)
	for _, zoneID := range zoneIDs {
		hosts := byZone[zoneID]
		changed, err := c.syncZoneRedirects(ctx, zoneID, hosts)
		if err != nil {
			// One zone failing — typically a permission scoped to some zones
			// but not others — must not stop the remaining zones. Collect and
			// carry on; the caller reports every failure at once.
			//
			// The zone is deliberately left out of redirectZones: a failed PUT
			// wrote nothing, so there is nothing to clean up later.
			errs = append(errs, fmt.Errorf("zone %s: %w", zoneNames[zoneID], err))
			continue
		}

		c.mu.Lock()
		if len(hosts) > 0 {
			c.redirectZones[zoneID] = true
		} else {
			delete(c.redirectZones, zoneID)
		}
		c.mu.Unlock()

		if changed {
			written = append(written, zoneNames[zoneID])
		}
	}
	return written, errors.Join(errs...)
}

func (c *Client) syncZoneRedirects(ctx context.Context, zoneID string, hostnames []string) (bool, error) {
	existing, err := c.redirectRules(ctx, zoneID)
	if err != nil {
		return false, err
	}

	var foreign []json.RawMessage
	var oursNow []string
	for _, raw := range existing {
		desc, isOurs := ruleOwnership(raw)
		if isOurs {
			oursNow = append(oursNow, desc)
			continue
		}
		clean, err := stripReadOnlyFields(raw)
		if err != nil {
			return false, fmt.Errorf("reading existing redirect rules: %w", err)
		}
		foreign = append(foreign, clean)
	}

	wanted := make([]string, 0, len(hostnames))
	sorted := append([]string(nil), hostnames...)
	sort.Strings(sorted)
	for _, h := range sorted {
		wanted = append(wanted, ruleDescription(h))
	}

	sort.Strings(oursNow)
	if equalStrings(oursNow, wanted) {
		return false, nil
	}

	// Ours last: a pre-existing rule matching the same request still wins.
	rules := make([]any, 0, len(foreign)+len(sorted))
	for _, raw := range foreign {
		rules = append(rules, raw)
	}
	for _, h := range sorted {
		rules = append(rules, httpsRedirectRule(h))
	}

	body := map[string]any{"rules": rules}
	if err := c.do(ctx, http.MethodPut, redirectPath(zoneID), body, nil); err != nil {
		return false, err
	}
	return true, nil
}

// redirectRules reads the zone's redirect ruleset. A zone with no redirect
// rules at all answers 404, which means "empty", not "broken".
func (c *Client) redirectRules(ctx context.Context, zoneID string) ([]json.RawMessage, error) {
	var out struct {
		Rules []json.RawMessage `json:"rules"`
	}
	err := c.do(ctx, http.MethodGet, redirectPath(zoneID), nil, &out)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return out.Rules, nil
}

func redirectPath(zoneID string) string {
	return "/zones/" + zoneID + "/rulesets/phases/" + redirectPhase + "/entrypoint"
}

// ruleOwnership reports a rule's description and whether DockFlare wrote it.
func ruleOwnership(raw json.RawMessage) (string, bool) {
	var rule struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &rule); err != nil {
		// Undecodable means "not ours" — preserve it untouched.
		return "", false
	}
	return rule.Description, len(rule.Description) >= len(ruleMarker) &&
		rule.Description[:len(ruleMarker)] == ruleMarker
}

// stripReadOnlyFields removes the server-generated fields that a rule carries
// on read but that are not accepted on write. Everything else — including id,
// ref and any action_parameters we know nothing about — is kept, so a foreign
// rule survives the round-trip unchanged.
func stripReadOnlyFields(raw json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	delete(fields, "version")
	delete(fields, "last_updated")
	return json.Marshal(fields)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
