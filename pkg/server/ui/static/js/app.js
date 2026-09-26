// Vanilla, dependency-free, and never builds markup from data: every value from
// the API goes in with textContent, so a container named "<script>" is just text.
// (The markup below is built with createElement for the same reason.)
const KEY = "native-ops-token";
const REFRESH_MS = 10000;
const view = document.getElementById("view");
let timer = null;
let snapshot = null;
let loadError = "";

// h("div", {class: "card"}, child, "text", ...) -> element. Strings become text nodes.
function h(tag, attrs, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === false || v === null || v === undefined) continue;
    e.setAttribute(k, v === true ? "" : String(v));
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    e.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return e;
}

const icon = (name, extra) => h("i", { class: `fa-solid fa-${name}${extra ? " " + extra : ""}` });

// A Bootstrap card the way the theta-suite apps draw one: icon left, title centred, actions right.
function card(iconName, title, body, right) {
  return h("div", { class: "card shadow-lg mb-3" },
    h("div", { class: "card-header text-center" },
      h("span", { class: "card-icon float-start" }, icon(iconName)),
      h("span", { class: "card-title" }, title),
      h("span", { class: "float-end" }, right || null)),
    body);
}

function table(head, rows) {
  const cell = (tag, c) => {
    if (c && typeof c === "object" && !(c instanceof Node)) return h(tag, {}, h("span", { class: c.cls }, c.text));
    return h(tag, {}, c ?? "");
  };
  return h("div", { class: "table-responsive" },
    h("table", { class: "table table-hover table-sm align-middle mb-0" },
      h("thead", {}, h("tr", {}, head.map((c) => cell("th", c)))),
      h("tbody", {}, rows.map((r) => h("tr", {}, r.map((c) => cell("td", c)))))));
}

const empty = (text) => h("div", { class: "card-body text-muted" }, text);
const badge = (text, kind) => ({ text, cls: `badge text-bg-${kind}` });
const plural = (n, word) => `${n} ${word}${n === 1 ? "" : "s"}`;

async function api(path) {
  const token = sessionStorage.getItem(KEY);
  const res = await fetch(path, { headers: { Authorization: `Bearer ${token}` }, cache: "no-store" });
  if (res.status === 401) throw Object.assign(new Error("unauthorized"), { auth: true });
  if (!res.ok) throw new Error(`${res.status} ${(await res.json().catch(() => ({}))).error || res.statusText}`);
  return res.json();
}

// ---- views -----------------------------------------------------------------

function loginView(message) {
  const token = h("input", { type: "password", class: "form-control", placeholder: "nops_…", autocomplete: "off", spellcheck: "false", "aria-label": "API token", required: true });
  const error = h("div", { class: "alert alert-danger mt-3 mb-0", role: "alert", hidden: !message }, message || "");
  const form = h("form", { autocomplete: "off" },
    h("div", { class: "mb-3" },
      h("div", { class: "input-group" }, h("span", { class: "input-group-text" }, icon("key")), token)),
    h("p", { class: "text-muted small" }, "Paste an API token. It is kept for this browser tab only."),
    h("button", { type: "submit", class: "btn btn-info" }, "Sign in"),
    error);
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    sessionStorage.setItem(KEY, token.value.trim());
    token.value = "";
    start();
  });
  return h("div", { class: "row justify-content-center" },
    h("div", { class: "col-md-4" }, card("lock", "API Token Login", h("div", { class: "card-body" }, form))));
}

function warningsCard(s) {
  const w = s.warnings || [];
  if (!w.length) {
    return card("circle-check", "Attention", h("div", { class: "card-body text-success" }, icon("check", "me-2"), "Nothing needs attention."));
  }
  return card("triangle-exclamation", "Attention",
    h("ul", { class: "list-group list-group-flush" }, w.map((m) => h("li", { class: "list-group-item list-group-item-warning" }, m))),
    h("span", { class: "badge text-bg-warning" }, plural(w.length, "thing")));
}

function hostCard(s) {
  const im = s.images || { count: 0, total_bytes: 0, unaliased: 0 };
  const line = (label, value) => h("tr", {}, h("th", { class: "w-25" }, label), h("td", {}, value));
  return card("server", "Host",
    h("div", { class: "table-responsive" }, h("table", { class: "table table-sm mb-0" }, h("tbody", {},
      line("Host", s.host || ""),
      line("Incus", s.incus_version || ""),
      line("Storage pool", s.pool || ""),
      line("Instances", `${(s.instances || []).filter((i) => i.status === "Running").length} running of ${(s.instances || []).length}`),
      line("Volumes", (s.volumes || []).length),
      line("Images", `${im.count} (${Math.round(im.total_bytes / 1e6)} MB), ${im.unaliased} without an alias`)))));
}

