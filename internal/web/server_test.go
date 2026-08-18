package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/livresolucoes/dockflare/internal/docker"
	"github.com/livresolucoes/dockflare/internal/logger"
)

const testToken = "0123456789abcdef0123456789abcdef" // 32 chars, the minimum

type fakeDocker struct {
	containers []docker.ContainerInfo
	listErr    error
}

func (f *fakeDocker) ListContainers(context.Context) ([]docker.ContainerInfo, error) {
	return f.containers, f.listErr
}

func (f *fakeDocker) InspectContainer(_ context.Context, name string) (*docker.ContainerInfo, error) {
	for i := range f.containers {
		if f.containers[i].Name == name {
			return &f.containers[i], nil
		}
	}
	return nil, nil
}

type fakeNets struct{ joined []string }

func (f *fakeNets) Reachable(n string) bool {
	for _, j := range f.joined {
		if j == n {
			return true
		}
	}
	return false
}
func (f *fakeNets) Tracking() bool           { return true }
func (f *fakeNets) JoinedNetworks() []string { return f.joined }

type fakeProc struct{ running bool }

func (f *fakeProc) Running() bool { return f.running }
func (f *fakeProc) PID() int {
	if f.running {
		return 42
	}
	return 0
}

type fakeReloader struct {
	calls int
	err   error
}

func (f *fakeReloader) Reload(context.Context) error {
	f.calls++
	return f.err
}

type harness struct {
	t        *testing.T
	server   *Server
	handler  http.Handler
	reloader *fakeReloader
	path     string
}

func newHarness(t *testing.T, configBody string) *harness {
	t.Helper()
	t.Setenv("TUNNEL_TOKEN", "eyJhtest")
	t.Setenv("CLOUDFLARE_API_TOKEN", "cfapi")
	t.Setenv("DOCKFLARE_UI_TOKEN", testToken)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(configBody), 0o644); err != nil {
		t.Fatal(err)
	}

	rl := &fakeReloader{}
	s := New(Options{
		ConfigPath: path,
		Token:      testToken,
		Port:       8080,
		Docker: &fakeDocker{containers: []docker.ContainerInfo{
			{Name: "web", Networks: []string{"app_net"}, Ports: []int{80}},
			{Name: "api", Networks: []string{"app_net"}, Ports: []int{3000}},
			{Name: "lonely", Networks: []string{"other_net"}, Ports: []int{9000}},
		}},
		Nets:     &fakeNets{joined: []string{"app_net"}},
		Proc:     &fakeProc{running: true},
		Reloader: rl,
		Log:      logger.New(io.Discard),
	})
	return &harness{t: t, server: s, handler: s.Handler(), reloader: rl, path: path}
}

// do sends a request. When authed is true it carries the bearer token.
func (h *harness) do(method, path string, body any, authed bool) *httptest.ResponseRecorder {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = strings.NewReader(string(encoded))
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authed {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON (%d): %s", rec.Code, rec.Body.String())
	}
	return out
}

const validConfig = `
containers: [web, api]
routes:
  - hostname: a.example.com
    container: web
    port: 80
manage_dns: false
`

// ---------------------------------------------------------------- auth

func TestAPIRequiresAuth(t *testing.T) {
	h := newHarness(t, validConfig)
	for _, ep := range []struct{ method, path string }{
		{http.MethodGet, "/api/config"},
		{http.MethodPut, "/api/config"},
		{http.MethodPost, "/api/validate"},
		{http.MethodPost, "/api/reload"},
		{http.MethodGet, "/api/status"},
		{http.MethodGet, "/api/containers"},
	} {
		rec := h.do(ep.method, ep.path, nil, false)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401 without credentials", ep.method, ep.path, rec.Code)
		}
	}
}

func TestUnauthenticatedRequestCannotReload(t *testing.T) {
	h := newHarness(t, validConfig)
	h.do(http.MethodPost, "/api/reload", nil, false)
	if h.reloader.calls != 0 {
		t.Error("an unauthenticated request must not reach the reloader")
	}
}

