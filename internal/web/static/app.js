// loghorn web query view. Every piece of log data reaches the DOM through
// textContent or attributes, never innerHTML.

import { drawGraph } from "./graph.js";
import { renderHeatmap, cellTime, bandLabel } from "./heatmap.js";
import { makeSplit } from "./split.js";
import { initTrace, openTrace, restoreTraceFromURL, traceURLParams } from "./trace.js";
import { copyButton, copyTableBar } from "./copy.js";
import { aggRangeChanged, aggURLParams, cancelAggregate, initAggregate, openAggregate, restoreAggregateFromURL } from "./agg.js";
import { initNav } from "./nav.js";

const $ = (id) => document.getElementById(id);

const DEFAULT_FROM = "this-run";

// state is the query, in the server's Query JSON shape.
export const state = {
  stream: "",
  source: "log", // the mode: "log" (Logs) or "otel" (Spans); `stream` is one of its kinds
  service: "", // the service key, "*" for all; "" until loadSession chooses one
  range: { from: DEFAULT_FROM, to: "now" },
  filters: [],
  search: "",
  columns: [],
  sort: { field: "", desc: true },
  limit: 0,
  view: "samples",
  groupBy: [],
  aggregates: [],
  plot: { op: "count" },
  order: { op: "count" },
  chart: "line",
  top: 10,
  bucket: "auto",
};

const VIEWS = ["samples", "table", "graph", "heatmap"];
const CHARTS = ["line", "stacked"];
const DEFAULT_TOP = 10;
const TOPS = [5, 10, 20, 50, 100, 500, 1000];
// DEFAULT_LIMIT and MAX_LIMIT match the server's samples row limits; a limit
// of 0 in state means the default.
const DEFAULT_LIMIT = 100;
const MAX_LIMIT = 10000;
const MAX_GRAPH_TOP = 50; // the server draws at most this many series

// stackable: only metrics that add up across groups can be stacked; a stack
// of averages or percentiles has a meaningless total.
const stackable = (plot) => !plot || plot.op === "count" || plot.op === "sum";

let session = null;
let kinds = []; // last /api/kinds answer: [{name, count}] of state.service, by count
let services = []; // [{key, count}] of every service in the interval
let kindsOpen = false; // the kind list's More is expanded
let started = false; // the first refresh has checked the URL's kind

const ALL_SERVICES = "*";
export const ALL_KINDS = "*"; // the stream of every span kind at once (Spans mode only)
const SERVICE_KEY = "loghorn.service"; // localStorage: the last service chosen
const SIDEBAR_KEY = "loghorn.sidebar"; // localStorage: "closed" when the controls are hidden
const TOP_KINDS = 10;
let lastResult = null;
let lastResultStream = ""; // the streamKey() lastResult belongs to
let schemaData = null; // {schema, ops} of state.stream

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

// The page shows a wait cursor while a server call is out (stream, service
// and view changes, queries, traces). It appears after BUSY_DELAY_MS so a
// quick call doesn't flicker, and clears only after the caller has drawn the
// answer: the count drops in a later task, behind the caller's continuation.
const BUSY_DELAY_MS = 150;
let busyCount = 0;
let busyTimer = 0;

function busyStart() {
  if (busyCount++ === 0) busyTimer = setTimeout(() => document.documentElement.classList.add("busy"), BUSY_DELAY_MS);
}

function busyEnd() {
  setTimeout(() => {
    if (--busyCount > 0) return;
    clearTimeout(busyTimer);
    document.documentElement.classList.remove("busy");
  }, 0);
}

async function api(method, path, body) {
  busyStart();
  try {
    return await apiCall(method, path, body);
  } finally {
    busyEnd();
  }
}