function overviewView(s) {
  return h("div", { class: "row" },
    h("div", { class: "col-lg-6" }, hostCard(s)),
    h("div", { class: "col-lg-6" }, warningsCard(s)));
}

function instancesView(s) {
  const rows = (s.instances || []).map((i) => [
    i.name,
    badge(i.status, i.status === "Running" ? "success" : "danger"),
    (i.recorded && i.recorded.image) || i.image || i.base_image || "",
    (i.ipv4 || []).join(", "),
    Object.entries(i.limits || {}).map(([k, v]) => `${k.replace("limits.", "")}=${v}`).join(" "),
    (i.devices || []).filter((d) => d.type === "disk" && d.source).map((d) => `${d.source}${d.host_path ? " (host path)" : ""}`).join(", "),
    i.snapshots,
    (i.env_keys || []).length || "",
  ]);
  return card("cubes", "Instances",
    rows.length ? table(["Name", "Status", "Image", "IPv4", "Limits", "Volumes", "Snapshots", "Env keys"], rows) : empty("No instances on this host."),
    h("span", { class: "badge text-bg-secondary" }, rows.length));
}

function volumesView(s) {
  const rows = (s.volumes || []).map((v) => [
    v.name,
    (v.used_by || []).join(", ") || badge("unattached", "secondary"),
    v.security_shifted || "",
    v.snapshots,
    v.latest_daily || "",
  ]);
  return card("hard-drive", "Volumes",
    rows.length ? table(["Name", "Used by", "Shifted", "Snapshots", "Latest daily"], rows) : empty("No custom volumes in this pool."),
    h("span", { class: "badge text-bg-secondary" }, rows.length));
}

const routes = { "/": overviewView, "/instances": instancesView, "/volumes": volumesView };

function currentRoute() {
  const path = location.hash.replace(/^#/, "") || "/";
  return routes[path] ? path : "/";
}

function render() {
  const signedIn = !!sessionStorage.getItem(KEY);
  document.getElementById("top-nav").hidden = !signedIn;
  document.getElementById("signout").hidden = !signedIn;
  const route = currentRoute();
  for (const a of document.querySelectorAll(".top-nav a")) {
    a.classList.toggle("active", a.getAttribute("href") === "#" + route);
  }
  if (!signedIn) return view.replaceChildren(h("div", { class: "mt-4" }, loginView(loadError)));

  const parts = [];
  if (loadError) parts.push(h("div", { class: "alert alert-warning", role: "alert" }, loadError));
  if (snapshot) parts.push(routes[route](snapshot));
  else if (!loadError) parts.push(h("div", { class: "text-muted mt-4 text-center" }, "Loading…"));
  if (snapshot) parts.push(h("p", { class: "text-muted small text-end" }, `Updated ${new Date(snapshot.time).toLocaleTimeString()}`));
  view.replaceChildren(h("div", { class: "mt-4" }, parts));
}

// ---- session ---------------------------------------------------------------

async function refresh() {
  try {
    snapshot = await api("/v1/status");
    loadError = "";
  } catch (e) {
    if (e.auth) return signOut("That token was not accepted.");
    loadError = `Could not refresh: ${e.message}`;
  }
  render();
}

function start() {
  loadError = "";
  snapshot = null;
  render();
  api("/v1/whoami").then((w) => {
    document.getElementById("who-text").textContent = `${w.name} (${w.role})`;
    document.getElementById("who").hidden = false;
  }).catch(() => {});
  refresh();
  clearInterval(timer);
  timer = setInterval(refresh, REFRESH_MS);
}

function signOut(message) {
  sessionStorage.removeItem(KEY);
  clearInterval(timer);
  snapshot = null;
  loadError = message || "";
  document.getElementById("who").hidden = true;
  document.getElementById("who-text").textContent = "";
  render();
}

document.getElementById("signout").addEventListener("click", () => signOut());
window.addEventListener("hashchange", render);

// The footer shows the daemon's version; /healthz is open and carries nothing else.
fetch("/healthz", { cache: "no-store" }).then((r) => r.json()).then((j) => {
  if (j.version) document.getElementById("version").textContent = `native-ops ${j.version}`;
}).catch(() => {});

if (sessionStorage.getItem(KEY)) start(); else render();
