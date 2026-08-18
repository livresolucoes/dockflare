"use strict";

// DockFlare web UI. No framework, no build step: the file is served straight
// from the binary.
//
// The server is the source of truth for validation — this page never decides on
// its own whether a config is acceptable, it asks /api/validate and renders
// whatever comes back. That keeps one set of rules for the UI, the file watcher
// and startup.

const $ = (id) => document.getElementById(id);

/** Current editable state, mirroring the JSON payload the API expects. */
let state = { containers: [], routes: [], manageDns: false, webUi: {} };
/** Containers discovered on the host, for the pickers. */
let hostContainers = [];
/** How many routes the loaded config had, to detect a wipe before saving. */
let loadedRouteCount = 0;

// ---------------------------------------------------------------- errors
//
// A thrown exception used to abort the boot sequence silently, leaving a
// half-rendered page with no clue why. Everything below makes a failure visible
// instead: whatever breaks, the user sees the message rather than a blank panel.

/** AuthRequired is the expected 401 on first load, not something to report. */
class AuthRequired extends Error {}

function reportFatal(err) {
  if (err instanceof AuthRequired) return;
  const message = (err && (err.message || err.toString())) || "erro desconhecido";
  const where = err && err.stack ? ` (${err.stack.split("\n")[1] || ""})`.trim() : "";
  const text = `Erro na interface: ${message} ${where}`.trim();

  if (!$("app").hidden) {
    banner("err", text);
  } else {
    showLogin(text);
  }
  console.error(err);
}

window.addEventListener("error", (e) => reportFatal(e.error || new Error(e.message)));
window.addEventListener("unhandledrejection", (e) => reportFatal(e.reason));

// ---------------------------------------------------------------- transport

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });

  let data = {};
  try {
    data = await res.json();
  } catch {
    // Empty or non-JSON body; the status still tells us what happened.
  }

  if (res.status === 401) {
    showLogin();
    throw new AuthRequired("sessão expirada");
  }
  return { status: res.status, ok: res.ok, data };
}

// ---------------------------------------------------------------- login

function showLogin(message) {
  $("app").hidden = true;
  $("login").hidden = false;
  const err = $("login-error");
  err.hidden = !message;
  err.textContent = message || "";
}

async function login(event) {
  event.preventDefault();
  const token = $("token").value;
  const res = await fetch("/api/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ token }),
  });

  if (!res.ok) {
    let msg = "token inválido";
    try {
      msg = (await res.json()).error || msg;
    } catch {}
    showLogin(msg);
    return;
  }
  $("token").value = "";
  await start();
}

async function logout() {
  await fetch("/api/logout", { method: "POST" });
  showLogin();
}

// ---------------------------------------------------------------- rendering

function renderContainers() {
  const list = $("containers");
  list.textContent = "";

  if (state.containers.length === 0) {
    const li = document.createElement("li");
    li.textContent = "nenhum container configurado";
    list.append(li);
  }

  state.containers.forEach((name, index) => {
    const li = document.createElement("li");
    // Flag names Docker does not know about — a typo here is the most common
    // reason a route fails validation.
    const known = hostContainers.some((c) => c.name === name);
    if (!known) {
      li.className = "missing";
      li.title = "container não encontrado no Docker";
    }

    const label = document.createElement("span");
    label.textContent = name;

    const remove = document.createElement("button");
    remove.type = "button";
    remove.className = "danger";
    remove.textContent = "×";
    remove.title = `remover ${name}`;
    remove.addEventListener("click", () => {
      state.containers.splice(index, 1);
      renderContainers();
      renderRoutes();
    });

    li.append(label, remove);
    list.append(li);
  });

  // The picker offers only containers not already listed.
  const select = $("container-add");
  select.textContent = "";
  const available = hostContainers.filter((c) => !state.containers.includes(c.name));
  if (available.length === 0) {
    const opt = document.createElement("option");
    opt.textContent = "todos os containers já estão na lista";
    opt.value = "";
    select.append(opt);
    $("container-add-btn").disabled = true;
    return;
  }
  $("container-add-btn").disabled = false;
  available.forEach((c) => {
    const opt = document.createElement("option");
    opt.value = c.name;
    const ports = c.ports || [];
    opt.textContent = ports.length ? `${c.name} — portas ${ports.join(", ")}` : c.name;
    select.append(opt);
  });
}

function renderRoutes() {
  const body = $("routes-body");
  body.textContent = "";

  state.routes.forEach((route, index) => {
    const tr = document.createElement("tr");
    tr.append(
      cell(textInput(route.hostname, "app.example.com", (v) => (route.hostname = v))),
      cell(containerSelect(route)),
      cell(numberInput(route.port, (v) => (route.port = v))),
      cell(schemeSelect(route)),
      cell(checkbox(route.forceHttps, (v) => (route.forceHttps = v))),
      cell(removeButton(index)),
    );
    body.append(tr);
  });
}

function cell(child) {
  const td = document.createElement("td");
  td.append(child);
  return td;
}