async function apiCall(method, path, body) {
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
    const err = new Error(method + " " + path + ": " + msg);
    err.status = resp.status;
    err.detail = msg;
    throw err;
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

const DURATION_SCALE = { s: 1, ms: 1e3, us: 1e6, ns: 1e9 };

// formatDuration takes seconds. With a display unit (s|ms|us|ns) it prints in
// that unit; otherwise it picks ms below one second and s above.
export function formatDuration(sec, unit) {
  const scale = DURATION_SCALE[unit];
  if (scale) {
    const n = Math.round(sec * scale * 100) / 100; // at most two decimals
    return n.toLocaleString() + " " + unit;
  }
  const ms = Math.round(sec * 10000) / 10; // ms, at most one decimal
  if (Math.abs(ms) < 1000) return ms + " ms";
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

// URL_RE finds http(s) URLs; it stops at whitespace and at characters that
// cannot belong to a URL in prose or markup.
const URL_RE = /https?:\/\/[^\s<>"'`]+/g;
const URL_TRAIL = /[).,\]\u201d;:!?]+$/;

// safeURL returns href when it parses as an http(s) URL, else "".
function safeURL(href) {
  try {
    const u = new URL(href);
    return u.protocol === "http:" || u.protocol === "https:" ? u.href : "";
  } catch {
    return "";
  }
}

function linkNode(href, shown) {
  const a = el("a", "", shown);
  a.href = href;
  a.target = "_blank";
  a.rel = "noopener noreferrer";
  a.addEventListener("click", (e) => e.stopPropagation());
  return a;
}

// linkify returns a <span> of text nodes and <a>s for the http(s) URLs in
// text. With max, only the first max characters are shown (ending in an
// ellipsis, full text in the title); a clipped URL still links to its full
// address. Log data only ever reaches the DOM as text or as an href that
// safeURL accepted.
export function linkify(text, max) {
  const out = el("span");
  let budget = max === undefined ? Infinity : max;
  const clip = max !== undefined && text.length > max;
  if (clip) budget = max - 1;
  const add = (node, len) => {
    if (budget <= 0) return;
    out.append(node);
    budget -= len;
  };
  const piece = (str, href) => {
    if (budget <= 0) return;
    const shown = str.length > budget ? str.slice(0, budget) : str;
    add(href ? linkNode(href, shown) : document.createTextNode(shown), shown.length);
  };
  let pos = 0;
  for (const m of text.matchAll(URL_RE)) {
    const url = m[0].replace(URL_TRAIL, "");
    const href = url ? safeURL(url) : "";
    if (!href) continue;
    piece(text.slice(pos, m.index));
    piece(url, href);
    pos = m.index + url.length;
  }
  piece(text.slice(pos));
  if (clip) {
    out.append(document.createTextNode("…"));
    out.title = text;
    out.dataset.full = text;
  }
  return out;
}

// sqlSpan renders SQL colour tokens ({c: class, t: text}) as a <span>, each
// classed token in a span of class sql-<c>. With max, it clips like linkify:
// the first max characters, an ellipsis, and the full text as the title.
export function sqlSpan(tokens, text, max) {
  const out = el("span", "sql");
  const clip = max !== undefined && text.length > max;
  let budget = clip ? max - 1 : Infinity;
  for (const tk of tokens) {
    if (budget <= 0) break;
    const t = tk.t.length > budget ? tk.t.slice(0, budget) : tk.t;
    budget -= t.length;
    out.append(tk.c ? el("span", "sql-" + tk.c, t) : document.createTextNode(t));
  }
  if (clip) {
    out.append(document.createTextNode("…"));
    out.title = text;
    out.dataset.full = text;
  }
  return out;
}

// WRAP_MAX is the cell clip for a column set to wrap: wrapping is for
// reading long values, but one huge value should not fill the page.
const WRAP_MAX = 2000;

function truncated(s, n) {
  return linkify(s, n);
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
  const wrap = !!(fieldByPath(column.path) || {}).wrap;
  const clip = (n) => (wrap ? WRAP_MAX : n);
  switch (column.visualizer) {
    case "local-time": {
      const d = toDate(v);
      if (!d || isNaN(d.getTime())) return el("span", "", asText(v));
      const e = el("span", "", localTime(d));
      e.title = d.toISOString();
      return e;
    }
    case "duration":
      if (typeof v === "number") return el("span", "", formatDuration(v, column.unit));
      return el("span", "", asText(v));
    case "plain":
      return el("span", "", String(v));
    case "link": {
      const text = asText(v);
      const href = /^https?:\/\//.test(text) ? safeURL(text) : "";
      if (!href) return truncated(text, clip(120));
      const n = clip(120);
      const a = linkNode(href, text.length > n ? text.slice(0, n - 1) + "…" : text);
      if (text.length > n) {
        a.title = text;
        a.dataset.full = text;
      }
      return a;
    }
    case "number":
      if (typeof v === "number") return el("span", "", v.toLocaleString());
      return el("span", "", groupDigits(String(v)));
    case "severity":
      return el("span", "sev sev-" + severityClass(v), asText(v));
    case "json":
      // The server sends JSON cells as text; never re-parse it, because
      // JSON.parse rounds integers above 2^53.
      return truncated(asText(v), clip(80));
    case "sql":
      if (cell && Array.isArray(cell.sql)) return sqlSpan(cell.sql, asText(v), clip(120));
    // falls through: a value the server did not colour shows as raw
    case "raw":
    default:
      return truncated(asText(v), clip(120));
  }
}

// ---- URL state ----

// streamKey identifies the current stream: the same name can exist as a log
// stream and as a span stream, and in several services.
function streamKey() {
  return state.service + "|" + state.source + ":" + state.stream;
}

// svcParam is the &service= query parameter of the selected service.
function svcParam() {
  return "&service=" + encodeURIComponent(state.service);
}

// withSvc appends the selected service to an endpoint path.
function withSvc(path) {
  return path.includes("?") ? path + svcParam() : path + "?" + svcParam().slice(1);
}

// schemaURL is the schema endpoint for one (stream, source) of the selected
// service.
function schemaURL(stream, source) {
  return "/api/schema?stream=" + encodeURIComponent(stream) + "&source=" + encodeURIComponent(source) + svcParam();
}

function saveURL() {
  // Columns are written as they are: empty means "inferred over the
  // interval".
  const full = Object.assign({}, state);
  // A missing source means log, so only otel is written.
  if (full.source !== "otel") delete full.source;
  if (!full.service) delete full.service;
  const q = "?q=" + encodeURIComponent(JSON.stringify(full)) + traceURLParams() + aggURLParams();
  // history.state marks an entry the trace or aggregate view pushed; keep it.
  if (location.search !== q) history.replaceState(history.state, "", q);
}

// applyQuery replaces state with q (from the URL or a favorite), filling in
// defaults for anything missing.
function applyQuery(q) {
  if (!q || typeof q !== "object") return;
  Object.assign(state, q);
  state.stream = typeof q.stream === "string" ? q.stream : "";
  state.source = q.source === "otel" ? "otel" : "log";
  state.service = typeof q.service === "string" ? q.service : "";
  state.search = typeof q.search === "string" ? q.search : "";
  state.range = Object.assign({ from: DEFAULT_FROM, to: "now" }, q.range || {});
  state.sort = Object.assign({ field: "", desc: true }, q.sort || {});
  state.filters = Array.isArray(q.filters) ? q.filters : [];
  state.columns = Array.isArray(q.columns) ? q.columns : [];
  state.view = VIEWS.includes(q.view) ? q.view : "samples";
  if (state.view === "heatmap" && state.source !== "otel") state.view = "samples"; // logs have no durations
  state.groupBy = Array.isArray(q.groupBy) ? q.groupBy : [];
  state.aggregates = Array.isArray(q.aggregates) ? q.aggregates : [];
  state.plot = q.plot && typeof q.plot === "object" && q.plot.op ? q.plot : { op: "count" };
  state.order = q.order && typeof q.order === "object" && q.order.op ? q.order : { op: "count" };
  state.bucket = typeof q.bucket === "string" && q.bucket ? q.bucket : "auto";
  state.chart = CHARTS.includes(q.chart) ? q.chart : "line";
  state.top = Number.isInteger(q.top) && q.top > 0 ? q.top : DEFAULT_TOP;
  state.limit = validLimit(q.limit) ? q.limit : 0;
  delete state.offset; // a batch offset is never part of a saved query
}

function loadURL() {
  const raw = new URLSearchParams(location.search).get("q");
  if (!raw) return;
  try {
    applyQuery(JSON.parse(raw));
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

// AGO matches a relative From such as 24m, 3.7h, 1d or 1d12h, as the
// server's query.ParseAgo reads it.
const AGO = /^\s*(?:(?:\d+\.?\d*|\.\d+)(?:ms|s|m|h|d|w))+\s*$/i;

function isAgo(v) {
  return typeof v === "string" && AGO.test(v) && /[1-9]/.test(v);
}

function syncRangeControl(which, value, presets) {
  const sel = $(which);
  const custom = $(which + "-custom");
  const ago = $(which + "-ago");
  if (ago) ago.hidden = true;
  if (presets.includes(value)) {
    sel.value = value;
    custom.hidden = true;
  } else if (ago && isAgo(value)) {
    sel.value = "ago";
    custom.hidden = true;
    ago.hidden = false;
    ago.value = value;
    ago.classList.remove("bad-input");
  } else {
    sel.value = "custom";
    custom.hidden = false;
    custom.value = localInputValue(value);
  }
}

const FROM_PRESETS = ["this-run", "15m", "1h", "24h", "all"];
const TO_PRESETS = ["now"];

function syncBar() {
  syncMode();
  renderServices();
  renderKindList();
  syncRangeControl("from", state.range.from || DEFAULT_FROM, FROM_PRESETS);
  syncRangeControl("to", state.range.to || "now", TO_PRESETS);
  $("search").value = state.search;
}

function rangeChanged(which) {
  const sel = $(which);
  const custom = $(which + "-custom");
  const ago = $(which + "-ago");
  if (ago) ago.hidden = sel.value !== "ago";
  if (sel.value === "ago") {
    custom.hidden = true;
    if (!ago.value) ago.value = "1d";
    ago.focus();
    ago.select();
    agoChanged(which);
    return;
  }
  if (sel.value !== "custom") {
    custom.hidden = true;
    state.range[which] = sel.value;
    rangeApplied();
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

// agoChanged applies a relative From typed in its box; a value the server
// would refuse is marked and not run.
function agoChanged(which) {
  const box = $(which + "-ago");
  const v = box.value.trim().toLowerCase();
  const ok = isAgo(v);
  box.classList.toggle("bad-input", !ok);
  if (!ok || state.range[which] === v) return;
  box.value = v;
  state.range[which] = v;
  rangeApplied();
}

function customChanged(which) {
  const v = $(which + "-custom").value;
  if (!v) return;
  const d = new Date(v); // datetime-local is local time
  if (isNaN(d.getTime())) return;
  state.range[which] = d.toISOString();
  rangeApplied();
}

// rangeApplied runs what follows a new range: the kinds, the results and an
// open aggregate, which follows From and To. The aggregate goes after the
// results, so the results' clearError doesn't wipe its error.
function rangeApplied() {
  reloadKinds();
  Promise.resolve(runQuery()).finally(aggRangeChanged);
}

function wireBar() {
  $("bar").addEventListener("submit", (e) => e.preventDefault());
  for (const b of document.querySelectorAll("#sb-mode [data-source]")) {
    b.addEventListener("click", () => setMode(b.dataset.source));
  }
  $("service").addEventListener("change", () => setService($("service").value));
  $("kind-more").addEventListener("click", () => {
    kindsOpen = !kindsOpen;
    if (!kindsOpen) $("kind-filter").value = "";
    renderKindList();
    if (kindsOpen) $("kind-filter").focus();
  });
  $("kind-filter").addEventListener("input", () => renderKindList());
  $("kind-filter").addEventListener("keydown", (e) => {
    if (e.key !== "Escape") return;
    e.preventDefault();
    $("kind-filter").value = "";
    renderKindList();
  });
  $("from").addEventListener("change", () => rangeChanged("from"));
  $("to").addEventListener("change", () => rangeChanged("to"));
  $("from-custom").addEventListener("change", () => customChanged("from"));
  $("from-ago").addEventListener("change", () => agoChanged("from"));
  $("from-ago").addEventListener("input", () => $("from-ago").classList.toggle("bad-input", !!$("from-ago").value.trim() && !isAgo($("from-ago").value)));
  $("to-custom").addEventListener("change", () => customChanged("to"));
  $("search").addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
      // Esc clears the box and the search at once; only Chrome and Safari
      // clear a search input on Esc themselves.
      e.preventDefault();
      $("search").value = "";
      if (state.search) {
        state.search = "";
        runQuery();
      }
      return;
    }
    if (e.key !== "Enter") return;
    e.preventDefault();
    state.search = $("search").value;
    runQuery();
  });
  // The box's own clear (×) button empties it without a key press.
  $("search").addEventListener("input", () => {
    if ($("search").value === "" && state.search) {
      state.search = "";
      runQuery();
    }
  });
  $("refresh").addEventListener("click", () => refresh());
  for (const b of document.querySelectorAll("#sb-view [data-view]")) {
    b.addEventListener("click", () => setView(b.dataset.view));
  }
}

// ---- view tabs ----

// syncView marks the current tab and shows only the sidebar sections whose
// data-views list the current view.
function syncView() {
  for (const b of document.querySelectorAll("#sb-view [data-view]")) {
    const on = b.dataset.view === state.view;
    b.classList.toggle("active", on);
    b.setAttribute("aria-selected", String(on));
  }
  for (const s of document.querySelectorAll("[data-views]")) {
    s.hidden = !s.dataset.views.split(" ").includes(state.view);
  }
  $("table").hidden = state.view === "graph";
  $("graph").hidden = state.view !== "graph";
  $("heatmap").hidden = state.view !== "heatmap";
  document.querySelector('#sb-view [data-view="heatmap"]').hidden = state.source !== "otel";
}

function setView(v) {
  if (!VIEWS.includes(v) || v === state.view) return;
  if (v === "heatmap" && state.source !== "otel") return;
  if (state.view === "heatmap") forgetHeatSelection(); // coming back starts afresh
  state.view = v;
  syncView();
  if (v === "heatmap" && state.stream !== ALL_KINDS) {
    enterAllKinds();
    return;
  }
  runQuery();
}

// enterAllKinds selects the All kinds stream on entering the Heatmap view.
// Unlike selectKind it keeps the filters, columns and sort: the All kinds
// schema has every kind's fields.
async function enterAllKinds() {
  state.stream = ALL_KINDS;
  closeColumnMenu();
  hiddenThisQuery = [];
  tempWidths = {};
  renderKindList();
  if (!(await loadSchema())) return; // a newer selection took over
  rebuildExplore();
  runQuery();
}

// ---- heatmap view ----

let heatSplit = null;

// initHeatSplit wires the bar between the chart and the span list below it.
function initHeatSplit() {
  heatSplit = makeSplit({
    bar: $("heat-split"),
    pane: $("heat-chart"),
    container: document.querySelector(".content"),
    storageKey: "loghorn.heatSplit",
    initial: 0.55,
    min: 80,
  });
  // Showing the view, the window, the sidebar toggle or the error banner can
  // move the pane.
  const layout = () => {
    if (state.view === "heatmap") heatSplit.layout();
  };
  new ResizeObserver(layout).observe($("heatmap"));
  new ResizeObserver(layout).observe(document.querySelector(".content"));
}

// ---- heatmap cell selection and the list below the chart ----
// None of this is saved in the URL or a favorite.

let heatData = null; // the heatmap drawn, or null
let heatFrom = ""; // the drawn result's resolved range start (RFC 3339)
let heatKeyDrawn = ""; // heatKey() of the drawn heatmap
let heatSel = null; // the selected cell {ti, bi} of heatData, or null
let heatSelCell = null; // heatSel's {start (ms), lo, hi}, to find it in a new result
let heatKeep = false; // the next heatmap query keeps the selection (a Grouped name click)
let heatTab = "spans"; // the list under the chart: "spans" or "grouped"
let listSeq = 0; // the list load in progress; an older answer is dropped

const heatCount = (h, ti, bi) => ((h.counts && h.counts[ti]) || [])[bi] || 0;

// heatKey is what decides the heatmap's cells: a query that changes any of
// it (range, filters, search, bucket, stream, service) clears the selection.
function heatKey() {
  const { source, service, stream, range, filters, search, bucket } = state;
  return JSON.stringify({ source, service, stream, range, filters, search, bucket });
}

// findHeatCell is the {ti, bi} of h with bucket start and band edges c, or
// null when h has no such cell or it is empty.
function findHeatCell(h, c) {
  const ti = (h.starts || []).findIndex((s) => new Date(s).getTime() === c.start);
  const bi = (h.bands || []).findIndex((b) => b.lo === c.lo && b.hi === c.hi);
  if (ti < 0 || bi < 0 || !heatCount(h, ti, bi)) return null;
  return { ti, bi };
}

// heatCell is the query's `cell` for the selected cell, or undefined: the
// bucket [start, next start) with its start clipped to the range's (the first
// bucket can begin before it), and the band [lo, hi), with no lo for the
// lowest band, which also holds zero and negative durations.
function heatCell() {
  if (!heatSel || !heatData) return undefined;
  const h = heatData;
  const { ti, bi } = heatSel;
  const start = new Date(h.starts[ti]).getTime();
  const end = ti + 1 < h.starts.length ? new Date(h.starts[ti + 1]).getTime() : start + h.bucketSeconds * 1000;
  // The range start is sent as the server wrote it: a Date keeps only ms.
  const from = Date.parse(heatFrom) > start ? heatFrom : new Date(start).toISOString();
  const cell = { from, to: new Date(end).toISOString(), hi: h.bands[bi].hi };
  if (bi > 0) cell.lo = h.bands[bi].lo;
  return cell;
}

// drawHeat draws the heatmap with the selection, and the banner.
function drawHeat() {
  renderHeatmap($("heat-chart"), heatData, { onSelect: selectHeatCell, selected: heatSel });
  const sel = heatSel && heatData;
  $("heat-banner").hidden = !sel;
  $("heat-hint").hidden = !!sel || !heatData || !heatData.total;
  if (!sel) return;
  const { ti, bi } = heatSel;
  $("heat-banner-text").textContent = "Cell selected: " + cellTime(heatData, ti) + ", Spans: " +
    heatCount(heatData, ti, bi).toLocaleString() + ", Duration: " + bandLabel(heatData.bands, bi);
}

// selectHeatCell selects cell sel ({ti, bi}, or null for none) and lists it.
function selectHeatCell(sel) {
  heatSel = sel;
  heatSelCell = null;
  if (sel) {
    const b = heatData.bands[sel.bi];
    heatSelCell = { start: new Date(heatData.starts[sel.ti]).getTime(), lo: b.lo, hi: b.hi };
  }
  drawHeat();
  loadHeatList();
}

function clearHeatSelection() {
  if (heatSel) selectHeatCell(null);
}

// forgetHeatSelection drops the selection without drawing or querying.
function forgetHeatSelection() {
  heatSel = null;
  heatSelCell = null;
  heatKeep = false;
}

function syncHeatTabs() {
  for (const b of document.querySelectorAll("#heat-tabs [data-tab]")) {
    const on = b.dataset.tab === heatTab;
    b.classList.toggle("active", on);
    b.setAttribute("aria-selected", String(on));
  }
}

function setHeatTab(t) {
  if (t === heatTab) return;
  heatTab = t;
  closeColumnMenu();
  syncHeatTabs();
  loadHeatList();
}

// groupedList reports whether the list shows the Grouped tab's table.
const groupedList = () => state.view === "heatmap" && heatTab === "grouped";

// GROUPED_BY is the Grouped tab's fixed grouping.
const GROUPED_BY = ["name", "service.name"];

// request is the body sent for q. Samples and Heatmap use none of the Table
// and Graph settings, so they are left out: the server still checks each
// metric's field, and under All kinds a field numeric in one kind but text in
// another is text, so a leftover metric would fail the query.
function request(q) {
  if (q.view !== "samples" && q.view !== "heatmap") return q;
  const { groupBy, aggregates, plot, order, top, ...rest } = q;
  return rest;
}

// listQuery is the request of the rows below: in the Heatmap view, the
// active tab's list of the selected cell (or the whole heatmap); the Spans
// tab longest first unless the user sorted. The Grouped tab's grouping and
// metrics are its own and never enter state.
function listQuery() {
  if (state.view !== "heatmap") return request(Object.assign({}, state));
  const cell = heatCell();
  if (heatTab === "grouped") {
    return Object.assign({}, state, {
      view: "table",
      groupBy: GROUPED_BY.slice(),
      aggregates: ["p50", "p90", "p95"].map((op) => ({ op, field: "duration_ms" })).concat([{ op: "error_rate" }]),
      order: { op: "p90", field: "duration_ms" },
      plot: { op: "count" },
      top: state.limit || DEFAULT_LIMIT,
      cell,
    });
  }
  return request(Object.assign({}, state, { view: "samples", sort: listSort(), cell }));
}

// loadHeatList runs the active tab's list query. A newer list load (another
// cell, tab or heatmap) or a newer query drops its answer.
async function loadHeatList() {
  const mine = ++listSeq;
  moreSeq++; // a Load next on its way belongs to the old list
  const q0 = querySeq;
  const live = () => mine === listSeq && q0 === querySeq;
  const grouped = heatTab === "grouped";
  try {
    const r = await api("POST", "/api/query", listQuery());
    if (!live()) return;
    clearError();
    renderResult(r);
    if (grouped) linkGroupedNames(r);
  } catch (err) {
    if (live()) showError(err.message);
  }
}

// linkGroupedNames makes each Grouped row's name a button that lists the
// row's spans in the Spans tab.
function linkGroupedNames(result) {
  const ni = result.columns.findIndex((c) => c.group && c.path === "name");
  const si = result.columns.findIndex((c) => c.group && c.path === "service.name");
  if (ni < 0) return;
  const trs = $("body").children;
  result.rows.forEach((row, i) => {
    const name = row.cells[ni] && row.cells[ni].v;
    const svc = si >= 0 && row.cells[si] ? row.cells[si].v : null;
    const td = trs[i] && trs[i].children[ni];
    if (typeof name !== "string" || !name || !td) return;
    const b = el("button", "heat-name", name);
    b.type = "button";
    b.title = "list the " + name + " spans" + (heatSel ? " of this cell" : "");
    b.addEventListener("click", () => narrowToGroup(name, typeof svc === "string" ? svc : ""));
    if (td.firstChild) td.firstChild.replaceWith(b); // keep the cell's copy button
    else td.append(b);
  });
}

// narrowToGroup adds the filter name = <name> (and service.name = <svc>
// when the service is not already narrowed) and lists the spans, keeping
// the cell selection.
function narrowToGroup(name, svc) {
  const has = (field, value) => state.filters.some((f) => f.field === field && f.op === "=" &&
    Array.isArray(f.values) && f.values.length === 1 && String(f.values[0]) === value);
  const add = [];
  if (!has("name", name)) add.push({ field: "name", op: "=", values: [name] });
  const narrowed = state.service !== ALL_SERVICES || state.filters.some((f) => f.field === "service.name");
  if (svc && !narrowed) add.push({ field: "service.name", op: "=", values: [svc] });
  state.filters = state.filters.concat(add);
  rebuildTriples();
  heatTab = "spans";
  syncHeatTabs();
  heatKeep = !!heatSel;
  runQuery();
}

// wireHeatList wires the tabs, Clear selection and Esc.
function wireHeatList() {
  for (const b of document.querySelectorAll("#heat-tabs [data-tab]")) {
    b.addEventListener("mousedown", (e) => e.preventDefault()); // keep a filter box's blur from redrawing first
    b.addEventListener("click", () => setHeatTab(b.dataset.tab));
  }
  $("heat-clear").addEventListener("mousedown", (e) => e.preventDefault());
  $("heat-clear").addEventListener("click", clearHeatSelection);
  syncHeatTabs();
  // Capture, so this sees a menu still open before its own Esc closes it.
  document.addEventListener("keydown", (e) => {
    if (e.key !== "Escape" || !heatSel || state.view !== "heatmap") return;
    // An open menu, pane or dialog takes the Esc for itself. Capture runs
    // first, so a new menu or pane must be added to this check.
    if (menuPath || !$("fav-menu").hidden || !$("trace").hidden || !$("agg").hidden || document.querySelector("dialog[open]")) return;
    if (e.target instanceof Element && e.target.closest("input, textarea, select, [contenteditable]")) return;
    clearHeatSelection();
  }, true);
}

// HEAT_SORT is the Heatmap list's order when the user chose none.
const HEAT_SORT = { field: "duration_ms", desc: true };

// listSort is the order the samples rows are in: the user's, else the
// view's default (the server's timestamp order, or longest first under
// the heatmap).
function listSort() {
  if (state.sort.field) return state.sort;
  return state.view === "heatmap" ? HEAT_SORT : { field: "timestamp", desc: true };
}

// renderHeatmapResult draws a heatmap result (of the query with heatKey key)
// in the chart pane. The selection carries over, found again by its bucket
// start and band, when keep is set or the cells' query is unchanged (a
// refresh, a sort or column change); otherwise the new result clears it.
function renderHeatmapResult(result, key, keep) {
  const h = result.heatmap || {};
  autoBucket = state.bucket === "auto" ? h.bucket || "" : "";
  drawBucket();
  const carry = (keep || key === heatKeyDrawn) && heatSelCell;
  heatData = h;
  heatFrom = result.from || "";
  heatKeyDrawn = key;
  heatSel = carry ? findHeatCell(h, heatSelCell) : null;
  if (!heatSel) heatSelCell = null;
  drawHeat();
}

// runHeatmapQuery runs the heatmap, then the list of its selected cell (the
// selection depends on the new heatmap), as query mine; a newer query drops
// both answers.
async function runHeatmapQuery(mine) {
  const live = () => mine === querySeq;
  const keep = heatKeep;
  heatKeep = false;
  const key = heatKey();
  listSeq++; // a list answer on its way belongs to the old heatmap
  moreSeq++;
  try {
    const r = await api("POST", "/api/query", request(state));
    if (!live()) return;
    renderHeatmapResult(r, key, keep);
  } catch (err) {
    if (live()) showError(err.message);
    return;
  }
  await loadHeatList();
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

async function loadSession() {
  session = await api("GET", "/api/session");
  renderHeader();
  const none = session.count === 0;
  $("empty").hidden = !none;
  $("results").hidden = none;
  $("footer").hidden = none;
  await chooseService();
}

// ---- mode, service and kind list ----

function storedService() {
  try {
    return localStorage.getItem(SERVICE_KEY) || "";
  } catch {
    return "";
  }
}

function storeService(svc) {
  try {
    localStorage.setItem(SERVICE_KEY, svc);
  } catch {
    // the last service is a per-browser convenience only
  }
}

// kindsURL is the /api/kinds request of the mode and range for service svc.
function kindsURL(svc) {
  return "/api/kinds?source=" + encodeURIComponent(state.source) + "&service=" + encodeURIComponent(svc) +
    "&from=" + encodeURIComponent(state.range.from || DEFAULT_FROM) + "&to=" + encodeURIComponent(state.range.to || "now");
}

// chooseService picks the service when the URL named none: with exactly one
// service in the interval, that one; else the last one used in this browser;
// else the one with the most records. With no records and nothing stored the
// choice waits for the next refresh.
async function chooseService() {
  if (state.service) return;
  const data = await api("GET", kindsURL(ALL_SERVICES));
  const svcs = Array.isArray(data.services) ? data.services : [];
  if (svcs.length === 1) {
    state.service = svcs[0].key;
    return;
  }
  const top = svcs.reduce((a, b) => (!a || b.count > a.count ? b : a), null);
  state.service = storedService() || (top ? top.key : "");
}

let kindsSeq = 0;

// loadKinds fetches the kind and service counts of the mode, service and
// range, and redraws the service selector and the kind list. It returns false
// when a newer call superseded this one (its answer is dropped).
async function loadKinds() {
  const mine = ++kindsSeq;
  const data = await api("GET", kindsURL(state.service));
  if (mine !== kindsSeq) return false;
  kinds = Array.isArray(data.kinds) ? data.kinds : [];
  services = Array.isArray(data.services) ? data.services : [];
  try {
    await loadPins();
  } catch (err) {
    showError(err.message);
  }
  if (mine !== kindsSeq) return false;
  renderServices();
  renderKindList();
  return true;
}

// reloadKinds is loadKinds for callers that don't wait for it.
function reloadKinds() {
  loadKinds().catch((err) => showError(err.message));
}

function syncMode() {
  for (const b of document.querySelectorAll("#sb-mode [data-source]")) {
    const on = b.dataset.source === state.source;
    b.classList.toggle("active", on);
    b.setAttribute("aria-pressed", String(on));
  }
}

// renderServices lists All, then the services in the interval by count. A
// selected service with no records in the interval stays listed.
function renderServices() {
  const sel = $("service");
  sel.replaceChildren();
  const all = el("option", "", "All");
  all.value = ALL_SERVICES;
  sel.append(all);
  const byCount = services.slice().sort((a, b) => b.count - a.count || (a.key < b.key ? -1 : a.key > b.key ? 1 : 0));
  for (const s of byCount) {
    const o = el("option", "", s.key + " (" + s.count.toLocaleString() + ")");
    o.value = s.key;
    sel.append(o);
  }
  const cur = state.service || ALL_SERVICES;
  if (cur !== ALL_SERVICES && !services.some((s) => s.key === cur)) {
    const o = el("option", "", cur + " (none in range)");
    o.value = cur;
    sel.append(o);
  }
  sel.value = cur;
  syncReadOnly();
}

const NEED_SERVICE = "Choose a service to save";

// syncReadOnly disables the save controls under All and re-enables them for
// one service. (Pin toggles, the column menu and the favorites list read
// readOnly() as they are drawn.)
function syncReadOnly() {
  const ro = readOnly();
  for (const id of ["fav-save", "fav-save-menu", "schema-save"]) {
    const b = $(id);
    b.disabled = ro;
    if (ro) b.title = NEED_SERVICE;
    else b.removeAttribute("title");
  }
}

// readOnly is true under All: the union of services is shown but nothing is
// saved, because config is per service.
const readOnly = () => state.service === ALL_SERVICES;

// pins is every pin of the services on show, [{source, kind, service}]: the
// selected service's, or under All each present service's.
let pins = [];
// pinCounts is, under All, the kind counts of each pinned service, keyed
// service + "\n" + kind.
let pinCounts = new Map();
let pinsSeq = 0;

// loadPins loads the pins of the selected service, or under All of every
// service present in the interval, and (under All) the counts of the pinned
// kinds per service. A newer call supersedes an older one.
async function loadPins() {
  const mine = ++pinsSeq;
  const all = readOnly();
  const svcs = all ? services.map((s) => s.key) : [state.service];
  const got = await Promise.all(svcs.map(async (svc) => {
    const r = await api("GET", "/api/pins?service=" + encodeURIComponent(svc));
    return ((r && r.pins) || []).map((p) => ({ source: p.source, kind: p.kind, service: svc }));
  }));
  const counts = new Map();
  if (all) {
    const withPins = [...new Set(got.flat().filter((p) => p.source === state.source).map((p) => p.service))];
    await Promise.all(withPins.map(async (svc) => {
      const r = await api("GET", kindsURL(svc));
      for (const k of (r && r.kinds) || []) counts.set(svc + "\n" + k.name, k.count);
    }));
  }
  if (mine !== pinsSeq) return false;
  pins = got.flat();
  pinCounts = counts;
  return true;
}

// pinnedKinds is the mode's pinned kinds, listed first: {name, service}.
function pinnedKinds() {
  return pins.filter((p) => p.source === state.source).map((p) => ({ name: p.kind, service: p.service }));
}

// togglePin pins or unpins kind name of the selected service and saves the
// service's whole pin list.
let pinSaves = Promise.resolve();

function togglePin(name) {
  if (readOnly() || name === ALL_KINDS) return;
  const svc = state.service;
  const source = state.source;
  // Saves run one after another and each builds its list when it runs, from
  // the list the previous save left, so two quick toggles both land.
  const run = async () => {
    try {
      // The service's list is read from the server, not from the shared pins
      // array, which after a service switch holds another service's pins.
      const cur = await api("GET", "/api/pins?service=" + encodeURIComponent(svc));
      const list = ((cur && cur.pins) || []).map((p) => ({ source: p.source, kind: p.kind }));
      const i = list.findIndex((p) => p.source === source && p.kind === name);
      if (i >= 0) list.splice(i, 1);
      else list.push({ source, kind: name });
      const r = await api("PUT", "/api/pins?service=" + encodeURIComponent(svc), { pins: list });
      // Merge only while svc is on show; otherwise the next loadPins of the
      // selected service is the source of truth.
      if (state.service !== svc) return;
      pins = pins.filter((p) => p.service !== svc).concat(((r && r.pins) || []).map((p) => ({ source: p.source, kind: p.kind, service: svc })));
      renderKindList();
    } catch (err) {
      showError("saving pins: " + (err.detail || err.message));
    }
  };
  pinSaves = pinSaves.then(run);
  return pinSaves;
}

// kindRowExtras adds the pin toggle to a kind row: shown on hover, always
// when pinned, and disabled under All.
function kindRowExtras(row, k) {
  const on = !!k.pinned;
  const b = el("button", "kind-pin" + (on ? " pinned" : ""), "\u{1F4CC}");
  b.type = "button";
  b.setAttribute("aria-pressed", String(on));
  if (readOnly()) {
    b.disabled = true;
    b.title = NEED_SERVICE;
  } else {
    b.title = on ? "Unpin" : "Pin to the top";
  }
  // The filter box redraws the list on blur; without this the press would
  // move focus and the redraw would swallow the click.
  b.addEventListener("mousedown", (e) => e.preventDefault());
  b.addEventListener("click", (e) => {
    e.stopPropagation();
    togglePin(k.name);
  });
  b.addEventListener("keydown", (e) => e.stopPropagation());
  row.append(b);
}

// listedKinds is every row of the list in order: the pinned kinds (count
// from kinds, else 0), then the other kinds in server order.
function listedKinds() {
  const counts = new Map(kinds.map((k) => [k.name, k.count]));
  const pk = pinnedKinds();
  // Under All a pin belongs to one service, so the kind stays listed too.
  const pinned = new Set(readOnly() ? [] : pk.map((p) => p.name));
  return pk.map((p) => ({
    name: p.name,
    count: (readOnly() ? pinCounts.get(p.service + "\n" + p.name) : counts.get(p.name)) || 0,
    pinned: true,
    service: readOnly() ? p.service : "",
  })).concat(kinds.filter((k) => !pinned.has(k.name)).map((k) => ({ name: k.name, count: k.count })));
}

// hasAllKinds: Spans mode lists an All kinds row; Logs mode has none.
const hasAllKinds = () => state.source === "otel";

// allKindsRow is the All kinds row: stream "*", the sum of the kinds' counts.
// It is never pinned and always listed first.
function allKindsRow() {
  return { name: ALL_KINDS, label: "All kinds", count: kinds.reduce((n, k) => n + k.count, 0) };
}

// firstKind is the first listed kind with records, else the first listed
// kind, else "".
function firstKind() {
  const rows = listedKinds();
  const r = rows.find((k) => k.count > 0) || rows[0];
  return r ? r.name : "";
}

function kindRow(k, sep) {
  const on = k.name === state.stream;
  const row = el("div", "kind-row" + (k.count ? "" : " kind-zero") + (on ? " selected" : "") + (sep ? " kind-sep" : ""));
  row.dataset.kind = k.name;
  row.setAttribute("role", "option");
  row.setAttribute("aria-selected", String(on));
  row.tabIndex = 0;
  const label = k.label || (k.service ? k.name + " · " + k.service : k.name);
  row.title = label;
  row.append(el("span", "kind-name", label), el("span", "kind-count", k.count.toLocaleString()));
  row.addEventListener("click", () => {
    if (k.name !== state.stream) selectKind(k.name);
  });
  row.addEventListener("keydown", (e) => {
    if (e.key !== "Enter" && e.key !== " ") return;
    e.preventDefault();
    if (k.name !== state.stream) selectKind(k.name);
  });
  if (k.name !== ALL_KINDS) kindRowExtras(row, k);
  return row;
}

// renderKindList draws the pinned kinds, the top 10 other kinds and, when
// More is open, the rest narrowed by the filter box. The selected kind is
// always shown, with 0 when the interval has none of it.
function renderKindList() {
  const rows = listedKinds();
  const all = hasAllKinds() ? [allKindsRow()] : [];
  const npin = rows.filter((r) => r.pinned).length;
  const top = rows.slice(0, npin + TOP_KINDS);
  const rest = rows.slice(npin + TOP_KINDS);
  const needle = $("kind-filter").value.trim().toLowerCase();
  const open = kindsOpen && rest.length > 0;
  const more = open ? rest.filter((r) => r.name.toLowerCase().includes(needle)) : [];
  const out = all.map((r) => kindRow(r, false));
  top.forEach((r, i) => out.push(kindRow(r, (npin > 0 && i === npin) || (i === 0 && all.length > 0))));
  const sel = state.stream;
  if (sel && !(sel === ALL_KINDS && all.length) && !top.some((r) => r.name === sel) && !more.some((r) => r.name === sel)) {
    out.push(kindRow(rest.find((r) => r.name === sel) || { name: sel, count: 0 }, out.length > 0));
  }
  more.forEach((r, i) => out.push(kindRow(r, i === 0)));
  $("kind-list").replaceChildren(...out);
  const btn = $("kind-more");
  btn.hidden = rest.length === 0;
  btn.textContent = open ? "Less ▴" : "More (" + rest.length.toLocaleString() + ") ▸";
  btn.setAttribute("aria-expanded", String(open));
  $("kind-filter").hidden = !open;
}

// selectKind makes name the current kind: it resets the columns, filters and
// explore settings, loads the kind's schema and runs the query. An empty name
// selects nothing and clears the results.
async function selectKind(name) {
  state.stream = name;
  resetQueryParts();
  closeColumnMenu();
  hiddenThisQuery = [];
  tempWidths = {};
  clearExplore();
  renderKindList();
  if (!name) {
    schemaData = null;
    clearResults();
    saveURL();
    return;
  }
  const favs = await serviceFavorites();
  if (state.stream !== name) return; // a newer selection took over
  state.columns = favoriteColumns(favs, name);
  if (!(await loadSchema())) return; // a newer selection took over
  rebuildExplore();
  runQuery();
}

// clearResults empties the table and graph when no kind is selected.
function clearResults() {
  querySeq++; // drop an answer still on its way
  lastResult = null;
  $("head").replaceChildren();
  $("body").replaceChildren();
  $("graph").replaceChildren();
  heatData = null;
  heatKeyDrawn = "";
  forgetHeatSelection();
  drawHeat();
  $("more").hidden = true;
  $("footer-text").textContent = "no " + (state.source === "otel" ? "spans" : "log records") + " in range";
}

// setMode switches between Logs and Spans and selects the new mode's first
// kind with records.
async function setMode(src) {
  if (src === state.source) return;
  state.source = src === "otel" ? "otel" : "log";
  state.stream = "";
  kindsOpen = false;
  $("kind-filter").value = "";
  if (state.view === "heatmap" && state.source !== "otel") state.view = "samples"; // logs have no durations
  syncMode();
  syncView();
  try {
    if (!(await loadKinds())) return;
  } catch (err) {
    showError(err.message);
    return;
  }
  selectKind(firstKind());
}

// setService switches the service, remembers it in this browser and keeps the
// kind when the service has records of it, else selects its first kind with
// records.
async function setService(svc) {
  if (svc === state.service) return;
  state.service = svc;
  storeService(svc);
  syncReadOnly();
  try {
    if (!(await loadKinds())) return;
  } catch (err) {
    showError(err.message);
    return;
  }
  if (!state.stream || !kinds.some((k) => k.name === state.stream && k.count > 0)) {
    selectKind(firstKind());
    return;
  }
  closeColumnMenu();
  hiddenThisQuery = [];
  tempWidths = {};
  clearExplore();
  // The columns of the previous service's favorite don't carry over.
  const favs = await serviceFavorites();
  if (state.service !== svc) return; // a newer switch took over
  state.columns = favoriteColumns(favs, state.stream);
  if (!(await loadSchema())) return; // field settings are per service
  rebuildExplore();
  runQuery();
}

// fallbackKind replaces a kind the list doesn't have (an old URL's kind, or
// the retired `span` stream) by the first kind with records, resetting the
// query parts that belonged to it. With no kinds at all it keeps it. It
// returns true when it changed the kind.
function fallbackKind() {
  if (state.stream === ALL_KINDS && hasAllKinds()) return false;
  if (state.stream && listedKinds().some((k) => k.name === state.stream)) return false;
  const name = firstKind();
  if (!name) return false;
  state.stream = name;
  resetQueryParts();
  return true;
}

// resetQueryParts clears the parts of the query that belong to one kind.
function resetQueryParts() {
  state.columns = [];
  state.filters = [];
  state.sort = { field: "", desc: true };
  state.groupBy = [];
  state.aggregates = [];
  state.plot = { op: "count" };
  state.order = { op: "count" };
}

// ---- results ----

const NUMERIC = new Set(["int", "float", "duration"]);

// Cells of these columns link to the trace waterfall.
const TRACE_PATHS = new Set(["trace_id", "trace"]);
const SPAN_PATHS = new Set(["span_id", "spanId", "parent_span_id"]);

// aggLinked follows node, the name cell of a root span, with a link to the
// aggregate view of that operation. The name goes to openAggregate as data.
function aggLinked(node, cell) {
  const v = cell && !cell.bad ? cell.v : undefined;
  if (typeof v !== "string" || !v.trim()) return node;
  const wrap = el("span", "agg-cell");
  const a = el("a", "agg-link", "∑");
  a.href = "#";
  a.title = "aggregate this operation";
  a.addEventListener("click", (e) => {
    e.preventDefault();
    e.stopPropagation(); // not the row's detail toggle
    openAggregate({ name: v, service: state.service === "*" ? "" : state.service });
  });
  wrap.append(node, a);
  return wrap;
}

// traceLinked wraps node, the rendered cell of column path, in a link that
// opens the waterfall when the cell holds a non-empty string ID; else it
// returns node as it is.
function traceLinked(node, cell, path, row) {
  if (path === "name" && state.source === "otel" && row && row.spanRoot) return aggLinked(node, cell);
  const isTrace = TRACE_PATHS.has(path);
  if (!isTrace && !SPAN_PATHS.has(path)) return node;
  const v = cell && !cell.bad ? cell.v : undefined;
  if (typeof v !== "string" || !v.trim()) return node;
  const a = el("a", "trace-link");
  a.href = "#";
  a.title = isTrace ? "open this trace" : "open this span in its trace";
  a.append(node);
  a.addEventListener("click", (e) => {
    e.preventDefault();
    e.stopPropagation(); // not the row's detail toggle
    openTrace(isTrace ? { trace: v } : { span: v });
  });
  return a;
}

function cellNode(row, cell, col) {
  if (col.path === "timestamp" && row.timeInherited && (!cell || cell.v === null || cell.v === undefined)) {
    const d = new Date(row.time);
    const e = el("span", "inherited", localTime(d));
    e.title = "no timestamp on this record; time inherited (" + d.toISOString() + ")";
    return e;
  }
  return visualize(cell, col);
}

// addSQLCopy gives td, a cell of a column shown as SQL, a copy button that
// shows on hover and copies the full value, not the cropped one.
function addSQLCopy(td, cell, col) {
  if (col.visualizer !== "sql" || !cell || cell.bad || cell.v === null || cell.v === undefined) return;
  const text = asText(cell.v);
  if (!text) return;
  td.classList.add("has-copy");
  td.append(copyButton(() => text, { cls: "cell-copy" }));
}

// cellCopyText is the text of a results cell as the user sees it, but whole
// where the page cropped it (an element marked with data-full).
function cellCopyText(td) {
  const full = td.querySelector("[data-full]");
  if (full) return full.dataset.full;
  const c = td.cloneNode(true);
  for (const x of c.querySelectorAll(".agg-link, .copy-btn")) x.remove(); // controls, not the value
  return c.textContent;
}

// fetchExport asks the server for the shown query in an export format and
// returns the text. It sends what runQuery sends (limit included) and, for a
// Samples batch past the first, that batch's offset and columns as loadBatch
// does, so the copy is the table on screen, not the first page of it.
// In the Heatmap view that table is the span list (listQuery), not the
// heatmap the CLI exports for the same URL: a deliberate exception (spec,
// "Copy buttons in the page").
function batchPart() {
  const base = lastResult;
  if (!base || (base.view && base.view !== "samples") || groupedList() || !base.offset) return {};
  return { columns: base.columns.map((c) => c.path), offset: base.offset };
}

async function fetchExport(format) {
  const resp = await fetch("/api/query", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(Object.assign({}, listQuery(), { format }, batchPart())),
  });
  if (!resp.ok) {
    let msg = resp.status + " " + resp.statusText;
    try {
      const d = await resp.json();
      if (d && d.error) msg = d.error;
    } catch {
      // keep the status line
    }
    throw new Error(msg);
  }
  return resp.text();
}

// shownTable is the results table as shown, for copying: the header labels
// and each row's cells; opened record details are skipped.
function shownTable() {
  const headers = Array.from($("head").children).map((th) => {
    const label = th.querySelector(".th-label");
    return label ? label.textContent : th.textContent;
  });
  const rows = [];
  for (const tr of $("body").children) {
    if (tr.classList.contains("detail")) continue;
    rows.push(Array.from(tr.children).map(cellCopyText));
  }
  return { headers, rows };
}

// renderResult shows a query result in the view it was run for.
function renderResult(result) {
  const view = result.view || "samples";
  $("more").hidden = true; // render shows it for a samples batch that has more
  $("table").hidden = view === "graph";
  $("graph").hidden = view !== "graph";
  if (view === "table") renderTable(result);
  else if (view === "graph") renderGraph(result);
  else render(result);
}

// metricLabel names an aggregate: "count", or its operator and field.
export function metricLabel(a) {
  if (!a || a.op === "count") return "count";
  if (fieldless(a.op)) return METRIC_LABELS[a.op] || a.op;
  const f = fieldByPath(a.field);
  return a.op + " " + ((f && f.label) || a.field);
}

// tableHeaderCell is a column header with a resize grip but no remove button.
function tableHeaderCell(c, names) {
  const th = el("th");
  th.dataset.path = c.path;
  th.title = c.path + " : " + c.type;
  const inner = el("div", "th-inner");
  inner.append(breakable(colName(c, names)));
  const grip = el("span", "col-resize");
  grip.title = "drag to resize · double-click to fit";
  if (!c.group) th.dataset.temp = "1"; // a metric: not a schema field
  th.append(inner, resizeGrip(th));
  return th;
}

// renderTable shows grouped results: one row per group, no expansion.
function renderTable(result) {
  const head = $("head");
  const names = shortNames(result.columns.map((c) => c.path));
  head.replaceChildren(...result.columns.map((c) => tableHeaderCell(c, names)));
  const body = $("body");
  body.replaceChildren();
  for (const row of result.rows) {
    const tr = el("tr");
    result.columns.forEach((c, i) => {
      const td = el("td", NUMERIC.has(c.type) ? "num" : "");
      const cell = row.cells[i];
      if (c.group && (!cell || cell.v === null || cell.v === undefined) && !(cell && cell.bad)) {
        td.append(el("span", "muted", "(empty)"));
      } else {
        // The error-rate metric is a fraction shown as a percentage; a group-by
        // column that happens to be named error_rate is the user's data.
        const rate = !c.group && c.path === "error_rate" && cell && typeof cell.v === "number";
        td.append(traceLinked(rate ? el("span", "", percent(cell.v)) : visualize(cell, c), cell, c.path, row));
        addSQLCopy(td, cell, c);
      }
      tr.append(td);
    });
    body.append(tr);
  }
  const n = result.groups || 0;
  let foot = n.toLocaleString() + " " + (n === 1 ? "group" : "groups") + " · " + result.matched.toLocaleString() + " records match";
  if (result.truncated) foot += " · showing the top " + result.rows.length.toLocaleString();
  $("footer-text").textContent = foot;
  applyWidths();
}

// percent shows a fraction as a percentage with one decimal: 0.125 -> 12.5%.
const percent = (v) => (v * 100).toLocaleString(undefined, { minimumFractionDigits: 1, maximumFractionDigits: 1 }) + "%";

// metricFormat returns a labeller for values of a metric column: durations in
// the column's unit, other numbers grouped with at most three decimals.
function metricFormat(col) {
  if (col && col.type === "duration") return (v) => formatDuration(v, col.unit);
  if (col && col.path === "error_rate") return percent;
  return (v) => v.toLocaleString(undefined, { maximumFractionDigits: 3 });
}

// renderGraph draws the time series of a graph result.
function renderGraph(result) {
  const g = result.graph || { series: [], starts: [] };
  autoBucket = state.bucket === "auto" ? g.bucket || "" : "";
  drawBucket();
  drawGraph($("graph"), g, metricFormat(g.metric), state.chart === "stacked" && stackable(state.plot));
  const n = (g.series || []).length;
  let foot = result.matched.toLocaleString() + " records match";
  if (g.bucket && (g.starts || []).length) foot += " · " + g.starts.length.toLocaleString() + " buckets of " + g.bucket;
  if (n) foot += " · " + n + " series";
  if (g.groups > g.top) {
    foot += " · top " + g.top + " of " + g.groups.toLocaleString() + " groups, the rest in other";
    if (state.top > g.top) foot += " (the graph draws at most " + g.top + ")";
  }
  $("footer-text").textContent = foot;
}

export function render(result) {
  lastResult = result;
  lastResultStream = streamKey();
  const head = $("head");
  head.replaceChildren();
  const names = shortNames(result.columns.map((c) => c.path));
  for (const c of result.columns) head.append(headerCell(c, names));
  $("body").replaceChildren();
  appendRows(result.rows, result.columns);
  renderSamplesFooter(result);
  renderColumnNames();
  applyWidths();
}

function appendRows(rows, columns) {
  const body = $("body");
  for (const row of rows) {
    const tr = el("tr", "row");
    tr.dataset.seq = String(row.seq);
    columns.forEach((c, i) => {
      const td = el("td", NUMERIC.has(c.type) ? "num" : "");
      td.append(traceLinked(cellNode(row, row.cells[i], c), row.cells[i], c.path, row));
      addSQLCopy(td, row.cells[i], c);
      tr.append(td);
    });
    tr.addEventListener("click", () => toggleDetail(tr, columns.length));
    body.append(tr);
  }
}

// renderSamplesFooter shows the counts, and the Load next button while the
// sorted matches go on past the rows shown.
// renderSamplesFooter shows the counts, and the batch buttons: Load next
// while the sorted matches go on past this batch, First after the first.
function renderSamplesFooter(result) {
  const off = result.offset || 0;
  const shown = result.rows.length;
  const span = shown ? (off + 1).toLocaleString() + "–" + (off + shown).toLocaleString() : "none";
  $("footer-text").textContent = result.matched.toLocaleString() + " of " + result.streamTotal.toLocaleString() + " " +
    (state.stream === ALL_KINDS ? "span" : state.stream) + " records match · showing " + (off ? span : shown.toLocaleString());
  $("limit").value = String(state.limit || DEFAULT_LIMIT);
  $("more").hidden = !result.truncated && !off;
  $("more-first").hidden = !off;
  $("more-btn").hidden = !result.truncated;
  if (result.truncated) {
    const next = Math.min(state.limit || DEFAULT_LIMIT, result.matched - off - shown);
    $("more-btn").textContent = "Load next " + next.toLocaleString() + " ▶";
  }
  $("more-range").textContent = span + " of " + result.matched.toLocaleString();
}

// loadBatch replaces the rows with the batch at offset (the next one, or
// the first again), keeping the shown columns. A new query, or a newer
// batch, drops its answer.
let moreSeq = 0;

async function loadBatch(next) {
  const base = lastResult;
  if (!base || (base.view && base.view !== "samples") || groupedList()) return;
  const offset = next ? (base.offset || 0) + base.rows.length : 0;
  const q0 = querySeq;
  const l0 = listSeq;
  const mine = ++moreSeq;
  const buttons = [$("more-btn"), $("more-first")];
  for (const b of buttons) b.disabled = true;
  try {
    const columns = base.columns.map((c) => c.path);
    const res = await api("POST", "/api/query", Object.assign(listQuery(), { columns, offset }));
    if (q0 !== querySeq || l0 !== listSeq || mine !== moreSeq || lastResult !== base) return;
    res.offset = offset;
    render(res);
    $("results").scrollTop = 0;
  } catch (err) {
    if (q0 === querySeq && mine === moreSeq) showError(err.message);
  } finally {
    for (const b of buttons) b.disabled = false;
  }
}

function validLimit(n) {
  return Number.isInteger(n) && n >= 1 && n <= MAX_LIMIT;
}

function wireLimit() {
  const input = $("limit");
  input.value = String(state.limit || DEFAULT_LIMIT);
  input.addEventListener("change", () => {
    const n = Number(input.value);
    if (!validLimit(n)) {
      input.value = String(state.limit || DEFAULT_LIMIT);
      return;
    }
    state.limit = n === DEFAULT_LIMIT ? 0 : n;
    runQuery();
  });
  $("more-btn").addEventListener("click", () => loadBatch(true));
  $("more-first").addEventListener("click", () => loadBatch(false));
}

// VAGUE_TAILS are last path parts too generic to show alone: the part before
// is kept too, so db.query.text shows as query.text.
const VAGUE_TAILS = new Set(["text"]);

// shortNames maps each path to its shortest dot-separated tail that no other
// path in paths shares as a tail, so otel.attributes.httpRequest.status shows
// as status, and latency next to otel.attributes.httpRequest.latency shows as
// httpRequest.latency. A vague last part (VAGUE_TAILS) keeps the part before.
export function shortNames(paths) {
  const out = {};
  paths.forEach((p, i) => {
    const parts = p.split(".");
    const first = parts.length > 1 && VAGUE_TAILS.has(parts[parts.length - 1]) ? 2 : 1;
    for (let n = first; n <= parts.length; n++) {
      const tail = parts.slice(-n).join(".");
      if (!paths.some((q, j) => j !== i && (q === tail || q.endsWith("." + tail)))) {
        out[p] = tail;
        return;
      }
    }
    out[p] = p;
  });
  return out;
}

// colName is a column's shown name: its label, else its short name. The
// server sends the path as the label when none is set.
function colName(c, names) {
  if (c.label && c.label !== c.path) return c.label;
  return names[c.path] || c.path;
}

// breakable renders text with a line-break opportunity after each period, so
// a long dotted path wraps instead of setting the column's minimum width.
function breakable(text) {
  const s = el("span", "th-label");
  text.split(".").forEach((part, i, all) => {
    s.append(document.createTextNode(part + (i < all.length - 1 ? "." : "")));
    if (i < all.length - 1) s.append(document.createElement("wbr"));
  });
  return s;
}

const SVG = "http://www.w3.org/2000/svg";

// ICONS are 16×16 stroke paths for the column menu's icon row.
const ICONS = {
  hide: ["M2 8s2.2-4 6-4 6 4 6 4-2.2 4-6 4-6-4-6-4z", "M8 6.3a1.7 1.7 0 1 0 0 3.4 1.7 1.7 0 1 0 0-3.4", "M2.5 13.5l11-11"],
  filter: ["M2 3h12L9.5 8.5V13l-3-1.5v-3z"],
  group: ["M2 3.5h12", "M5 6.5h9", "M5 9.5h9", "M2 12.5h12", "M2.5 6v4"],
  fit: ["M2.5 3v10", "M13.5 3v10", "M5 8h6", "M6.8 6.2L5 8l1.8 1.8", "M9.2 6.2L11 8l-1.8 1.8"],
  wrap: ["M2 4h12", "M2 8h9.5a2 2 0 0 1 0 4H8", "M9.5 10.5L8 12l1.5 1.5", "M2 12h3.5"],
  crop: ["M2 4h12", "M2 8h12", "M2 12h8", "M12 12h.5", "M13.8 12h.2"],
};

// VCR are the move buttons' glyphs; U+FE0E asks for the text form, not emoji.
const VCR = { first: "\u23EE\uFE0E", left: "\u25C0\uFE0E", right: "\u25B6\uFE0E", last: "\u23ED\uFE0E" };

function icon(name) {
  const svg = document.createElementNS(SVG, "svg");
  svg.setAttribute("viewBox", "0 0 16 16");
  svg.setAttribute("width", "15");
  svg.setAttribute("height", "15");
  svg.setAttribute("aria-hidden", "true");
  for (const d of ICONS[name]) {
    const p = document.createElementNS(SVG, "path");
    p.setAttribute("d", d);
    p.setAttribute("fill", "none");
    p.setAttribute("stroke", "currentColor");
    p.setAttribute("stroke-width", "1.3");
    p.setAttribute("stroke-linecap", "round");
    p.setAttribute("stroke-linejoin", "round");
    svg.append(p);
  }
  return svg;
}

function headerCell(c, names) {
  const th = el("th");
  th.dataset.path = c.path;
  th.title = c.path + " : " + c.type;
  const inner = el("div", "th-inner");
  const label = breakable(colName(c, names));
  label.classList.add("th-menu");
  label.tabIndex = 0;
  label.setAttribute("role", "button");
  label.setAttribute("aria-haspopup", "true");
  label.addEventListener("click", () => openColumnMenu(c.path, label));
  label.addEventListener("keydown", (e) => {
    if (e.key !== "Enter" && e.key !== " ") return;
    e.preventDefault();
    openColumnMenu(c.path, label);
  });
  inner.append(label, sortArrows(c.path));
  th.append(inner, resizeGrip(th));
  return th;
}

// ---- column widths ----
// A dragged width is saved in the stream's schema (field width; 0 = natural).
// Table-view metric columns are not schema fields: their widths last until
// the stream changes.

let tempWidths = {};

function resizeGrip(th) {
  const grip = el("span", "col-resize");
  grip.title = "drag to resize · double-click to fit";
  grip.addEventListener("pointerdown", (e) => startResize(e, th));
  grip.addEventListener("dblclick", () => {
    const path = th.dataset.path;
    if (th.dataset.temp) {
      delete tempWidths[path];
      applyWidths();
    } else if ((fieldByPath(path) || {}).width) {
      saveFieldFormat(path, { width: 0 });
    }
  });
  return grip;
}

function savedWidths() {
  const w = {};
  for (const f of schemaFields()) if (f.width > 0) w[f.path] = f.width;
  return Object.assign(w, tempWidths);
}

// applyWraps marks the cells of columns set to wrap; the others crop. It
// returns, per column, whether it wraps.
function applyWraps(ths) {
  const wrap = ths.map((th) => !th.dataset.temp && !!(fieldByPath(th.dataset.path) || {}).wrap);
  for (const tr of $("body").children) {
    if (tr.classList.contains("detail")) continue;
    Array.from(tr.children).forEach((td, i) => td.classList.toggle("wrap", !!wrap[i]));
  }
  return wrap;
}

// AUTO_MAX_CH caps a column sized from its content, in characters of the
// table font, so one long value doesn't make the column as wide as itself.
// message and columns set to wrap are not capped.
const AUTO_MAX_CH = 40;

let chPx = 0;

// autoMaxPx is AUTO_MAX_CH in pixels of the table's monospace font.
function autoMaxPx() {
  if (!chPx) {
    const probe = el("span", "", "0".repeat(100));
    probe.style.cssText = "position:absolute;visibility:hidden;white-space:pre;font-family:var(--mono);font-size:12.5px";
    document.body.append(probe);
    chPx = probe.getBoundingClientRect().width / 100;
    probe.remove();
  }
  return Math.round(AUTO_MAX_CH * chPx);
}

// capped reports whether a column without a dragged width is held to
// autoMaxPx.
function capped(th, i, wrap) {
  return th.dataset.path !== "message" && !wrap[i];
}

// applyWidths lays the table out from its content, then, if any column has a
// dragged width or a content width over autoMaxPx, freezes every column at
// its width (dragged, capped or natural) with a fixed table layout so a
// column can be narrower than its content; a value cut short by the cap gets
// its full text as a tooltip. It sets each column's crop or wrap first, since
// that changes the natural widths.
// widthsStale marks a layout skipped while the table was not displayed (a
// live run re-renders behind the trace or aggregate view, where every column
// measures 0 px); watchTableShown redoes it when the table shows again.
let widthsStale = false;

function applyWidths(extra) {
  const table = $("table");
  if (!table.getClientRects().length) {
    widthsStale = true;
    return;
  }
  widthsStale = false;
  const ths = Array.from($("head").children);
  const wrap = applyWraps(ths);
  const w = Object.assign(savedWidths(), extra || {});
  table.classList.remove("fixed");
  table.style.width = "";
  for (const th of ths) th.style.width = "";
  renderRestore();
  const natural = ths.map((th) => th.getBoundingClientRect().width);
  const max = autoMaxPx();
  const cut = ths.map((th, i) => !(th.dataset.path in w) && capped(th, i, wrap) && natural[i] > max);
  if (!ths.some((th) => th.dataset.path in w) && !cut.some(Boolean)) return;
  let total = 0;
  ths.forEach((th, i) => {
    const px = th.dataset.path in w ? w[th.dataset.path] : cut[i] ? max : natural[i];
    th.style.width = px + "px";
    total += px;
  });
  table.style.width = total + "px";
  table.classList.add("fixed");
  if (!cut.some(Boolean)) return;
  for (const tr of $("body").children) {
    if (tr.classList.contains("detail")) continue;
    Array.from(tr.children).forEach((td, i) => {
      if (cut[i] && !td.title && td.scrollWidth > td.clientWidth) td.title = td.textContent;
    });
  }
}

function watchTableShown() {
  new ResizeObserver(() => {
    if (widthsStale && $("table").getClientRects().length) applyWidths();
  }).observe($("results"));
}

const MIN_COL = 40;

function startResize(e, th) {
  if (e.button !== 0) return;
  if (lastResultStream !== streamKey()) return;
  e.preventDefault();
  const path = th.dataset.path;
  const startX = e.clientX;
  const startW = th.getBoundingClientRect().width;
  applyWidths({ [path]: startW });
  const table = $("table");
  const tableW = parseFloat(table.style.width);
  const grip = e.currentTarget;
  grip.setPointerCapture(e.pointerId);
  document.body.classList.add("resizing");
  let width = startW;
  const move = (ev) => {
    width = Math.max(MIN_COL, Math.round(startW + ev.clientX - startX));
    th.style.width = width + "px";
    table.style.width = tableW - startW + width + "px";
  };
  const up = () => {
    grip.removeEventListener("pointermove", move);
    grip.removeEventListener("pointerup", up);
    grip.removeEventListener("pointercancel", up);
    document.body.classList.remove("resizing");
    if (width === startW) {
      // A press and release with no movement is a click (a double-click on
      // the grip delivers two): never save a width for it.
      applyWidths();
    } else if (th.dataset.temp) {
      tempWidths[path] = width;
    } else {
      saveFieldFormat(path, { width });
    }
  };
  grip.addEventListener("pointermove", move);
  grip.addEventListener("pointerup", up);
  grip.addEventListener("pointercancel", up);
}

// renderRestore shows the restore button while the shown columns differ from
// the stream's default columns.
function renderRestore() {
  const def = schemaData && schemaData.schema && Array.isArray(schemaData.schema.columns) ? schemaData.schema.columns : [];
  const cols = currentColumns();
  $("col-restore").hidden = !def.length || (cols.length === def.length && cols.every((p, i) => p === def[i]));
  $("col-all").hidden = !unshownFields().length;
}

// unshownFields are the stream's fields, not hidden, that are not shown as
// columns, by path.
function unshownFields() {
  const shown = new Set(currentColumns());
  return schemaFields()
    .filter((f) => !f.hidden && !shown.has(f.path))
    .map((f) => f.path)
    .sort();
}

// showAllColumns keeps the shown columns in order and adds every other field.
function showAllColumns() {
  setColumns([...currentColumns(), ...unshownFields()]);
}

// preOf is a <pre> whose http(s) URLs are links.
function preOf(text) {
  const p = el("pre");
  p.append(linkify(String(text)));
  return p;
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
  await fillRecordDetail(td, tr.dataset.seq);
}

// detailSection is a collapsible titled block of a record's detail.
function detailSection(title, body, open) {
  const box = el("details", "detail-sec");
  box.open = open;
  box.append(el("summary", "", title), body);
  return box;
}

// fillRecordDetail replaces container's children with record seq: its SQL
// fields formatted and coloured, then the raw record, each in a section that
// opens and closes; recordOpen sets whether the raw record starts open. An
// error shows in place.
export async function fillRecordDetail(container, seq, { recordOpen = true } = {}) {
  try {
    const rec = await api("GET", "/api/record?seq=" + encodeURIComponent(seq));
    container.replaceChildren();
    for (const path of Object.keys(rec.sql || {}).sort()) {
      const toks = rec.sqlTokens && rec.sqlTokens[path];
      let pre = preOf(rec.sql[path]);
      if (Array.isArray(toks)) {
        pre = el("pre");
        pre.append(sqlSpan(toks, rec.sql[path]));
      }
      const sec = detailSection(path + " (SQL)", pre, true);
      const sql = String(rec.sql[path]);
      sec.querySelector("summary").append(copyButton(() => sql, { cls: "sum-copy" }));
      container.append(sec);
    }
    let raw = preOf(rec.raw);
    if (Array.isArray(rec.rawTokens)) {
      raw = el("pre");
      raw.append(sqlSpan(rec.rawTokens, rec.raw));
    }
    container.append(detailSection("record", raw, recordOpen));
  } catch (err) {
    container.replaceChildren(el("span", "bad", err.message));
  }
}

let querySeq = 0;

export async function runQuery() {
  saveURL();
  if (!state.stream) {
    clearResults();
    return;
  }
  const mine = ++querySeq;
  if (state.view === "heatmap") return runHeatmapQuery(mine);
  try {
    const result = await api("POST", "/api/query", request(state));
    if (mine !== querySeq) return; // a newer query superseded this one
    clearError();
    renderResult(result);
  } catch (err) {
    if (mine === querySeq) showError(err.message);
  }
}

async function refresh() {
  try {
    await loadSession();
    await loadKinds();
    // The URL's kind is checked once; later a kind that leaves the interval
    // stays selected and shows 0.
    if ((!started || !state.stream) && fallbackKind()) {
      const name = state.stream;
      const favs = await serviceFavorites();
      if (state.stream === name) state.columns = favoriteColumns(favs, name);
    }
    started = true;
    syncBar();
    if (!state.stream) {
      schemaData = null;
      clearResults();
      saveURL();
      return;
    }
    if (!(await loadSchema())) return; // a newer selection took over
    rebuildExplore();
  } catch (err) {
    showError(err.message);
    return;
  }
  // Awaited so init opens a deep-linked trace after the query's answer, which
  // would otherwise clear the trace's error banner.
  await runQuery();
}

// ---- schema of the current stream ----

let schemaSeq = 0;

// loadSchema loads the current kind's schema. It returns false, dropping the
// answer, when a newer call (another kind, mode or service) superseded it.
async function loadSchema() {
  const mine = ++schemaSeq;
  schemaData = null;
  if (!state.stream) return true;
  let data = null;
  try {
    data = await api("GET", schemaURL(state.stream, state.source));
  } catch (err) {
    if (mine === schemaSeq) showError(err.message);
  }
  if (mine !== schemaSeq) return false;
  schemaData = data;
  fillFieldPaths();
  return true;
}

function schemaFields() {
  return schemaData && schemaData.schema && Array.isArray(schemaData.schema.fields) ? schemaData.schema.fields : [];
}

function fieldByPath(path) {
  return schemaFields().find((f) => f.path === path);
}

function badge(text, cls) {
  return el("span", "badge" + (cls ? " badge-" + cls : ""), text);
}

// currentColumns is the column paths the table shows now.
function currentColumns() {
  if (lastResult && lastResultStream === streamKey() && Array.isArray(lastResult.columns)) return lastResult.columns.map((c) => c.path);
  if (state.columns.length) return state.columns.slice();
  return schemaData && schemaData.schema && Array.isArray(schemaData.schema.columns) ? schemaData.schema.columns.slice() : [];
}

function renderColumnNames() {
  const box = $("colnames");
  box.replaceChildren();
  const cols = lastResult ? lastResult.columns : [];
  const names = shortNames(cols.map((c) => c.path));
  cols.forEach((c, i) => {
    if (i) box.append(el("span", "sep", " · "));
    const b = el("button", "colname", colName(c, names));
    b.type = "button";
    b.title = c.path;
    b.setAttribute("aria-haspopup", "true");
    b.addEventListener("click", () => openColumnMenu(c.path, b));
    box.append(b);
  });
}

// ---- field picker ----

// fillFieldPaths lists the stream's field paths in the shared datalist that
// every field picker offers.
function fillFieldPaths() {
  const list = $("field-paths");
  list.replaceChildren();
  for (const f of schemaFields()) {
    const o = el("option", "", (f.label ? f.label + " · " : "") + f.type);
    o.value = f.path;
    list.append(o);
  }
}

// fieldPicker returns a text input offering the stream's field paths. onPick
// gets the trimmed path when the user commits one (change, or choosing a
// suggestion), and "" when the input is cleared.
function fieldPicker(path, onPick) {
  const input = el("input", "field-pick");
  input.type = "text";
  input.setAttribute("list", "field-paths");
  input.placeholder = "field";
  input.spellcheck = false;
  input.value = path;
  let last = path;
  const fire = () => {
    const v = input.value.trim();
    if (v === last) return;
    last = v;
    onPick(v);
  };
  input.addEventListener("change", fire);
  input.addEventListener("input", (e) => {
    // A datalist choice arrives as an input event with no typing inputType.
    if ((!e.inputType || e.inputType === "insertReplacementText") && fieldByPath(input.value.trim())) fire();
  });
  return input;
}

// distinctValues lists up to 50 distinct values of path in the current result.
function distinctValues(path) {
  const out = [];
  if (!lastResult) return out;
  const idx = lastResult.columns.findIndex((c) => c.path === path);
  if (idx < 0) return out;
  const seen = new Set();
  for (const row of lastResult.rows) {
    const cell = row.cells[idx];
    if (!cell || cell.bad || cell.v === null || cell.v === undefined) continue;
    const s = asText(cell.v);
    if (s === "" || seen.has(s)) continue;
    seen.add(s);
    out.push(s);
    if (out.length >= 50) break;
  }
  return out.sort();
}

// tagInput fills box with an editable list of values (tags, changed in
// place) plus suggestions from the current result's values of path, and
// calls onChange after each add or remove.
function tagInput(box, tags, path, onChange) {
  const list = el("span", "tags");
  const input = el("input");
  input.type = "text";
  input.placeholder = "value, Enter to add";
  const sugg = el("div", "suggestions");
  const all = distinctValues(path);

  const addTag = (v) => {
    v = v.trim();
    input.value = "";
    if (v && !tags.includes(v)) {
      tags.push(v);
      onChange();
    }
    draw();
    input.focus();
  };
  const draw = () => {
    list.replaceChildren();
    tags.forEach((t, i) => {
      const tag = el("span", "tag", t);
      const x = el("button", "chip-x", "×");
      x.type = "button";
      x.setAttribute("aria-label", "remove " + t);
      x.addEventListener("click", () => {
        tags.splice(i, 1);
        draw();
        onChange();
      });
      tag.append(x);
      list.append(tag);
    });
    sugg.replaceChildren();
    const q = input.value.trim().toLowerCase();
    for (const v of all) {
      if (tags.includes(v) || (q && !v.toLowerCase().includes(q))) continue;
      const b = el("button", "suggestion", v);
      b.type = "button";
      // Keep focus in the input, so its blur doesn't add the typed text as a
      // tag and redraw this button away before the click lands.
      b.addEventListener("mousedown", (e) => e.preventDefault());
      b.addEventListener("click", () => addTag(v));
      sugg.append(b);
    }
  };
  input.addEventListener("input", draw);
  input.addEventListener("keydown", (e) => {
    if ((e.key === "Enter" || e.key === ",") && input.value.trim()) {
      e.preventDefault();
      addTag(input.value);
    } else if (e.key === "Backspace" && !input.value && tags.length) {
      tags.pop();
      draw();
      onChange();
    }
  });
  input.addEventListener("change", () => {
    if (input.value.trim()) addTag(input.value);
  });
  box.append(list, input, sugg);
  draw();
}

// ---- filter triples ----
// Each sidebar row is a triple {path, op, tags, value}. Its filter is set
// once the server has accepted it, and state.filters is exactly those
// filters, in row order. A triple the server rejects shows the message on
// its row and stays out of the query. The last row is always empty.

let triples = [];

const isMulti = (op) => op === "in" || op === "not-in";
const noValue = (op) => op === "empty" || op === "not-empty";

function newTriple(f) {
  const t = { path: "", op: "", tags: [], value: "", filter: null, error: "", row: null };
  if (!f) return t;
  t.path = f.field;
  t.op = f.op;
  t.filter = f;
  const vals = Array.isArray(f.values) ? f.values.map(String) : [];
  if (isMulti(f.op)) t.tags = vals;
  else {
    t.value = vals[0] ?? "";
    const field = fieldByPath(f.field);
    if (field && field.type === "timestamp") t.value = localInputValue(t.value) || t.value;
  }
  return t;
}

// clearTriples drops every row at once, so no in-flight commit can write an
// old stream's filter back while the new schema loads.
function clearTriples() {
  triples = [];
  $("triples").replaceChildren();
}

function syncFilters() {
  state.filters = triples.filter((t) => t.filter).map((t) => t.filter);
}

// rebuildTriples redraws the rows from state.filters; call it once the
// stream's schema is loaded.
function rebuildTriples() {
  triples = state.filters.map(newTriple);
  $("triples").replaceChildren(...triples.map(tripleRow));
  ensureEmptyTriple();
}

function ensureEmptyTriple() {
  const last = triples[triples.length - 1];
  if (last && !last.path) return;
  const t = newTriple();
  triples.push(t);
  $("triples").append(tripleRow(t));
}

// candidate is the filter t describes, or null while it is incomplete.
function candidate(t) {
  if (!t.path || !t.op) return null;
  if (noValue(t.op)) return { field: t.path, op: t.op, values: [] };
  if (isMulti(t.op)) return t.tags.length ? { field: t.path, op: t.op, values: t.tags.slice() } : null;
  let v = t.value;
  if (v === "") return null;
  const field = fieldByPath(t.path);
  if (field && field.type === "timestamp") {
    const d = new Date(v); // datetime-local is local time
    if (!isNaN(d.getTime())) v = d.toISOString();
  }
  return { field: t.path, op: t.op, values: [v] };
}

function sameFilter(a, b) {
  return !!a && !!b && JSON.stringify(a) === JSON.stringify(b);
}

function setTripleError(t, msg) {
  t.error = msg;
  const e = t.row.querySelector(".t-error");
  e.textContent = msg;
  e.hidden = !msg;
  t.row.classList.toggle("rejected", !!msg);
}

// commitTriple brings t's filter in line with its inputs: it validates a
// complete triple by running the query with it, and drops an incomplete one.
async function commitTriple(t) {
  const f = candidate(t);
  if (!f) {
    setTripleError(t, "");
    if (t.filter) {
      t.filter = null;
      syncFilters();
      runQuery();
    }
    return;
  }
  if (sameFilter(t.filter, f)) return;
  // Validate the triple on its own, so errors of the whole query (bucket
  // count, plot, aggregates) are not blamed on it.
  try {
    await api("POST", "/api/query", { stream: state.stream, source: state.source, service: state.service, range: state.range, filters: [f], limit: 1 });
  } catch (err) {
    if (!triples.includes(t) || !sameFilter(candidate(t), f)) return; // edited meanwhile
    const had = !!t.filter;
    t.filter = null;
    syncFilters();
    setTripleError(t, err.detail || err.message);
    if (had) runQuery();
    else saveURL();
    return;
  }
  if (!triples.includes(t) || !sameFilter(candidate(t), f)) return;
  t.filter = f;
  setTripleError(t, "");
  syncFilters();
  runQuery();
}

function removeTriple(t) {
  const i = triples.indexOf(t);
  if (i < 0) return;
  triples.splice(i, 1);
  t.row.remove();
  ensureEmptyTriple();
  if (t.filter) {
    syncFilters();
    runQuery();
  }
}

function pickTripleField(t, path) {
  if (!path && t !== triples[triples.length - 1]) {
    removeTriple(t); // keep exactly one empty triple, at the end
    return;
  }
  const had = !!t.filter;
  t.path = path;
  t.tags = [];
  t.value = "";
  t.filter = null;
  const field = fieldByPath(path);
  const ops = field && schemaData && schemaData.ops ? schemaData.ops[field.type] || [] : [];
  t.op = ops[0] || "";
  drawTripleControls(t);
  setTripleError(t, path && !field ? "no field " + path + " in " + state.stream : "");
  if (path) ensureEmptyTriple();
  if (had) {
    syncFilters();
    runQuery();
  }
  if (field) t.row.querySelector(".t-op").focus();
}

function tripleRow(t) {
  const row = el("div", "triple");
  t.row = row;
  const x = el("button", "chip-x t-x", "×");
  x.type = "button";
  x.title = "remove this filter";
  x.setAttribute("aria-label", "remove filter");
  x.addEventListener("click", () => removeTriple(t));
  const op = el("select", "t-op");
  op.setAttribute("aria-label", "operator");
  op.addEventListener("change", () => {
    const was = t.op;
    t.op = op.value;
    if (isMulti(t.op) && !isMulti(was)) t.tags = t.value ? [t.value] : [];
    else if (!isMulti(t.op) && isMulti(was)) t.value = t.tags[0] || "";
    drawValue(t);
    commitTriple(t);
  });
  const err = el("span", "inline-error t-error");
  err.setAttribute("role", "alert");
  err.hidden = true;
  row.append(fieldPicker(t.path, (p) => pickTripleField(t, p)), x, op, el("span", "t-value"), err);
  drawTripleControls(t);
  if (t.error) setTripleError(t, t.error);
  return row;
}

// drawTripleControls fills the operator select and value input for t's field.
function drawTripleControls(t) {
  const row = t.row;
  row.querySelector(".t-x").hidden = !t.path;
  const op = row.querySelector(".t-op");
  op.replaceChildren();
  const field = fieldByPath(t.path);
  const ops = field && schemaData && schemaData.ops ? (schemaData.ops[field.type] || []).slice() : [];
  if (t.op && !ops.includes(t.op)) ops.push(t.op);
  for (const o of ops) {
    const opt = el("option", "", o);
    opt.value = o;
    op.append(opt);
  }
  op.value = t.op;
  op.hidden = !ops.length;
  drawValue(t);
}

function drawValue(t) {
  const box = t.row.querySelector(".t-value");
  box.replaceChildren();
  if (!t.op || noValue(t.op)) return;
  const field = fieldByPath(t.path) || { path: t.path, type: "string" };
  if (isMulti(t.op)) {
    tagInput(box, t.tags, t.path, () => commitTriple(t));
    return;
  }
  let input;
  if (field.type === "bool") {
    input = el("select");
    for (const v of ["", "true", "false"]) {
      const o = el("option", "", v || "value");
      o.value = v;
      input.append(o);
    }
  } else {
    input = el("input");
    if (field.type === "timestamp") {
      input.type = "datetime-local";
      input.step = "0.001";
    } else {
      input.type = "text";
      input.placeholder = field.type === "duration" ? "e.g. 50ms" : "value";
    }
  }
  input.value = t.value;
  input.setAttribute("aria-label", "value");
  input.addEventListener("change", () => {
    t.value = input.value;
    commitTriple(t);
  });
  input.addEventListener("keydown", (e) => {
    if (e.key !== "Enter") return;
    e.preventDefault();
    t.value = input.value;
    commitTriple(t);
  });
  box.append(input);
}

// ---- group by and metrics ----

const METRIC_OPS = ["sum", "avg", "min", "max", "p50", "p90", "p95", "p99", "error_rate"];

// The error rate reads no field: its aggregate is just {op}.
const METRIC_LABELS = { error_rate: "error rate" };
const fieldless = (op) => op === "error_rate";
// ready: the metric row has what its operator needs.
const metricReady = (r) => fieldless(r.op) || !!r.field;
const aggOf = (r) => (fieldless(r.op) ? { op: r.op } : { op: r.op, field: r.field });
const sameAgg = (a, b) => a.op === b.op && (a.field || "") === (b.field || "");

// metricRows are the rows of the Metrics section; state.aggregates holds only
// those with a field chosen.
let metricRows = [];

function clearExplore() {
  clearTriples();
  metricRows = [];
  $("sb-group-body").replaceChildren();
  $("sb-metrics-body").replaceChildren();
  $("sb-plot-body").replaceChildren();
  $("sb-order-body").replaceChildren();
}

function rebuildExplore() {
  rebuildTriples();
  metricRows = state.aggregates.map((a) => ({ op: a.op, field: a.field || "" }));
  drawGroupBy();
  drawMetrics();
  drawPlot();
  drawOrder();
  drawTop();
  drawBucket();
}

function drawGroupBy() {
  const box = $("sb-group-body");
  box.replaceChildren();
  const tags = el("span", "tags");
  state.groupBy.forEach((path, i) => {
    const f = fieldByPath(path);
    const tag = el("span", "tag", (f && f.label) || path);
    tag.title = path;
    const x = el("button", "chip-x", "×");
    x.type = "button";
    x.setAttribute("aria-label", "remove group by " + path);
    x.addEventListener("mousedown", (e) => e.preventDefault());
    x.addEventListener("click", () => {
      state.groupBy.splice(i, 1);
      drawGroupBy();
      runQuery();
    });
    tag.append(x);
    tags.append(tag);
  });
  const pick = fieldPicker("", (p) => {
    if (p && fieldByPath(p) && !state.groupBy.includes(p)) {
      state.groupBy.push(p);
      drawGroupBy();
      runQuery();
      return;
    }
    drawGroupBy();
  });
  pick.placeholder = "add a field";
  pick.setAttribute("aria-label", "group by field");
  box.append(tags, pick);
}

function syncAggregates() {
  state.aggregates = metricRows.filter(metricReady).map(aggOf);
  // The plotted metric must stay one of the aggregates, or every query fails.
  const p = state.plot;
  if (p && p.op !== "count" && !state.aggregates.some((a) => sameAgg(a, p))) {
    state.plot = { op: "count" };
  }
  // Likewise the order metric; its direction survives the fallback.
  const o = state.order;
  if (o && o.op !== "count" && !state.aggregates.some((a) => sameAgg(a, o))) {
    state.order = o.asc ? { op: "count", asc: true } : { op: "count" };
  }
  drawPlot();
  drawOrder();
}

const BUCKETS = ["auto", "1s", "5s", "10s", "30s", "1m", "5m", "15m", "1h", "6h", "1d"];

// autoBucket is the size the server chose for "auto" in the last graph.
let autoBucket = "";

const plotKey = (a) => (a && a.op !== "count" ? a.op + "\u0000" + (a.field || "") : "count");

// drawPlot fills the Plot select: count and each aggregate.
function drawPlot() {
  const box = $("sb-plot-body");
  const sel = el("select");
  sel.setAttribute("aria-label", "plotted metric");
  for (const a of [{ op: "count" }, ...state.aggregates]) {
    const o = el("option", "", metricLabel(a));
    o.value = plotKey(a);
    if (a.field) o.title = a.op + " " + a.field;
    sel.append(o);
  }
  sel.value = plotKey(state.plot);
  sel.addEventListener("change", () => {
    const a = state.aggregates.find((x) => plotKey(x) === sel.value);
    state.plot = a ? { ...a } : { op: "count" };
    drawChart();
    runQuery();
  });
  box.replaceChildren(sel);
  drawChart();
}

// drawChart fills the Chart select: lines, or stacked areas when the plotted
// metric can be added up. Otherwise stacked is disabled and lines are drawn,
// but the choice is kept for when a stackable metric is plotted again.
function drawChart() {
  const box = $("sb-chart-body");
  const sel = el("select");
  sel.id = "chart";
  const ok = stackable(state.plot);
  for (const [v, text] of [["line", "line"], ["stacked", "stacked area"]]) {
    const o = el("option", "", text);
    o.value = v;
    if (v === "stacked" && !ok) {
      o.disabled = true;
      o.textContent = "stacked area (count or sum only)";
    }
    sel.append(o);
  }
  sel.value = ok ? state.chart : "line";
  sel.addEventListener("change", () => {
    state.chart = sel.value;
    runQuery();
  });
  box.replaceChildren(sel);
}

// drawTop fills the Top select: how many groups the table shows and the graph
// draws before the rest fold into "other".
function drawTop() {
  const box = $("sb-top-body");
  const sel = el("select");
  sel.id = "top";
  sel.title = "groups shown, ranked by Order by; the graph draws at most " + MAX_GRAPH_TOP;
  const list = TOPS.includes(state.top) ? TOPS : [...TOPS, state.top].sort((a, b) => a - b);
  for (const n of list) {
    const o = el("option", "", String(n));
    o.value = String(n);
    sel.append(o);
  }
  sel.value = String(state.top);
  sel.addEventListener("change", () => {
    state.top = Number(sel.value);
    runQuery();
  });
  box.replaceChildren(sel);
}

// drawOrder fills the Order by control: a metric (count or an aggregate) that
// ranks the groups, and a direction toggle. Groups with no value sort last.
function drawOrder() {
  const box = $("sb-order-body");
  const row = el("div", "metric");
  const sel = el("select", "m-field");
  sel.setAttribute("aria-label", "order groups by");
  for (const a of [{ op: "count" }, ...state.aggregates]) {
    const o = el("option", "", metricLabel(a));
    o.value = plotKey(a);
    if (a.field) o.title = a.op + " " + a.field;
    sel.append(o);
  }
  sel.value = plotKey(state.order);
  const asc = !!(state.order && state.order.asc);
  sel.addEventListener("change", () => {
    const a = state.aggregates.find((x) => plotKey(x) === sel.value);
    const o = a ? { ...a } : { op: "count" };
    if (asc) o.asc = true;
    state.order = o;
    runQuery();
  });
  const dir = el("button", "order-dir", asc ? "↑ asc" : "↓ desc");
  dir.type = "button";
  dir.title = asc ? "smallest first; click for largest first" : "largest first; click for smallest first";
  dir.setAttribute("aria-label", asc ? "ascending; switch to descending" : "descending; switch to ascending");
  dir.addEventListener("click", () => {
    const o = Object.assign({}, state.order);
    if (o.asc) delete o.asc;
    else o.asc = true;
    state.order = o;
    drawOrder();
    runQuery();
  });
  row.append(sel, dir);
  box.replaceChildren(row);
}

// drawBucket fills the Granularity select; "auto" shows the size it chose.
function drawBucket() {
  const box = $("sb-bucket-body");
  const sel = el("select");
  sel.setAttribute("aria-label", "time bucket size");
  const list = BUCKETS.includes(state.bucket) ? BUCKETS : [...BUCKETS, state.bucket];
  for (const b of list) {
    const o = el("option", "", b === "auto" && autoBucket ? "auto (" + autoBucket + ")" : b);
    o.value = b;
    sel.append(o);
  }
  sel.value = state.bucket;
  sel.addEventListener("change", () => {
    state.bucket = sel.value;
    autoBucket = "";
    drawBucket();
    runQuery();
  });
  box.replaceChildren(sel);
}

function numericFields() {
  return schemaFields().filter((f) => NUMERIC.has(f.type));
}

function drawMetrics() {
  const box = $("sb-metrics-body");
  box.replaceChildren();
  const fixed = el("div", "metric");
  fixed.append(el("span", "metric-fixed", "count"));
  box.append(fixed);
  for (const r of metricRows) {
    const row = el("div", "metric");
    const op = el("select", "m-op");
    op.setAttribute("aria-label", "operator");
    for (const o of METRIC_OPS) {
      const opt = el("option", "", METRIC_LABELS[o] || o);
      opt.value = o;
      op.append(opt);
    }
    op.value = r.op;
    op.addEventListener("change", () => {
      const had = metricReady(r);
      r.op = op.value;
      syncAggregates();
      drawMetrics();
      if (had || metricReady(r)) runQuery();
    });
    const fsel = el("select", "m-field");
    fsel.setAttribute("aria-label", "number field");
    const none = el("option", "", "field");
    none.value = "";
    fsel.append(none);
    const fields = numericFields();
    if (r.field && !fields.some((f) => f.path === r.field)) fields.push({ path: r.field, label: "" });
    for (const f of fields) {
      const opt = el("option", "", f.label || f.path);
      opt.value = f.path;
      opt.title = f.path;
      fsel.append(opt);
    }
    fsel.value = r.field;
    fsel.hidden = fieldless(r.op);
    fsel.addEventListener("change", () => {
      r.field = fsel.value;
      syncAggregates();
      runQuery();
    });
    const x = el("button", "chip-x", "×");
    x.type = "button";
    x.setAttribute("aria-label", "remove metric");
    x.addEventListener("click", () => {
      metricRows.splice(metricRows.indexOf(r), 1);
      const had = metricReady(r);
      syncAggregates();
      drawMetrics();
      if (had) runQuery();
    });
    row.append(op, fsel, x);
    box.append(row);
  }
  const add = el("button", "", "+ metric");
  add.type = "button";
  add.addEventListener("click", () => {
    metricRows.push({ op: "avg", field: "" });
    drawMetrics();
  });
  box.append(add);
}

// ---- column menu ----
// Clicking a Samples header label or a sidebar column name opens a menu with
// the column's formatting (saved to the schema), hide, sort and placement.

let hiddenThisQuery = []; // paths hidden in this query, most recent first
let menuPath = "";
let menuAnchor = null;
let labelCommit = null; // saves the menu's label input if it changed

// colInfo is what the page knows of a column: its schema field, else its
// result column, else a bare string field.
function colInfo(path) {
  const f = fieldByPath(path);
  if (f) return f;
  const c = lastResult && lastResultStream === streamKey() && Array.isArray(lastResult.columns)
    ? lastResult.columns.find((x) => x.path === path) : null;
  return { path, type: (c && c.type) || "string", label: (c && c.label) || "" };
}

function setColumns(cols) {
  closeColumnMenu();
  state.columns = cols;
  runQuery();
}

// hideColumn removes path from this query's columns; the last one stays.
function hideColumn(path) {
  const cols = currentColumns().filter((p) => p !== path);
  if (!cols.length) return;
  hiddenThisQuery = [path, ...hiddenThisQuery.filter((p) => p !== path)];
  setColumns(cols);
}

// moveColumnAfter places path (shown or not) immediately right of afterPath.
function moveColumnAfter(path, afterPath) {
  if (path === afterPath) return;
  const cols = currentColumns().filter((p) => p !== path);
  const i = cols.indexOf(afterPath);
  if (i < 0) return;
  cols.splice(i + 1, 0, path);
  setColumns(cols);
}

// columnIcons is the column menu's icon row: hide; move to the start, left,
// right and to the end; then filter, group by, fit width and crop/wrap.
function columnIcons(path, shown) {
  const row = el("div", "menu-icons");
  const i = shown.indexOf(path);
  const last = shown.length - 1;
  const f = fieldByPath(path); // null for a Table-view metric column
  const ro = readOnly();
  const add = (content, tip, disabled, onClick) => {
    const b = el("button", "menu-icon");
    b.type = "button";
    b.title = tip;
    b.setAttribute("aria-label", tip + " (" + path + ")");
    b.disabled = disabled;
    if (typeof content === "string") {
      b.classList.add("glyph");
      b.textContent = content;
    } else b.append(content);
    b.addEventListener("click", onClick);
    row.append(b);
  };
  const gap = () => row.append(el("span", "menu-icons-gap"));
  add(icon("hide"), shown.length <= 1 ? "the last column cannot be hidden" : "hide column", i < 0 || shown.length <= 1, () => hideColumn(path));
  gap();
  add(VCR.first, "move to start", i <= 0, () => moveColumnTo(path, 0));
  add(VCR.left, "move left", i <= 0, () => moveColumnTo(path, i - 1));
  add(VCR.right, "move right", i < 0 || i === last, () => moveColumnTo(path, i + 1));
  add(VCR.last, "move to end", i < 0 || i === last, () => moveColumnTo(path, last));
  gap();
  // The Grouped list's grouping is fixed: no filter or group by from it.
  if (!groupedList()) {
    add(icon("filter"), f ? "filter by this column" : "not a field of the stream", !f, () => filterByColumn(path));
    const grouped = state.groupBy.includes(path);
    add(icon("group"), !f ? "not a field of the stream" : grouped ? "already grouped by this column" : "group by this column", !f || grouped, () => groupByColumn(path));
  }
  const th = $("head").querySelector('th[data-path="' + CSS.escape(path) + '"]');
  const temp = !!(th && th.dataset.temp);
  const sized = temp ? path in tempWidths : !!(f && f.width);
  add(icon("fit"), sized ? "fit width to content" : "width already fits its content", !sized, () => {
    closeColumnMenu();
    fitColumn(path, temp);
  });
  if (f) {
    const tip = ro ? NEED_SERVICE : f.wrap ? "crop long text" : "wrap long text";
    add(icon(f.wrap ? "crop" : "wrap"), tip, ro, () => {
      closeColumnMenu();
      saveFieldFormat(path, { wrap: !f.wrap });
    });
  }
  return row;
}

// fitColumn drops a column's dragged width so it sizes to its content again.
function fitColumn(path, temp) {
  if (temp) {
    delete tempWidths[path];
    applyWidths();
  } else if ((fieldByPath(path) || {}).width) {
    saveFieldFormat(path, { width: 0 });
  }
}

// filterByColumn fills the sidebar's empty filter row with path and puts the
// cursor in its value box.
function filterByColumn(path) {
  closeColumnMenu();
  const t = triples[triples.length - 1];
  if (!t) return;
  setSidebar(true);
  const old = t.row;
  pickTripleField(t, path);
  old.replaceWith(tripleRow(t)); // tripleRow shows path in the field box
  const box = t.row.querySelector(".t-value input, .t-value select") || t.row.querySelector(".t-op");
  if (box) {
    box.scrollIntoView({ block: "nearest" });
    box.focus();
  }
}

// groupByColumn adds path to the group-by fields, switching Samples to the
// Table view, where grouping shows.
function groupByColumn(path) {
  closeColumnMenu();
  if (!state.groupBy.includes(path)) state.groupBy.push(path);
  drawGroupBy();
  if (state.view === "samples" || state.view === "heatmap") setView("table");
  else runQuery();
}

// copyColumnValues copies the column's shown values, one per line.
async function copyColumnValues(path, button) {
  const ths = Array.from($("head").children);
  const i = ths.findIndex((th) => th.dataset.path === path);
  if (i < 0) return;
  const vals = [];
  for (const tr of $("body").children) {
    if (tr.classList.contains("detail")) continue; // an opened record, not a value
    const td = tr.children[i];
    if (td) vals.push(td.textContent);
  }
  try {
    await navigator.clipboard.writeText(vals.join("\n"));
    button.textContent = "✓ Copied " + vals.length.toLocaleString() + (vals.length === 1 ? " value" : " values");
    setTimeout(closeColumnMenu, 700);
  } catch (err) {
    button.textContent = "Copy failed: " + err.message;
  }
}

// moveColumnTo puts shown column path at index to, then reopens the menu on
// its new heading so the arrows can be clicked again.
async function moveColumnTo(path, to) {
  const cols = currentColumns().filter((p) => p !== path);
  cols.splice(to, 0, path);
  closeColumnMenu();
  state.columns = cols;
  await runQuery();
  const label = $("head").querySelector('th[data-path="' + CSS.escape(path) + '"] .th-menu');
  if (label) openColumnMenu(path, label);
}

// alphabetizeColumns orders the shown columns by their last path element,
// ignoring case, then by full path. A shown timestamp column stays first.
function alphabetizeColumns() {
  const key = (p) => p.slice(p.lastIndexOf(".") + 1).toLowerCase();
  const cols = currentColumns().sort((a, b) => {
    if (a === "timestamp" || b === "timestamp") return a === "timestamp" ? -1 : b === "timestamp" ? 1 : 0;
    const ka = key(a);
    const kb = key(b);
    if (ka !== kb) return ka < kb ? -1 : 1;
    return a < b ? -1 : a > b ? 1 : 0;
  });
  setColumns(cols);
}

function closeColumnMenu() {
  if (labelCommit) labelCommit();
  labelCommit = null;
  $("col-menu").hidden = true;
  $("col-sub").hidden = true;
  $("col-menu").replaceChildren();
  $("col-sub").replaceChildren();
  menuPath = "";
  menuAnchor = null;
}

// placeMenu puts a menu below anchor (or to the right of it with side),
// kept inside the viewport.
function placeMenu(menu, anchor, side) {
  menu.hidden = false;
  const r = anchor.getBoundingClientRect();
  const m = menu.getBoundingClientRect();
  let left = side ? r.right + 2 : r.left;
  let top = side ? r.top : r.bottom + 4;
  if (left + m.width > window.innerWidth - 8) left = side ? r.left - m.width - 2 : window.innerWidth - 8 - m.width;
  if (top + m.height > window.innerHeight - 8) top = Math.max(8, window.innerHeight - 8 - m.height);
  menu.style.left = Math.max(8, left) + window.scrollX + "px";
  menu.style.top = top + window.scrollY + "px";
}

function menuButton(text, onClick, disabled) {
  const b = el("button", "menu-item", text);
  b.type = "button";
  b.disabled = !!disabled;
  b.addEventListener("click", onClick);
  return b;
}

function menuRow(label, control) {
  const l = el("label", "menu-row");
  l.append(el("span", "", label), control);
  return l;
}

function openColumnMenu(path, anchorEl) {
  if (lastResultStream !== streamKey()) return;
  if (menuPath === path && menuAnchor === anchorEl) {
    closeColumnMenu();
    return;
  }
  closeColumnMenu();
  closeFavorites();
  menuPath = path;
  menuAnchor = anchorEl;
  const menu = $("col-menu");
  const f = colInfo(path);
  const shown = currentColumns();
  const title = el("div", "menu-title fpath");
  title.append(breakable(path));
  menu.append(title, columnIcons(path, shown));
  const ro = readOnly();
  const fmt = (control) => {
    if (ro) {
      control.disabled = true;
      control.title = NEED_SERVICE;
    }
    return control;
  };
  const vis = fmt(selectOf(VISUALIZERS, f.visualizer || defaultVisualizer(path, f.type), (v) => saveFieldFormat(path, { visualizer: v })));
  menu.append(menuRow("visualizer", vis));
  if (UNIT_TYPES.has(f.type)) {
    menu.append(menuRow("unit", fmt(selectOf(UNITS, f.unit || "", (v) => saveFieldFormat(path, { unit: v })))));
  }
  const label = el("input");
  label.type = "text";
  label.placeholder = path;
  label.value = f.label || "";
  let saved = label.value;
  labelCommit = () => {
    if (label.value === saved) return;
    saved = label.value;
    saveFieldFormat(path, { label: label.value });
  };
  label.addEventListener("keydown", (e) => {
    if (e.key === "Enter") {
      e.preventDefault();
      labelCommit();
    }
  });
  label.addEventListener("blur", () => labelCommit && labelCommit());
  menu.append(menuRow("label", fmt(label)));
  menu.append(el("hr"));
  for (const [text, d] of [["Sort ascending", false], ["Sort descending", true]]) {
    const on = sortedBy(path, d);
    const b = menuButton((on ? "✓ " : "") + text, () => sortBy(path, d));
    b.setAttribute("role", "menuitemcheckbox");
    b.setAttribute("aria-checked", on ? "true" : "false");
    b.classList.toggle("checked", on);
    menu.append(b);
  }
  const sub = menuButton("Add / Move ▸", () => openColumnSubmenu(path, sub));
  sub.setAttribute("aria-haspopup", "true");
  menu.append(sub);
  menu.append(menuButton("Copy values", (e) => copyColumnValues(path, e.currentTarget), !shown.includes(path)));
  placeMenu(menu, anchorEl, false);
}

// sortedBy reports whether the rows are in path's order, descending or not.
// With no sort chosen the server orders by timestamp, newest first (the
// Heatmap list by duration, longest first).
function sortedBy(path, desc) {
  const s = listSort();
  return s.field === path && s.desc === desc;
}

// sortArrows are a column's ascending and descending buttons. The one matching
// the current order is lit (with no sort chosen, the server orders by
// timestamp, newest first); clicking a lit arrow returns to that default.
function sortArrows(path) {
  const box = el("span", "col-sort");
  for (const d of [false, true]) {
    const b = el("button", "sort-btn", d ? "▼" : "▲");
    b.type = "button";
    const on = sortedBy(path, d);
    b.classList.toggle("on", on);
    b.setAttribute("aria-pressed", on ? "true" : "false");
    b.title = on ? "sorted " + (d ? "descending" : "ascending") + "; click for the default order" : "sort " + (d ? "descending" : "ascending");
    b.setAttribute("aria-label", "sort " + path + (d ? " descending" : " ascending"));
    b.addEventListener("click", () => {
      state.sort = on ? { field: "", desc: true } : { field: path, desc: d };
      runQuery();
    });
    box.append(b);
  }
  return box;
}

function sortBy(path, desc) {
  closeColumnMenu();
  state.sort = { field: path, desc };
  runQuery();
}

// openColumnSubmenu lists fields to add, and the other shown columns to move,
// right of path: those hidden in this query first under Add.
function openColumnSubmenu(path, anchorEl) {
  const sub = $("col-sub");
  sub.replaceChildren();
  const shown = currentColumns();
  const add = hiddenThisQuery.filter((p) => !shown.includes(p));
  for (const f of schemaFields()) {
    if (!shown.includes(f.path) && !add.includes(f.path)) add.push(f.path);
  }
  const list = (title, paths, empty) => {
    const box = el("div", "menu-list");
    box.append(el("div", "menu-title", title));
    if (!paths.length) box.append(el("div", "muted menu-empty", empty));
    for (const p of paths) {
      const f = fieldByPath(p);
      const b = menuButton("", () => moveColumnAfter(p, path));
      b.classList.add("fpath");
      b.append(document.createTextNode(p));
      if (f && f.label) b.append(el("span", "flabel", " " + f.label));
      if (hiddenThisQuery.includes(p) && !shown.includes(p)) b.append(el("span", "flabel", " · hidden"));
      b.title = "put " + p + " right of " + path;
      box.append(b);
    }
    return box;
  };
  sub.append(list("Add", add, "every field is shown"), list("Move", shown.filter((p) => p !== path), "no other columns"));
  placeMenu(sub, anchorEl, true);
}

function wireColumns() {
  const menus = [$("col-menu"), $("col-sub")];
  for (const m of menus) {
    // Keep focus (and the label input) from being lost to a button press
    // before its click lands; the label still saves when the menu closes.
    m.addEventListener("mousedown", (e) => {
      if (e.target.closest("button")) e.preventDefault();
    });
  }
  document.addEventListener("click", (e) => {
    if (!menuPath) return;
    if (menus.some((m) => m.contains(e.target))) return;
    if (menuAnchor && menuAnchor.contains(e.target)) return;
    closeColumnMenu();
  });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && menuPath) closeColumnMenu();
  });
  $("col-restore").addEventListener("click", () => {
    setColumns([]);
  });
  $("col-az").addEventListener("click", alphabetizeColumns);
  $("col-all").addEventListener("click", showAllColumns);
}

