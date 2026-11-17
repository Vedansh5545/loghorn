// loghorn web query view. Every piece of log data reaches the DOM through
// textContent or attributes, never innerHTML.

const $ = (id) => document.getElementById(id);

const DEFAULT_FROM = "this-run";

// state is the query, in the server's Query JSON shape.
export const state = {
  stream: "",
  range: { from: DEFAULT_FROM, to: "now" },
  filters: [],
  search: "",
  columns: [],
  sort: { field: "", desc: true },
  limit: 0,
};

let session = null;
let streams = [];
let lastResult = null;

// el builds an element; text, when given, is set with textContent.
function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function showError(msg) {
  const b = $("error");
  b.textContent = msg;
  b.hidden = false;
}

function clearError() {
  $("error").hidden = true;
  $("error").textContent = "";
}

async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch(path, opts);
  let data = null;
  try {
    data = await resp.json();
  } catch {
    // fall through with null
  }
  if (!resp.ok) {
    const msg = data && data.error ? data.error : resp.status + " " + resp.statusText;
    throw new Error(method + " " + path + ": " + msg);
  }
  return data;
}

// ---- visualizers ----

const pad = (n, w = 2) => String(n).padStart(w, "0");

function sameLocalDate(a, b) {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

function localTime(d) {
  const t = pad(d.getHours()) + ":" + pad(d.getMinutes()) + ":" + pad(d.getSeconds()) + "." + pad(d.getMilliseconds(), 3);
  if (sameLocalDate(d, new Date())) return t;
  return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + " " + t;
}

function toDate(v) {
  if (v instanceof Date) return v;
  if (typeof v === "number") return new Date(v);
  if (typeof v === "string") return new Date(v);
  return null;
}

// formatDuration takes seconds.
function formatDuration(sec) {
  if (Math.abs(sec) < 1) {
    const ms = Math.round(sec * 10000) / 10; // ms, at most one decimal
    return ms + " ms";
  }
  return sec.toFixed(2) + " s";
}

// groupDigits groups an integer string exactly, without going through Number.
function groupDigits(s) {
  const m = /^(-?)(\d+)$/.exec(s);
  if (!m) return s;
  return m[1] + m[2].replace(/\B(?=(\d{3})+(?!\d))/g, ",");
}

const SEV_NAMES = {
  DEFAULT: "debug", DEBUG: "debug", TRACE: "debug",
  INFO: "info",
  NOTICE: "notice",
  WARN: "warning", WARNING: "warning",
  ERROR: "error", CRITICAL: "error", ALERT: "error", EMERGENCY: "error", FATAL: "error",
};

function severityClass(v) {
  if (typeof v === "number") {
    if (v <= 60) return v >= 50 ? "error" : v >= 40 ? "warning" : v >= 30 ? "info" : "debug"; // pino levels
    return v >= 500 ? "error" : v >= 400 ? "warning" : v >= 300 ? "notice" : v >= 200 ? "info" : "debug"; // LogEntry
  }
  return SEV_NAMES[String(v).toUpperCase()] || "debug";
}

function truncated(s, n) {
  const e = el("span", "", s.length > n ? s.slice(0, n - 1) + "…" : s);
  if (s.length > n) e.title = s;
  return e;
}

function asText(v) {
  if (v === null || v === undefined) return "";
  if (typeof v === "string") return v;
  return JSON.stringify(v);
}

// visualize renders one result cell ({v, bad}) of a column ({path, type,
// visualizer, ...}) as a DOM node.
export function visualize(cell, column) {
  const v = cell ? cell.v : undefined;
  if (cell && cell.bad) {
    const e = el("span", "bad", asText(v));
    e.title = "doesn't match type " + column.type;
    return e;
  }
  if (v === null || v === undefined) return el("span");
  switch (column.visualizer) {
    case "local-time": {
      const d = toDate(v);
      if (!d || isNaN(d.getTime())) return el("span", "", asText(v));
      const e = el("span", "", localTime(d));
      e.title = d.toISOString();
      return e;
    }
    case "duration":
      if (typeof v === "number") return el("span", "", formatDuration(v));
      return el("span", "", asText(v));
    case "number":
      if (typeof v === "number") return el("span", "", v.toLocaleString());
      return el("span", "", groupDigits(String(v)));
    case "severity":
      return el("span", "sev sev-" + severityClass(v), asText(v));
    case "json": {
      let s = v;
      if (typeof v === "string") {
        try {
          s = JSON.stringify(JSON.parse(v));
        } catch {
          s = v;
        }
      } else {
        s = JSON.stringify(v);
      }
      return truncated(s, 80);
    }
    case "sql":
    case "raw":
    default:
      return truncated(asText(v), 120);
  }
}

// ---- URL state ----

function saveURL() {
  const q = "?q=" + encodeURIComponent(JSON.stringify(state));
  if (location.search !== q) history.replaceState(null, "", q);
}

function loadURL() {
  const raw = new URLSearchParams(location.search).get("q");
  if (!raw) return;
  try {
    const q = JSON.parse(raw);
    if (q && typeof q === "object") {
      Object.assign(state, q);
      state.range = Object.assign({ from: DEFAULT_FROM, to: "now" }, q.range || {});
      state.sort = Object.assign({ field: "", desc: true }, q.sort || {});
      state.filters = Array.isArray(q.filters) ? q.filters : [];
      state.columns = Array.isArray(q.columns) ? q.columns : [];
    }
  } catch {
    showError("ignoring an unreadable query in the URL");
  }
}

// ---- query bar ----

// localInputValue formats a time for a datetime-local input.
function localInputValue(iso) {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + "T" +
    pad(d.getHours()) + ":" + pad(d.getMinutes()) + ":" + pad(d.getSeconds()) + "." + pad(d.getMilliseconds(), 3);
}

function syncRangeControl(which, value, presets) {
  const sel = $(which);
  const custom = $(which + "-custom");
  if (presets.includes(value)) {
    sel.value = value;
    custom.hidden = true;
  } else {
    sel.value = "custom";
    custom.hidden = false;
    custom.value = localInputValue(value);
  }
}

const FROM_PRESETS = ["this-run", "15m", "1h", "24h", "all"];
const TO_PRESETS = ["now"];

function syncBar() {
  $("stream").value = state.stream;
  syncRangeControl("from", state.range.from || DEFAULT_FROM, FROM_PRESETS);
  syncRangeControl("to", state.range.to || "now", TO_PRESETS);
  $("search").value = state.search;
}

function rangeChanged(which) {
  const sel = $(which);
  const custom = $(which + "-custom");
  if (sel.value !== "custom") {
    custom.hidden = true;
    state.range[which] = sel.value;
    runQuery();
    return;
  }
  custom.hidden = false;
  if (!custom.value) {
    const seed = which === "from" && lastResult && lastResult.from && !lastResult.from.startsWith("0001-")
      ? lastResult.from : new Date().toISOString();
    custom.value = localInputValue(seed);
  }
  customChanged(which);
}

function customChanged(which) {
  const v = $(which + "-custom").value;
  if (!v) return;
  const d = new Date(v); // datetime-local is local time
  if (isNaN(d.getTime())) return;
  state.range[which] = d.toISOString();
  runQuery();
}

function wireBar() {
  $("bar").addEventListener("submit", (e) => e.preventDefault());
  $("stream").addEventListener("change", () => {
    state.stream = $("stream").value;
    state.columns = [];
    state.filters = [];
    state.sort = { field: "", desc: true };
    runQuery();
  });
  $("from").addEventListener("change", () => rangeChanged("from"));
  $("to").addEventListener("change", () => rangeChanged("to"));
  $("from-custom").addEventListener("change", () => customChanged("from"));
  $("to-custom").addEventListener("change", () => customChanged("to"));
  $("search").addEventListener("keydown", (e) => {
    if (e.key !== "Enter") return;
    e.preventDefault();
    state.search = $("search").value;
    runQuery();
  });
  $("refresh").addEventListener("click", () => refresh());
}

// ---- header ----

function renderHeader() {
  if (!session) return;
  const parts = ["loghorn"];
  let mode = session.mode === "live" ? "live run" : session.mode;
  if (session.runStart) mode += " since " + localTime(new Date(session.runStart));
  parts.push(mode);
  parts.push(session.count.toLocaleString() + " records");
  $("title").textContent = parts.join(" · ");
}

function renderStreams() {
  const sel = $("stream");
  sel.replaceChildren();
  for (const s of streams) {
    const o = el("option", "", s.name + " (" + s.count.toLocaleString() + ")");
    o.value = s.name;
    sel.append(o);
  }
  const none = streams.length === 0;
  $("empty").hidden = !none;
  $("results").hidden = none;
  $("footer").hidden = none;
}

async function loadSession() {
  [session, streams] = await Promise.all([api("GET", "/api/session"), api("GET", "/api/streams")]);
  renderHeader();
  renderStreams();
  if (streams.length && !streams.some((s) => s.name === state.stream)) {
    state.stream = streams[0].name;
    state.columns = [];
    state.filters = [];
  }
}

// ---- results ----

const NUMERIC = new Set(["int", "float", "duration"]);

function cellNode(row, cell, col) {
  if (col.path === "timestamp" && row.timeInherited && (!cell || cell.v === null || cell.v === undefined)) {
    const d = new Date(row.time);
    const e = el("span", "inherited", localTime(d));
    e.title = "no timestamp on this record; time inherited (" + d.toISOString() + ")";
    return e;
  }
  return visualize(cell, col);
}

export function render(result) {
  lastResult = result;
  const head = $("head");
  head.replaceChildren();
  for (const c of result.columns) {
    const th = el("th", "", c.label || c.path);
    th.title = c.path + " : " + c.type;
    head.append(th);
  }
  const body = $("body");
  body.replaceChildren();
  for (const row of result.rows) {
    const tr = el("tr", "row");
    tr.dataset.seq = String(row.seq);
    result.columns.forEach((c, i) => {
      const td = el("td", NUMERIC.has(c.type) ? "num" : "");
      td.append(cellNode(row, row.cells[i], c));
      tr.append(td);
    });
    tr.addEventListener("click", () => toggleDetail(tr, result.columns.length));
    body.append(tr);
  }
  let foot = result.matched.toLocaleString() + " of " + result.streamTotal.toLocaleString() + " " +
    state.stream + " records match · showing " + result.rows.length.toLocaleString();
  if (result.truncated) foot += " · limit reached";
  $("footer").textContent = foot;
}

async function toggleDetail(tr, span) {
  const next = tr.nextElementSibling;
  if (next && next.classList.contains("detail")) {
    next.remove();
    tr.classList.remove("open");
    return;
  }
  tr.classList.add("open");
  const d = el("tr", "detail");
  const td = el("td");
  td.colSpan = span;
  td.append(el("span", "", "loading…"));
  d.append(td);
  tr.after(d);
  try {
    const rec = await api("GET", "/api/record?seq=" + encodeURIComponent(tr.dataset.seq));
    td.replaceChildren();
    for (const path of Object.keys(rec.sql || {}).sort()) {
      td.append(el("h3", "", path + " (SQL)"), el("pre", "", rec.sql[path]));
    }
    td.append(el("h3", "", "record"), el("pre", "", rec.raw));
  } catch (err) {
    td.replaceChildren(el("span", "bad", err.message));
  }
}

let querySeq = 0;

export async function runQuery() {
  saveURL();
  if (!state.stream) return;
  const mine = ++querySeq;
  try {
    const result = await api("POST", "/api/query", state);
    if (mine !== querySeq) return; // a newer query superseded this one
    clearError();
    render(result);
  } catch (err) {
    if (mine === querySeq) showError(err.message);
  }
}

async function refresh() {
  try {
    await loadSession();
    syncBar();
  } catch (err) {
    showError(err.message);
    return;
  }
  runQuery();
}

async function init() {
  loadURL();
  wireBar();
  await refresh();
}

init();
