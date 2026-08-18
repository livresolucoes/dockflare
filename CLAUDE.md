# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

**DockFlare** — "Cloudflare Tunnel for Docker, done right."

Connects Docker containers to a Cloudflare Tunnel with minimal configuration. DockFlare joins the right Docker networks so `cloudflared` can reach containers by name. Routing (hostname → service) is either left to the Cloudflare Zero Trust dashboard (default) or driven from `config.yml` via the optional `routes:` section.

## Build & Run Commands

```bash
# Build the binary
go build -o bin/dockflare ./cmd/dockflare

# Build static binary (for Docker/Alpine)
CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o bin/dockflare ./cmd/dockflare

# Run locally (requires Docker socket, a config.yml, and TUNNEL_TOKEN env var)
TUNNEL_TOKEN=eyJh... ./bin/dockflare --config config.yml

# Run all tests
go test ./...

# Run tests for a specific package
go test ./internal/config/...

# Lint (install golangci-lint first)
golangci-lint run

# Build Docker image
docker build -t dockflare:latest .

# Download dependencies
go mod tidy
```

## Architecture

```
cmd/dockflare/        — main entrypoint; wires together all internal packages
internal/config/      — YAML parsing; token resolution (env var > config file); static route validation
internal/docker/      — Docker SDK client: container lookup, network connect/disconnect, exposed ports
internal/cloudflared/ — manages the cloudflared subprocess (start/stop/reload/crash recovery)
internal/cloudflare/  — Cloudflare REST API client (tunnel ingress + DNS) and connector-token decoding
internal/ingress/     — routes → ingress rules; Docker destination resolution; reconciles the tunnel
internal/network/     — orchestration: connects DockFlare to each container's Docker networks
internal/watcher/     — fsnotify-based file watcher; triggers reload on config.yml changes
internal/web/         — optional web UI: JSON API + go:embed static page; auth, sessions
internal/logger/      — shared structured logger with [INFO]/[WARN]/[ERROR] prefix format
cmd/dockflare/reload.go — the reloader shared by the file watcher and the web UI
```

### Data flow

1. `config` reads `config.yml` + env vars → produces `Config{Token, Containers, Routes, ManageDNS, APIToken}`
2. `docker` inspects each container → discovers its Docker networks and exposed ports
3. `network` connects the DockFlare container to every network the target containers are on
4. `ingress` (only when `routes` is non-empty) validates each route against live Docker state, builds the ingress list and asks `cloudflare` to update the tunnel — but only when the desired state differs
5. `cloudflared` starts `cloudflared tunnel run --token TOKEN` as a managed subprocess
6. Cloudflare pushes ingress config to `cloudflared` via remote API — either what DockFlare just wrote, or the dashboard's own if no routes are configured
7. `watcher` detects `config.yml` changes and re-runs steps 1–4; cloudflared is reloaded

Networks are synced before routes are validated: reachability is only true after the join.

### Key design constraints

- **Token-only auth for the tunnel** — connector tokens from the Zero Trust dashboard are the only supported `cloudflared` auth method. No credentials file, no locally-generated ingress config file — even in automatic mode DockFlare writes the *remote* config the connector receives.
- **Routing is opt-in** — with no `routes:`, DockFlare makes zero Cloudflare API calls and the dashboard owns all hostname → service routing, exactly as before. With `routes:`, `config.yml` is the complete desired state, including removals.
- **Cloudflare API is confined to `internal/cloudflare`** — no other package builds a Cloudflare HTTP request. DockFlare still never creates tunnels; users pre-create them via the dashboard.
- **Account ID and tunnel ID come from the connector token** — it is base64 JSON `{"a":account,"t":tunnel,"s":secret}`, so no extra identifiers are needed in config.
- **Secrets never touch config.yml** — `CLOUDFLARE_API_TOKEN` has no YAML field at all (`yaml:"-"`), and neither token is ever logged or included in an error.
- **Whole-state ingress** — the list is replaced wholesale, so any validation failure aborts the update instead of applying a partial set that would take working hostnames offline.
- **Three HTTPS hops, only one is ours** — browser→edge (automatic Universal SSL) and edge→cloudflared (inherent to the tunnel) need no configuration. `origin_scheme` controls only cloudflared→container, and implies `noTLSVerify` because a container's certificate cannot match its Docker network name.
- **`force_https` is per hostname, never zone-wide** — implemented as a Redirect Rule scoped to one hostname, not the zone's "Always Use HTTPS" toggle, so a shared zone's other hostnames are never affected.
- **Shared lists are merged, not replaced** — the redirect ruleset and each rule's `originRequest` hold config DockFlare does not own. Foreign redirect rules are round-tripped verbatim (minus `version`/`last_updated`) and identified by the `dockflare:` description prefix; DockFlare's rules are appended **last** so pre-existing rules keep precedence. Inside `originRequest`, DockFlare owns only `noTLSVerify`.
- **Secondary concerns warn, never fail** — DNS and redirect-rule errors are logged as `[WARN]`: the ingress is already live and both need zone permissions the tunnel does not.
- **One reload path, and it does not restart the tunnel** — `cmd/dockflare/reload.go` is shared by the file watcher and the web UI, serialised by a mutex. `cloudflared` is bounced only when `TUNNEL_TOKEN` changes, since that is the only thing it reads from the command line; routes, DNS and redirects reach the connector through Cloudflare's remote config, so a routing edit causes no downtime.
- **The web UI is optional, off by default, and cannot unlock itself** — enabling it requires `web_ui.enabled` plus a `DOCKFLARE_UI_TOKEN` of at least 32 characters; publishing it through the tunnel is a *second* opt-in (`web_ui.hostname`) that also requires `routes`, because writing ingress where DockFlare previously wrote none would wipe dashboard-managed routing. `PUT /api/config` ignores the `webUi` field of the request and takes those settings from disk: letting the UI disable itself or move its own port would lock the user out.
- **`config.Save` cannot leak a secret** — it marshals a separate `fileDoc` type rather than `Config`, and carries `token:` over from the file on disk rather than from `cfg.Token`, which may hold the value of `TUNNEL_TOKEN`. Writes are atomic (temp + rename), which is why the config *directory* must be bind-mounted, not the file.
- **No database** — configuration is a YAML file; the web UI edits that file and nothing else.
- **Docker socket** — all container/network operations go through `/var/run/docker.sock` via the official Go SDK.
- **cloudflared binary** — bundled inside the Docker image (downloaded at build time); DockFlare manages it as a subprocess.