// ---- saving a column's formatting ----

let formatSaves = Promise.resolve();

// saveFieldFormat merges patch (visualizer, unit, label, width, type) into
// path's field of the current stream's effective schema and saves the whole
// schema, keeping the stream's default columns. Saves run one after another,
// each from the schema the previous one reloaded, so none loses another's edit.
async function saveFieldFormat(path, patch) {
  const stream = state.stream;
  const source = state.source;
  const loaded = () => schemaData && schemaData.schema && schemaData.schema.stream === stream && schemaData.schema.source === source;
  if (!stream || !loaded()) return;
  if (readOnly()) {
    // Under All nothing is saved: a width drag only lasts for this query.
    if ("width" in patch) {
      if (patch.width) tempWidths[path] = patch.width;
      else delete tempWidths[path];
      applyWidths();
    }
    return;
  }
  if (!fieldByPath(path) && Object.keys(patch).length === 1 && (patch.width === 0 || patch.wrap === false)) return;
  // Apply locally first so the page shows the change before the reload, and
  // again when the save runs: an earlier save's reload replaces schemaData.
  const apply = () => {
    const fields = schemaData.schema.fields || (schemaData.schema.fields = []);
    let f = fields.find((x) => x.path === path);
    if (!f) {
      f = { path, type: "string", visualizer: defaultVisualizer(path, "string") };
      fields.push(f);
    }
    Object.assign(f, patch);
    if (!UNIT_TYPES.has(f.type)) f.unit = "";
  };
  apply();
  // Headers and cells are formatted by the server, so a visual change shows
  // once the save lands; a width shows now. Wrap is visual: it changes how
  // much of each value a cell shows.
  const visual = Object.keys(patch).some((k) => k !== "width");
  if (!visual) applyWidths();
  const svc = state.service;
  const run = async () => {
    if (state.service !== svc || state.stream !== stream || state.source !== source || !loaded()) return;
    apply();
    // Only the edited field is sent; the server merges it by path. Field
    // settings are source-wide, so the kind's inferred type and visualizer go
    // along only when this edit set them (a unit edit needs its type).
    const edited = schemaData.schema.fields.find((x) => x.path === path);
    const fb = schemaFieldBody(edited);
    if (!("type" in patch) && !("unit" in patch)) fb.type = "";
    if (!("visualizer" in patch)) fb.visualizer = "";
    const body = { fields: [fb] };
    try {
      await api("PUT", schemaURL(stream, source), body);
      const fresh = await api("GET", schemaURL(stream, source));
      if (state.service !== svc || state.stream !== stream || state.source !== source) return;
      schemaData = fresh;
      fillFieldPaths();
      if (visual) {
        rebuildExplore();
        runQuery();
      } else {
        applyWidths();
      }
    } catch (err) {
      showError("saving the schema: " + (err.detail || err.message));
    }
  };
  formatSaves = formatSaves.then(run);
  return formatSaves;
}

