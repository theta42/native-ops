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
let methods = { local_signin: false, oidc: false, oidc_label: "" }; // how this daemon lets people sign in
let authUser = null; // {username, role} when signed in with a session cookie

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

async function api(path, method, body) {
  // A signed-in session cookie rides along automatically; a pasted API token is sent as a bearer.
  const token = sessionStorage.getItem(KEY);
  const headers = token ? { Authorization: `Bearer ${token}` } : {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const res = await fetch(path, {
    method: method || "GET",
    headers,
    credentials: "same-origin",
    cache: "no-store",
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401) throw Object.assign(new Error("unauthorized"), { auth: true });
  if (!res.ok) throw Object.assign(new Error(`${res.status} ${(await res.json().catch(() => ({}))).error || res.statusText}`), { status: res.status });
  return res.json();
}

const signedIn = () => !!(authUser || sessionStorage.getItem(KEY));

const when = (t) => (t ? new Date(t).toLocaleString() : "");
const short = (hash) => (hash || "").slice(0, 12);
const summary = (c) => Object.entries(c || {}).filter(([, n]) => n).map(([k, n]) => `${n} ${k}`).join(", ") || "nothing";
const PLAN_STATE = { pending: "warning", approved: "success", used: "secondary", expired: "dark", blocked: "danger", nothing: "light" };
const JOB_STATE = { running: "primary", succeeded: "success", failed: "danger", interrupted: "warning" };
const stateBadge = (state, kinds) => badge(state, kinds[state] || "secondary");
const RECIPE_STATE = { approved: "success", waiting: "warning" };

// ---- views -----------------------------------------------------------------

function loginView(message) {
  const error = h("div", { class: "alert alert-danger mt-3 mb-0", role: "alert", hidden: !message }, message || "");
  const blocks = [];
  const staff = methods.local_signin || methods.oidc;

  if (methods.local_signin) {
    const username = h("input", { type: "text", class: "form-control", placeholder: "username", autocomplete: "username", "aria-label": "Username", required: true });
    const password = h("input", { type: "password", class: "form-control", placeholder: "password", autocomplete: "current-password", "aria-label": "Password", required: true });
    const form = h("form", { autocomplete: "on" },
      h("div", { class: "mb-2" }, h("div", { class: "input-group" }, h("span", { class: "input-group-text" }, icon("user")), username)),
      h("div", { class: "mb-2" }, h("div", { class: "input-group" }, h("span", { class: "input-group-text" }, icon("key")), password)),
      h("button", { type: "submit", class: "btn btn-info w-100" }, "Sign in"));
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      try {
        const out = await api("/api/login", "POST", { username: username.value.trim(), password: password.value });
        authUser = out.user || { username: username.value.trim() };
        password.value = "";
        start();
      } catch (err) {
        loadError = err.message;
        render();
      }
    });
    blocks.push(form);
  }

  if (methods.oidc) {
    blocks.push(h("a", { href: "/auth/oidc", class: "btn btn-outline-secondary w-100" + (methods.local_signin ? " mt-2" : "") },
      icon("right-to-bracket", "me-1"), "Sign in with " + (methods.oidc_label || "single sign-on")));
  }

  // A pasted API token still works: a machine-style credential, or an older daemon with no sign-in.
  const token = h("input", { type: "password", class: "form-control form-control-sm", placeholder: "nops_…", autocomplete: "off", spellcheck: "false", "aria-label": "API token" });
  const tokenForm = h("form", { autocomplete: "off" },
    h("div", { class: "input-group input-group-sm" }, h("span", { class: "input-group-text" }, icon("key")), token,
      h("button", { type: "submit", class: "btn btn-outline-secondary" }, "Use token")));
  tokenForm.addEventListener("submit", (e) => {
    e.preventDefault();
    if (!token.value.trim()) return;
    sessionStorage.setItem(KEY, token.value.trim());
    token.value = "";
    start();
  });
  blocks.push(h("div", { class: "mt-3 pt-3 border-top" },
    h("p", { class: "text-muted small mb-2" }, "Or paste an API token (kept for this browser tab only)."),
    tokenForm));

  return h("div", { class: "row justify-content-center" },
    h("div", { class: "col-md-4" }, card("lock", staff ? "Sign in" : "API Token Login", h("div", { class: "card-body" }, blocks, error))));
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
  return h("div", {},
    h("div", { class: "row" },
      h("div", { class: "col-lg-6" }, hostCard(s)),
      h("div", { class: "col-lg-6" }, metricsCard(s))),
    h("div", { class: "row" }, h("div", { class: "col-12" }, warningsCard(s))));
}

