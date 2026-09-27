// Vanilla, dependency-free, and never builds markup from data: every value from
// the API goes in with textContent, so a container named "<script>" is just text.
// (The markup below is built with createElement for the same reason.)
const KEY = "native-ops-token";
const REFRESH_MS = 10000;
const view = document.getElementById("view");
let timer = null;
let snapshot = null;
let loadError = "";
let role = "";      // the signed-in token's role (viewer, planner, deployer, admin)
let extra = null;   // data for the current Plans / Jobs page: {path, data}
let notice = null;  // {kind, text} shown above the page after an action

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

async function api(path, method) {
  const token = sessionStorage.getItem(KEY);
  const res = await fetch(path, { method: method || "GET", headers: { Authorization: `Bearer ${token}` }, cache: "no-store" });
  if (res.status === 401) throw Object.assign(new Error("unauthorized"), { auth: true });
  if (!res.ok) throw Object.assign(new Error(`${res.status} ${(await res.json().catch(() => ({}))).error || res.statusText}`), { status: res.status });
  return res.json();
}

const when = (t) => (t ? new Date(t).toLocaleString() : "");
const short = (hash) => (hash || "").slice(0, 12);
const summary = (c) => Object.entries(c || {}).filter(([, n]) => n).map(([k, n]) => `${n} ${k}`).join(", ") || "nothing";
const PLAN_STATE = { pending: "warning", approved: "success", used: "secondary", expired: "dark", blocked: "danger", nothing: "light" };
const JOB_STATE = { running: "primary", succeeded: "success", failed: "danger", interrupted: "warning" };
const stateBadge = (state, kinds) => badge(state, kinds[state] || "secondary");

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

// ---- plans and jobs ---------------------------------------------------------

function plansView(d) {
  const rows = (d.plans || []).map((p) => [
    when(p.last_seen),
    p.actor,
    p.sha ? p.sha.slice(0, 8) : "",
    summary(p.counts),
    stateBadge(p.state, PLAN_STATE),
    h("a", { href: "#/plans/" + p.hash }, short(p.hash)),
  ]);
  const ttl = Math.round((d.approval_ttl_seconds || 0) / 60);
  return card("clipboard-check", "Plans",
    rows.length
      ? table(["Made", "By", "Commit", "Would", "State", "Plan"], rows)
      : empty("No plan has been made yet. CI makes one on every pull request (POST /v1/plan)."),
    h("span", { class: "badge text-bg-secondary", title: "How long an approval lasts" }, `approval ${ttl} min`));
}

function infoTable(pairs) {
  // A badge is {text, cls} (what a table cell takes); here it has to become an element.
  const node = (v) => (v && typeof v === "object" && !(v instanceof Node) ? h("span", { class: v.cls }, v.text) : v);
  return h("div", { class: "table-responsive" }, h("table", { class: "table table-sm mb-0" }, h("tbody", {},
    pairs.filter(([, v]) => v !== "" && v !== null && v !== undefined).map(([k, v]) => h("tr", {}, h("th", { class: "w-25" }, k), h("td", {}, node(v)))))));
}