// schemaFieldBody is the saved form of a field: the PUT /api/schema body.
function schemaFieldBody(f) {
  return {
    path: f.path, type: f.type, unit: UNIT_TYPES.has(f.type) ? f.unit || "" : "",
    visualizer: f.visualizer, label: f.label || "", hidden: !!f.hidden, width: f.width || 0,
    wrap: !!f.wrap,
  };
}

// ---- schema panel ----

const TYPES = ["int", "float", "bool", "string", "duration", "timestamp", "json"];
const VISUALIZERS = ["raw", "number", "plain", "link", "local-time", "duration", "severity", "sql", "json"];
const UNITS = ["", "s", "ms", "us", "ns"];
const UNIT_TYPES = new Set(["timestamp", "duration"]);

// defaultVisualizer mirrors schema.DefaultVisualizer in Go.
function defaultVisualizer(path, type) {
  if (path === "pid" || path === "httpRequest.status") return "plain";
  if (["insertId", "trace", "spanId", "hostname"].includes(path) || path.startsWith("labels.")) return "raw";
  if (path === "timestamp" || path === "receiveTimestamp") return "local-time";
  if (path === "severity") return "severity";
  if (path === "httpRequest.latency") return "duration";
  if (path === "httpRequest.requestUrl") return "link";
  switch (type) {
    case "int":
    case "float":
      return "number";
    case "duration":
      return "duration";
    case "timestamp":
      return "local-time";
    case "json":
      return "json";
  }
  return "raw";
}

