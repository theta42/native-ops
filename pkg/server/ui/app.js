// Vanilla, dependency-free. Every value from the API is inserted with
// textContent, never as HTML, so a container named "<script>" is just text.
const $ = (id) => document.getElementById(id);
const KEY = "native-ops-token";
let timer = null;

function el(tag, text, cls) {
  const e = document.createElement(tag);
  if (text !== undefined && text !== null) e.textContent = String(text);
  if (cls) e.className = cls;
  return e;
}

function row(cells, header) {
  const tr = document.createElement("tr");
  for (const c of cells) {
    const td = el(header ? "th" : "td");
    if (c && typeof c === "object" && c.text !== undefined) { td.textContent = c.text; if (c.cls) td.className = c.cls; }
    else td.textContent = c ?? "";
    tr.appendChild(td);
  }
  return tr;
}

function fill(table, head, rows) {
  table.replaceChildren(row(head, true), ...rows.map((r) => row(r)));
}

async function api(path) {
  const token = sessionStorage.getItem(KEY);
  const res = await fetch(path, { headers: { Authorization: `Bearer ${token}` }, cache: "no-store" });
  if (res.status === 401) throw Object.assign(new Error("unauthorized"), { auth: true });
  if (!res.ok) throw new Error(`${res.status} ${(await res.json().catch(() => ({}))).error || res.statusText}`);
  return res.json();
}

function render(s) {
  $("host").textContent = [s.host, s.incus_version && `incus ${s.incus_version}`].filter(Boolean).join(" · ");
  const w = $("warnings");
  w.replaceChildren();
  if (s.warnings && s.warnings.length) {
    const box = el("div", null, "warnings");
    box.appendChild(el("strong", `${s.warnings.length} thing${s.warnings.length === 1 ? "" : "s"} to look at`));
    const ul = el("ul");
    for (const m of s.warnings) ul.appendChild(el("li", m));
    box.appendChild(ul);
    w.appendChild(box);
  }
  $("instCount").textContent = `(${s.instances.length})`;
  fill($("instances"), ["Name", "Status", "Image", "IPv4", "Limits", "Volumes", "Snapshots", "Env keys"],
    s.instances.map((i) => [
      i.name,
      { text: i.status, cls: i.status === "Running" ? "ok" : "bad" },
      (i.recorded && i.recorded.image) || i.image || i.base_image || "",
      (i.ipv4 || []).join(", "),
      Object.entries(i.limits || {}).map(([k, v]) => `${k.replace("limits.", "")}=${v}`).join(" "),
      (i.devices || []).filter((d) => d.type === "disk" && d.source).map((d) => `${d.source}${d.host_path ? " (host path)" : ""}`).join(", "),
      i.snapshots,
      (i.env_keys || []).length || "",
    ]));
  $("volCount").textContent = `(${s.volumes.length})`;
  fill($("volumes"), ["Name", "Used by", "Shifted", "Snapshots", "Latest daily"],
    s.volumes.map((v) => [v.name, (v.used_by || []).join(", ") || { text: "unattached", cls: "muted" }, v.security_shifted || "", v.snapshots, v.latest_daily || ""]));
  const im = s.images;
  $("images").textContent = `${im.count} images (${Math.round(im.total_bytes / 1e6)} MB), ${im.unaliased} without an alias`;
  $("updated").textContent = `Updated ${new Date(s.time).toLocaleTimeString()}`;
}

async function refresh() {
  try {
    render(await api("/v1/status"));
  } catch (e) {
    if (e.auth) return signOut("That token was not accepted.");
    $("updated").textContent = `Could not refresh: ${e.message}`;
  }
}

function showApp() {
  $("login").hidden = true; $("app").hidden = false; $("signout").hidden = false;
  api("/v1/whoami").then((w) => { $("who").textContent = `${w.name} (${w.role})`; }).catch(() => {});
  refresh();
  clearInterval(timer);
  timer = setInterval(refresh, 10000);
}

function signOut(message) {
  sessionStorage.removeItem(KEY);
  clearInterval(timer);
  $("app").hidden = true; $("signout").hidden = true; $("login").hidden = false; $("who").textContent = "";
  $("loginError").textContent = message || "";
}

$("loginForm").addEventListener("submit", (e) => {
  e.preventDefault();
  sessionStorage.setItem(KEY, $("token").value.trim());
  $("token").value = "";
  $("loginError").textContent = "";
  showApp();
});
$("signout").addEventListener("click", () => signOut());

if (sessionStorage.getItem(KEY)) showApp(); else signOut();