const gb = (kb) => (kb ? `${(kb / 1048576).toFixed(1)} GB` : "");

function metricsCard(s) {
  const m = s.metrics;
  const line = (label, value) => h("tr", {}, h("th", { class: "w-25" }, label), h("td", {}, value));
  if (!m) return card("gauge-high", "Metrics", empty("This daemon does not report host metrics."));
  const mem = m.mem_total_kb ? `${gb(m.mem_total_kb - m.mem_avail_kb)} used of ${gb(m.mem_total_kb)} (${gb(m.mem_avail_kb)} free)` : "";
  const swap = m.swap_total_kb ? `${gb(m.swap_used_kb)} of ${gb(m.swap_total_kb)}` : "";
  const uptime = m.uptime_sec ? `${Math.floor(m.uptime_sec / 86400)}d ${Math.floor((m.uptime_sec % 86400) / 3600)}h` : "";
  return card("gauge-high", "Metrics", h("div", { class: "table-responsive" }, h("table", { class: "table table-sm mb-0" }, h("tbody", {},
    line("Load", `${m.load1} / ${m.load5} / ${m.load15}${m.cores ? ` (${m.cores} cores)` : ""}`),
    line("Memory", mem),
    swap ? line("Swap", swap) : null,
    m.disk ? line("Pool disk", `${gb(m.disk.used_kb)} of ${gb(m.disk.total_kb)} used (${m.disk.use_percent}%), ${gb(m.disk.available_kb)} free at ${m.disk.mount}`) : null,
    uptime ? line("Uptime", uptime) : null))));
}

