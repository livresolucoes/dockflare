package ingress

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/livresolucoes/dockflare/internal/cloudflare"
	"github.com/livresolucoes/dockflare/internal/config"
	"github.com/livresolucoes/dockflare/internal/docker"
)

// fakeDocker answers InspectContainer from a fixed map. A name absent from the
// map means "container not found", matching docker.Client's (nil, nil).
type fakeDocker struct {
	containers map[string]*docker.ContainerInfo
	err        error
}

func (f *fakeDocker) InspectContainer(_ context.Context, name string) (*docker.ContainerInfo, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.containers[name], nil
}

// fakeNets reports a fixed set of joined networks.
type fakeNets struct {
	joined   map[string]bool
	tracking bool
}

func (f *fakeNets) Reachable(n string) bool { return f.joined[n] }
func (f *fakeNets) Tracking() bool          { return f.tracking }

func reachableNets(names ...string) *fakeNets {
	joined := make(map[string]bool, len(names))
	for _, n := range names {
		joined[n] = true
	}
	return &fakeNets{joined: joined, tracking: true}
}

func TestBuildIngress_AlwaysEndsWithCatchAll(t *testing.T) {
	rules := BuildIngress([]config.Route{
		{Hostname: "meuapp.example.com", Container: "meuapp_web", Port: 4000},
		{Hostname: "api.meuapp.example.com", Container: "meuapp_api", Port: 3000},
	})

	if len(rules) != 3 {
		t.Fatalf("rules = %d, want 3 (2 routes + catch-all)", len(rules))
	}
	if rules[0].Hostname != "meuapp.example.com" || rules[0].Service != "http://meuapp_web:4000" {
		t.Errorf("rules[0] = %+v", rules[0])
	}
	if rules[1].Hostname != "api.meuapp.example.com" || rules[1].Service != "http://meuapp_api:3000" {
		t.Errorf("rules[1] = %+v", rules[1])
	}
	last := rules[len(rules)-1]
	if last.Hostname != "" || last.Service != "http_status:404" {
		t.Errorf("last rule = %+v, want the bare http_status:404 catch-all", last)
	}
}

func TestBuildIngress_EmptyRoutesStillHasCatchAll(t *testing.T) {
	rules := BuildIngress(nil)
	if len(rules) != 1 || rules[0].Service != "http_status:404" {
		t.Fatalf("rules = %+v, want only the catch-all", rules)
	}
}

func TestBuildIngress_PreservesConfigOrder(t *testing.T) {
	// Cloudflare matches ingress top-down, so the user's order must survive.
	rules := BuildIngress([]config.Route{
		{Hostname: "z.example.com", Container: "z", Port: 1},
		{Hostname: "a.example.com", Container: "a", Port: 2},
	})
	if rules[0].Hostname != "z.example.com" || rules[1].Hostname != "a.example.com" {
		t.Errorf("order changed: %+v", rules)
	}
}

func TestServiceURL_UsesInternalContainerAddress(t *testing.T) {
	got := ServiceURL(config.Target{Container: "meuapp_api", Port: 3000})
	if got != "http://meuapp_api:3000" {
		t.Errorf("ServiceURL = %q, want http://meuapp_api:3000", got)
	}
}

func TestSameRules(t *testing.T) {
	a := BuildIngress([]config.Route{{Hostname: "a.example.com", Container: "a", Port: 80}})
	b := BuildIngress([]config.Route{{Hostname: "a.example.com", Container: "a", Port: 80}})
	c := BuildIngress([]config.Route{{Hostname: "a.example.com", Container: "a", Port: 81}})
	d := BuildIngress([]config.Route{
		{Hostname: "a.example.com", Container: "a", Port: 80},
		{Hostname: "b.example.com", Container: "b", Port: 80},
	})

	if !SameRules(a, b) {
		t.Error("identical rule sets should compare equal")
	}
	if SameRules(a, c) {
		t.Error("a changed port must compare unequal")
	}
	if SameRules(a, d) {
		t.Error("different lengths must compare unequal")
	}
}

func TestSameRules_ComparesOriginRequest(t *testing.T) {
	// DockFlare owns noTLSVerify inside originRequest, so a difference there is
	// a real difference that must trigger a write.
	a := []cloudflare.IngressRule{{Hostname: "a.example.com", Service: "https://a:443"}}
	b := []cloudflare.IngressRule{{
		Hostname:      "a.example.com",
		Service:       "https://a:443",
		OriginRequest: []byte(`{"noTLSVerify":true}`),
	}}
	if SameRules(a, b) {
		t.Error("a missing noTLSVerify must count as a difference")
	}
}

