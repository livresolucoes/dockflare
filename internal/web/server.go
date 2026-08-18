// Package web serves the optional browser interface: a small JSON API plus a
// static page, both embedded in the binary so the image stays self-contained.
//
// The UI can rewrite production routing and, with manage_dns, write DNS
// records. It is therefore off unless web_ui.enabled is set, refuses to start
// without DOCKFLARE_UI_TOKEN, and — when published through the tunnel — refuses
// to accept a login over plain HTTP.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/livresolucoes/dockflare/internal/config"
	"github.com/livresolucoes/dockflare/internal/docker"
	"github.com/livresolucoes/dockflare/internal/ingress"
	"github.com/livresolucoes/dockflare/internal/logger"
)

//go:embed static
var staticFiles embed.FS

const (
	sessionCookie  = "dockflare_session"
	sessionTTL     = 12 * time.Hour
	maxRequestBody = 1 << 20 // 1 MiB — a config document is tiny
	shutdownGrace  = 5 * time.Second
)

// Reloader re-runs the pipeline. Implemented in cmd/dockflare, shared with the
// file watcher so a save through the UI and an edit on disk take the same path.
type Reloader interface {
	Reload(ctx context.Context) error
}

// ContainerLister is the slice of the Docker client the UI needs.
type ContainerLister interface {
	ListContainers(ctx context.Context) ([]docker.ContainerInfo, error)
	InspectContainer(ctx context.Context, nameOrID string) (*docker.ContainerInfo, error)
}

// ProcessStatus reports on the cloudflared subprocess.
type ProcessStatus interface {
	Running() bool
	PID() int
}

// NetworkStatus reports which Docker networks DockFlare has joined.
type NetworkStatus interface {
	Reachable(netName string) bool
	Tracking() bool
	JoinedNetworks() []string
}

type Options struct {
	ConfigPath string
	Token      string
	Port       int
	// Exposed is true when the UI is published through the tunnel, which is
	// what makes plain-HTTP logins a real risk rather than a local curiosity.
	Exposed  bool
	Docker   ContainerLister
	Nets     NetworkStatus
	Proc     ProcessStatus
	Reloader Reloader
	Log      *logger.Logger
}

type Server struct {
	opts Options

	mu       sync.Mutex
	sessions map[string]time.Time
}

func New(opts Options) *Server {
	return &Server{opts: opts, sessions: make(map[string]time.Time)}
}

// Start serves until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", s.opts.Port),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.opts.Log.Info("Web UI listening on port %d", s.opts.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// Handler builds the router. Exported so tests can drive it without a socket.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated: the login endpoint and the page that posts to it.
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/logout", s.handleLogout)

	mux.Handle("/api/config", s.authed(s.handleConfig))
	mux.Handle("/api/validate", s.authed(s.handleValidate))
	mux.Handle("/api/reload", s.authed(s.handleReload))
	mux.Handle("/api/status", s.authed(s.handleStatus))
	mux.Handle("/api/containers", s.authed(s.handleContainers))

	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		// Impossible: the directory is embedded at build time.
		panic(err)
	}
	mux.Handle("/", securityHeaders(http.FileServer(http.FS(sub))))
	return mux
}

// securityHeaders keeps the page from being framed or sniffed, and forbids
// loading anything from another origin — the UI is entirely self-hosted.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self'; script-src 'self'; form-action 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- auth

func (s *Server) authed(next http.HandlerFunc) http.Handler {
	return securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		next(w, r)
	}))
}