function planView(p) {
  const canApprove = role === "admin" && ["pending", "expired", "approved"].includes(p.state);
  const buttons = [];
  if (canApprove) {
    buttons.push(h("button", { type: "button", class: "btn btn-sm btn-success me-1", "data-act": "approve" }, icon("check", "me-1"), p.state === "approved" ? "Renew approval" : "Approve"));
  }
  if (role === "admin" && p.state === "approved") {
    buttons.push(h("button", { type: "button", class: "btn btn-sm btn-outline-danger", "data-act": "withdraw" }, icon("ban", "me-1"), "Withdraw"));
  }
  for (const b of buttons) {
    b.addEventListener("click", () => act(b.getAttribute("data-act") === "approve" ? "POST" : "DELETE",
      `/v1/plans/${p.hash}/${b.getAttribute("data-act") === "approve" ? "approve" : "approval"}`,
      b.getAttribute("data-act") === "approve" ? "Approved. One apply of exactly this plan may now run." : "Approval withdrawn."));
  }
  const approval = p.approval ? `${p.approval.by}, until ${when(p.approval.expires)}` : "";
  const used = p.used ? h("a", { href: "#/jobs/" + p.used.job }, p.used.job) : "";
  const hint = p.state === "pending" && role !== "admin" ? h("p", { class: "text-muted small mt-2 mb-0" }, "An admin has to approve this plan before an apply can run it.") : null;
  return h("div", {},
    card("clipboard-check", `Plan ${short(p.hash)}`,
      h("div", {}, infoTable([
        ["State", stateBadge(p.state, PLAN_STATE)],
        ["Would", summary(p.counts)],
        ["Requested by", p.actor],
        ["Commit", p.sha],
        ["Service", p.service],
        ["First made", when(p.created)],
        ["Last made", when(p.last_seen)],
        ["Approval", approval],
        ["Used by job", used],
      ]), hint ? h("div", { class: "card-body pt-0" }, hint) : null),
      h("span", {}, buttons)),
    card("file-lines", "What apply would do", h("div", { class: "card-body" }, h("pre", { class: "mb-0 small" }, p.text || "")),
      h("a", { href: "#/plans", class: "btn btn-sm btn-outline-secondary" }, icon("arrow-left", "me-1"), "All plans")));
}

function jobsView(d) {
  if (d.disabled) return card("list-check", "Jobs", empty("Apply is not enabled on this daemon (it was started without --enable-apply), so there are no jobs."));
  const rows = (d.jobs || []).map((j) => [
    when(j.created),
    stateBadge(j.status, JOB_STATE),
    j.actor,
    j.sha ? j.sha.slice(0, 8) : "",
    h("a", { href: "#/plans/" + j.plan_hash }, short(j.plan_hash)),
    h("a", { href: "#/jobs/" + j.id }, j.id),
  ]);
  return card("list-check", "Jobs",
    rows.length ? table(["Started", "Status", "By", "Commit", "Plan", "Job"], rows) : empty("No apply has run yet."),
    h("span", { class: "badge text-bg-secondary" }, rows.length));
}

function jobView(j) {
  return h("div", {},
    card("list-check", `Job ${j.id}`, infoTable([
      ["Status", stateBadge(j.status, JOB_STATE)],
      ["Started by", j.actor],
      ["Commit", j.sha],
      ["Service", j.service],
      ["Plan", h("a", { href: "#/plans/" + j.plan_hash }, short(j.plan_hash))],
      ["Started", when(j.created)],
      ["Finished", when(j.finished)],
      ["Error", j.error],
    ]), h("span", {})),
    card("terminal", "Log", h("div", { class: "card-body" },
      j.log ? h("pre", { class: "mb-0 small" }, j.log) : h("span", { class: "text-muted" }, "The log is only shown to deployers and admins.")),
      h("a", { href: "#/jobs", class: "btn btn-sm btn-outline-secondary" }, icon("arrow-left", "me-1"), "All jobs")));
}

// Pages that load their own data. `nav` is the top-nav item that is highlighted for it.
const pages = [
  { re: /^\/plans$/, nav: "/plans", load: () => api("/v1/plans"), render: plansView },
  { re: /^\/plans\/([0-9a-f]{64})$/, nav: "/plans", load: (m) => api("/v1/plans/" + m[1]), render: planView },
  { re: /^\/jobs$/, nav: "/jobs", load: () => api("/v1/jobs").catch((e) => { if (e.status === 404) return { disabled: true }; throw e; }), render: jobsView },
  { re: /^\/jobs\/(j-[0-9]+-[0-9a-f]{8})$/, nav: "/jobs", load: (m) => api("/v1/jobs/" + m[1]), render: jobView },
];

const routes = { "/": overviewView, "/instances": instancesView, "/volumes": volumesView };