func TestSameRules_OriginRequestComparedByContentNotBytes(t *testing.T) {
	// JSON key order is not meaningful, and absent/null/{} all mean "unset".
	same := func(x, y string) bool {
		return SameRules(
			[]cloudflare.IngressRule{{Hostname: "a.example.com", Service: "http://a:80", OriginRequest: []byte(x)}},
			[]cloudflare.IngressRule{{Hostname: "a.example.com", Service: "http://a:80", OriginRequest: []byte(y)}},
		)
	}
	if !same(`{"noTLSVerify":true,"connectTimeout":30}`, `{"connectTimeout":30,"noTLSVerify":true}`) {
		t.Error("key order must not matter")
	}
	if !same(``, `{}`) {
		t.Error("empty and {} both mean unset")
	}
	if !same(``, `null`) {
		t.Error("null means unset")
	}
	if same(`{"noTLSVerify":true}`, `{"noTLSVerify":false}`) {
		t.Error("differing values must compare unequal")
	}
}

func TestBuildIngress_OriginSchemeHTTPS(t *testing.T) {
	rules := BuildIngress([]config.Route{
		{Hostname: "vault.example.com", Container: "vault", Port: 8200, OriginScheme: config.SchemeHTTPS},
	})
	if rules[0].Service != "https://vault:8200" {
		t.Errorf("service = %q, want https://vault:8200", rules[0].Service)
	}
	// A container's cert cannot match its Docker network name, so verification
	// must be off or every https origin would fail.
	if !hasOriginKey(rules[0].OriginRequest, noTLSVerifyKey) {
		t.Errorf("originRequest = %s, want noTLSVerify set", rules[0].OriginRequest)
	}
}

func TestBuildIngress_DefaultSchemeIsHTTPWithNoOriginRequest(t *testing.T) {
	rules := BuildIngress([]config.Route{
		{Hostname: "a.example.com", Container: "a", Port: 80},
	})
	if rules[0].Service != "http://a:80" {
		t.Errorf("service = %q, want http://a:80", rules[0].Service)
	}
	if len(rules[0].OriginRequest) != 0 {
		t.Errorf("originRequest = %s, want none for a plain http origin", rules[0].OriginRequest)
	}
}

func TestServiceURL_HTTPSScheme(t *testing.T) {
	got := ServiceURL(config.Target{Container: "vault", Port: 8200, Scheme: config.SchemeHTTPS})
	if got != "https://vault:8200" {
		t.Errorf("ServiceURL = %q, want https://vault:8200", got)
	}
}

func TestServiceURL_EmptySchemeFallsBackToHTTP(t *testing.T) {
	// A hand-built Target that skipped normalizeRoutes must not produce "://".
	got := ServiceURL(config.Target{Container: "a", Port: 80})
	if got != "http://a:80" {
		t.Errorf("ServiceURL = %q, want http://a:80", got)
	}
}

func TestCarryOverOriginRequests_PreservesForeignKeys(t *testing.T) {
	// connectTimeout was set outside DockFlare; writing the ingress must not
	// wipe it.
	desired := BuildIngress([]config.Route{
		{Hostname: "a.example.com", Container: "a", Port: 80},
	})
	remote := []cloudflare.IngressRule{
		{Hostname: "a.example.com", Service: "http://a:80", OriginRequest: []byte(`{"connectTimeout":30}`)},
		{Service: "http_status:404"},
	}

	got := CarryOverOriginRequests(desired, remote)
	fields := decodeOriginRequest(got[0].OriginRequest)
	if fields["connectTimeout"] == nil {
		t.Errorf("connectTimeout was dropped: %s", got[0].OriginRequest)
	}
	if _, ok := fields[noTLSVerifyKey]; ok {
		t.Errorf("noTLSVerify must not be set for an http origin: %s", got[0].OriginRequest)
	}
	// And the carried-over state must now compare equal, so no pointless write.
	if !SameRules(remote, got) {
		t.Error("after carry-over the desired state should match remote, avoiding a needless PUT")
	}
}

func TestCarryOverOriginRequests_OursWinsOnOwnedKey(t *testing.T) {
	desired := BuildIngress([]config.Route{
		{Hostname: "v.example.com", Container: "v", Port: 8200, OriginScheme: config.SchemeHTTPS},
	})
	remote := []cloudflare.IngressRule{
		{Hostname: "v.example.com", Service: "http://v:8200",
			OriginRequest: []byte(`{"noTLSVerify":false,"connectTimeout":30}`)},
	}

	got := CarryOverOriginRequests(desired, remote)
	fields := decodeOriginRequest(got[0].OriginRequest)
	if fields[noTLSVerifyKey] != true {
		t.Errorf("noTLSVerify = %v, want true — DockFlare owns this key", fields[noTLSVerifyKey])
	}
	if fields["connectTimeout"] == nil {
		t.Error("connectTimeout was dropped")
	}
}