func (s *Server) authorized(r *http.Request) bool {
	// Bearer token, for curl and scripts.
	if header := r.Header.Get("Authorization"); strings.HasPrefix(header, "Bearer ") {
		if s.tokenMatches(strings.TrimPrefix(header, "Bearer ")) {
			return true
		}
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	return s.validSession(cookie.Value)
}

// tokenMatches compares in constant time so a wrong guess reveals nothing about
// how much of the token was right.
func (s *Server) tokenMatches(candidate string) bool {
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(s.opts.Token)) == 1
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	// Through the tunnel, Cloudflare tells us the scheme the browser used. A
	// plain-HTTP request means the token would cross the internet in the clear,
	// so refuse rather than accept it and hope.
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" && proto != "https" {
		writeError(w, http.StatusForbidden,
			"refusing to accept a token over plain HTTP — use https://")
		return
	}

	var body struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.tokenMatches(body.Token) {
		s.opts.Log.Warn("Web UI login rejected")
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}

	id, err := newSessionID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start a session")
		return
	}
	s.mu.Lock()
	s.sessions[id] = time.Now().Add(sessionTTL)
	s.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true, // unreadable from JS, so XSS cannot steal it
		SameSite: http.SameSiteStrictMode,
		Secure:   s.opts.Exposed || r.Header.Get("X-Forwarded-Proto") == "https",
		MaxAge:   int(sessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		s.mu.Lock()
		delete(s.sessions, cookie.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) validSession(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, ok := s.sessions[id]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(s.sessions, id)
		return false
	}
	return true
}

func newSessionID() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ---------------------------------------------------------------- config

// configPayload is what crosses the wire. It mirrors the editable parts of the
// config and, by construction, contains no token field at all.
type configPayload struct {
	Containers []string       `json:"containers"`
	Routes     []routePayload `json:"routes"`
	ManageDNS  bool           `json:"manageDns"`
	WebUI      webUIPayload   `json:"webUi"`
}

type routePayload struct {
	Hostname     string `json:"hostname"`
	Container    string `json:"container"`
	Port         int    `json:"port"`
	OriginScheme string `json:"originScheme"`
	ForceHTTPS   bool   `json:"forceHttps"`
}

type webUIPayload struct {
	Enabled  bool   `json:"enabled"`
	Port     int    `json:"port"`
	Hostname string `json:"hostname"`
}

func toPayload(cfg *config.Config) configPayload {
	out := configPayload{
		Containers: cfg.Containers,
		Routes:     make([]routePayload, 0, len(cfg.Routes)),
		ManageDNS:  cfg.ManageDNS,
		WebUI: webUIPayload{
			Enabled:  cfg.WebUI.Enabled,
			Port:     cfg.WebUI.Port,
			Hostname: cfg.WebUI.Hostname,
		},
	}
	if out.Containers == nil {
		out.Containers = []string{}
	}
	for _, r := range cfg.Routes {
		out.Routes = append(out.Routes, routePayload{
			Hostname:     r.Hostname,
			Container:    r.Container,
			Port:         r.Port,
			OriginScheme: r.OriginScheme,
			ForceHTTPS:   r.ForceHTTPS,
		})
	}
	return out
}

// fromPayload builds a Config from a request.
//
// web_ui is taken from disk, never from the request: letting the UI switch
// itself off or move its own port would lock the user out of the very page they
// are using. The same goes for the tokens, which have no payload field.
func fromPayload(p configPayload, current *config.Config) *config.Config {
	cfg := &config.Config{
		Token:      current.Token,
		APIToken:   current.APIToken,
		UIToken:    current.UIToken,
		Containers: p.Containers,
		ManageDNS:  p.ManageDNS,
		WebUI:      current.WebUI,
	}
	if cfg.Containers == nil {
		cfg.Containers = []string{}
	}
	for _, r := range p.Routes {
		cfg.Routes = append(cfg.Routes, config.Route{
			Hostname:     r.Hostname,
			Container:    r.Container,
			Port:         r.Port,
			OriginScheme: r.OriginScheme,
			ForceHTTPS:   r.ForceHTTPS,
		})
	}
	config.Normalize(cfg)
	return cfg
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg, err := config.Load(s.opts.ConfigPath)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"config":   toPayload(cfg),
			"writable": config.CheckWritable(s.opts.ConfigPath) == nil,
		})

	case http.MethodPut:
		s.saveConfig(w, r)

	default:
		writeError(w, http.StatusMethodNotAllowed, "GET or PUT required")
	}
}

// confirmDropAll is the query flag a caller must set to remove every route at
// once. Without it, a request carrying no routes is refused.
const confirmDropAll = "drop-all-routes"