function networkView(s) {
  const e = s.edge || {};
  const routes = (e.routes || []).map((r) => [r.host, (r.upstreams || []).join(", ") || badge("no upstream", "secondary")]);
  const certs = (e.certs || []).map((c) => {
    const days = Math.floor((new Date(c.not_after) - Date.now()) / 86400000);
    return [(c.names || []).join(", "), c.issuer || "", when(c.not_before), when(c.not_after), badge(`${days}d`, days < 14 ? "danger" : days < 30 ? "warning" : "success")];
  });
  const dns = (s.dns || []).map((r) => [r.domain, r.type, r.name, r.value, r.ttl || ""]);
  return h("div", {},
    card("network-wired", "Routes (where they point)",
      routes.length ? table(["Host", "Upstream"], routes) : empty(e.container ? `Nothing is published on the edge (${e.container}).` : "This daemon does not read the edge."),
      h("span", { class: "badge text-bg-secondary" }, routes.length)),
    card("certificate", "Certificates",
      certs.length ? table(["Names", "Issuer", "From", "Expires", "Left"], certs) : empty("No certificates found in the edge's store."),
      h("span", { class: "badge text-bg-secondary" }, certs.length)),
    card("globe", "DNS records",
      dns.length ? table(["Zone", "Type", "Name", "Value", "TTL"], dns) : empty("No DNS records (the daemon needs --dns-provider and --dns-domains)."),
      h("span", { class: "badge text-bg-secondary" }, dns.length)));
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

// ---- API tokens -----------------------------------------------------------------

// CI's tokens are made here, by an admin, so nobody needs a shell on the host. A new secret is shown
// once, in `created`, until the admin leaves the page.
let created = null; // {name, secret}

function tokensView(d) {
  const rows = (d.tokens || []).map((t) => {
    const revoke = h("button", { type: "button", class: "btn btn-sm btn-outline-danger" }, icon("trash", "me-1"), "Revoke");
    revoke.addEventListener("click", () => {
      if (confirm(`Revoke the token "${t.name}"? Anything using it stops working at once.`)) {
        act("DELETE", `/v1/tokens/${encodeURIComponent(t.id)}`, `Token "${t.name}" revoked.`);
      }
    });
    const scope = t.scope ? [
      (t.scope.names || []).length ? `names ${t.scope.names.join(",")}` : "",
      (t.scope.images || []).length ? `images ${t.scope.images.join(",")}` : "",
      (t.scope.domains || []).length ? `domains ${t.scope.domains.join(",")}` : "",
    ].filter(Boolean).join("; ") : "";
    return [t.name, badge(t.role, t.role === "admin" ? "danger" : t.role === "deployer" ? "warning" : "secondary"), scope, when(t.created), h("code", {}, t.id), revoke];
  });

  const name = h("input", { type: "text", class: "form-control form-control-sm", placeholder: "name, e.g. ci-plan", required: true, maxlength: "60", "aria-label": "Token name" });
  const roleSel = h("select", { class: "form-select form-select-sm", "aria-label": "Role" },
    ["planner", "deployer", "viewer", "admin"].map((r) => h("option", { value: r }, r)));
  const names = h("input", { type: "text", class: "form-control form-control-sm", placeholder: "instance globs, e.g. demo-*", "aria-label": "Instance name globs" });
  const images = h("input", { type: "text", class: "form-control form-control-sm", placeholder: "image globs, e.g. app-web:*", "aria-label": "Image globs" });
  const domains = h("input", { type: "text", class: "form-control form-control-sm", placeholder: "domain globs, e.g. *.example.com", "aria-label": "Domain globs" });
  const list = (el) => el.value.split(",").map((x) => x.trim()).filter(Boolean);
  const form = h("form", { class: "card-body border-top" },
    h("div", { class: "row g-2 align-items-end" },
      h("div", { class: "col-md-3" }, name),
      h("div", { class: "col-md-2" }, roleSel),
      h("div", { class: "col-md-2" }, names),
      h("div", { class: "col-md-2" }, images),
      h("div", { class: "col-md-2" }, domains),
      h("div", { class: "col-md-1" }, h("button", { type: "submit", class: "btn btn-sm btn-success w-100" }, icon("plus"), " Create"))),
    h("p", { class: "text-muted small mt-2 mb-0" },
      "A planner token can plan and nothing else: give it to pull-request pipelines. A deployer token applies only plans an admin approved. " +
      "Fill in instance and image globs to make a scoped deployer token that can manage only those tenant instances."));
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const body = { name: name.value.trim(), role: roleSel.value };
    if (names.value || images.value || domains.value) body.scope = { names: list(names), images: list(images), domains: list(domains) };
    try {
      const out = await api("/v1/tokens", "POST", body);
      created = { name: out.token.name, secret: out.secret };
      notice = null;
    } catch (err) {
      if (err.auth) return signOut("Your sign-in is no longer valid.");
      notice = { kind: "danger", text: err.message };
    }
    await refresh();
  });

  const shown = created ? h("div", { class: "alert alert-success" },
    h("div", { class: "mb-2" }, icon("key", "me-1"), `Token "${created.name}" created. Copy the secret now: it is not shown again.`),
    h("input", { type: "text", class: "form-control font-monospace", readonly: true, value: created.secret, "aria-label": "New token secret" })) : null;

  return h("div", {}, shown,
    card("key", "API tokens",
      h("div", {}, rows.length ? table(["Name", "Role", "Scope", "Created", "Id", ""], rows) : empty("No tokens yet (the bootstrap token is not listed)."), form),
      h("span", { class: "badge text-bg-secondary" }, rows.length)),
    d.secrets ? secretsCard(d.secrets) : null);
}