function textInput(value, placeholder, onChange) {
  const input = document.createElement("input");
  input.type = "text";
  input.value = value || "";
  input.placeholder = placeholder;
  input.addEventListener("input", () => onChange(input.value.trim()));
  return input;
}

function numberInput(value, onChange) {
  const input = document.createElement("input");
  input.type = "number";
  input.min = "1";
  input.max = "65535";
  input.value = value || "";
  input.placeholder = "3000";
  input.addEventListener("input", () => onChange(parseInt(input.value, 10) || 0));
  return input;
}

function containerSelect(route) {
  const select = document.createElement("select");

  // Only containers in the configured list can be routed to, so offering
  // anything else would just produce a validation error later.
  const options = [...state.containers];
  if (route.container && !options.includes(route.container)) {
    options.unshift(route.container); // keep an unknown value visible
  }
  if (options.length === 0) {
    const opt = document.createElement("option");
    opt.value = "";
    opt.textContent = "adicione um container primeiro";
    select.append(opt);
    select.disabled = true;
    return select;
  }

  options.forEach((name) => {
    const opt = document.createElement("option");
    opt.value = name;
    opt.textContent = name;
    opt.selected = name === route.container;
    select.append(opt);
  });
  select.addEventListener("change", () => {
    route.container = select.value;
    suggestPort(route);
    renderRoutes();
  });
  return select;
}

// suggestPort fills an empty port when the chosen container exposes exactly one,
// which covers most cases and avoids a guess.
function suggestPort(route) {
  if (route.port) return;
  const found = hostContainers.find((c) => c.name === route.container);
  const ports = (found && found.ports) || [];
  if (ports.length === 1) route.port = ports[0];
}

function schemeSelect(route) {
  const select = document.createElement("select");
  [
    ["http", "http"],
    ["https", "https"],
  ].forEach(([value, label]) => {
    const opt = document.createElement("option");
    opt.value = value;
    opt.textContent = label;
    opt.selected = (route.originScheme || "http") === value;
    select.append(opt);
  });
  select.title = "o que o container fala internamente";
  select.addEventListener("change", () => (route.originScheme = select.value));
  return select;
}

function checkbox(checked, onChange) {
  const input = document.createElement("input");
  input.type = "checkbox";
  input.checked = !!checked;
  input.addEventListener("change", () => onChange(input.checked));
  return input;
}

function removeButton(index) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = "danger";
  button.textContent = "×";
  button.title = "remover rota";
  button.addEventListener("click", () => {
    state.routes.splice(index, 1);
    renderRoutes();
  });
  return button;
}

function renderWebUIInfo() {
  const dl = $("webui-info");
  dl.textContent = "";
  addStatus(dl, "Interface", state.webUi.enabled ? "ativa" : "desativada");
  addStatus(dl, "Porta", String(state.webUi.port || "—"));
  addStatus(
    dl,
    "Publicada no túnel",
    state.webUi.hostname ? state.webUi.hostname : "não (somente pela porta)",
  );
}

function addStatus(dl, label, value, cls) {
  const dt = document.createElement("dt");
  dt.textContent = label;
  const dd = document.createElement("dd");
  dd.textContent = value;
  if (cls) dd.className = cls;
  dl.append(dt, dd);
}

// ---------------------------------------------------------------- status

async function refreshStatus() {
  const { ok, data } = await api("GET", "/api/status");
  if (!ok) return;

  const dl = $("status-list");
  dl.textContent = "";
  addStatus(
    dl,
    "cloudflared",
    data.cloudflaredRunning ? `rodando (pid ${data.cloudflaredPid})` : "parado",
    data.cloudflaredRunning ? "ok" : "bad",
  );
  addStatus(
    dl,
    "Redes conectadas",
    data.joinedNetworks && data.joinedNetworks.length
      ? data.joinedNetworks.join(", ")
      : data.networkTracking
        ? "nenhuma"
        : "não gerenciadas (fora do Docker)",
  );
  addStatus(
    dl,
    "config.yml",
    data.configWritable ? "gravável" : data.configWritableError || "somente leitura",
    data.configWritable ? "ok" : "bad",
  );

  const pill = $("status-pill");
  pill.textContent = data.cloudflaredRunning ? "túnel ativo" : "túnel parado";
  pill.className = "pill " + (data.cloudflaredRunning ? "ok" : "bad");

  if (!data.configWritable) {
    banner(
      "err",
      "O config.yml não é gravável — salvar vai falhar. Monte o diretório (./config:/config), não o arquivo, e sem :ro.",
    );
  }
}

// ---------------------------------------------------------------- actions

function banner(kind, message) {
  const el = $("banner");
  el.hidden = false;
  el.className = "banner " + kind;
  el.textContent = message;
}

function showProblems(problems) {
  const section = $("problems");
  const list = $("problems-list");
  list.textContent = "";

  if (!problems || problems.length === 0) {
    section.hidden = true;
    return false;
  }
  problems.forEach((p) => {
    const li = document.createElement("li");
    li.textContent = p;
    list.append(li);
  });
  section.hidden = false;
  return true;
}