func (s *Server) saveConfig(w http.ResponseWriter, r *http.Request) {
	cfg, problems, ok := s.parseAndValidate(w, r)
	if !ok {
		return
	}
	if len(problems) > 0 {
		// Nothing invalid ever reaches the file.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"problems": problems})
		return
	}

	// Wiping every route takes every hostname offline, and an empty request is
	// just as likely to be a UI that failed to render as a deliberate reset.
	// Require the caller to say it meant it.
	if len(cfg.Routes) == 0 && r.URL.Query().Get("confirm") != confirmDropAll {
		current, err := config.Load(s.opts.ConfigPath)
		if err == nil && len(current.Routes) > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": fmt.Sprintf(
					"this would remove all %d routes and take every hostname offline. "+
						"If that is intended, repeat the request with ?confirm=%s",
					len(current.Routes), confirmDropAll),
				"wouldRemove": len(current.Routes),
			})
			return
		}
	}

	if err := config.Save(s.opts.ConfigPath, cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.opts.Log.Info("Config saved from the web UI (%d routes)", len(cfg.Routes))

	if err := s.opts.Reloader.Reload(r.Context()); err != nil {
		// The file is already saved, so report it as a partial success rather
		// than implying nothing happened.
		writeJSON(w, http.StatusOK, map[string]any{
			"saved":       true,
			"reloaded":    false,
			"reloadError": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"saved": true, "reloaded": true})
}

func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	_, problems, ok := s.parseAndValidate(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"problems": problems})
}

// parseAndValidate runs the same checks the startup path runs: the static ones
// from config, then the live Docker ones from ingress. Reusing them is the point
// — the UI shows exactly the messages the logs would show.
func (s *Server) parseAndValidate(w http.ResponseWriter, r *http.Request) (*config.Config, []string, bool) {
	var payload configPayload
	if err := decodeJSON(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return nil, nil, false
	}
	current, err := config.Load(s.opts.ConfigPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, nil, false
	}

	cfg := fromPayload(payload, current)
	var problems []string
	if err := config.Validate(cfg); err != nil {
		problems = append(problems, splitErrors(err)...)
	}
	// Only worth asking Docker once the shape is right.
	if len(problems) == 0 && len(cfg.Routes) > 0 {
		if err := ingress.Resolve(r.Context(), cfg.Routes, s.opts.Docker, s.opts.Nets); err != nil {
			problems = append(problems, splitErrors(err)...)
		}
	}
	return cfg, problems, true
}

// ---------------------------------------------------------------- status

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if err := s.opts.Reloader.Reload(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reloaded": true})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	status := map[string]any{
		"cloudflaredRunning": s.opts.Proc.Running(),
		"cloudflaredPid":     s.opts.Proc.PID(),
		"joinedNetworks":     s.opts.Nets.JoinedNetworks(),
		"networkTracking":    s.opts.Nets.Tracking(),
		"configWritable":     config.CheckWritable(s.opts.ConfigPath) == nil,
	}
	if err := config.CheckWritable(s.opts.ConfigPath); err != nil {
		status["configWritableError"] = err.Error()
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	list, err := s.opts.Docker.ListContainers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, c := range list {
		reachable := !s.opts.Nets.Tracking()
		for _, n := range c.Networks {
			if s.opts.Nets.Reachable(n) {
				reachable = true
				break
			}
		}
		out = append(out, map[string]any{
			"name": c.Name,
			// Normalised here as well as at the source: a nil slice marshals to
			// `null`, and the browser then reads `.length` off it. Every field
			// this API declares as a list must always be a list.
			"networks":  orEmptyStrings(c.Networks),
			"ports":     orEmptyInts(c.Ports),
			"reachable": reachable,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"containers": out})
}

func orEmptyStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func orEmptyInts(in []int) []int {
	if in == nil {
		return []int{}
	}
	return in
}

// ---------------------------------------------------------------- helpers

func decodeJSON(r *http.Request, out any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// splitErrors turns a joined/aggregate error into one string per problem, so the
// UI can list them instead of showing a wall of text.
func splitErrors(err error) []string {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var out []string
		for _, e := range joined.Unwrap() {
			out = append(out, splitErrors(e)...)
		}
		if len(out) > 0 {
			return out
		}
	}
	// errors.Join renders as newline-separated text when it cannot be unwrapped.
	parts := strings.Split(err.Error(), "\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
