package cloudflare

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// rulesetStub emulates a zone's redirect ruleset: a GET returns the current
// rule list, a PUT replaces it wholesale — exactly the API's contract.
type rulesetStub struct {
	t *testing.T
	// rules is the stored list, as raw JSON objects.
	rules []json.RawMessage
	// notFound makes the GET answer 404, the way a zone with no redirect rules
	// at all does.
	notFound bool
	gets     int
	puts     int
	putErr   int // HTTP status to answer PUTs with; 0 means success
}

func (s *rulesetStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/zones":
			writeResult(s.t, w, []Zone{
				{ID: "zone-example", Name: "example.com"},
				{ID: "zone-other", Name: "other.com"},
			})

		case strings.Contains(r.URL.Path, "/rulesets/") && r.Method == http.MethodGet:
			s.gets++
			if s.notFound {
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]any{
					"success": false,
					"errors":  []map[string]any{{"code": 10007, "message": "ruleset not found"}},
				})
				return
			}
			writeResult(s.t, w, map[string]any{"id": "rs1", "phase": redirectPhase, "rules": s.rules})

		case strings.Contains(r.URL.Path, "/rulesets/") && r.Method == http.MethodPut:
			s.puts++
			if s.putErr != 0 {
				w.WriteHeader(s.putErr)
				json.NewEncoder(w).Encode(map[string]any{
					"success": false,
					"errors":  []map[string]any{{"code": 10000, "message": "Authentication error"}},
				})
				return
			}
			var body struct {
				Rules []json.RawMessage `json:"rules"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				s.t.Error(err)
			}
			s.rules = body.Rules
			s.notFound = false
			writeResult(s.t, w, map[string]any{"id": "rs1", "rules": s.rules})

		default:
			s.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
	}
}

func newRulesetClient(t *testing.T, stub *rulesetStub) *Client {
	t.Helper()
	stub.t = t
	c, _ := stubAPI(t, stub.handler())
	return c
}

// descriptions returns the description of every stored rule, in order.
func (s *rulesetStub) descriptions() []string {
	out := make([]string, 0, len(s.rules))
	for _, raw := range s.rules {
		var r struct {
			Description string `json:"description"`
		}
		json.Unmarshal(raw, &r)
		out = append(out, r.Description)
	}
	return out
}

func rawRule(t *testing.T, fields map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSyncHTTPSRedirects_CreatesRuleOnEmptyZone(t *testing.T) {
	stub := &rulesetStub{notFound: true}
	c := newRulesetClient(t, stub)

	zones, err := c.SyncHTTPSRedirects(context.Background(), []string{"grafana.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(zones) != 1 || zones[0] != "example.com" {
		t.Errorf("zones = %v, want [example.com]", zones)
	}
	if stub.puts != 1 {
		t.Fatalf("puts = %d, want 1", stub.puts)
	}

	var rule redirectRule
	if err := json.Unmarshal(stub.rules[0], &rule); err != nil {
		t.Fatal(err)
	}
	if rule.Action != "redirect" || !rule.Enabled {
		t.Errorf("rule = %+v", rule)
	}
	if rule.ActionParameters.FromValue.StatusCode != 301 {
		t.Errorf("status = %d, want 301", rule.ActionParameters.FromValue.StatusCode)
	}
	// The expression must pin the hostname AND only fire on plain HTTP,
	// otherwise it would redirect https requests too and loop forever.
	for _, want := range []string{`http.host eq "grafana.example.com"`, "not ssl"} {
		if !strings.Contains(rule.Expression, want) {
			t.Errorf("expression %q missing %q", rule.Expression, want)
		}
	}
	// http.request.uri carries the query string, so preserve_query_string must
	// be off or it would be appended twice.
	if rule.ActionParameters.FromValue.PreserveQueryString {
		t.Error("preserve_query_string must be false when the target expression already includes the query")
	}
	if !strings.Contains(rule.ActionParameters.FromValue.TargetURL.Expression, "http.request.uri") {
		t.Errorf("target = %q", rule.ActionParameters.FromValue.TargetURL.Expression)
	}
}

func TestSyncHTTPSRedirects_PreservesForeignRulesAndAppendsOursLast(t *testing.T) {
	// Rules execute in order, so ours must go last: a pre-existing rule that
	// matches the same request keeps winning.
	foreign := rawRule(t, map[string]any{
		"id":                "foreign1",
		"description":       "marketing campaign redirect",
		"action":            "redirect",
		"expression":        `(http.host eq "promo.example.com")`,
		"enabled":           true,
		"action_parameters": map[string]any{"from_value": map[string]any{"status_code": 302}},
		"version":           "7",
		"last_updated":      "2024-01-01T00:00:00Z",
	})
	stub := &rulesetStub{rules: []json.RawMessage{foreign}}
	c := newRulesetClient(t, stub)

	if _, err := c.SyncHTTPSRedirects(context.Background(), []string{"grafana.example.com"}); err != nil {
		t.Fatal(err)
	}

	got := stub.descriptions()
	if len(got) != 2 {
		t.Fatalf("rules = %v, want the foreign rule plus ours", got)
	}
	if got[0] != "marketing campaign redirect" {
		t.Errorf("rules[0] = %q, want the foreign rule kept first", got[0])
	}
	if !strings.HasPrefix(got[1], ruleMarker) {
		t.Errorf("rules[1] = %q, want ours appended last", got[1])
	}

	// The foreign rule must survive intact, minus only the read-only fields.
	var kept map[string]any
	if err := json.Unmarshal(stub.rules[0], &kept); err != nil {
		t.Fatal(err)
	}
	if kept["id"] != "foreign1" {
		t.Errorf("foreign rule lost its id: %v", kept)
	}
	if kept["action_parameters"] == nil {
		t.Error("foreign rule lost its action_parameters")
	}
	for _, readOnly := range []string{"version", "last_updated"} {
		if _, present := kept[readOnly]; present {
			t.Errorf("%q must be stripped before writing back", readOnly)
		}
	}
}

func TestSyncHTTPSRedirects_UnchangedMakesNoWrite(t *testing.T) {
	stub := &rulesetStub{notFound: true}
	c := newRulesetClient(t, stub)
	ctx := context.Background()

	if _, err := c.SyncHTTPSRedirects(ctx, []string{"grafana.example.com"}); err != nil {
		t.Fatal(err)
	}
	writes := stub.puts

	zones, err := c.SyncHTTPSRedirects(ctx, []string{"grafana.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if stub.puts != writes {
		t.Errorf("puts %d→%d, want no second write for an unchanged rule set", writes, stub.puts)
	}
	if len(zones) != 0 {
		t.Errorf("zones = %v, want none reported as written", zones)
	}
}

func TestSyncHTTPSRedirects_RemovesOurRuleWhenHostnameDrops(t *testing.T) {
	stub := &rulesetStub{notFound: true}
	c := newRulesetClient(t, stub)
	ctx := context.Background()

	if _, err := c.SyncHTTPSRedirects(ctx, []string{"a.example.com", "b.example.com"}); err != nil {
		t.Fatal(err)
	}
	if len(stub.rules) != 2 {
		t.Fatalf("rules = %v, want 2", stub.descriptions())
	}

	if _, err := c.SyncHTTPSRedirects(ctx, []string{"a.example.com"}); err != nil {
		t.Fatal(err)
	}
	got := stub.descriptions()
	if len(got) != 1 || !strings.Contains(got[0], "a.example.com") {
		t.Errorf("rules = %v, want only the rule for a.example.com", got)
	}
}

func TestSyncHTTPSRedirects_EmptyListClearsOursAndKeepsForeign(t *testing.T) {
	foreign := rawRule(t, map[string]any{
		"id": "f1", "description": "someone else", "action": "redirect",
		"expression": `(http.host eq "x.example.com")`, "enabled": true,
	})
	stub := &rulesetStub{rules: []json.RawMessage{foreign}}
	c := newRulesetClient(t, stub)
	ctx := context.Background()

	if _, err := c.SyncHTTPSRedirects(ctx, []string{"grafana.example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SyncHTTPSRedirects(ctx, nil); err != nil {
		t.Fatal(err)
	}

	got := stub.descriptions()
	if len(got) != 1 || got[0] != "someone else" {
		t.Errorf("rules = %v, want only the foreign rule left", got)
	}
}

func TestSyncHTTPSRedirects_NoHostnamesAndNothingSyncedMakesNoCall(t *testing.T) {
	stub := &rulesetStub{notFound: true}
	c := newRulesetClient(t, stub)

	zones, err := c.SyncHTTPSRedirects(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(zones) != 0 || stub.gets != 0 || stub.puts != 0 {
		t.Errorf("zones=%v gets=%d puts=%d, want no traffic at all", zones, stub.gets, stub.puts)
	}
}

func TestSyncHTTPSRedirects_GroupsHostnamesByZone(t *testing.T) {
	// Two hostnames in one zone must cost one read and one write, not two.
	stub := &rulesetStub{notFound: true}
	c := newRulesetClient(t, stub)

	if _, err := c.SyncHTTPSRedirects(context.Background(),
		[]string{"a.example.com", "b.example.com", "c.example.com"}); err != nil {
		t.Fatal(err)
	}
	if stub.gets != 1 || stub.puts != 1 {
		t.Errorf("gets=%d puts=%d, want 1 each for three hostnames in one zone", stub.gets, stub.puts)
	}
	if len(stub.rules) != 3 {
		t.Errorf("rules = %v, want one per hostname", stub.descriptions())
	}
}

func TestSyncHTTPSRedirects_RuleOrderIsDeterministic(t *testing.T) {
	// Config order must not cause a spurious rewrite on the next sync.
	stub := &rulesetStub{notFound: true}
	c := newRulesetClient(t, stub)
	ctx := context.Background()

	if _, err := c.SyncHTTPSRedirects(ctx, []string{"b.example.com", "a.example.com"}); err != nil {
		t.Fatal(err)
	}
	writes := stub.puts
	if _, err := c.SyncHTTPSRedirects(ctx, []string{"a.example.com", "b.example.com"}); err != nil {
		t.Fatal(err)
	}
	if stub.puts != writes {
		t.Error("reordering the same hostnames must not trigger a write")
	}
}

func TestSyncHTTPSRedirects_UnknownZoneIsAClearError(t *testing.T) {
	stub := &rulesetStub{notFound: true}
	c := newRulesetClient(t, stub)

	_, err := c.SyncHTTPSRedirects(context.Background(), []string{"app.notmine.com"})
	if err == nil || !strings.Contains(err.Error(), "no Cloudflare zone") {
		t.Fatalf("want a clear no-zone error, got: %v", err)
	}
	if stub.puts != 0 {
		t.Error("nothing may be written when the zone cannot be resolved")
	}
}

func TestSyncHTTPSRedirects_PermissionErrorIsSurfaced(t *testing.T) {
	stub := &rulesetStub{notFound: true, putErr: http.StatusForbidden}
	c := newRulesetClient(t, stub)

	_, err := c.SyncHTTPSRedirects(context.Background(), []string{"grafana.example.com"})
	if err == nil {
		t.Fatal("expected the 403 to surface")
	}
	for _, want := range []string{"example.com", "403"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q; got %v", want, err)
		}
	}
}

func TestSyncHTTPSRedirects_OneZoneFailingDoesNotStopTheOthers(t *testing.T) {
	// A token often carries the Rules permission for some zones and not others.
	// The zones that work must still get their rules, and every failure must be
	// reported — not just the first.
	var putsOK, putsDenied int
	c, _ := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/zones":
			writeResult(t, w, []Zone{
				{ID: "zone-denied", Name: "denied.com"},
				{ID: "zone-allowed", Name: "allowed.com"},
				{ID: "zone-allowed2", Name: "allowed2.com"},
			})

		case strings.Contains(r.URL.Path, "zone-denied"):
			putsDenied++
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"errors":  []map[string]any{{"code": 10000, "message": "Authentication error"}},
			})

		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"errors":  []map[string]any{{"code": 10007, "message": "ruleset not found"}},
			})

		default:
			putsOK++
			writeResult(t, w, map[string]any{"id": "rs1"})
		}
	})

	zones, err := c.SyncHTTPSRedirects(context.Background(), []string{
		"a.denied.com", "b.allowed.com", "c.allowed2.com",
	})

	if err == nil {
		t.Fatal("expected the denied zone to be reported")
	}
	if putsDenied == 0 {
		t.Error("the denied zone should have been attempted")
	}
	if putsOK != 2 {
		t.Errorf("successful writes = %d, want 2 — the other zones must not be skipped", putsOK)
	}
	if len(zones) != 2 {
		t.Errorf("zones = %v, want the two that succeeded", zones)
	}
	if !strings.Contains(err.Error(), "denied.com") {
		t.Errorf("error should name the failing zone; got %v", err)
	}
	for _, ok := range []string{"allowed.com", "allowed2.com"} {
		if strings.Contains(err.Error(), "zone "+ok) {
			t.Errorf("error must not blame the zones that worked; got %v", err)
		}
	}
}

func TestSyncHTTPSRedirects_ReportsEveryFailingZone(t *testing.T) {
	c, _ := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/zones" {
			writeResult(t, w, []Zone{
				{ID: "z1", Name: "one.com"},
				{ID: "z2", Name: "two.com"},
			})
			return
		}
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"errors":  []map[string]any{{"code": 10000, "message": "Authentication error"}},
		})
	})

	_, err := c.SyncHTTPSRedirects(context.Background(), []string{"a.one.com", "b.two.com"})
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"one.com", "two.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention every failing zone, missing %q: %v", want, err)
		}
	}
}

func TestSyncHTTPSRedirects_FailedZoneIsRetriedNextTime(t *testing.T) {
	// A failed PUT wrote nothing, so the zone must not be recorded as synced —
	// otherwise a later fix to the token would never be picked up.
	denied := true
	puts := 0
	c, _ := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/zones":
			writeResult(t, w, []Zone{{ID: "z1", Name: "example.com"}})
		case denied:
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"errors":  []map[string]any{{"code": 10000, "message": "Authentication error"}},
			})
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{}})
		default:
			puts++
			writeResult(t, w, map[string]any{"id": "rs1"})
		}
	})
	ctx := context.Background()
	hosts := []string{"grafana.example.com"}

	if _, err := c.SyncHTTPSRedirects(ctx, hosts); err == nil {
		t.Fatal("expected the first attempt to fail")
	}
	denied = false
	zones, err := c.SyncHTTPSRedirects(ctx, hosts)
	if err != nil {
		t.Fatalf("second attempt should succeed, got: %v", err)
	}
	if puts != 1 || len(zones) != 1 {
		t.Errorf("puts=%d zones=%v, want the rule written on the retry", puts, zones)
	}
}

func TestRuleOwnership(t *testing.T) {
	cases := []struct {
		desc string
		ours bool
	}{
		{ruleDescription("a.example.com"), true},
		{"dockflare:v99 something", true},
		{"marketing campaign", false},
		{"", false},
		{"my dockflare: rule", false}, // marker must be a prefix, not a substring
	}
	for _, tc := range cases {
		raw := rawRule(t, map[string]any{"description": tc.desc})
		got, ours := ruleOwnership(raw)
		if ours != tc.ours {
			t.Errorf("ruleOwnership(%q) ours = %v, want %v", tc.desc, ours, tc.ours)
		}
		if got != tc.desc {
			t.Errorf("description = %q, want %q", got, tc.desc)
		}
	}
}

func TestRuleOwnership_UndecodableRuleIsTreatedAsForeign(t *testing.T) {
	// Anything we cannot parse must be preserved, never claimed and deleted.
	if _, ours := ruleOwnership(json.RawMessage(`not json`)); ours {
		t.Error("an undecodable rule must not be treated as ours")
	}
}

func TestRuleDescriptionCarriesVersion(t *testing.T) {
	// The version is what lets a future template change rewrite existing rules.
	desc := ruleDescription("a.example.com")
	if !strings.HasPrefix(desc, ruleMarker+ruleVersion+" ") {
		t.Errorf("description = %q, want it to start with %q", desc, ruleMarker+ruleVersion)
	}
	if !strings.HasSuffix(desc, "a.example.com") {
		t.Errorf("description = %q, want the hostname at the end", desc)
	}
}