// The daemon's own credentials, pushed from the git server's secret store by CI. Names only: a value
// is never sent back.
function secretsCard(list) {
  const rows = list.map((s) => {
    const del = h("button", { type: "button", class: "btn btn-sm btn-outline-danger" }, icon("trash", "me-1"), "Delete");
    del.addEventListener("click", () => {
      if (confirm(`Delete the secret ${s.name} from this daemon? Whatever needs it stops working until it is synced again.`)) {
        act("DELETE", `/v1/secrets/${encodeURIComponent(s.name)}`, `Secret ${s.name} deleted.`);
      }
    });
    return [h("code", {}, s.name), s.set_by, when(s.set_at), del];
  });
  return card("lock", "Daemon secrets",
    h("div", {},
      h("div", { class: "card-body pb-0" }, h("p", { class: "text-muted small mb-2" },
        "Set in the git server's secret store and pushed here by CI (native-ops remote secret-sync). Values are never shown.")),
      rows.length ? table(["Name", "Set by", "Set at", ""], rows) : empty("No secrets synced yet.")),
    h("span", { class: "badge text-bg-secondary" }, rows.length));
}

// ---- image recipes ------------------------------------------------------------

// An image build runs the uploaded recipe (scripts/ and images/) on the host, so the daemon builds only
// from a recipe an admin approved. This page lists the recipes builds were asked for.
function recipesView(d) {
  const rows = (d.recipes || []).map((r) => {
    const approved = !!r.approved_at;
    const buttons = [];
    if (role === "admin") {
      const b = approved
        ? h("button", { type: "button", class: "btn btn-sm btn-outline-danger" }, icon("ban", "me-1"), "Withdraw")
        : h("button", { type: "button", class: "btn btn-sm btn-success" }, icon("check", "me-1"), "Approve");
      b.addEventListener("click", () => approved
        ? act("DELETE", `/v1/images/recipes/${r.digest}/approval`, "Approval withdrawn: builds from this recipe are refused again.")
        : act("POST", `/v1/images/recipes/${r.digest}/approve`, "Approved: builds from this recipe may run, for any ref."));
      buttons.push(b);
    }
    return [
      when(r.last_seen),
      r.last_actor || "",
      r.last_app || "",
      r.last_sha ? r.last_sha.slice(0, 8) : "",
      stateBadge(approved ? "approved" : "waiting", RECIPE_STATE),
      approved ? `${r.approved_by}, ${when(r.approved_at)}` : "",
      h("code", { title: r.digest }, short(r.digest)),
      h("span", {}, buttons),
    ];
  });
  return card("hammer", "Image recipes",
    h("div", {},
      h("div", { class: "card-body pb-0" }, h("p", { class: "text-muted small mb-2" },
        "A build runs the uploaded scripts/ and images/ on this host, so it runs only from a recipe an admin approved. " +
        "One approval covers every ref built from the same recipe; any change to a build script is a new recipe.")),
      rows.length
        ? table(["Last asked", "By", "App", "Commit", "State", "Approved", "Recipe", ""], rows)
        : empty("No build has been asked for yet.")),
    h("span", { class: "badge text-bg-secondary" }, rows.length));
}

function jobsView(d) {
  if (d.disabled) return card("list-check", "Jobs", empty("Nothing that changes the host is enabled on this daemon, so there are no jobs."));
  const rows = (d.jobs || []).map((j) => [
    when(j.created),
    stateBadge(j.status, JOB_STATE),
    j.actor,
    j.sha ? j.sha.slice(0, 8) : "",
    [j.kind || "apply", j.service].filter(Boolean).join(" "),
    j.plan_hash ? h("a", { href: "#/plans/" + j.plan_hash }, short(j.plan_hash)) : "",
    h("a", { href: "#/jobs/" + j.id }, j.id),
  ]);
  return card("list-check", "Jobs",
    rows.length ? table(["Started", "Status", "By", "Commit", "What", "Plan", "Job"], rows) : empty("No job has run yet."),
    h("span", { class: "badge text-bg-secondary" }, rows.length));
}