func TestCarryOverOriginRequests_RemovesOwnedKeyWhenSwitchingToHTTP(t *testing.T) {
	// origin_scheme went https → http: our key must go, theirs must stay.
	desired := BuildIngress([]config.Route{
		{Hostname: "v.example.com", Container: "v", Port: 8200},
	})
	remote := []cloudflare.IngressRule{
		{Hostname: "v.example.com", Service: "https://v:8200",
			OriginRequest: []byte(`{"noTLSVerify":true,"connectTimeout":30}`)},
	}

	got := CarryOverOriginRequests(desired, remote)
	fields := decodeOriginRequest(got[0].OriginRequest)
	if _, ok := fields[noTLSVerifyKey]; ok {
		t.Errorf("noTLSVerify should be gone: %s", got[0].OriginRequest)
	}
	if fields["connectTimeout"] == nil {
		t.Error("connectTimeout was dropped")
	}
}

func TestCarryOverOriginRequests_NoRemoteMatchIsUntouched(t *testing.T) {
	desired := BuildIngress([]config.Route{
		{Hostname: "new.example.com", Container: "n", Port: 80},
	})
	got := CarryOverOriginRequests(desired, []cloudflare.IngressRule{
		{Hostname: "other.example.com", Service: "http://o:80", OriginRequest: []byte(`{"connectTimeout":9}`)},
	})
	if len(got[0].OriginRequest) != 0 {
		t.Errorf("originRequest = %s, want none — no remote rule for this hostname", got[0].OriginRequest)
	}
}

func TestWithUIRule_InsertsBeforeCatchAll(t *testing.T) {
	rules := BuildIngress([]config.Route{
		{Hostname: "a.example.com", Container: "a", Port: 80},
	})
	got := WithUIRule(rules, "ui.example.com", 8080)

	if len(got) != 3 {
		t.Fatalf("rules = %+v, want route + UI + catch-all", got)
	}
	if got[1].Hostname != "ui.example.com" {
		t.Errorf("rules[1] = %+v, want the UI rule", got[1])
	}
	// cloudflared runs beside the UI in the same container, so the origin needs
	// no Docker network or container name.
	if got[1].Service != "http://localhost:8080" {
		t.Errorf("service = %q, want http://localhost:8080", got[1].Service)
	}
	last := got[len(got)-1]
	if last.Hostname != "" || last.Service != "http_status:404" {
		t.Errorf("catch-all must stay last, got %+v", last)
	}
}

func TestWithUIRule_NoHostnameIsANoOp(t *testing.T) {
	rules := BuildIngress([]config.Route{{Hostname: "a.example.com", Container: "a", Port: 80}})
	if got := WithUIRule(rules, "", 8080); len(got) != len(rules) {
		t.Errorf("rules = %+v, want unchanged", got)
	}
}

func TestWithUIRule_EmptyRulesIsANoOp(t *testing.T) {
	// With no routes DockFlare does not manage ingress, so there is nothing to
	// insert into. Config validation rejects that pairing before this point.
	if got := WithUIRule(nil, "ui.example.com", 8080); len(got) != 0 {
		t.Errorf("rules = %+v, want none", got)
	}
}

func TestWithUIRule_ChangedPortIsADifference(t *testing.T) {
	base := BuildIngress([]config.Route{{Hostname: "a.example.com", Container: "a", Port: 80}})
	if SameRules(WithUIRule(base, "ui.example.com", 8080), WithUIRule(base, "ui.example.com", 9090)) {
		t.Error("a different UI port must compare unequal so the ingress is rewritten")
	}
}

func TestResolve_ValidRoute(t *testing.T) {
	insp := &fakeDocker{containers: map[string]*docker.ContainerInfo{
		"api": {Name: "api", Networks: []string{"backend"}, Ports: []int{3000}},
	}}
	err := Resolve(context.Background(),
		[]config.Route{{Hostname: "api.example.com", Container: "api", Port: 3000}},
		insp, reachableNets("backend"))
	if err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestResolve_ContainerNotFound(t *testing.T) {
	insp := &fakeDocker{containers: map[string]*docker.ContainerInfo{}}
	err := Resolve(context.Background(),
		[]config.Route{{Hostname: "api.example.com", Container: "api", Port: 3000}},
		insp, reachableNets("backend"))
	if err == nil {
		t.Fatal("expected an error for a missing container")
	}
	if !IsValidationError(err) {
		t.Errorf("want a ValidationError, got %T", err)
	}
	want := `Route api.example.com references container "api" but that container was not found.`
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q\nwant it to contain %q", err.Error(), want)
	}
}