### Config schema (user-facing `config.yml`)

```yaml
# Token: prefer TUNNEL_TOKEN env var; this field is a fallback
# token: eyJh...

containers:
  - my_app        # Docker container name
  - grafana

# Optional. Absent → ingress is never touched.
routes:
  - hostname: app.example.com
    container: my_app
    port: 8080              # port INSIDE the container, not a host-published port
    origin_scheme: https    # optional, default http — what the CONTAINER speaks
    force_https: yes        # optional, default false — redirect http→https at the edge

# Optional, default false. Also manages the proxied CNAME per hostname.
manage_dns: false

# Optional, default off. Needs DOCKFLARE_UI_TOKEN (32+ chars) and the config
# DIRECTORY bind-mounted read-write.
web_ui:
  enabled: true
  port: 8080
  # Second, separate opt-in: publishes the UI through the tunnel itself, with
  # origin http://localhost:<port>. Requires routes to be configured.
  hostname: dockflare.example.com
```

A route has exactly one target. `config.Route.Targets()` returns a slice so a
future `targets:` list (Cloudflare Load Balancing) can land without reshaping
the pipeline; the multi-target YAML form is deliberately not parsed yet.

### Token precedence

`TUNNEL_TOKEN` environment variable > `token:` field in config.yml

`CLOUDFLARE_API_TOKEN` environment variable only — required when `routes` is set.

`DOCKFLARE_UI_TOKEN` environment variable only — required when `web_ui.enabled`.

### Cloudflare API endpoints

- `GET`/`PUT /accounts/{account_id}/cfd_tunnel/{tunnel_id}/configurations` — tunnel ingress
- `GET /zones` — zone lookup by longest hostname suffix; cached per process
- `GET`/`POST`/`PATCH /zones/{zone_id}/dns_records` — only with `manage_dns: true`
- `GET`/`PUT /zones/{zone_id}/rulesets/phases/http_request_dynamic_redirect/entrypoint` — only with `force_https`

The GET before every PUT is what preserves `warp-routing`, `originRequest` and
foreign redirect rules, none of which DockFlare owns. The redirect endpoint
answers 404 for a zone with no rules yet — `ErrNotFound` makes that mean
"empty", not "broken".

Required token permissions, by feature: `Account · Cloudflare Tunnel · Edit`
always; `Zone · DNS · Edit` for `manage_dns`; `Zone · Single Redirect · Edit` for
`force_https` — the API phase is `http_request_dynamic_redirect`, but the
dashboard names the product "Single Redirects", so that is the label to look for
in the token editor.

Permissions are granted per zone, so `force_https` across several zones can
partially fail. `SyncHTTPSRedirects` therefore collects errors and continues
instead of returning on the first — one denied zone must not stop the rest — and
returns the zones it wrote alongside the joined error.

### cloudflared subprocess command

```
cloudflared tunnel run --token <TOKEN>
```

No `--config` flag. Ingress rules come from the Cloudflare dashboard.

## Log format

```
[INFO] Connected container meuapp_api to network meuapp_default
[WARN] Container grafana not found, skipping
[INFO] Network sync complete: 2 containers
[INFO] Ingress route api.meuapp.example.com → http://meuapp_api:3000
[INFO] Ingress updated: 2 routes
[INFO] HTTPS redirect rules updated in zone meuapp.example.com
[INFO] cloudflared started (pid 11)
[INFO] Config changed, reloading
[INFO] Ingress unchanged, skipping Cloudflare API (2 routes)
```

## Dockerfile structure

Multi-stage build:
1. `golang:1.22-alpine` builder — compiles the static `dockflare` binary
2. Second stage downloads the `cloudflared` binary from the official Cloudflare release URL
3. Final `alpine` image — copies both binaries, sets `dockflare` as the entrypoint

## Future extensibility (not implemented)

The architecture should not block these later additions: Docker label auto-discovery, TCP tunnel support, multi-target routes / Cloudflare Load Balancing, healthchecks, and Prometheus metrics.