func TestLogin_WrongTokenRejected(t *testing.T) {
	h := newHarness(t, validConfig)
	rec := h.do(http.MethodPost, "/api/login", map[string]string{"token": "wrong"}, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("no session cookie may be issued for a wrong token")
	}
}

func TestLogin_SetsHardenedSessionCookie(t *testing.T) {
	h := newHarness(t, validConfig)
	rec := h.do(http.MethodPost, "/api/login", map[string]string{"token": testToken}, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v, want exactly one session cookie", cookies)
	}
	c := cookies[0]
	if c.Name != sessionCookie {
		t.Errorf("cookie name = %q", c.Name)
	}
	if !c.HttpOnly {
		t.Error("the session cookie must be HttpOnly so XSS cannot read it")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Error("the session cookie must be SameSite=Strict")
	}
	// The cookie carries an opaque session id, never the token itself.
	if strings.Contains(c.Value, testToken) {
		t.Error("the cookie must not contain the token")
	}
}

func TestLogin_SessionCookieGrantsAccess(t *testing.T) {
	h := newHarness(t, validConfig)
	login := h.do(http.MethodPost, "/api/login", map[string]string{"token": testToken}, false)
	cookies := login.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected a session cookie")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.AddCookie(cookies[0])
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200 with a valid session", rec.Code)
	}
}

func TestLogin_RefusedOverPlainHTTPThroughTheTunnel(t *testing.T) {
	// Cloudflare reports the browser's scheme. Accepting a token over plain HTTP
	// would send it across the internet in the clear.
	h := newHarness(t, validConfig)
	req := httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"token":"`+testToken+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "http")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("no session may be created over plain HTTP")
	}
}

func TestLogin_AllowedOverHTTPSThroughTheTunnel(t *testing.T) {
	h := newHarness(t, validConfig)
	req := httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"token":"`+testToken+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if c := rec.Result().Cookies(); len(c) == 0 || !c[0].Secure {
		t.Error("the cookie must be Secure when the request came over https")
	}
}

func TestLogout_InvalidatesTheSession(t *testing.T) {
	h := newHarness(t, validConfig)
	login := h.do(http.MethodPost, "/api/login", map[string]string{"token": testToken}, false)
	cookie := login.Result().Cookies()[0]

	out := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	out.AddCookie(cookie)
	h.handler.ServeHTTP(httptest.NewRecorder(), out)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401 after logout", rec.Code)
	}
}

// ---------------------------------------------------------------- config

func TestGetConfig_NeverExposesSecrets(t *testing.T) {
	h := newHarness(t, "token: file-token-value\n"+validConfig)
	rec := h.do(http.MethodGet, "/api/config", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	for _, secret := range []string{"eyJhtest", "cfapi", testToken, "file-token-value"} {
		if strings.Contains(body, secret) {
			t.Errorf("the response leaks a secret (%q):\n%s", secret, body)
		}
	}
	if !strings.Contains(body, "a.example.com") {
		t.Errorf("expected the routes in the response: %s", body)
	}
}

func TestPutConfig_SavesAndReloads(t *testing.T) {
	h := newHarness(t, validConfig)
	rec := h.do(http.MethodPut, "/api/config", configPayload{
		Containers: []string{"web", "api"},
		Routes: []routePayload{
			{Hostname: "a.example.com", Container: "web", Port: 80, OriginScheme: "http"},
			{Hostname: "b.example.com", Container: "api", Port: 3000, OriginScheme: "https", ForceHTTPS: true},
		},
		ManageDNS: true,
	}, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	body := decode(t, rec)
	if body["saved"] != true || body["reloaded"] != true {
		t.Errorf("body = %v", body)
	}
	if h.reloader.calls != 1 {
		t.Errorf("reload calls = %d, want 1", h.reloader.calls)
	}

	saved, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"b.example.com", "origin_scheme: https", "force_https: true", "manage_dns: true"} {
		if !strings.Contains(string(saved), want) {
			t.Errorf("saved file missing %q:\n%s", want, saved)
		}
	}
}

func TestPutConfig_InvalidRouteWritesNothing(t *testing.T) {
	h := newHarness(t, validConfig)
	before, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatal(err)
	}

	rec := h.do(http.MethodPut, "/api/config", configPayload{
		Containers: []string{"web"},
		Routes: []routePayload{
			{Hostname: "ghost.example.com", Container: "does_not_exist", Port: 80},
		},
	}, true)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	problems, _ := decode(t, rec)["problems"].([]any)
	if len(problems) == 0 {
		t.Fatal("expected the problems to be listed")
	}
	if !strings.Contains(problems[0].(string), "was not found") {
		t.Errorf("problem = %v", problems[0])
	}

	after, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the config file must be untouched when validation fails")
	}
	if h.reloader.calls != 0 {
		t.Error("no reload may happen when nothing was saved")
	}
}