let schemaDraft = [];

function selectOf(values, current, onChange) {
  const s = el("select");
  for (const v of values) {
    const o = el("option", "", v === "" ? "—" : v);
    o.value = v;
    s.append(o);
  }
  if (!values.includes(current)) {
    const o = el("option", "", current);
    o.value = current;
    s.append(o);
  }
  s.value = current;
  s.addEventListener("change", () => onChange(s.value));
  return s;
}

function renderSchemaTable() {
  const body = $("schema-body");
  body.replaceChildren();
  for (const f of schemaDraft) {
    const tr = el("tr");
    tr.dataset.path = f.path;
    const unitTd = el("td");
    const drawUnit = () => {
      unitTd.replaceChildren();
      if (UNIT_TYPES.has(f.type)) unitTd.append(selectOf(UNITS, f.unit || "", (v) => (f.unit = v)));
    };
    const visSel = selectOf(VISUALIZERS, f.visualizer, (v) => (f.visualizer = v));
    visSel.className = "vis";
    const typeSel = selectOf(TYPES, f.type, (v) => {
      if (f.visualizer === defaultVisualizer(f.path, f.type)) {
        f.visualizer = defaultVisualizer(f.path, v);
        visSel.value = f.visualizer;
      }
      f.type = v;
      if (!UNIT_TYPES.has(v)) f.unit = "";
      drawUnit();
    });
    typeSel.className = "type";
    drawUnit();
    const label = el("input");
    label.type = "text";
    label.value = f.label || "";
    label.addEventListener("input", () => (f.label = label.value));
    const hidden = el("input");
    hidden.type = "checkbox";
    hidden.checked = !!f.hidden;
    hidden.addEventListener("change", () => (f.hidden = hidden.checked));
    const badges = el("td");
    if (f.saved) badges.append(badge("saved", "saved"));
    const td = (node) => {
      const c = el("td");
      c.append(node);
      return c;
    };
    tr.append(el("td", "fpath", f.path), el("td", "muted", f.inferred || ""), td(typeSel), unitTd, td(visSel),
      td(label), td(hidden), el("td", "num", (f.count || 0).toLocaleString()), badges);
    body.append(tr);
  }
}