// The current place: a status page (from the snapshot) or a page that loads its own data.
function currentRoute() {
  const path = location.hash.replace(/^#/, "") || "/";
  for (const page of pages) {
    const match = page.re.exec(path);
    if (match) return { path, nav: page.nav, page, match };
  }
  return routes[path] ? { path, nav: path } : { path: "/", nav: "/" };
}

// Do something as the signed-in user (approve, withdraw), say how it went, and show the result.
async function act(method, path, okText) {
  try {
    await api(path, method);
    notice = { kind: "success", text: okText };
  } catch (e) {
    if (e.auth) return signOut("That token was not accepted.");
    notice = { kind: "danger", text: e.message };
  }
  await refresh();
}

function render() {
  const signedIn = !!sessionStorage.getItem(KEY);
  document.getElementById("top-nav").hidden = !signedIn;
  document.getElementById("signout").hidden = !signedIn;
  const route = currentRoute();
  for (const a of document.querySelectorAll(".top-nav a")) {
    a.classList.toggle("active", a.getAttribute("href") === "#" + route.nav);
  }
  if (!signedIn) return view.replaceChildren(h("div", { class: "mt-4" }, loginView(loadError)));

  const parts = [];
  if (loadError) parts.push(h("div", { class: "alert alert-warning", role: "alert" }, loadError));
  if (notice) parts.push(h("div", { class: `alert alert-${notice.kind}`, role: "alert" }, notice.text));
  if (route.page) {
    if (extra && extra.path === route.path) parts.push(route.page.render(extra.data));
    else if (!loadError) parts.push(h("div", { class: "text-muted mt-4 text-center" }, "Loading…"));
  } else if (snapshot) {
    parts.push(routes[route.path](snapshot));
    parts.push(h("p", { class: "text-muted small text-end" }, `Updated ${new Date(snapshot.time).toLocaleTimeString()}`));
  } else if (!loadError) {
    parts.push(h("div", { class: "text-muted mt-4 text-center" }, "Loading…"));
  }
  view.replaceChildren(h("div", { class: "mt-4" }, parts));
}

// ---- session ---------------------------------------------------------------

async function refresh() {
  const route = currentRoute();
  try {
    if (route.page) extra = { path: route.path, data: await route.page.load(route.match) };
    else snapshot = await api("/v1/status");
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
  extra = null;
  notice = null;
  render();
  api("/v1/whoami").then((w) => {
    role = w.role;
    document.getElementById("who-text").textContent = `${w.name} (${w.role})`;
    document.getElementById("who").hidden = false;
    render();
  }).catch(() => {});
  // Plans and Jobs are only in the nav when this daemon serves them (an older one does not).
  api("/v1/plans").then(() => { document.getElementById("nav-plans").hidden = false; }).catch(() => {});
  api("/v1/jobs").then(() => { document.getElementById("nav-jobs").hidden = false; }).catch(() => {});
  refresh();
  clearInterval(timer);
  timer = setInterval(refresh, REFRESH_MS);
}

function signOut(message) {
  sessionStorage.removeItem(KEY);
  clearInterval(timer);
  snapshot = null;
  extra = null;
  notice = null;
  role = "";
  loadError = message || "";
  document.getElementById("nav-plans").hidden = true;
  document.getElementById("nav-jobs").hidden = true;
  document.getElementById("who").hidden = true;
  document.getElementById("who-text").textContent = "";
  render();
}

document.getElementById("signout").addEventListener("click", () => signOut());
window.addEventListener("hashchange", () => {
  notice = null;
  render();
  if (sessionStorage.getItem(KEY) && currentRoute().page) refresh();
});

// The footer shows the daemon's version; /healthz is open and carries nothing else.
fetch("/healthz", { cache: "no-store" }).then((r) => r.json()).then((j) => {
  if (j.version) document.getElementById("version").textContent = `native-ops ${j.version}`;
}).catch(() => {});

if (sessionStorage.getItem(KEY)) start(); else render();