function payload() {
  return {
    containers: state.containers,
    routes: state.routes.map((r) => ({
      hostname: r.hostname || "",
      container: r.container || "",
      port: r.port || 0,
      originScheme: r.originScheme || "http",
      forceHttps: !!r.forceHttps,
    })),
    manageDns: state.manageDns,
    webUi: state.webUi,
  };
}

async function withBusy(button, fn) {
  const label = button.textContent;
  button.disabled = true;
  button.textContent = "…";
  try {
    await fn();
  } catch (err) {
    banner("err", err.message || String(err));
  } finally {
    button.disabled = false;
    button.textContent = label;
  }
}

async function validate() {
  await withBusy($("validate"), async () => {
    const { data } = await api("POST", "/api/validate", payload());
    if (showProblems(data.problems)) {
      banner("warn", `${data.problems.length} problema(s) encontrado(s).`);
    } else {
      banner("ok", "Configuração válida.");
    }
  });
}

async function save() {
  // Saving an empty table would take every hostname offline, and the server
  // refuses it without an explicit confirmation. Ask here so the user sees what
  // is about to happen rather than an error.
  let query = "";
  if (state.routes.length === 0 && loadedRouteCount > 0) {
    const message =
      `Isso vai REMOVER todas as ${loadedRouteCount} rotas e derrubar todos os hostnames.\n\n` +
      `Se a tabela ficou vazia por um erro da tela, cancele e recarregue a página.\n\n` +
      `Continuar?`;
    if (!window.confirm(message)) return;
    query = "?confirm=drop-all-routes";
  }

  await withBusy($("save"), async () => {
    const { status, data } = await api("PUT", "/api/config" + query, payload());

    if (status === 422) {
      showProblems(data.problems);
      banner("err", "Nada foi gravado — corrija os problemas abaixo.");
      return;
    }
    if (status === 409) {
      banner("err", data.error);
      return;
    }
    if (data.error) {
      banner("err", data.error);
      return;
    }

    showProblems([]);
    loadedRouteCount = state.routes.length;
    if (data.reloaded) {
      banner("ok", "Salvo e aplicado. O túnel não foi reiniciado.");
    } else {
      banner("warn", `Salvo, mas o reload falhou: ${data.reloadError}`);
    }
    await refreshStatus();
  });
}

async function reload() {
  await withBusy($("reload"), async () => {
    const { data } = await api("POST", "/api/reload");
    if (data.error) banner("err", data.error);
    else banner("ok", "Recarregado.");
    await refreshStatus();
  });
}

// ---------------------------------------------------------------- boot

async function start() {
  const config = await api("GET", "/api/config");
  if (!config.ok) {
    // 401 already redirected to the login screen; anything else is a real error.
    if (config.data && config.data.error) showLogin(config.data.error);
    return;
  }

  state = config.data.config || {};
  state.containers = state.containers || [];
  state.routes = state.routes || [];
  state.webUi = state.webUi || {};
  loadedRouteCount = state.routes.length;

  const containers = await api("GET", "/api/containers");
  hostContainers = containers.ok ? containers.data.containers || [] : [];

  $("login").hidden = true;
  $("app").hidden = false;
  $("manage-dns").checked = !!state.manageDns;

  // Each panel is rendered independently: one that fails must not take the rest
  // of the page down with it.
  step("containers", renderContainers);
  step("rotas", renderRoutes);
  step("interface", renderWebUIInfo);
  await refreshStatus();

  if (state.routes.length === 0) {
    banner("warn", "Nenhuma rota configurada — o roteamento está sendo definido no dashboard Zero Trust.");
  }
}

/** step runs a render function and reports a failure without aborting the boot. */
function step(label, fn) {
  try {
    fn();
  } catch (err) {
    reportFatal(new Error(`ao renderizar ${label}: ${err.message}`));
  }
}

function wire() {
  $("login-form").addEventListener("submit", login);
  $("logout").addEventListener("click", logout);

  $("container-add-btn").addEventListener("click", () => {
    const name = $("container-add").value;
    if (!name || state.containers.includes(name)) return;
    state.containers.push(name);
    renderContainers();
    renderRoutes();
  });

  $("route-add").addEventListener("click", () => {
    state.routes.push({
      hostname: "",
      container: state.containers[0] || "",
      port: 0,
      originScheme: "http",
      forceHttps: false,
    });
    suggestPort(state.routes[state.routes.length - 1]);
    renderRoutes();
  });

  $("manage-dns").addEventListener("change", (e) => (state.manageDns = e.target.checked));
  $("validate").addEventListener("click", validate);
  $("save").addEventListener("click", save);
  $("reload").addEventListener("click", reload);
}

wire();
// An existing session cookie means we can go straight in; otherwise start()
// gets a 401 and the login screen appears.
start().catch(reportFatal);
