// The API reference: draws /openapi.json with DOM nodes (no innerHTML, no inline styles, nothing remote), so it
// obeys the daemon UI's CSP and can never show markup the document did not mean as text.
const h = (tag, attrs, ...children) => {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) { if (v !== false && v != null) e.setAttribute(k, v === true ? "" : String(v)); }
  for (const c of children.flat()) { if (c !== null && c !== undefined && c !== false) e.append(c instanceof Node ? c : document.createTextNode(String(c))); }
  return e;
};
const METHOD_KIND = { get: "success", post: "primary", put: "warning", patch: "info", delete: "danger" };

// Inline `code` and **bold**.
function inline(text) {
  const out = [];
  for (const part of String(text).split(/(`[^`]+`|\*\*[^*]+\*\*)/)) {
    if (part.startsWith("`") && part.endsWith("`") && part.length > 1) out.push(h("code", {}, part.slice(1, -1)));
    else if (part.startsWith("**") && part.endsWith("**") && part.length > 4) out.push(h("strong", {}, part.slice(2, -2)));
    else if (part) out.push(part);
  }
  return out;
}
// Paragraphs, "- " and "1." lists and ``` blocks, which is all the descriptions use.
function markdown(text) {
  const out = [];
  let para = []; let list = null; let ordered = false; let code = null;
  const flush = () => { if (para.length) out.push(h("p", {}, inline(para.join(" ")))); para = []; if (list) { out.push(h(ordered ? "ol" : "ul", {}, list.map((li) => h("li", {}, inline(li))))); list = null; } };
  for (const line of String(text || "").split("\n")) {
    if (line.trim().startsWith("```")) { if (code) { out.push(h("pre", { class: "bg-body-secondary p-2 rounded small" }, code.join("\n"))); code = null; } else { flush(); code = []; } continue; }
    if (code) { code.push(line); continue; }
    if (!line.trim()) { flush(); continue; }
    const m = /^\s*(?:[-*]|\d+\.) (.*)$/.exec(line);
    if (m) { if (para.length) { out.push(h("p", {}, inline(para.join(" ")))); para = []; } if (!list) ordered = /^\s*\d+\./.test(line); (list = list || []).push(m[1]); continue; }
    if (list && /^\s+\S/.test(line)) { list[list.length - 1] += " " + line.trim(); continue; }
    if (list) flush();
    para.push(line.trim());
  }
  flush();
  return h("div", { class: "desc" }, out);
}

let doc;
// Follow a "#/components/..." reference to what it names (schemas, parameters, responses, request bodies).
const deref = (x) => {
  let cur = x;
  for (let i = 0; cur && cur.$ref && i < 8; i++) cur = cur.$ref.replace(/^#\//, "").split("/").reduce((o, k) => (o ? o[k] : undefined), doc) ?? {};
  return cur || {};
};
const resolve = deref;
const typeOf = (s) => {
  const r = resolve(s);
  if (r.oneOf) return r.oneOf.map(typeOf).join(" | ");
  if (r.enum) return r.enum.map((v) => JSON.stringify(v)).join(" | ");
  if (r.type === "array") return `${typeOf(r.items)}[]`;
  if (r.type === "object" || r.properties) return s && s.$ref ? s.$ref.split("/").pop() : "object";
  return `${r.type || "any"}${r.format ? ` (${r.format})` : ""}${r.nullable ? ", or null" : ""}`;
};

// The name of the component schema a type points at (directly or as an array of it), if any.
const schemaRef = (r) => (r && typeof r.$ref === "string" && r.$ref.startsWith("#/components/schemas/") ? r.$ref.split("/").pop() : null);
const refName = (s) => schemaRef(s) || (resolve(s).type === "array" ? schemaRef(s && s.items) : null);
const typeCell = (s) => {
  const name = refName(s);
  if (!name) return typeOf(s);
  return [h("a", { href: `#schema-${name}` }, name), resolve(s).type === "array" ? "[]" : ""];
};

// A schema as rows of a table: name, type, what it means; objects open into their fields.
function schemaRows(s, required = [], depth = 0) {
  const r = resolve(s);
  const rows = [];
  const props = r.properties || (r.type === "array" ? resolve(r.items).properties : null);
  if (!props) return rows;
  const req = (r.type === "array" ? resolve(r.items).required : r.required) || required;
  for (const [name, prop] of Object.entries(props)) {
    const p = resolve(prop);
    rows.push(h("tr", {},
      h("td", { class: "path" }, h("span", { class: depth ? "ps-3" : "" }, name), req && req.includes(name) ? h("span", { class: "text-danger", title: "required" }, " *") : ""),
      h("td", { class: "type" }, typeCell(prop)),
      h("td", {}, inline(p.description || ""))));
  }
  return rows;
}
const schemaTable = (s) => {
  const rows = schemaRows(s);
  if (!rows.length) return h("p", { class: "text-muted small mb-2" }, typeOf(s), resolve(s).description ? " — " : "", inline(resolve(s).description || ""));
  return h("div", { class: "table-responsive" }, h("table", { class: "table table-sm schema mb-2" }, h("thead", {}, h("tr", {}, h("th", {}, "Field"), h("th", {}, "Type"), h("th", {}, "Meaning"))), h("tbody", {}, rows)));
};

function roleBadge(o) {
  const role = o["x-native-ops-role"];
  const badges = [];
  if (role && role !== "none") badges.push(h("span", { class: "badge text-bg-secondary", title: "The least role that may call it" }, `${role}${role === "admin" ? "" : "+"}`));
  else badges.push(h("span", { class: "badge text-bg-light border", title: "No token needed" }, "public"));
  if (o["x-native-ops-scoped"]) badges.push(h("span", { class: "badge text-bg-info", title: "A token with a scope may call it, for what its scope allows" }, "scoped ok"));
  return badges;
}

function curl(method, path, o) {
  const base = location.origin;
  const url = base + path.replace(/\{([^}]+)\}/g, (m, n) => `<${n}>`);
  const parts = [`curl -X ${method.toUpperCase()} '${url}'`];
  if (!o.security || o.security.length) parts.push("-H \"Authorization: Bearer $NATIVE_OPS_TOKEN\"");
  const body = o.requestBody?.content?.["application/json"]?.schema;
  if (body) {
    const sample = {};
    const r = resolve(body);
    for (const [k, v] of Object.entries(r.properties || {})) { if ((r.required || []).includes(k)) sample[k] = resolve(v).enum ? resolve(v).enum[0] : (resolve(v).type === "integer" ? 0 : resolve(v).type === "boolean" ? true : `<${k}>`); }
    parts.push("-H 'Content-Type: application/json'", `-d '${JSON.stringify(sample)}'`);
  }
  return parts.join(" \\\n  ");
}

function operation(path, method, o) {
  const id = `${method}-${path}`.replace(/[^a-z0-9]+/gi, "-").replace(/^-|-$/g, "");
  const params = (o.parameters || []).map(deref).map((p) => h("tr", {}, h("td", { class: "path" }, p.name, p.required ? h("span", { class: "text-danger" }, " *") : ""), h("td", {}, p.in), h("td", { class: "type" }, typeOf(p.schema)), h("td", {}, inline(p.description || ""))));
  const reqBody = deref(o.requestBody);
  const body = reqBody.content && Object.entries(reqBody.content)[0];
  const responses = Object.entries(o.responses || {}).map(([code, rr]) => {
    const r = deref(rr);
    // The error shape is described once, at the top; only a success answer spells out its fields.
    const schema = /^2/.test(code) && r.content && Object.values(r.content)[0]?.schema;
    return h("div", { class: "mb-2" }, h("span", { class: `badge text-bg-${code.startsWith("2") || code.startsWith("3") ? "success" : "secondary"} me-2` }, code), inline(r.description || ""), schema ? schemaTable(schema) : null);
  });
  return h("div", { class: "card mb-3 op", id, "data-search": `${method} ${path} ${o.summary} ${(o.tags || []).join(" ")}`.toLowerCase() },
    h("div", { class: "card-header d-flex flex-wrap gap-2 align-items-center" },
      h("span", { class: `badge method text-bg-${METHOD_KIND[method] || "secondary"}` }, method),
      h("a", { class: "path text-reset text-decoration-none", href: `#${id}` }, path), h("span", { class: "ms-auto d-flex gap-2 align-items-center" }, h("span", { class: "text-muted" }, o.summary), ...roleBadge(o))),
    h("div", { class: "card-body" },
      o["x-native-ops-enabled-by"] ? h("p", { class: "small text-muted" }, "Only when the daemon is started with ", h("code", {}, o["x-native-ops-enabled-by"]), "; otherwise it answers 404.") : null,
      o.description ? markdown(o.description) : null,
      params.length ? [h("h6", {}, "Parameters"), h("div", { class: "table-responsive" }, h("table", { class: "table table-sm mb-2" }, h("tbody", {}, params)))] : null,
      body ? [h("h6", {}, `Request body (${body[0]})${reqBody.required === false ? ", optional" : ""}`), schemaTable(body[1].schema)] : null,
      h("h6", {}, "Responses"), responses,
      h("details", {}, h("summary", { class: "small text-muted" }, "Example"), h("pre", { class: "bg-body-secondary p-2 rounded small mb-0" }, curl(method, path, o)))));
}

function render() {
  const root = document.getElementById("docs");
  document.title = doc.info.title;
  const byTag = new Map(doc.tags.map((t) => [t.name, []]));
  for (const [path, item] of Object.entries(doc.paths)) {
    for (const [method, o] of Object.entries(item)) { const tag = o.tags?.[0] || "Other"; if (!byTag.has(tag)) byTag.set(tag, []); byTag.get(tag).push([path, method, o]); }
  }
  const search = h("input", { type: "search", class: "form-control form-control-sm mb-2", placeholder: "Filter endpoints", "aria-label": "Filter endpoints" });
  const nav = h("nav", { class: "nav-docs", "aria-label": "Sections" }, search, h("ul", { class: "list-unstyled small" }, [...byTag].filter(([, ops]) => ops.length).map(([tag, ops]) => h("li", { class: "mb-1" }, h("a", { href: `#tag-${tag.replace(/\W+/g, "-")}` }, tag), h("span", { class: "text-muted" }, ` (${ops.length})`)))));
  const sections = [...byTag].filter(([, ops]) => ops.length).map(([tag, ops]) => {
    const desc = doc.tags.find((t) => t.name === tag)?.description;
    return h("section", { id: `tag-${tag.replace(/\W+/g, "-")}`, class: "mb-4" }, h("h3", { class: "h4" }, tag), desc ? h("p", { class: "text-muted" }, desc) : null, ops.map(([p, m, o]) => operation(p, m, o)));
  });
  const named = Object.entries((doc.components && doc.components.schemas) || {}).filter(([, sc]) => sc.properties);
  if (named.length) {
    sections.push(h("section", { id: "tag-Schemas", class: "mb-4" }, h("h3", { class: "h4" }, "Schemas"), h("p", { class: "text-muted" }, "The shapes the operations above refer to."),
      named.map(([name, sc]) => h("div", { class: "card mb-3", id: `schema-${name}` }, h("div", { class: "card-header path" }, name), h("div", { class: "card-body" }, sc.description ? h("p", {}, inline(sc.description)) : null, schemaTable({ $ref: `#/components/schemas/${name}` }))))));
    nav.querySelector("ul").append(h("li", { class: "mb-1" }, h("a", { href: "#tag-Schemas" }, "Schemas"), h("span", { class: "text-muted" }, ` (${named.length})`)));
  }
  search.addEventListener("input", () => {
    const q = search.value.trim().toLowerCase();
    for (const card of root.querySelectorAll(".op")) card.hidden = !!q && !card.dataset.search.includes(q);
    for (const sec of root.querySelectorAll("section:not(#tag-Schemas)")) sec.hidden = !!q && ![...sec.querySelectorAll(".op")].some((c) => !c.hidden);
  });
  root.replaceChildren(
    h("header", { class: "mb-3" }, h("h1", { class: "h3 mb-1" }, doc.info.title, " ", h("small", { class: "text-muted" }, doc.info.version)), h("p", { class: "lead mb-2" }, doc.info.summary || ""), markdown(doc.info.description),
      h("p", { class: "small" }, h("a", { href: "/openapi.json" }, "openapi.json"), " is this document, for a client generator or a viewer you prefer.")),
    h("div", { class: "row" }, h("div", { class: "col-md-3 mb-3" }, nav), h("div", { class: "col-md-9" }, sections)));
  if (location.hash) document.getElementById(location.hash.slice(1))?.scrollIntoView();
}

fetch("/openapi.json", { cache: "no-store" }).then((r) => r.json()).then((d) => { doc = d; render(); }).catch(() => {
  document.getElementById("docs").replaceChildren(h("p", {}, "The API description could not be loaded. It is plain JSON at ", h("a", { href: "/openapi.json" }, "/openapi.json"), "."));
});