func TestPutConfig_UnreachableContainerIsRejected(t *testing.T) {
	h := newHarness(t, validConfig)
	rec := h.do(http.MethodPut, "/api/config", configPayload{
		Containers: []string{"lonely"},
		Routes: []routePayload{
			{Hostname: "x.example.com", Container: "lonely", Port: 9000},
		},
	}, true)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not connected to any of its Docker networks") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestPutConfig_DuplicateHostnameIsRejected(t *testing.T) {
	h := newHarness(t, validConfig)
	rec := h.do(http.MethodPut, "/api/config", configPayload{
		Containers: []string{"web"},
		Routes: []routePayload{
			{Hostname: "a.example.com", Container: "web", Port: 80},
			{Hostname: "a.example.com", Container: "web", Port: 80},
		},
	}, true)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "declared more than once") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestPutConfig_ProblemsAreListedIndividually(t *testing.T) {
	// The UI renders a bullet list, so a wall of newline-joined text is no good.
	h := newHarness(t, validConfig)
	rec := h.do(http.MethodPut, "/api/config", configPayload{
		Routes: []routePayload{
			{Hostname: "", Container: "", Port: 0},
		},
	}, true)

	problems, _ := decode(t, rec)["problems"].([]any)
	if len(problems) < 3 {
		t.Errorf("problems = %v, want one entry per missing field", problems)
	}
	for _, p := range problems {
		if strings.Contains(p.(string), "\n") {
			t.Errorf("problem entries must be single lines, got %q", p)
		}
	}
}

func TestPutConfig_CannotChangeWebUISettings(t *testing.T) {
	// Letting the UI disable itself or move its own port would lock the user out
	// of the page they are using.
	h := newHarness(t, `
containers: [web]
routes:
  - hostname: a.example.com
    container: web
    port: 80
web_ui:
  enabled: true
  port: 8080
`)
	rec := h.do(http.MethodPut, "/api/config", configPayload{
		Containers: []string{"web"},
		Routes:     []routePayload{{Hostname: "a.example.com", Container: "web", Port: 80}},
		WebUI:      webUIPayload{Enabled: false, Port: 9999, Hostname: "evil.example.com"},
	}, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	saved, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "9999") || strings.Contains(string(saved), "evil.example.com") {
		t.Errorf("web_ui settings from the request must be ignored:\n%s", saved)
	}
	if !strings.Contains(string(saved), "enabled: true") {
		t.Errorf("the UI must not be able to disable itself:\n%s", saved)
	}
}

func TestPutConfig_RefusesToWipeAllRoutesWithoutConfirmation(t *testing.T) {
	// An empty request is as likely to be a UI that failed to render as a
	// deliberate reset, and applying it takes every hostname offline.
	h := newHarness(t, validConfig)
	before, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatal(err)
	}

	rec := h.do(http.MethodPut, "/api/config", configPayload{
		Containers: []string{"web"},
		Routes:     nil,
	}, true)

	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	body := decode(t, rec)
	if !strings.Contains(body["error"].(string), "confirm=drop-all-routes") {
		t.Errorf("the error must say how to proceed on purpose: %v", body["error"])
	}
	if body["wouldRemove"].(float64) != 1 {
		t.Errorf("wouldRemove = %v, want 1", body["wouldRemove"])
	}

	after, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the config must be untouched")
	}
	if h.reloader.calls != 0 {
		t.Error("no reload may happen")
	}
}

