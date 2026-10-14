// Recap developer console. Plain JavaScript, no build step, no dependencies.
// Everything is rendered with DOM nodes (never innerHTML): news text is crawled content and is
// treated as untrusted. Tokens are kept in localStorage — acceptable for a local test console only;
// mobile apps must use the Keychain / Android Keystore.
"use strict";

// ---------- state ----------
const store = {
  get(k, d) { try { const v = localStorage.getItem("recap." + k); return v === null ? d : JSON.parse(v); } catch { return d; } },
  set(k, v) { try { localStorage.setItem("recap." + k, JSON.stringify(v)); } catch { /* private mode */ } },
  del(k) { try { localStorage.removeItem("recap." + k); } catch { /* ignore */ } },
};
const state = {
  tokens: store.get("tokens", null), // {access_token, refresh_token}
  me: null,
  lang: store.get("lang", "ro"),
  log: [],
  logOpen: store.get("logOpen", false),
};
const STAFF_ROLES = ["super_admin", "admin", "editor", "fact_checker", "source_manager", "ai_operator", "analyst"];

// ---------- DOM helpers ----------
function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith("on") && typeof v === "function") el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else if (k === "value") el.value = v;
    else el.setAttribute(k, v === true ? "" : String(v));
  }
  for (const c of children.flat(Infinity)) {
    if (c === undefined || c === null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}
const $ = (sel) => document.querySelector(sel);
const view = () => $("#view");
// put replaces an element's children; arrays are flattened and null/false entries skipped.
function put(el, ...nodes) { el.replaceChildren(...nodes.flat(Infinity).filter((n) => n !== null && n !== undefined && n !== false)); }
function render(...nodes) { put(view(), ...nodes); window.scrollTo(0, 0); }
function fmtDate(s) { if (!s) return "—"; const d = new Date(s); return isNaN(d) ? s : d.toLocaleString("ro-RO", { dateStyle: "medium", timeStyle: "short" }); }
function pill(text, kind) { return h("span", { class: "pill " + (kind || "") }, text); }
function notice(kind, ...text) { return h("div", { class: "notice " + kind }, ...text); }
function empty(text) { return h("div", { class: "empty" }, text); }
function json(o) { return h("pre", {}, JSON.stringify(o, null, 2)); }
function field(label, input) { return [h("label", {}, label), input]; }
function langName(o) { if (!o) return ""; return o[state.lang] || o.ro || o.en || Object.values(o)[0] || ""; }

const verdictKind = {
  supported: "ok", refuted: "bad", misleading_context: "warn", disputed: "warn", unverified: "",
  not_checkable: "", partially_supported: "warn", outdated: "warn",
};
const verdictRo = {
  supported: "susținută", refuted: "infirmată", misleading_context: "context înșelător", disputed: "disputată",
  unverified: "neverificată", not_checkable: "neverificabilă", partially_supported: "parțial susținută", outdated: "depășită",
};
function verdictPill(v) { return v ? pill(verdictRo[v] || v, verdictKind[v]) : null; }

// ---------- API client ----------
class ApiError extends Error {
  constructor(status, problem) {
    super((problem && (problem.detail || problem.title)) || ("HTTP " + status));
    this.status = status; this.problem = problem || {};
  }
}

// A response that arrives after the user navigated elsewhere is dropped (StaleError), so a slow
// page can never overwrite the current one. navSeq increases on every navigation.
let navSeq = 0;
class StaleError extends Error {}

let refreshing = null;
async function refreshTokens() {
  if (!state.tokens || !state.tokens.refresh_token) throw new ApiError(401, { detail: "Sesiune expirată." });
  if (!refreshing) {
    refreshing = rawCall("POST", "/v1/auth/refresh", { refresh_token: state.tokens.refresh_token }, false)
      .then((r) => { setTokens(r.data); return r.data; })
      .finally(() => { refreshing = null; });
  }
  return refreshing;
}

async function rawCall(method, path, body, auth) {
  const headers = { Accept: "application/json" };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (auth && state.tokens) headers.Authorization = "Bearer " + state.tokens.access_token;
  const t0 = performance.now();
  let res, data = null, text = "";
  try {
    res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  } catch (e) {
    addLog({ method, path, status: 0, ms: Math.round(performance.now() - t0), req: body, res: String(e) });
    throw new ApiError(0, { detail: "Serverul nu răspunde: " + e.message });
  }
  text = await res.text();
  if (text) { try { data = JSON.parse(text); } catch { data = text; } }
  addLog({ method, path, status: res.status, ms: Math.round(performance.now() - t0), req: redact(body), res: data });
  if (!res.ok) throw new ApiError(res.status, typeof data === "object" ? data : { detail: text });
  return { status: res.status, data };
}

// api calls an endpoint as the signed-in user, refreshing the access token once on expiry.
async function api(method, path, body, opts = {}) {
  const auth = opts.auth !== false;
  const seq = navSeq;
  const stale = () => seq !== navSeq;
  try {
    const r = await rawCall(method, path, body, auth);
    if (stale()) throw new StaleError("navigated away");
    return r.data;
  } catch (e) {
    if (e instanceof StaleError || stale()) throw new StaleError("navigated away");
    if (auth && e.status === 401 && state.tokens && !opts.retried) {
      try { await refreshTokens(); } catch { logout(true); throw e; }
      return api(method, path, body, { ...opts, retried: true });
    }
    throw e;
  }
}
const GET = (p) => api("GET", p);
const POST = (p, b) => api("POST", p, b === undefined ? {} : b);

function redact(body) {
  if (!body || typeof body !== "object") return body;
  const out = { ...body };
  for (const k of ["password", "new_password", "current_password", "refresh_token", "code", "token"]) if (k in out) out[k] = "•••";
  return out;
}

function setTokens(t) {
  state.tokens = { access_token: t.access_token, refresh_token: t.refresh_token };
  store.set("tokens", state.tokens);
}

async function logout(silent) {
  if (!silent && state.tokens) { try { await api("POST", "/v1/auth/logout"); } catch { /* already gone */ } }
  state.tokens = null; state.me = null; store.del("tokens");
  renderChrome(); location.hash = "#/login";
}

function errorBox(e) {
  const p = e.problem || {};
  return notice("error",
    h("strong", {}, p.title || "Eroare"), " ", e.message,
    p.code ? h("span", { class: "small" }, "  [" + p.code + "]") : null,
    p.errors ? h("ul", {}, p.errors.map((f) => h("li", {}, (f.field ? f.field + ": " : "") + f.message))) : null);
}

// guard runs an async action and shows errors in place of (or next to) the target.
async function guard(target, fn) {
  try { return await fn(); } catch (e) {
    if (e instanceof StaleError) return undefined;
    if (target) put(target, errorBox(e)); else render(errorBox(e));
    return undefined;
  }
}

// ---------- API log ----------
function addLog(entry) {
  state.log.unshift({ ...entry, at: new Date() });
  state.log.length = Math.min(state.log.length, 80);
  if (state.logOpen) renderLog();
}
function renderLog() {
  const el = $("#log");
  put(el,
    h("div", { class: "row between" }, h("strong", {}, "Jurnal API"),
      h("button", { class: "link", onclick: () => { state.log = []; renderLog(); } }, "golește")),
    h("p", { class: "small muted" }, "Fiecare cerere făcută de consolă. Exact aceleași apeluri le face aplicația mobilă."),
    state.log.length ? state.log.map((l) => h("details", { class: "log-item" },
      h("summary", {},
        h("span", { class: "method" }, l.method),
        h("span", { class: l.status >= 200 && l.status < 400 ? "status-ok" : "status-err" }, String(l.status)),
        h("code", { style: "flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" }, l.path),
        h("span", { class: "muted" }, l.ms + "ms")),
      l.req !== undefined ? [h("div", { class: "muted" }, "cerere"), json(l.req)] : null,
      h("div", { class: "muted" }, "răspuns"), json(l.res))) : empty("Nicio cerere încă."));
}

// ---------- chrome ----------
function isStaff() { return !!(state.me && (state.me.roles || []).some((r) => STAFF_ROLES.includes(r))); }

function renderChrome() {
  const nav = $("#nav");
  const link = (href, label) => h("a", { href, "data-href": href }, label);
  const items = [];
  if (!state.tokens) {
    items.push(h("div", { class: "group" }, "Cont"), link("#/login", "Autentificare"), link("#/register", "Înregistrare"),
      link("#/verify", "Verificare email"), link("#/forgot", "Am uitat parola"));
  } else {
    items.push(h("div", { class: "group" }, "Știri"), link("#/feed", "Feed personalizat"), link("#/catchup", "Ce s-a schimbat"),
      link("#/events", "Toate evenimentele"), link("#/search", "Căutare"), link("#/bookmarks", "Salvate"),
      h("div", { class: "group" }, "Verificare"), link("#/factchecks", "Fact-checks"), link("#/trends", "Tendințe dezinformare"),
      h("div", { class: "group" }, "Asistent"), link("#/chat", "Chat"), link("#/briefings", "Briefing-uri și audio"),
      h("div", { class: "group" }, "Personalizare"), link("#/interests", "Categorii și interese"), link("#/notifications", "Notificări și dispozitive"),
      link("#/account", "Contul meu"));
    if (isStaff()) {
      items.push(h("div", { class: "group" }, "Administrare"), link("#/admin/sources", "Surse"), link("#/admin/ai", "AI: rute și modele"),
        link("#/admin/review", "Coada de verificare"), link("#/admin/jobs", "Joburi și dead letters"));
    }
  }
  items.push(h("div", { class: "group" }, "Dezvoltare"), link("#/about", "Despre API"),
    h("a", { href: "/openapi.yaml", target: "_blank" }, "OpenAPI (YAML)"),
    h("a", { href: "http://" + location.hostname + ":8025", target: "_blank" }, "Mailpit (email)"));
  put(nav, ...items);
  markActive();
  $("#who").textContent = state.me ? state.me.email : "";
  $("#logout").hidden = !state.tokens;
  $("#log").hidden = !state.logOpen;
  $("#layout").classList.toggle("with-log", state.logOpen);
  if (state.logOpen) renderLog();
}
function markActive() {
  const cur = location.hash.split("?")[0];
  document.querySelectorAll("nav.side a[data-href]").forEach((a) => {
    const href = a.getAttribute("data-href");
    a.classList.toggle("active", cur === href || (href !== "#/" && cur.startsWith(href + "/")));
  });
}

// ---------- router ----------
const routes = [];
function route(pattern, fn, opts = {}) {
  const keys = [];
  const re = new RegExp("^" + pattern.replace(/:(\w+)/g, (_, k) => { keys.push(k); return "([^/]+)"; }) + "$");
  routes.push({ re, keys, fn, auth: opts.auth !== false });
}
function params() {
  const q = location.hash.split("?")[1] || "";
  return Object.fromEntries(new URLSearchParams(q));
}
async function dispatch() {
  const seq = ++navSeq;
  $("#nav").classList.remove("open");
  const path = (location.hash.slice(1) || "/").split("?")[0];
  markActive();
  for (const r of routes) {
    const m = path.match(r.re);
    if (!m) continue;
    if (r.auth && !state.tokens) { location.hash = "#/login"; return; }
    const args = Object.fromEntries(r.keys.map((k, i) => [k, decodeURIComponent(m[i + 1])]));
    render(h("p", { class: "loading" }, "Se încarcă…"));
    try { await r.fn(args, params()); } catch (e) { if (!(e instanceof StaleError) && seq === navSeq) render(errorBox(e)); }
    return;
  }
  location.hash = state.tokens ? "#/feed" : "#/login";
}

// ---------- auth pages ----------
route("/login", () => {
  const email = h("input", { type: "email", autocomplete: "username", required: true, value: params().email || "" });
  const pass = h("input", { type: "password", autocomplete: "current-password", required: true });
  const out = h("div");
  const form = h("form", { class: "form card", onsubmit: async (ev) => {
    ev.preventDefault();
    await guard(out, async () => {
      const r = await api("POST", "/v1/auth/login", { email: email.value.trim(), password: pass.value }, { auth: false });
      setTokens(r); await loadMe(); location.hash = "#/feed";
    });
  } }, field("Email", email), field("Parolă", pass), h("div", { class: "row", style: "margin-top:.8rem" },
    h("button", { class: "primary", type: "submit" }, "Intră")), out);
  render(h("h1", {}, "Autentificare"), form,
    h("p", { class: "small muted" }, "Cont demo: vezi DEMO_EMAIL / DEMO_PASSWORD în .env. Admin: ADMIN_EMAIL / ADMIN_PASSWORD."));
}, { auth: false });

route("/register", () => {
  const f = {
    first_name: h("input", { required: true }), last_name: h("input", { required: true }),
    email: h("input", { type: "email", required: true }), phone: h("input", { placeholder: "+37369123456", required: true }),
    password: h("input", { type: "password", autocomplete: "new-password", required: true }),
    locale: h("select", {}, h("option", { value: "ro" }, "ro"), h("option", { value: "ru" }, "ru"), h("option", { value: "en" }, "en")),
  };
  const terms = h("input", { type: "checkbox" });
  const out = h("div");
  render(h("h1", {}, "Înregistrare"), h("form", { class: "form card", onsubmit: async (ev) => {
    ev.preventDefault();
    await guard(out, async () => {
      const body = Object.fromEntries(Object.entries(f).map(([k, el]) => [k, el.value.trim()]));
      body.password = f.password.value; body.accept_terms = terms.checked; body.accept_privacy = terms.checked;
      await api("POST", "/v1/auth/register", body, { auth: false });
      location.hash = "#/verify?email=" + encodeURIComponent(body.email);
    });
  } },
  field("Prenume", f.first_name), field("Nume", f.last_name), field("Email", f.email), field("Telefon", f.phone),
  field("Parolă (min. 10 caractere, litere și cifre)", f.password), field("Limba", f.locale),
  h("div", { class: "checks" }, h("label", {}, terms, "Accept termenii și politica de confidențialitate")),
  h("div", { class: "row", style: "margin-top:.8rem" }, h("button", { class: "primary", type: "submit" }, "Creează contul")), out));
}, { auth: false });

route("/verify", (_, q) => {
  const email = h("input", { type: "email", value: q.email || "" });
  const code = h("input", { inputmode: "numeric", maxlength: 6, placeholder: "123456", autocomplete: "one-time-code" });
  const out = h("div");
  const mailpit = "http://" + location.hostname + ":8025";
  render(h("h1", {}, "Verificare email"),
    notice("info", "Codul de 6 cifre a fost trimis pe email. Local îl găsești în ", h("a", { href: mailpit, target: "_blank" }, "Mailpit"),
      " — sau apasă „Caută codul în Mailpit”."),
    h("form", { class: "form card", onsubmit: async (ev) => {
      ev.preventDefault();
      await guard(out, async () => {
        const r = await api("POST", "/v1/auth/verify-email", { email: email.value.trim(), code: code.value.trim() }, { auth: false });
        setTokens(r); await loadMe(); location.hash = "#/interests?onboarding=1";
      });
    } }, field("Email", email), field("Cod", code),
    h("div", { class: "row", style: "margin-top:.8rem" },
      h("button", { class: "primary", type: "submit" }, "Verifică"),
      h("button", { type: "button", onclick: () => findCode(email.value.trim(), code, out, false) }, "Caută codul în Mailpit"),
      h("button", { type: "button", onclick: () => guard(out, async () => {
        await api("POST", "/v1/auth/resend-verification", { email: email.value.trim() }, { auth: false });
        put(out, notice("ok", "Dacă adresa există, un cod nou a fost trimis."));
      }) }, "Retrimite codul")), out));
  if (q.email) findCode(q.email, code, out, true);
  else email.focus();
}, { auth: false });

// findCode polls the development mailbox (/v1/dev/mailbox: the dev sink, or Mailpit in compose)
// for the newest code sent to the address. Mail goes out asynchronously through the worker, so it
// may take a few seconds. Not available in production.
async function findCode(email, input, out, auto) {
  const mailpit = "http://" + location.hostname + ":8025";
  if (!email) { put(out, notice("warn", "Completează emailul.")); return; }
  put(out, notice("info", "Caut codul în căsuța de test…"));
  for (let i = 0; i < 30; i++) {
    if (!document.body.contains(input) || (auto && input.value)) return;
    try {
      const r = await api("GET", "/v1/dev/mailbox?to=" + encodeURIComponent(email), undefined, { auth: false });
      const msg = (r.items || [])[0];
      const m = msg && (msg.subject + " " + msg.body).match(/\b(\d{6})\b/);
      if (m) { input.value = m[1]; put(out, notice("ok", "Cod găsit și completat automat.")); return; }
    } catch (e) {
      if (e.status === 404) break; // dev mailbox not mounted
    }
    await new Promise((res) => setTimeout(res, 2000));
  }
  put(out, notice("warn", "Nu am găsit codul automat. Deschide ", h("a", { href: mailpit, target: "_blank" }, "Mailpit"),
    ", copiază codul din ultimul email și lipește-l aici."));
}

route("/forgot", () => {
  const email = h("input", { type: "email" });
  const code = h("input", { inputmode: "numeric", maxlength: 6 });
  const pass = h("input", { type: "password", autocomplete: "new-password" });
  const out = h("div");
  render(h("h1", {}, "Resetare parolă"), h("div", { class: "form card" },
    field("Email", email),
    h("button", { onclick: () => guard(out, async () => {
      await api("POST", "/v1/auth/forgot-password", { email: email.value.trim() }, { auth: false });
      put(out, notice("ok", "Dacă adresa există, un cod a fost trimis (vezi Mailpit)."));
    }) }, "Trimite codul"),
    field("Cod", code), field("Parola nouă", pass),
    h("div", { class: "row", style: "margin-top:.8rem" }, h("button", { class: "primary", onclick: () => guard(out, async () => {
      await api("POST", "/v1/auth/reset-password", { email: email.value.trim(), code: code.value.trim(), new_password: pass.value }, { auth: false });
      location.hash = "#/login?email=" + encodeURIComponent(email.value.trim());
    }) }, "Schimbă parola")), out));
}, { auth: false });

// ---------- news ----------
function eventCard(ev, extra) {
  const cats = (ev.categories || []).map((c) => pill(langName(c.names) || c.slug, "info"));
  return h("div", { class: "card click", onclick: () => { location.hash = "#/event/" + ev.id; } },
    h("div", { class: "row between" }, h("span", { class: "title" }, ev.title || "(fără titlu)"),
      h("span", { class: "small muted" }, fmtDate(ev.last_material_update_at || ev.earliest_published_at))),
    ev.summary && ev.summary.short ? h("p", {}, stripRefs(ev.summary.short)) : null,
    h("div", { class: "row small" }, cats,
      pill(ev.source_count + (ev.source_count === 1 ? " sursă" : " surse")),
      pill((ev.languages || []).join(", ") || ev.language),
      ev.language_fallback ? pill("rezumat în limba sursei", "warn") : null,
      ev.state ? pill(ev.state) : null, extra));
}
function stripRefs(t) { return String(t || "").replace(/\s*\[(?:a|e|c)\d+\]/g, ""); }

async function feedPage(path, title, intro) {
  const list = h("div");
  const more = h("div", { class: "row" });
  let cursor = "";
  async function load() {
    const sep = path.includes("?") ? "&" : "?";
    const r = await GET(path + sep + "limit=15&lang=" + state.lang + (cursor ? "&cursor=" + encodeURIComponent(cursor) : ""));
    if (r.cold_start && !list.childElementCount) list.append(notice("info", "Feed fără preferințe: ", h("a", { href: "#/interests?onboarding=1" }, "alege categoriile"), " pentru un feed personalizat."));
    for (const it of r.items || []) {
      const ev = it.event || it;
      list.append(eventCard(ev, [it.unseen ? pill("nou", "ok") : null, it.updated_since_seen ? pill("actualizat", "warn") : null,
        it.score !== undefined ? h("span", { class: "muted", title: (it.reasons || []).join(", ") }, "scor " + Number(it.score).toFixed(2)) : null]));
    }
    if (!list.childElementCount) list.append(empty("Nimic de afișat încă. Worker-ul procesează sursele; revino în câteva secunde."));
    cursor = r.next_cursor || "";
    put(more, cursor ? h("button", { onclick: () => guard(more, load) }, "Mai multe") : null);
  }
  render(h("h1", {}, title), intro ? h("p", { class: "muted" }, intro) : null, list, more);
  await load();
}
route("/feed", () => feedPage("/v1/feed", "Feed personalizat", "Clasat după interese, importanță, noutate și diversitate. Cursor stabil la paginare."));
route("/events", () => feedPage("/v1/events", "Toate evenimentele", "Cele mai noi evenimente, fără personalizare."));
route("/bookmarks", () => feedPage("/v1/users/me/bookmarks", "Evenimente salvate"));

route("/catchup", async () => {
  const r = await GET("/v1/catch-up?lang=" + state.lang);
  render(h("h1", {}, "Ce s-a schimbat de la ultima vizită"),
    h("p", { class: "muted" }, "De la ", fmtDate(r.since), " (", r.since_source || "", "). ", r.note || ""),
    (r.items || []).length ? r.items.map((it) => eventCard(it.event, [it.previously_seen ? pill("văzut anterior") : pill("nou", "ok"),
      ...(it.changes_since || []).slice(0, 2).map((c) => pill(c.kind || "schimbare", "warn"))])) : empty("Nimic nou."),
    h("button", { class: "primary", onclick: () => guard(null, async () => { await POST("/v1/catch-up/acknowledge", {}); location.hash = "#/feed"; }) },
      "Am văzut tot"));
});

function citedText(text, cites) {
  // Renders "… [a1]" markers as links to the citation list; text stays text (untrusted).
  const parts = String(text || "").split(/(\[(?:a|e|c)\d+\])/g);
  const known = new Set((cites || []).map((c) => c.ref));
  return parts.map((p) => {
    const m = p.match(/^\[((?:a|e|c)\d+)\]$/);
    if (!m) return p;
    return known.has(m[1]) ? h("sup", { class: "cite" }, h("a", { href: "#cite-" + m[1], onclick: (e) => { e.preventDefault(); const t = document.getElementById("cite-" + m[1]); if (t) t.scrollIntoView({ behavior: "smooth" }); } }, m[1])) : "";
  });
}
function citationList(cites) {
  if (!cites || !cites.length) return null;
  return h("ol", { class: "cites" }, cites.map((c) => h("li", { id: "cite-" + c.ref },
    h("strong", {}, c.ref + " "), c.url ? h("a", { href: c.url, target: "_blank", rel: "noopener noreferrer" }, c.title || c.url) : (c.title || ""),
    h("span", { class: "muted" }, " — ", c.publisher || c.type || "", c.published_at ? ", " + fmtDate(c.published_at) : ""),
    c.excerpt ? h("div", { class: "small muted" }, "„", c.excerpt, "”") : null)));
}

route("/event/:id", async ({ id }) => {
  const [ev, srcs, tl, claims] = await Promise.all([
    GET(`/v1/events/${id}?lang=${state.lang}`), GET(`/v1/events/${id}/sources`),
    GET(`/v1/events/${id}/timeline`), GET(`/v1/events/${id}/claims?lang=${state.lang}`)]);
  const s = ev.summary || {};
  const level = h("div", { class: "tabs" });
  const body = h("div", { class: "summary" });
  const showLevel = (k) => {
    put(body, ...citedText(s[k] || s.normal || s.short, s.citations));
    level.querySelectorAll("button").forEach((b) => b.classList.toggle("on", b.dataset.k === k));
  };
  for (const [k, l] of [["short", "Scurt"], ["normal", "Normal"], ["detailed", "Detaliat"]]) {
    level.append(h("button", { "data-k": k, onclick: () => showLevel(k) }, l));
  }
  const actOut = h("div");
  const act = (label, fn, msg) => h("button", { onclick: () => guard(actOut, async () => { await fn(); put(actOut, notice("ok", msg)); }) }, label);
  const explainOut = h("div");
  render(
    h("p", {}, h("a", { href: "#/feed" }, "← înapoi")),
    h("h1", {}, ev.title),
    h("div", { class: "row small" }, (ev.categories || []).map((c) => pill(langName(c.names) || c.slug, "info")),
      pill(ev.state), pill(ev.editorial_status), pill("încredere " + Math.round((ev.confidence || 0) * 100) + "%"),
      pill(ev.source_count + " surse"), pill("acoperire citări " + Math.round((ev.citation_coverage || 0) * 100) + "%"),
      ev.language_fallback ? pill("rezumat în " + ev.language + " (nu există încă în " + state.lang + ")", "warn") : null),
    h("p", { class: "small muted" }, "Prima publicare: ", fmtDate(ev.earliest_published_at), " · ultima actualizare: ", fmtDate(ev.last_material_update_at),
      " · model: ", s.model_ref || "—"),
    h("div", { class: "row" },
      act("Urmărește", () => POST(`/v1/events/${id}/follow`), "Urmărești evenimentul; primești notificări la schimbări importante."),
      act("Nu mai urmări", () => POST(`/v1/events/${id}/unfollow`), "Nu mai urmărești evenimentul."),
      act("Salvează", () => api("PUT", `/v1/events/${id}/bookmark`), "Salvat."),
      act("Marchează văzut", () => POST(`/v1/events/${id}/seen`, { version: ev.version }), "Marcat ca văzut."),
      act("Ascunde din feed", () => POST(`/v1/events/${id}/dismiss`), "Ascuns din feed."),
      h("button", { onclick: async () => {
        await guard(null, async () => { const c = await POST("/v1/chat/sessions", { scope_type: "event", scope_id: id, language: state.lang }); location.hash = "#/chat/" + c.id; });
      } }, "Întreabă despre eveniment")),
    actOut,
    h("div", { class: "card" }, level, body, (s.disputed_points || []).length ? [h("h3", {}, "Puncte disputate"), h("ul", {}, s.disputed_points.map((d) => h("li", {}, typeof d === "string" ? d : JSON.stringify(d))))] : null,
      h("h3", {}, "Surse citate"), citationList(s.citations)),
    h("div", { class: "card" }, h("div", { class: "row between" }, h("h3", { style: "margin:0" }, "De ce contează?"),
      h("button", { onclick: () => guard(explainOut, async () => {
        put(explainOut, h("p", { class: "loading" }, "Se generează…"));
        const m = await GET(`/v1/events/${id}/explain?lang=${state.lang}&question=${encodeURIComponent("De ce este important pentru o afacere din Moldova?")}`);
        put(explainOut, answer(m));
      }) }, "Explică")), explainOut),
    h("h2", {}, "Articole sursă (" + (srcs.items || []).length + ")"),
    h("div", { class: "card" }, h("table", {}, h("tr", {}, h("th", {}, "Publicație"), h("th", {}, "Titlu"), h("th", {}, "Relație"), h("th", {}, "Publicat")),
      (srcs.items || []).map((a) => h("tr", {}, h("td", {}, a.publisher), h("td", {}, h("a", { href: a.url, target: "_blank", rel: "noopener noreferrer" }, a.title), h("div", { class: "small muted" }, a.attribution || "")),
        h("td", {}, pill(a.relation), " ", h("span", { class: "small muted" }, Math.round((a.match_confidence || 0) * 100) + "%")), h("td", { class: "small" }, fmtDate(a.published_at)))))),
    h("h2", {}, "Cronologie"),
    (tl.items || []).length ? h("div", { class: "card" }, (tl.items || []).map((t) => h("div", { style: "margin:.35rem 0" },
      h("strong", {}, fmtDate(t.occurred_at)), " ", pill(t.kind), " ", pill(t.severity, t.severity === "major" ? "warn" : ""), " ",
      (t.what_changed && (t.what_changed.summary || JSON.stringify(t.what_changed))) || ""))) : empty("Nicio actualizare."),
    h("h2", {}, "Afirmații verificate"),
    (claims.items || []).length ? (claims.items || []).map(claimCard) : empty("Nicio afirmație verificabilă extrasă."));
  showLevel("normal");
});

function claimCard(c) {
  const id = c.claim_id || c.id;
  return h("div", { class: "card click", onclick: () => { location.hash = "#/factcheck/" + id; } },
    h("div", { class: "row between" }, h("span", {}, "„", c.claim, "”"), verdictPill(c.verdict)),
    c.headline ? h("p", { class: "small" }, h("strong", {}, c.headline)) : null,
    h("div", { class: "row small muted" }, c.attributed_to && c.attributed_to.length ? "atribuit: " + c.attributed_to.join(", ") : null,
      pill(c.review_status), c.status ? pill(c.status) : null, "evaluat " + fmtDate(c.assessed_at)));
}

route("/search", async (_, q) => {
  const input = h("input", { type: "search", placeholder: "ex: tarif energie, пенсии, parlament", value: q.q || "" });
  const type = h("select", { style: "width:auto" }, h("option", { value: "" }, "toate"), h("option", { value: "event" }, "evenimente"),
    h("option", { value: "article" }, "articole"), h("option", { value: "claim" }, "afirmații"));
  type.value = q.type || "";
  const out = h("div");
  const go = () => { location.hash = "#/search?q=" + encodeURIComponent(input.value) + (type.value ? "&type=" + type.value : ""); };
  render(h("h1", {}, "Căutare"), h("p", { class: "muted" }, "Hibrid: OpenSearch (lexical) + Qdrant (semantic, multilingv), fuziune RRF și reranking."),
    h("form", { class: "inline-input", onsubmit: (e) => { e.preventDefault(); go(); } }, input, type, h("button", { class: "primary" }, "Caută")), out);
  input.focus();
  if (!q.q) return;
  const r = await guard(out, () => GET("/v1/search?q=" + encodeURIComponent(q.q) + "&lang=" + state.lang + (q.type ? "&types=" + q.type : "")));
  if (!r) return;
  put(out, 
    (r.degraded || []).length ? notice("warn", "Componente degradate: " + r.degraded.join(", ")) : notice("ok", "Toate componentele de căutare au răspuns."),
    (r.items || []).length ? r.items.map((it) => h("div", { class: "card click", onclick: () => {
      location.hash = it.type === "event" ? "#/event/" + it.id : it.type === "claim" ? "#/factcheck/" + it.id : (it.event_id ? "#/event/" + it.event_id : location.hash);
    } }, h("div", { class: "row between" }, h("span", { class: "title" }, it.title), h("span", { class: "row" }, pill(it.type, "info"), verdictPill(it.verdict))),
      it.snippet ? h("p", { class: "small" }, it.snippet) : null,
      h("div", { class: "small muted" }, "scor ", Number(it.score).toFixed(4), " · lexical #", it.lexical_rank || "—", " · semantic #", it.semantic_rank || "—",
        it.published_at ? " · " + fmtDate(it.published_at) : ""))) : empty("Niciun rezultat."));
});

// ---------- fact-checking ----------
route("/factchecks", async () => {
  const r = await GET("/v1/fact-checks?limit=30&lang=" + state.lang);
  render(h("h1", {}, "Fact-checks"), h("p", { class: "muted" }, "Fără dovezi suficiente verdictul implicit este „neverificată”, niciodată „falsă”. Verdictele negative necesită revizie editorială."),
    (r.items || []).length ? r.items.map(claimCard) : empty("Nicio verificare publicată."));
});
route("/factcheck/:id", async ({ id }) => {
  const r = await GET(`/v1/fact-checks/${id}?lang=${state.lang}`);
  const fc = r.fact_check || r;
  render(h("p", {}, h("a", { href: "#/factchecks" }, "← fact-checks")),
    h("h1", {}, "„", fc.claim, "”"),
    h("div", { class: "row" }, verdictPill(fc.verdict), pill(fc.review_status), fc.confidence !== null && fc.confidence !== undefined ? pill("încredere " + Math.round(fc.confidence * 100) + "%") : null),
    h("div", { class: "card" }, h("p", {}, h("strong", {}, fc.headline || "")), h("p", {}, fc.explanation || ""),
      (fc.limitations || []).length ? [h("h3", {}, "Limitări"), h("ul", {}, fc.limitations.map((l) => h("li", {}, l)))] : null,
      h("p", { class: "small muted" }, fc.disclaimer || "")),
    h("h2", {}, "Dovezi"),
    (fc.evidence || []).length ? h("div", { class: "card" }, h("table", {}, h("tr", {}, h("th", {}, "Relație"), h("th", {}, "Sursă"), h("th", {}, "Data")),
      fc.evidence.map((e) => h("tr", {}, h("td", {}, pill(e.relation || e.stance || "", e.relation === "supports" ? "ok" : e.relation === "contradicts" ? "bad" : "")),
        h("td", {}, e.url ? h("a", { href: e.url, target: "_blank", rel: "noopener noreferrer" }, e.title || e.url) : (e.title || ""), h("div", { class: "small muted" }, e.publisher || "", e.is_primary ? " · sursă primară" : "", e.quote ? " · „" + e.quote + "”" : "")),
        h("td", { class: "small" }, fmtDate(e.published_at)))))) : empty("Nicio dovadă independentă găsită."),
    (r.history || []).length ? [h("h2", {}, "Istoric și corecții"), h("div", { class: "card" }, r.history.map((v) => h("div", {}, fmtDate(v.assessed_at || v.created_at), " ", verdictPill(v.verdict), " ", v.correction_note || "")))] : null);
});
route("/trends", async () => {
  const r = await GET("/v1/misinformation/trends?lang=" + state.lang);
  render(h("h1", {}, r.title || "Tendințe"), h("p", { class: "muted" }, r.method || ""),
    h("h2", {}, "Narațiuni"), (r.narratives || []).length ? r.narratives.map((n) => h("div", { class: "card" }, h("div", { class: "title" }, n.title || n.label || ""), h("p", {}, n.summary || ""))) : empty("Nicio narațiune revizuită în fereastra curentă."),
    h("h2", {}, "Afirmații"), (r.claims || []).length ? r.claims.map(claimCard) : empty("Nicio afirmație revizuită în fereastra curentă."));
});

// ---------- chat ----------
function answer(m) {
  return h("div", {},
    m.abstained ? notice("warn", "Asistentul s-a abținut: sursele disponibile nu acoperă întrebarea.") : null,
    h("p", {}, ...citedText(m.content, m.citations)),
    m.uncertainty ? h("p", { class: "small muted" }, "Incertitudine: ", m.uncertainty) : null,
    citationList(m.citations));
}
route("/chat", async () => {
  const r = await GET("/v1/chat/sessions");
  const out = h("div");
  render(h("h1", {}, "Chat"), h("p", { class: "muted" }, "Răspunsurile sunt bazate doar pe surse găsite; citările sunt verificate pe server. Fără surse, asistentul se abține."),
    h("button", { class: "primary", onclick: () => guard(out, async () => {
      const s = await POST("/v1/chat/sessions", { scope_type: "global", language: state.lang }); location.hash = "#/chat/" + s.id;
    }) }, "Conversație nouă"), out,
    h("h2", {}, "Conversații"),
    (r.items || []).length ? r.items.map((s) => h("div", { class: "card click", onclick: () => { location.hash = "#/chat/" + s.id; } },
      h("div", { class: "row between" }, h("span", { class: "title" }, s.title || "Conversație " + s.scope_type), h("span", { class: "small muted" }, fmtDate(s.last_message_at))),
      h("div", { class: "row small" }, pill(s.scope_type), pill(s.language), h("span", { class: "muted" }, "expiră " + fmtDate(s.expires_at))))) : empty("Nicio conversație."));
});
route("/chat/:id", async ({ id }) => {
  const s = await GET("/v1/chat/sessions/" + id);
  const box = h("div", { class: "chat" });
  const addMsg = (m) => box.append(h("div", { class: "msg " + m.role }, m.role === "user" ? m.content : answer(m)));
  (s.messages || []).forEach(addMsg);
  const input = h("textarea", { placeholder: "ex: De ce este important pentru o afacere din Moldova?" });
  const out = h("div");
  const send = async (e) => {
    e.preventDefault();
    const content = input.value.trim(); if (!content) return;
    addMsg({ role: "user", content }); input.value = "";
    const btn = e.target.querySelector("button"); btn.disabled = true;
    await guard(out, async () => { put(out, ); addMsg(await POST(`/v1/chat/sessions/${id}/messages`, { content })); });
    btn.disabled = false;
  };
  render(h("p", {}, h("a", { href: "#/chat" }, "← conversații")), h("h1", {}, s.title || "Conversație"),
    h("div", { class: "row small" }, pill(s.scope_type), pill(s.language)),
    h("div", { class: "card" }, box), out,
    h("form", { onsubmit: send }, input, h("div", { class: "row", style: "margin-top:.5rem" }, h("button", { class: "primary" }, "Trimite"),
      h("button", { type: "button", class: "danger", onclick: () => guard(out, async () => { await api("DELETE", "/v1/chat/sessions/" + id); location.hash = "#/chat"; }) }, "Șterge conversația"))));
});

// ---------- briefings ----------
route("/briefings", async () => {
  const r = await GET("/v1/briefings");
  const minutes = h("select", { style: "width:auto" }, [3, 5, 10, 15].map((m) => h("option", { value: m }, m + " min")));
  minutes.value = "5";
  const out = h("div");
  render(h("h1", {}, "Briefing-uri"), h("p", { class: "muted" }, "Rezumat ordonat al zilei, cu citări. Audio necesită o rută TTS configurată; fără ea statusul este provider_unavailable (nu se fabrică audio)."),
    h("div", { class: "row" }, minutes, h("button", { class: "primary", onclick: () => guard(out, async () => {
      const b = await POST("/v1/briefings", { duration_minutes: Number(minutes.value), language: state.lang });
      location.hash = "#/briefing/" + b.id;
    }) }, "Generează briefing")), out,
    h("h2", {}, "Recente"),
    (r.items || []).length ? r.items.map((b) => h("div", { class: "card click", onclick: () => { location.hash = "#/briefing/" + b.id; } },
      h("div", { class: "row between" }, h("span", { class: "title" }, b.title || "Briefing"), pill(b.status, b.status === "ready" ? "ok" : b.status === "failed" ? "bad" : "")),
      h("div", { class: "small muted" }, fmtDate(b.requested_at), " · ", b.duration_minutes, " min · ", b.language))) : empty("Niciun briefing."));
});
route("/briefing/:id", async ({ id }) => {
  let b = await GET("/v1/briefings/" + id);
  for (let i = 0; i < 20 && (b.status === "queued" || b.status === "generating" || b.status === "pending"); i++) {
    render(h("p", { class: "loading" }, "Se generează briefing-ul… (" + b.status + ")"));
    await new Promise((res) => setTimeout(res, 1000));
    b = await GET("/v1/briefings/" + id);
  }
  const audioOut = h("div");
  const showAudio = (a) => {
    put(audioOut, h("div", { class: "row" }, pill("audio: " + a.status, a.status === "ready" ? "ok" : a.status === "provider_unavailable" ? "warn" : ""),
      a.provider ? pill(a.provider + (a.model ? "/" + a.model : "")) : null),
    a.error ? h("p", { class: "small muted" }, a.error) : null,
    a.status === "provider_unavailable" ? notice("warn", "Nu există un provider TTS configurat. Adaugă unul în AI: rute și modele (sarcina tts).") : null,
    a.url ? h("audio", { controls: true, src: a.url, style: "width:100%" }) : null);
  };
  render(h("p", {}, h("a", { href: "#/briefings" }, "← briefing-uri")), h("h1", {}, b.title || "Briefing"),
    h("div", { class: "row small" }, pill(b.status, b.status === "ready" ? "ok" : ""), pill(b.duration_minutes + " min"), b.estimated_seconds ? pill("~" + Math.round(b.estimated_seconds / 60) + " min citire") : null,
      b.model_ref ? pill(b.model_ref) : null),
    b.error_message ? notice("error", b.error_message) : null,
    h("div", { class: "card" }, h("div", { class: "row between" }, h("h3", { style: "margin:0" }, "Audio"),
      h("div", { class: "row" },
        h("button", { onclick: () => guard(audioOut, async () => showAudio(await POST(`/v1/briefings/${id}/audio`, {}))) }, "Cere audio"),
        h("button", { onclick: () => guard(audioOut, async () => showAudio(await GET(`/v1/briefings/${id}/audio`))) }, "Status audio"))), audioOut),
    (b.items || []).map((it) => h("div", { class: "card" }, h("div", { class: "title" }, it.position + ". ",
      h("a", { href: "#/event/" + it.event_id }, it.title)), h("p", {}, ...citedText(it.text, it.citations)), citationList(it.citations))));
});

// ---------- personalization ----------
route("/interests", async (_, q) => {
  const [tree, interests, prefs] = await Promise.all([GET("/v1/categories/tree?lang=" + state.lang), GET("/v1/users/me/interests"), GET("/v1/users/me/preferences")]);
  const followed = new Set((interests.items || []).filter((i) => i.interest_type === "category" && i.mode === "follow").map((i) => i.target_key || i.target));
  const boxes = [];
  const walk = (nodes, depth) => nodes.map((c) => {
    const cb = h("input", { type: "checkbox", value: c.id });
    cb.checked = followed.has(c.id); boxes.push(cb);
    return [h("label", { style: "margin-left:" + depth * 1.2 + "rem" }, cb, langName(c.names) || c.slug), walk(c.children || [], depth + 1)];
  });
  const out = h("div");
  const kw = h("input", { placeholder: "cuvânt cheie (ex: Bălți)" });
  const mode = h("select", { style: "width:auto" }, h("option", { value: "follow" }, "urmăresc"), h("option", { value: "mute" }, "ascund"));
  const summaryLang = h("select", { style: "width:auto" }, ["ro", "ru", "en"].map((l) => h("option", { value: l }, l)));
  summaryLang.value = prefs.summary_language || "ro";
  render(h("h1", {}, q.onboarding ? "Bine ai venit! Alege ce te interesează" : "Categorii și interese"),
    h("div", { class: "card checks" }, walk(tree.items || [], 0),
      h("div", { class: "row", style: "margin-top:.8rem" }, h("button", { class: "primary", onclick: () => guard(out, async () => {
        await api("PUT", "/v1/users/me/categories", { category_ids: boxes.filter((b) => b.checked).map((b) => b.value) });
        if (q.onboarding) location.hash = "#/feed"; else put(out, notice("ok", "Categorii salvate."));
      }) }, q.onboarding ? "Continuă spre feed" : "Salvează categoriile"))),
    out,
    h("h2", {}, "Cuvinte cheie"),
    h("div", { class: "card" }, h("div", { class: "inline-input" }, kw, mode, h("button", { onclick: () => guard(out, async () => {
      await POST("/v1/users/me/interests", { interest_type: "keyword", target: kw.value.trim(), mode: mode.value }); dispatch();
    }) }, "Adaugă")),
    h("table", {}, (interests.items || []).filter((i) => i.interest_type !== "category").map((i) => h("tr", {}, h("td", {}, pill(i.interest_type)), h("td", {}, i.label || i.target_key || i.target), h("td", {}, pill(i.mode, i.mode === "mute" ? "warn" : "ok")),
      h("td", {}, h("button", { class: "link", onclick: () => guard(out, async () => { await api("DELETE", "/v1/users/me/interests/" + i.id); dispatch(); }) }, "șterge")))))),
    h("h2", {}, "Preferințe"),
    h("div", { class: "card form" }, field("Limba rezumatelor", summaryLang),
      h("div", { class: "row", style: "margin-top:.6rem" }, h("button", { onclick: () => guard(out, async () => {
        await api("PUT", "/v1/users/me/preferences", { summary_language: summaryLang.value, version: prefs.version });
        put(out, notice("ok", "Preferințe salvate."));
      }) }, "Salvează")), h("details", {}, h("summary", { class: "small muted" }, "JSON"), json(prefs))));
});

// ---------- notifications ----------
route("/notifications", async () => {
  const [sch, dev, del] = await Promise.all([GET("/v1/notifications/schedules"), GET("/v1/devices"), GET("/v1/notifications/deliveries")]);
  const time = h("input", { type: "time", value: "08:00", style: "width:auto" });
  const qs = h("input", { type: "time", value: "22:00", style: "width:auto" });
  const qe = h("input", { type: "time", value: "07:00", style: "width:auto" });
  const platform = h("select", { style: "width:auto" }, h("option", { value: "android" }, "android (FCM)"), h("option", { value: "ios" }, "ios (APNs)"));
  const token = h("input", { placeholder: "token push (de test)" });
  const out = h("div");
  render(h("h1", {}, "Notificări și dispozitive"),
    h("h2", {}, "Program digest"),
    h("div", { class: "card" }, h("div", { class: "row" }, "Zilnic la", time, "liniște între", qs, "și", qe,
      h("button", { class: "primary", onclick: () => guard(out, async () => {
        await api("PUT", "/v1/notifications/schedules", { kind: "daily", local_times: [time.value], timezone: Intl.DateTimeFormat().resolvedOptions().timeZone || "Europe/Chisinau", quiet_start: qs.value, quiet_end: qe.value });
        dispatch();
      }) }, "Salvează")),
    (sch.items || []).length ? h("table", {}, (sch.items || []).map((s) => h("tr", {}, h("td", {}, s.name), h("td", {}, s.kind), h("td", {}, (s.local_times || []).join(", ")), h("td", {}, s.timezone), h("td", { class: "small" }, "următorul: ", fmtDate(s.next_run_at))))) : h("p", { class: "muted" }, "Niciun program.")),
    out,
    h("h2", {}, "Dispozitive (token-urile nu sunt returnate niciodată)"),
    h("div", { class: "card" }, h("div", { class: "inline-input" }, platform, token, h("button", { onclick: () => guard(out, async () => {
      await POST("/v1/devices", { platform: platform.value, token: token.value.trim(), app_version: "console", locale: state.lang }); dispatch();
    }) }, "Înregistrează")),
    (dev.items || []).length ? h("table", {}, dev.items.map((d) => h("tr", {}, h("td", {}, d.platform), h("td", {}, d.provider), h("td", { class: "small" }, fmtDate(d.created_at)),
      h("td", {}, h("button", { class: "link", onclick: () => guard(out, async () => { await api("DELETE", "/v1/devices/" + d.id); dispatch(); }) }, "revocă"))))) : h("p", { class: "muted" }, "Niciun dispozitiv.")),
    h("h2", {}, "Istoric livrări"),
    (del.items || []).length ? h("div", { class: "card" }, h("table", {}, del.items.map((d) => h("tr", {}, h("td", { class: "small" }, fmtDate(d.created_at || d.sent_at)), h("td", {}, d.kind || d.type || ""),
      h("td", {}, pill(d.status, d.status === "sent" ? "ok" : "warn")), h("td", { class: "small muted" }, d.reason || d.skip_reason || d.error || ""))))) : empty("Nicio livrare încă."));
});

// ---------- account ----------
route("/account", async () => {
  const [me, sessions, reqs] = await Promise.all([GET("/v1/users/me"), GET("/v1/users/me/sessions"), GET("/v1/users/me/data-requests")]);
  const out = h("div");
  const cur = h("input", { type: "password", autocomplete: "current-password" });
  const nw = h("input", { type: "password", autocomplete: "new-password" });
  render(h("h1", {}, "Contul meu"),
    h("div", { class: "card" }, h("div", { class: "title" }, me.first_name, " ", me.last_name), h("div", {}, me.email, " ", me.email_verified_at ? pill("verificat", "ok") : pill("neverificat", "warn")),
      h("div", { class: "small muted" }, me.phone, " · ", me.locale, " · ", me.timezone, " · roluri: ", (me.roles || []).join(", "))),
    out,
    h("h2", {}, "Sesiuni active"),
    h("div", { class: "card" }, h("table", {}, (sessions.items || []).map((s) => h("tr", {}, h("td", {}, s.device_label || s.user_agent || "—", s.current ? " " : "", s.current ? pill("aceasta", "info") : null),
      h("td", { class: "small" }, "ultima folosire ", fmtDate(s.last_used_at)),
      h("td", {}, s.current ? null : h("button", { class: "link", onclick: () => guard(out, async () => { await api("DELETE", "/v1/users/me/sessions/" + s.id); dispatch(); }) }, "revocă"))))),
      h("button", { onclick: () => guard(out, async () => { await POST("/v1/auth/logout-all"); logout(true); }) }, "Ieși de pe toate dispozitivele")),
    h("h2", {}, "Schimbă parola"),
    h("div", { class: "card form" }, field("Parola curentă", cur), field("Parola nouă", nw), h("div", { class: "row", style: "margin-top:.6rem" },
      h("button", { onclick: () => guard(out, async () => { await POST("/v1/auth/change-password", { current_password: cur.value, new_password: nw.value }); put(out, notice("ok", "Parola a fost schimbată; celelalte sesiuni au fost închise.")); }) }, "Schimbă"))),
    h("h2", {}, "Datele mele"),
    h("div", { class: "card" }, h("p", { class: "muted small" }, "Exportul se generează asincron și se descarcă printr-un link semnat (valabil 15 minute)."),
      h("button", { onclick: () => guard(out, async () => { await POST("/v1/users/me/data-export"); setTimeout(() => { if (location.hash.startsWith("#/account")) dispatch(); }, 2500); put(out, notice("info", "Export cerut; pagina se reîncarcă…")); }) }, "Cere exportul datelor"),
      h("table", {}, (reqs.items || []).map((d) => h("tr", {}, h("td", {}, d.kind), h("td", {}, pill(d.status, d.status === "succeeded" ? "ok" : d.status === "failed" ? "bad" : "")),
        h("td", { class: "small" }, fmtDate(d.requested_at)), h("td", {}, d.download_url ? (d.download_url.startsWith("http") ? h("a", { href: d.download_url, target: "_blank" }, "descarcă")
          : h("button", { class: "link", onclick: () => downloadAuthed(d.download_url, "recap-export.json") }, "descarcă")) : null))))),
    h("details", { class: "card" }, h("summary", { class: "danger" }, "Șterge contul"),
      h("p", { class: "small" }, "Sesiunile sunt închise imediat; datele se șterg după perioada de grație."),
      h("button", { class: "danger", onclick: () => guard(out, async () => {
        const pw = prompt("Confirmă cu parola:"); if (!pw) return;
        await POST("/v1/users/me/deletion", { password: pw }); logout(true);
      }) }, "Șterge definitiv contul")));
});
async function downloadAuthed(path, name) {
  const res = await fetch(path, { headers: { Authorization: "Bearer " + state.tokens.access_token } });
  const blob = await res.blob();
  const a = h("a", { href: URL.createObjectURL(blob), download: name }); a.click(); URL.revokeObjectURL(a.href);
}

// ---------- admin ----------
route("/admin/sources", async () => {
  const r = await GET("/v1/admin/sources?limit=100");
  const out = h("div");
  const healthKind = { healthy: "ok", degraded: "warn", failing: "bad", unknown: "" };
  const action = (s, verb, label) => h("button", { onclick: () => guard(out, async () => {
    const res = await POST(`/v1/admin/sources/${s.id}/${verb}`, verb === "crawl" ? { max_items: 20 } : undefined);
    put(out, notice("ok", label + ": ", JSON.stringify(res || "ok")));
  }) }, label);
  render(h("h1", {}, "Surse"), h("p", { class: "muted" }, "Surse, test, activare, crawl — fără restart. Sursele noi se adaugă din API (POST /v1/admin/publishers, /v1/admin/sources) sau cu newsctl verify-sources."),
    out,
    (r.items || []).map((s) => h("div", { class: "card" },
      h("div", { class: "row between" }, h("span", { class: "title" }, s.name), h("span", { class: "row" }, pill(s.status, s.status === "enabled" ? "ok" : ""), pill(s.health_state, healthKind[s.health_state]))),
      h("div", { class: "small muted" }, s.endpoint_type, " · ", s.entry_url, " · la fiecare ", Math.round(s.poll_interval_seconds / 60), " min · ultimul succes ", fmtDate(s.last_success_at)),
      h("div", { class: "row", style: "margin-top:.4rem" }, action(s, "test", "Testează"), action(s, "crawl", "Crawl acum"),
        s.status === "enabled" ? action(s, "disable", "Dezactivează") : action(s, "enable", "Activează"),
        h("button", { onclick: () => guard(out, async () => { put(out, json(await GET(`/v1/admin/sources/${s.id}/runs?limit=5`))); }) }, "Rulări")))));
});

route("/admin/ai", async () => {
  const [routesR, models, providers, usage] = await Promise.all([GET("/v1/admin/ai/routes"), GET("/v1/admin/ai/models"), GET("/v1/admin/ai/providers"), GET("/v1/admin/ai/usage")]);
  const out = h("div");
  const kindFor = (task) => task === "embeddings" ? "embedding" : task === "reranking" ? "rerank" : task === "tts" ? "tts" : "chat";
  render(h("h1", {}, "AI: rute și modele"),
    h("p", { class: "muted" }, "Schimbarea modelului unei sarcini se aplică imediat în API și worker (LISTEN/NOTIFY). Pentru modele externe pune cheia în .env (ANTHROPIC_API_KEY, OPENAI_API_KEY, GEMINI_API_KEY), repornește containerele o dată, apoi activează providerul aici."),
    out,
    h("h2", {}, "Rute pe sarcini"),
    h("div", { class: "card" }, h("table", {}, h("tr", {}, h("th", {}, "Sarcină"), h("th", {}, "Mod"), h("th", {}, "Model principal"), h("th", {}, "")),
      (routesR.items || []).map((rt) => {
        const sel = h("select", {}, (models.items || []).filter((m) => m.kind === kindFor(rt.task)).map((m) => h("option", { value: m.id }, m.provider_slug + " / " + m.slug + (m.enabled ? "" : " (dezactivat)"))));
        sel.value = rt.primary_model_id;
        return h("tr", {}, h("td", {}, rt.task), h("td", {}, pill(rt.mode)), h("td", {}, sel),
          h("td", {}, h("button", { onclick: () => guard(out, async () => {
            await api("PATCH", "/v1/admin/ai/routes/" + rt.id, { primary_model_id: sel.value, version: rt.version });
            put(out, notice("ok", "Ruta " + rt.task + " folosește acum " + sel.selectedOptions[0].textContent)); setTimeout(dispatch, 900);
          }) }, "Aplică")));
      }))),
    h("h2", {}, "Provideri"),
    h("div", { class: "card" }, h("table", {}, (providers.items || []).map((p) => h("tr", {}, h("td", {}, h("strong", {}, p.slug), h("div", { class: "small muted" }, p.kind, p.base_url ? " · " + p.base_url : "")),
      h("td", {}, p.key_configured ? pill("cheie configurată", "ok") : pill("fără cheie", "warn")), h("td", {}, pill(p.enabled ? "activ" : "inactiv", p.enabled ? "ok" : "")),
      h("td", {}, h("button", { onclick: () => guard(out, async () => { await api("PATCH", "/v1/admin/ai/providers/" + p.id, { enabled: !p.enabled, version: p.version }); dispatch(); }) }, p.enabled ? "Dezactivează" : "Activează")))))),
    h("h2", {}, "Modele"),
    h("div", { class: "card" }, h("table", {}, (models.items || []).map((m) => h("tr", {}, h("td", {}, m.provider_slug, " / ", h("strong", {}, m.slug)), h("td", {}, pill(m.kind)),
      h("td", {}, pill(m.enabled ? "activ" : "inactiv", m.enabled ? "ok" : "")),
      h("td", {}, h("button", { onclick: () => guard(out, async () => { await api("PATCH", "/v1/admin/ai/models/" + m.id, { enabled: !m.enabled, version: m.version }); dispatch(); }) }, m.enabled ? "Dezactivează" : "Activează")))))),
    h("h2", {}, "Utilizare"),
    h("div", { class: "card" }, h("table", {}, h("tr", {}, h("th", {}, "Sarcină"), h("th", {}, "Model"), h("th", {}, "Apeluri"), h("th", {}, "Reușite"), h("th", {}, "p95 ms"), h("th", {}, "Cost $")),
      (usage.items || []).map((u) => h("tr", {}, h("td", {}, u.task), h("td", {}, u.model), h("td", {}, u.calls), h("td", {}, u.succeeded), h("td", {}, u.latency_p95_ms), h("td", {}, Number(u.cost_usd || 0).toFixed(4)))))));
});

route("/admin/review", async () => {
  const r = await GET("/v1/admin/fact-checks/queue");
  const out = h("div");
  render(h("h1", {}, "Coada de verificare"), h("p", { class: "muted" }, "Verdictele negative propuse automat așteaptă decizia unui editor; publicarea cere permisiunea separată factcheck:publish."), out,
    (r.items || []).length ? r.items.map((it) => {
      const verdict = h("select", { style: "width:auto" }, Object.keys(verdictRo).map((v) => h("option", { value: v }, verdictRo[v])));
      verdict.value = it.proposed_verdict || "unverified";
      const why = h("input", { placeholder: "motivare (obligatorie)" });
      const decide = (decision) => guard(out, async () => {
        await POST(`/v1/admin/fact-checks/${it.id}/decision`, { decision, verdict: verdict.value, rationale: why.value.trim() });
        put(out, notice("ok", "Decizie înregistrată: " + decision));
      });
      return h("div", { class: "card" }, h("div", { class: "row between" }, h("span", {}, "„", it.claim, "”"), h("span", { class: "row" }, pill("propus: " + (verdictRo[it.proposed_verdict] || it.proposed_verdict), "warn"), pill("risc " + it.risk_level))),
        h("p", { class: "small muted" }, it.reason, " · prioritate ", Number(it.priority_score).toFixed(2)),
        h("div", { class: "row" }, verdict, why),
        h("div", { class: "row", style: "margin-top:.4rem" }, h("button", { onclick: () => decide("accept") }, "Acceptă"), h("button", { onclick: () => decide("modify") }, "Modifică"),
          h("button", { onclick: () => decide("reject") }, "Respinge"), h("button", { onclick: () => decide("request_more_evidence") }, "Cere dovezi"),
          h("button", { class: "primary", onclick: () => guard(out, async () => { await POST(`/v1/admin/fact-checks/${it.id}/publish`); put(out, notice("ok", "Publicat.")); }) }, "Publică")));
    }) : empty("Coada este goală."));
});

route("/admin/jobs", async () => {
  const [jobs, dl] = await Promise.all([GET("/v1/admin/jobs?limit=50"), GET("/v1/admin/dead-letters?limit=50")]);
  const out = h("div");
  const kind = { succeeded: "ok", failed: "bad", quarantined: "bad", processing: "info", queued: "", cancelled: "" };
  render(h("h1", {}, "Joburi și dead letters"), out,
    h("h2", {}, "Joburi durabile"),
    h("div", { class: "card" }, h("table", {}, h("tr", {}, h("th", {}, "Tip"), h("th", {}, "Status"), h("th", {}, "Încercări"), h("th", {}, "Creat"), h("th", {}, "")),
      (jobs.items || []).map((j) => h("tr", {}, h("td", {}, j.kind, j.last_error ? h("div", { class: "small muted" }, j.last_error.slice(0, 160)) : null), h("td", {}, pill(j.status, kind[j.status])),
        h("td", {}, j.attempts + "/" + j.max_attempts), h("td", { class: "small" }, fmtDate(j.created_at)),
        h("td", {}, ["failed", "quarantined", "cancelled"].includes(j.status) ? h("button", { class: "link", onclick: () => guard(out, async () => { await POST(`/v1/admin/jobs/${j.id}/retry`); dispatch(); }) }, "reîncearcă")
          : ["queued", "processing"].includes(j.status) ? h("button", { class: "link", onclick: () => guard(out, async () => { await POST(`/v1/admin/jobs/${j.id}/cancel`); dispatch(); }) }, "anulează") : null))))),
    h("h2", {}, "Mesaje eșuate (dead letters)"),
    (dl.items || []).length ? h("div", { class: "card" }, h("table", {}, dl.items.map((d) => h("tr", {}, h("td", {}, d.topic || d.event_type, h("div", { class: "small muted" }, d.consumer, " · ", (d.error || "").slice(0, 160))),
      h("td", { class: "small" }, fmtDate(d.created_at)), h("td", {}, d.replayed_at ? pill("reluat", "ok") : h("button", { class: "link", onclick: () => guard(out, async () => { await POST(`/v1/admin/dead-letters/${d.id}/replay`); dispatch(); }) }, "reia")))))) : empty("Niciun mesaj eșuat."));
});

// ---------- about ----------
route("/about", async () => {
  const ready = await guard(null, () => api("GET", "/readyz", undefined, { auth: false }).catch((e) => e.problem));
  render(h("h1", {}, "Despre API"),
    h("div", { class: "card stack" },
      h("p", {}, "Consola folosește exact API-ul public pe care îl vor folosi aplicațiile iOS și Android. Deschide „Jurnal API” (sus) ca să vezi fiecare cerere și răspuns JSON."),
      h("p", {}, "Contractul complet: ", h("a", { href: "/openapi.yaml", target: "_blank" }, "/openapi.yaml"), " (OpenAPI 3.1 — se poate genera client Swift/Kotlin din el)."),
      h("p", {}, "Ghid pentru aplicațiile mobile: docs/mobile-integration.md în repository."),
      h("p", {}, "Autentificare: Bearer JWT (15 minute) + refresh token rotativ; la 401 consola reîmprospătează automat o dată, ca o aplicație mobilă.")),
    h("h2", {}, "Starea serviciilor (/readyz)"), json(ready));
}, { auth: false });

// ---------- boot ----------
async function loadMe() {
  try { state.me = await GET("/v1/users/me"); } catch { state.me = null; }
  renderChrome();
}
function boot() {
  const langSel = $("#lang");
  langSel.value = state.lang;
  langSel.addEventListener("change", () => { state.lang = langSel.value; store.set("lang", state.lang); dispatch(); });
  $("#toggle-log").addEventListener("click", () => { state.logOpen = !state.logOpen; store.set("logOpen", state.logOpen); renderChrome(); });
  $("#logout").addEventListener("click", () => logout(false));
  $("#menu-toggle").addEventListener("click", () => $("#nav").classList.toggle("open"));
  window.addEventListener("hashchange", dispatch);
  renderChrome();
  (state.tokens ? loadMe() : Promise.resolve()).then(dispatch);
}
boot();