function fillSchemaDialog() {
  schemaDraft = schemaFields().map((f) => Object.assign({}, f));
  $("schema-title").textContent = "Schema · " + state.stream + (state.source === "otel" ? " (otel)" : "");
  renderSchemaTable();
}

async function openSchema() {
  $("schema-status").textContent = "";
  await loadSchema();
  fillSchemaDialog();
  const d = $("schema-dialog");
  if (!d.open) d.showModal();
}

async function saveSchema() {
  if (readOnly()) return;
  const status = $("schema-status");
  status.className = "status";
  status.textContent = "saving…";
  // Only fields that differ from the loaded schema are sent. Field settings
  // are source-wide, so the kind's inferred type and visualizer go along only
  // when this edit changed them (a unit edit needs its type).
  const loadedFields = new Map(schemaFields().map((f) => [f.path, f]));
  const changed = [];
  for (const d of schemaDraft) {
    const l = loadedFields.get(d.path);
    const b = schemaFieldBody(d);
    const lb = l && schemaFieldBody(l);
    if (lb && JSON.stringify(lb) === JSON.stringify(b)) continue;
    if (lb && lb.type === b.type && lb.unit === b.unit) b.type = "";
    if (lb && lb.visualizer === b.visualizer) b.visualizer = "";
    changed.push(b);
  }
  if (!changed.length) {
    status.textContent = "no changes";
    return;
  }
  const body = { fields: changed };
  try {
    const res = await api("PUT", schemaURL(state.stream, state.source), body);
    await loadSchema();
    rebuildExplore();
    fillSchemaDialog();
    status.textContent = "saved to " + res.path;
    runQuery();
  } catch (err) {
    status.className = "status inline-error";
    status.textContent = err.detail || err.message;
  }
}