func TestPutConfig_WipesAllRoutesWhenConfirmed(t *testing.T) {
	h := newHarness(t, validConfig)
	rec := h.do(http.MethodPut, "/api/config?confirm=drop-all-routes", configPayload{
		Containers: []string{"web"},
		Routes:     nil,
	}, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	saved, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "a.example.com") {
		t.Errorf("the route should be gone once confirmed:\n%s", saved)
	}
}

func TestPutConfig_EmptyRoutesFineWhenThereWereNone(t *testing.T) {
	// Nothing to lose, so no confirmation should be demanded.
	h := newHarness(t, "containers: [web]\n")
	rec := h.do(http.MethodPut, "/api/config", configPayload{
		Containers: []string{"web", "api"},
		Routes:     nil,
	}, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestPutConfig_PartialRemovalNeedsNoConfirmation(t *testing.T) {
	// Removing some routes is ordinary editing; only wiping them all is guarded.
	h := newHarness(t, `
containers: [web, api]
routes:
  - hostname: a.example.com
    container: web
    port: 80
  - hostname: b.example.com
    container: api
    port: 3000
`)
	rec := h.do(http.MethodPut, "/api/config", configPayload{
		Containers: []string{"web", "api"},
		Routes:     []routePayload{{Hostname: "a.example.com", Container: "web", Port: 80}},
	}, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	saved, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "b.example.com") {
		t.Error("the removed route should be gone")
	}
	if !strings.Contains(string(saved), "a.example.com") {
		t.Error("the kept route should still be there")
	}
}

func TestPutConfig_ReloadFailureReportedAsPartialSuccess(t *testing.T) {
	h := newHarness(t, validConfig)
	h.reloader.err = errors.New("cloudflare api unreachable")

	rec := h.do(http.MethodPut, "/api/config", configPayload{
		Containers: []string{"web"},
		Routes:     []routePayload{{Hostname: "a.example.com", Container: "web", Port: 80}},
	}, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	body := decode(t, rec)
	if body["saved"] != true || body["reloaded"] != false {
		t.Errorf("body = %v, want saved but not reloaded", body)
	}
	if !strings.Contains(body["reloadError"].(string), "unreachable") {
		t.Errorf("reloadError = %v", body["reloadError"])
	}
}

// ---------------------------------------------------------------- validate

func TestValidate_DoesNotWrite(t *testing.T) {
	h := newHarness(t, validConfig)
	before, _ := os.ReadFile(h.path)

	rec := h.do(http.MethodPost, "/api/validate", configPayload{
		Containers: []string{"web"},
		Routes:     []routePayload{{Hostname: "new.example.com", Container: "web", Port: 80}},
	}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}

	after, _ := os.ReadFile(h.path)
	if string(before) != string(after) {
		t.Error("validate must never touch the file")
	}
	if h.reloader.calls != 0 {
		t.Error("validate must never reload")
	}
}

func TestValidate_ValidConfigHasNoProblems(t *testing.T) {
	h := newHarness(t, validConfig)
	rec := h.do(http.MethodPost, "/api/validate", configPayload{
		Containers: []string{"web", "api"},
		Routes: []routePayload{
			{Hostname: "a.example.com", Container: "web", Port: 80},
			{Hostname: "b.example.com", Container: "api", Port: 3000},
		},
	}, true)

	problems := decode(t, rec)["problems"]
	if problems != nil {
		t.Errorf("problems = %v, want none", problems)
	}
}

// ---------------------------------------------------------------- misc

func TestReload_CallsThroughOnce(t *testing.T) {
	h := newHarness(t, validConfig)
	rec := h.do(http.MethodPost, "/api/reload", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	if h.reloader.calls != 1 {
		t.Errorf("reload calls = %d, want 1", h.reloader.calls)
	}
}

func TestStatus(t *testing.T) {
	h := newHarness(t, validConfig)
	body := decode(t, h.do(http.MethodGet, "/api/status", nil, true))

	if body["cloudflaredRunning"] != true {
		t.Errorf("cloudflaredRunning = %v", body["cloudflaredRunning"])
	}
	if body["configWritable"] != true {
		t.Errorf("configWritable = %v (%v)", body["configWritable"], body["configWritableError"])
	}
	nets, _ := body["joinedNetworks"].([]any)
	if len(nets) != 1 || nets[0] != "app_net" {
		t.Errorf("joinedNetworks = %v", body["joinedNetworks"])
	}
}

func TestContainers_FlagsReachability(t *testing.T) {
	h := newHarness(t, validConfig)
	body := decode(t, h.do(http.MethodGet, "/api/containers", nil, true))
	list, _ := body["containers"].([]any)
	if len(list) != 3 {
		t.Fatalf("containers = %v", list)
	}

	byName := map[string]map[string]any{}
	for _, item := range list {
		entry := item.(map[string]any)
		byName[entry["name"].(string)] = entry
	}
	if byName["web"]["reachable"] != true {
		t.Error("web is on a joined network and should be reachable")
	}
	if byName["lonely"]["reachable"] != false {
		t.Error("lonely is on no joined network and should not be reachable")
	}
}

func TestContainers_ListFieldsAreNeverNull(t *testing.T) {
	// A container with no published port is the normal case with a tunnel — it
	// is the whole point of not publishing ports. Serialising its ports as
	// `null` makes the browser read `.length` off null and kills the page.
	h := newHarness(t, validConfig)
	h.server.opts.Docker = &fakeDocker{containers: []docker.ContainerInfo{
		{Name: "no_ports_no_nets"}, // both slices nil
		{Name: "normal", Networks: []string{"app_net"}, Ports: []int{80}},
	}}

	body := h.do(http.MethodGet, "/api/containers", nil, true).Body.String()
	if strings.Contains(body, "null") {
		t.Errorf("no list field may serialise as null:\n%s", body)
	}

	var parsed struct {
		Containers []struct {
			Name     string   `json:"name"`
			Networks []string `json:"networks"`
			Ports    []int    `json:"ports"`
		} `json:"containers"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	for _, c := range parsed.Containers {
		if c.Networks == nil || c.Ports == nil {
			t.Errorf("%s: networks=%v ports=%v, both must be lists", c.Name, c.Networks, c.Ports)
		}
	}
}

func TestConfig_ListFieldsAreNeverNull(t *testing.T) {
	// Same contract for the config payload: the browser iterates these.
	h := newHarness(t, "token: t\n")
	body := h.do(http.MethodGet, "/api/config", nil, true).Body.String()

	var parsed struct {
		Config struct {
			Containers []string   `json:"containers"`
			Routes     []struct{} `json:"routes"`
		} `json:"config"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Config.Containers == nil {
		t.Errorf("containers must be a list even when empty: %s", body)
	}
	if parsed.Config.Routes == nil {
		t.Errorf("routes must be a list even when empty: %s", body)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := newHarness(t, validConfig)
	for _, ep := range []struct{ method, path string }{
		{http.MethodDelete, "/api/config"},
		{http.MethodGet, "/api/validate"},
		{http.MethodGet, "/api/reload"},
		{http.MethodGet, "/api/login"},
	} {
		rec := h.do(ep.method, ep.path, nil, true)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", ep.method, ep.path, rec.Code)
		}
	}
}

func TestStaticPageIsServedWithSecurityHeaders(t *testing.T) {
	h := newHarness(t, validConfig)
	rec := h.do(http.MethodGet, "/", nil, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want the login page to be reachable", rec.Code)
	}
	for header, want := range map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'self'") {
		t.Errorf("CSP = %q", rec.Header().Get("Content-Security-Policy"))
	}
}

func TestRejectsUnknownJSONFields(t *testing.T) {
	// A typo in a field name should fail loudly rather than be silently dropped.
	h := newHarness(t, validConfig)
	req := httptest.NewRequest(http.MethodPost, "/api/validate",
		strings.NewReader(`{"containers":["web"],"totallyUnknown":1}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400 for an unknown field", rec.Code)
	}
}

func TestSplitErrors(t *testing.T) {
	joined := errors.Join(errors.New("first"), errors.New("second"))
	got := splitErrors(joined)
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("splitErrors = %v", got)
	}

	if got := splitErrors(errors.New("only one")); len(got) != 1 || got[0] != "only one" {
		t.Errorf("splitErrors = %v", got)
	}
}