function jobView(j) {
  return h("div", {},
    card("list-check", `Job ${j.id}`, infoTable([
      ["Status", stateBadge(j.status, JOB_STATE)],
      ["Started by", j.actor],
      ["Commit", j.sha],
      ["Kind", j.kind],
      ["Subject", j.service],
      ["Plan", j.plan_hash ? h("a", { href: "#/plans/" + j.plan_hash }, short(j.plan_hash)) : ""],
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
  { re: /^\/recipes$/, nav: "/recipes", load: () => api("/v1/images/recipes"), render: recipesView },
  { re: /^\/tokens$/, nav: "/tokens", load: () => Promise.all([api("/v1/tokens"), api("/v1/secrets").catch(() => null)])
      .then(([t, s]) => ({ ...t, secrets: s ? s.secrets : null })), render: tokensView },
];

const routes = { "/": overviewView, "/instances": instancesView, "/volumes": volumesView, "/network": networkView };

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
    if (e.auth) return signOut("Your sign-in is no longer valid.");
    notice = { kind: "danger", text: e.message };
  }
  await refresh();
}

function render() {
  const signedInNow = signedIn();
  document.getElementById("top-nav").hidden = !signedInNow;
  document.getElementById("signout").hidden = !signedInNow;
  const route = currentRoute();
  for (const a of document.querySelectorAll(".top-nav a")) {
    a.classList.toggle("active", a.getAttribute("href") === "#" + route.nav);
  }
  if (!signedInNow) return view.replaceChildren(h("div", { class: "mt-4" }, loginView(loadError)));

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
    if (e.auth) return signOut("Your sign-in is no longer valid.");
    loadError = `Could not refresh: ${e.message}`;
  }
  render();
}

async function start() {
  loadError = "";
  snapshot = null;
  extra = null;
  notice = null;
  // A failed OIDC sign-in comes back as ?error=...; show it once and drop it from the URL.
  const authError = new URLSearchParams(location.search).get("error");
  if (authError) {
    loadError = authError;
    history.replaceState(null, "", location.hash || "#/");
  }
  // How this daemon lets people sign in, and who we already are (a session cookie rides along).
  try {
    const s = await fetch("/api/session", { cache: "no-store", credentials: "same-origin" }).then((r) => r.json());
    methods = s;
    authUser = s.user || null;
  } catch { /* an older daemon has no /api/session: token sign-in only */ }
  render();
  if (!signedIn()) return;
  api("/v1/whoami").then((w) => {
    role = w.role;
    // Recipes are an admin's page, and only on a daemon that builds images.
    if (role === "admin") api("/v1/images/recipes").then(() => { document.getElementById("nav-recipes").hidden = false; }).catch(() => {});
    if (role === "admin") api("/v1/tokens").then(() => { document.getElementById("nav-tokens").hidden = false; }).catch(() => {});
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
  authUser = null;
  if (methods.local_signin || methods.oidc) api("/api/logout", "POST").catch(() => {});
  clearInterval(timer);
  snapshot = null;
  extra = null;
  notice = null;
  role = "";
  loadError = message || "";
  document.getElementById("nav-plans").hidden = true;
  document.getElementById("nav-jobs").hidden = true;
  document.getElementById("nav-recipes").hidden = true;
  document.getElementById("nav-tokens").hidden = true;
  created = null;
  document.getElementById("who").hidden = true;
  document.getElementById("who-text").textContent = "";
  render();
}

document.getElementById("signout").addEventListener("click", () => signOut());
window.addEventListener("hashchange", () => {
  notice = null;
  created = null; // a new secret is shown only until the admin moves on
  render();
  if (signedIn() && currentRoute().page) refresh();
});

// The footer shows the daemon's version; /healthz is open and carries nothing else.
fetch("/healthz", { cache: "no-store" }).then((r) => r.json()).then((j) => {
  if (j.version) document.getElementById("version").textContent = `native-ops ${j.version}`;
}).catch(() => {});

// start() reads /api/session and shows the sign-in view when there is no session or token.
start();