function wireSchema() {
  $("schema-open").addEventListener("click", () => openSchema().catch((err) => showError(err.message)));
  $("schema-save").addEventListener("click", saveSchema);
  $("schema-close").addEventListener("click", () => $("schema-dialog").close());
}

// ---- favorites ----

function favStatus(msg, bad) {
  const s = $("fav-status");
  s.className = "status" + (bad ? " inline-error" : "");
  s.textContent = msg || "";
}

// favCache is the selected service's favorites, so selecting a kind doesn't
// refetch them. It is dropped when favorites change.
let favCache = null;

// serviceFavorites returns the selected service's favorites ([] under All or
// when they can't be read).
async function serviceFavorites() {
  if (readOnly()) return [];
  const svc = state.service;
  if (favCache && favCache.service === svc) return favCache.list;
  try {
    const list = (await api("GET", withSvc("/api/favorites"))) || [];
    favCache = { service: svc, list };
    return list;
  } catch {
    return [];
  }
}

// favoriteColumns is the columns of the first favorite of this service that
// names kind name in this mode and has columns, else [] (columns then follow
// the interval).
function favoriteColumns(list, name) {
  const f = list.find((x) => x.query && x.query.stream === name && (x.query.source || "log") === state.source &&
    Array.isArray(x.query.columns) && x.query.columns.length);
  return f ? f.query.columns.slice() : [];
}