func TestResolve_PortNotAvailable(t *testing.T) {
	insp := &fakeDocker{containers: map[string]*docker.ContainerInfo{
		"api": {Name: "api", Networks: []string{"backend"}, Ports: []int{8080, 9090}},
	}}
	err := Resolve(context.Background(),
		[]config.Route{{Hostname: "api.example.com", Container: "api", Port: 3000}},
		insp, reachableNets("backend"))
	if err == nil {
		t.Fatal("expected an error for an unexposed port")
	}
	want := `Route api.example.com references container "api", but port 3000 is not available.`
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q\nwant it to contain %q", err.Error(), want)
	}
	if !strings.Contains(err.Error(), "8080, 9090") {
		t.Errorf("error should list the exposed ports; got %q", err.Error())
	}
}

func TestResolve_NoExposedPortsIsAccepted(t *testing.T) {
	// Plenty of images skip EXPOSE. We cannot disprove the port, so we allow it
	// rather than blocking a working setup.
	insp := &fakeDocker{containers: map[string]*docker.ContainerInfo{
		"api": {Name: "api", Networks: []string{"backend"}},
	}}
	err := Resolve(context.Background(),
		[]config.Route{{Hostname: "api.example.com", Container: "api", Port: 3000}},
		insp, reachableNets("backend"))
	if err != nil {
		t.Fatalf("container without EXPOSE should be accepted, got: %v", err)
	}
}

func TestResolve_NetworkNotReachable(t *testing.T) {
	insp := &fakeDocker{containers: map[string]*docker.ContainerInfo{
		"api": {Name: "api", Networks: []string{"private_net"}, Ports: []int{3000}},
	}}
	err := Resolve(context.Background(),
		[]config.Route{{Hostname: "api.example.com", Container: "api", Port: 3000}},
		insp, reachableNets("some_other_net"))
	if err == nil {
		t.Fatal("expected an error for an unreachable network")
	}
	for _, want := range []string{"not connected to any of its Docker networks", "private_net"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q; got %q", want, err.Error())
		}
	}
}

func TestResolve_ReachabilityNotEnforcedWhenNotTracking(t *testing.T) {
	// Running outside Docker: we have no membership to compare against, so the
	// check would only ever produce false negatives.
	insp := &fakeDocker{containers: map[string]*docker.ContainerInfo{
		"api": {Name: "api", Networks: []string{"private_net"}, Ports: []int{3000}},
	}}
	err := Resolve(context.Background(),
		[]config.Route{{Hostname: "api.example.com", Container: "api", Port: 3000}},
		insp, &fakeNets{tracking: false})
	if err != nil {
		t.Fatalf("expected no reachability enforcement, got: %v", err)
	}
}

func TestResolve_ContainerOnNoNetwork(t *testing.T) {
	insp := &fakeDocker{containers: map[string]*docker.ContainerInfo{
		"api": {Name: "api"},
	}}
	err := Resolve(context.Background(),
		[]config.Route{{Hostname: "api.example.com", Container: "api", Port: 3000}},
		insp, reachableNets("backend"))
	if err == nil || !strings.Contains(err.Error(), "not attached to any Docker network") {
		t.Fatalf("want a no-network error, got: %v", err)
	}
}

func TestResolve_InspectFailureIsReported(t *testing.T) {
	insp := &fakeDocker{err: errors.New("docker daemon unreachable")}
	err := Resolve(context.Background(),
		[]config.Route{{Hostname: "api.example.com", Container: "api", Port: 3000}},
		insp, reachableNets("backend"))
	if err == nil || !strings.Contains(err.Error(), "docker daemon unreachable") {
		t.Fatalf("want the inspect failure surfaced, got: %v", err)
	}
}

func TestResolve_CollectsAllProblems(t *testing.T) {
	insp := &fakeDocker{containers: map[string]*docker.ContainerInfo{
		"good":      {Name: "good", Networks: []string{"backend"}, Ports: []int{80}},
		"wrongport": {Name: "wrongport", Networks: []string{"backend"}, Ports: []int{80}},
	}}
	err := Resolve(context.Background(), []config.Route{
		{Hostname: "ok.example.com", Container: "good", Port: 80},
		{Hostname: "gone.example.com", Container: "missing", Port: 80},
		{Hostname: "bad.example.com", Container: "wrongport", Port: 3000},
	}, insp, reachableNets("backend"))

	if err == nil {
		t.Fatal("expected errors")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *ValidationError, got %T", err)
	}
	if len(ve.Errs) != 2 {
		t.Fatalf("want 2 problems (missing container, bad port), got %d: %v", len(ve.Errs), ve.Errs)
	}
}

func TestResolve_NoRoutesIsValid(t *testing.T) {
	if err := Resolve(context.Background(), nil, &fakeDocker{}, reachableNets()); err != nil {
		t.Fatalf("no routes must be valid, got: %v", err)
	}
}

func TestIsValidationError_FalseForOtherErrors(t *testing.T) {
	if IsValidationError(errors.New("cloudflare api down")) {
		t.Error("a plain error must not be reported as a validation error")
	}
}