async function loadFavorites() {
  const list = $("fav-list");
  list.replaceChildren(el("li", "muted", "loading…"));
  const ro = readOnly();
  let favs;
  try {
    if (ro) {
      // Under All: every present service's favorites, labelled with it.
      const per = await Promise.all(services.map(async (s) => {
        const r = await api("GET", "/api/favorites?service=" + encodeURIComponent(s.key));
        return (r || []).map((f) => Object.assign({ service: s.key }, f));
      }));
      favs = per.flat();
    } else {
      favs = await api("GET", withSvc("/api/favorites"));
      favCache = { service: state.service, list: favs || [] };
    }
  } catch (err) {
    list.replaceChildren();
    favStatus(err.detail || err.message, true);
    return;
  }
  list.replaceChildren();
  if (!favs || !favs.length) list.append(el("li", "muted", "no favorites yet"));
  for (const f of favs || []) {
    const li = el("li", "fav");
    const load = el("button", "fav-load", ro ? f.name + " · " + f.service : f.name);
    load.type = "button";
    load.title = (f.query && f.query.stream ? f.query.stream + (f.query.source === "otel" ? " (otel)" : "") + " · " : "") + "load and run";
    load.addEventListener("click", () => loadFavorite(f));
    const del = el("button", "chip-x", "×");
    del.type = "button";
    del.title = ro ? NEED_SERVICE : "delete";
    del.disabled = ro;
    del.setAttribute("aria-label", "delete favorite " + f.name);
    del.addEventListener("click", () => deleteFavorite(f.name));
    li.append(load, del);
    list.append(li);
  }
}

function openFavorites() {
  closeColumnMenu();
  $("fav-menu").hidden = false;
  $("fav-button").setAttribute("aria-expanded", "true");
  favStatus("");
  loadFavorites();
}

function closeFavorites() {
  $("fav-menu").hidden = true;
  $("fav-button").setAttribute("aria-expanded", "false");
}

async function loadFavorite(f) {
  closeFavorites();
  const before = streamKey();
  const prevStream = state.stream;
  const prevSource = state.source;
  const prevService = state.service;
  applyQuery(JSON.parse(JSON.stringify(f.query || {})));
  // A favorite belongs to the selected service; under All, to its own.
  state.service = f.service || prevService;
  if (f.service) {
    storeService(f.service);
    syncReadOnly();
  }
  if (!state.stream) {
    state.stream = prevStream;
    state.source = prevSource;
  }
  closeColumnMenu();
  if (streamKey() !== before) {
    hiddenThisQuery = [];
    tempWidths = {};
  }
  syncBar();
  syncView();
  reloadKinds(); // the mode or range may have changed
  clearExplore();
  if ((streamKey() !== before || !schemaData) && !(await loadSchema())) return; // a newer selection took over
  rebuildExplore();
  runQuery();
}

async function saveFavorite() {
  if (readOnly()) return;
  const name = prompt("Name for this favorite query:");
  if (name === null || !name.trim()) return;
  const n = name.trim();
  const path = "/api/favorites/" + encodeURIComponent(n);
  const body = JSON.parse(JSON.stringify(state));
  try {
    try {
      await api("PUT", withSvc(path), body);
    } catch (err) {
      if (err.status !== 409) throw err;
      if (!confirm("A favorite named \"" + n + "\" exists. Overwrite it?")) return;
      await api("PUT", withSvc(path + "?overwrite=1"), body);
    }
    favCache = null;
    favStatus("saved \"" + n + "\"");
  } catch (err) {
    favStatus(err.detail || err.message, true);
    showError(err.message);
    return;
  }
  if (!$("fav-menu").hidden) loadFavorites();
}

async function deleteFavorite(name) {
  if (!confirm("Delete favorite \"" + name + "\"?")) return;
  try {
    await api("DELETE", withSvc("/api/favorites/" + encodeURIComponent(name)));
    favCache = null;
    favStatus("deleted \"" + name + "\"");
  } catch (err) {
    favStatus(err.detail || err.message, true);
  }
  loadFavorites();
}

function wireFavorites() {
  $("fav-button").addEventListener("click", (e) => {
    e.stopPropagation();
    if ($("fav-menu").hidden) openFavorites();
    else closeFavorites();
  });
  $("fav-menu").addEventListener("click", (e) => e.stopPropagation());
  document.addEventListener("click", () => closeFavorites());
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") closeFavorites();
  });
  $("fav-save").addEventListener("click", saveFavorite);
  $("table-copy").append(copyTableBar(shownTable, "", { formats: ["md", "csv", "tsv", "json", "jsonl", "yaml", "toon"], fetchText: fetchExport }));
  $("fav-save-menu").addEventListener("click", saveFavorite);
}

// setSidebar slides the controls panel open or closed. The panel stays in
// the DOM, inert while closed so its controls leave the tab order.
function setSidebar(open) {
  document.body.classList.toggle("sidebar-closed", !open);
  $("sidebar").inert = !open;
  const b = $("sb-toggle");
  b.textContent = open ? "◀" : "▶";
  b.title = open ? "hide the controls" : "show the controls";
  b.setAttribute("aria-expanded", open ? "true" : "false");
}

function wireSidebarToggle() {
  let open = true;
  try {
    open = localStorage.getItem(SIDEBAR_KEY) !== "closed";
  } catch {
    // remembering the panel is a per-browser convenience only
  }
  setSidebar(open);
  $("sb-toggle").addEventListener("click", () => {
    const next = document.body.classList.contains("sidebar-closed");
    setSidebar(next);
    try {
      localStorage.setItem(SIDEBAR_KEY, next ? "open" : "closed");
    } catch {
      // as above
    }
  });
}

// rootLabel names the results under the open views in the breadcrumb: the
// view tab and the stream.
function rootLabel() {
  const view = state.view.charAt(0).toUpperCase() + state.view.slice(1);
  const stream = state.stream === ALL_KINDS && state.source === "otel" ? "all kinds" : state.stream;
  return stream ? view + " · " + stream : view;
}

async function init() {
  wireSidebarToggle();
  loadURL();
  wireBar();
  syncView();
  initHeatSplit();
  wireHeatList();
  wireColumns();
  wireLimit();
  wireSchema();
  wireFavorites();
  watchTableShown();
  initNav({ rootLabel });
  initTrace({ api, el, showError, clearError, fillRecordDetail, localTime, formatDuration, severityClass, cancelAggregate, openAggregate });
  initAggregate({ api, el, showError, clearError, formatDuration, getRange: () => state.range, getService: () => state.service });
  await refresh();
  restoreTraceFromURL();
  restoreAggregateFromURL(); // a URL never names both views
  rebuildExplore();
}

init();
