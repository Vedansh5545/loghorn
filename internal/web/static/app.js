// loghorn web query view. Every piece of log data reaches the DOM through
// textContent or attributes, never innerHTML.

import { drawGraph } from "./graph.js";

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
  view: "samples",
  groupBy: [],
  aggregates: [],
  plot: { op: "count" },
  bucket: "auto",
};

const VIEWS = ["samples", "table", "graph"];

let session = null;
let streams = [];
let lastResult = null;
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
      if (typeof v === "number") return el("span", "", formatDuration(v, column.unit));
      return el("span", "", asText(v));
    case "number":
      if (typeof v === "number") return el("span", "", v.toLocaleString());
      return el("span", "", groupDigits(String(v)));
    case "severity":
      return el("span", "sev sev-" + severityClass(v), asText(v));
    case "json":
      // The server sends JSON cells as text; never re-parse it, because
      // JSON.parse rounds integers above 2^53.
      return truncated(asText(v), 80);
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

// applyQuery replaces state with q (from the URL or a favorite), filling in
// defaults for anything missing.
function applyQuery(q) {
  if (!q || typeof q !== "object") return;
  Object.assign(state, q);
  state.stream = typeof q.stream === "string" ? q.stream : "";
  state.search = typeof q.search === "string" ? q.search : "";
  state.range = Object.assign({ from: DEFAULT_FROM, to: "now" }, q.range || {});
  state.sort = Object.assign({ field: "", desc: true }, q.sort || {});
  state.filters = Array.isArray(q.filters) ? q.filters : [];
  state.columns = Array.isArray(q.columns) ? q.columns : [];
  state.view = VIEWS.includes(q.view) ? q.view : "samples";
  state.groupBy = Array.isArray(q.groupBy) ? q.groupBy : [];
  state.aggregates = Array.isArray(q.aggregates) ? q.aggregates : [];
  state.plot = q.plot && typeof q.plot === "object" && q.plot.op ? q.plot : { op: "count" };
  state.bucket = typeof q.bucket === "string" && q.bucket ? q.bucket : "auto";
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
    state.groupBy = [];
    state.aggregates = [];
    state.plot = { op: "count" };
    closeColumns();
    clearExplore();
    loadSchema().then(() => {
      rebuildExplore();
      runQuery();
    });
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
}

function setView(v) {
  if (!VIEWS.includes(v) || v === state.view) return;
  state.view = v;
  syncView();
  runQuery();
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
    state.groupBy = [];
    state.aggregates = [];
    state.plot = { op: "count" };
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

// renderResult shows a query result in the view it was run for.
function renderResult(result) {
  const view = result.view || "samples";
  $("table").hidden = view === "graph";
  $("graph").hidden = view !== "graph";
  if (view === "table") renderTable(result);
  else if (view === "graph") renderGraph(result);
  else render(result);
}

// metricLabel names an aggregate: "count", or its operator and field.
export function metricLabel(a) {
  if (!a || a.op === "count") return "count";
  const f = fieldByPath(a.field);
  return a.op + " " + ((f && f.label) || a.field);
}

// tableHeaderCell is a column header with a resize grip but no remove button.
function tableHeaderCell(c) {
  const th = el("th");
  th.dataset.path = c.path;
  th.title = c.path + " : " + c.type;
  const inner = el("div", "th-inner");
  inner.append(breakable(c.label || c.path));
  const grip = el("span", "col-resize");
  grip.title = "drag to resize · double-click to fit";
  grip.addEventListener("pointerdown", (e) => startResize(e, th));
  grip.addEventListener("dblclick", () => {
    const w = loadWidths();
    delete w[c.path];
    saveWidths(w);
    applyWidths();
  });
  th.append(inner, grip);
  return th;
}

// renderTable shows grouped results: one row per group, no expansion.
function renderTable(result) {
  const head = $("head");
  head.replaceChildren(...result.columns.map(tableHeaderCell));
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
        td.append(visualize(cell, c));
      }
      tr.append(td);
    });
    body.append(tr);
  }
  const n = result.groups || 0;
  let foot = n.toLocaleString() + " " + (n === 1 ? "group" : "groups") + " · " + result.matched.toLocaleString() + " records match";
  if (result.truncated) foot += " · limit reached";
  $("footer-text").textContent = foot;
  applyWidths();
}

// metricFormat returns a labeller for values of a metric column: durations in
// the column's unit, other numbers grouped with at most three decimals.
function metricFormat(col) {
  if (col && col.type === "duration") return (v) => formatDuration(v, col.unit);
  return (v) => v.toLocaleString(undefined, { maximumFractionDigits: 3 });
}

// renderGraph draws the time series of a graph result.
function renderGraph(result) {
  const g = result.graph || { series: [], starts: [] };
  autoBucket = g.bucket || "";
  drawBucket();
  drawGraph($("graph"), g, metricFormat(g.metric));
  const n = (g.series || []).length;
  let foot = result.matched.toLocaleString() + " records match";
  if (g.bucket && (g.starts || []).length) foot += " · " + g.starts.length.toLocaleString() + " buckets of " + g.bucket;
  if (n) foot += " · " + n + " series";
  $("footer-text").textContent = foot;
}

export function render(result) {
  lastResult = result;
  const head = $("head");
  head.replaceChildren();
  for (const c of result.columns) head.append(headerCell(c, result.columns.length));
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
  $("footer-text").textContent = foot;
  renderColumnNames();
  applyWidths();
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

function trashIcon() {
  const svg = document.createElementNS(SVG, "svg");
  svg.setAttribute("viewBox", "0 0 16 16");
  svg.setAttribute("width", "13");
  svg.setAttribute("height", "13");
  svg.setAttribute("aria-hidden", "true");
  for (const d of ["M2.5 4h11", "M6 4V2.5h4V4", "M4 4l.8 9.5h6.4L12 4", "M6.8 6.5v5", "M9.2 6.5v5"]) {
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

function headerCell(c, count) {
  const th = el("th");
  th.dataset.path = c.path;
  th.title = c.path + " : " + c.type;
  const inner = el("div", "th-inner");
  inner.append(breakable(c.label || c.path));
  const del = el("button", "col-del");
  del.type = "button";
  del.title = "remove this column (restore from Columns)";
  del.setAttribute("aria-label", "remove column " + c.path);
  del.disabled = count <= 1;
  del.append(trashIcon());
  del.addEventListener("click", () => removeColumn(c.path));
  inner.append(del);
  const grip = el("span", "col-resize");
  grip.title = "drag to resize · double-click to fit";
  grip.addEventListener("pointerdown", (e) => startResize(e, th));
  grip.addEventListener("dblclick", () => {
    const w = loadWidths();
    delete w[c.path];
    saveWidths(w);
    applyWidths();
  });
  th.append(inner, grip);
  return th;
}

function removeColumn(path) {
  const cols = currentColumns().filter((p) => p !== path);
  if (!cols.length) return;
  state.columns = cols;
  closeColumns();
  runQuery();
}

// ---- column widths ----
// Widths the user dragged are kept per stream in this browser only.

function widthsKey() {
  return "loghorn.widths." + state.stream;
}

function loadWidths() {
  try {
    const w = JSON.parse(localStorage.getItem(widthsKey()) || "{}");
    return w && typeof w === "object" ? w : {};
  } catch {
    return {};
  }
}

function saveWidths(w) {
  try {
    if (Object.keys(w).length) localStorage.setItem(widthsKey(), JSON.stringify(w));
    else localStorage.removeItem(widthsKey());
  } catch {
    // storage unavailable: widths last until reload
  }
}

// applyWidths lays the table out from its content, then, if any column has a
// dragged width, freezes every column at its width (dragged or natural) with
// a fixed table layout so a column can be narrower than its content.
function applyWidths(extra) {
  const table = $("table");
  const ths = Array.from($("head").children);
  const w = Object.assign(loadWidths(), extra || {});
  table.classList.remove("fixed");
  table.style.width = "";
  for (const th of ths) th.style.width = "";
  renderRestore();
  if (!ths.some((th) => th.dataset.path in w)) return;
  const natural = ths.map((th) => th.getBoundingClientRect().width);
  let total = 0;
  ths.forEach((th, i) => {
    const px = th.dataset.path in w ? w[th.dataset.path] : natural[i];
    th.style.width = px + "px";
    total += px;
  });
  table.style.width = total + "px";
  table.classList.add("fixed");
}

const MIN_COL = 40;

function startResize(e, th) {
  if (e.button !== 0) return;
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
    const w = loadWidths();
    w[path] = width;
    saveWidths(w);
    renderRestore();
  };
  grip.addEventListener("pointermove", move);
  grip.addEventListener("pointerup", up);
  grip.addEventListener("pointercancel", up);
}

// renderRestore shows the restore button while the columns or their widths
// differ from the defaults.
function renderRestore() {
  $("col-restore").hidden = !state.columns.length && !Object.keys(loadWidths()).length;
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
    renderResult(result);
  } catch (err) {
    if (mine === querySeq) showError(err.message);
  }
}

async function refresh() {
  try {
    await loadSession();
    syncBar();
    await loadSchema();
  } catch (err) {
    showError(err.message);
    return;
  }
  runQuery();
}

// ---- schema of the current stream ----

async function loadSchema() {
  schemaData = null;
  if (!state.stream) return;
  try {
    schemaData = await api("GET", "/api/schema?stream=" + encodeURIComponent(state.stream));
  } catch (err) {
    showError(err.message);
  }
  fillFieldPaths();
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
  if (lastResult && Array.isArray(lastResult.columns)) return lastResult.columns.map((c) => c.path);
  if (state.columns.length) return state.columns.slice();
  return schemaData && schemaData.schema && Array.isArray(schemaData.schema.columns) ? schemaData.schema.columns.slice() : [];
}

function renderColumnNames() {
  const box = $("colnames");
  box.replaceChildren();
  const cols = lastResult ? lastResult.columns : [];
  cols.forEach((c, i) => {
    if (i) box.append(el("span", "sep", " · "));
    const s = el("span", "", c.label || c.path);
    s.title = c.path;
    box.append(s);
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
  const filters = triples.map((x) => (x === t ? f : x.filter)).filter(Boolean);
  const q = Object.assign({}, state, { filters });
  const mine = ++querySeq;
  let result;
  try {
    result = await api("POST", "/api/query", q);
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
  if (mine !== querySeq) {
    runQuery(); // a newer query ran without this filter
    return;
  }
  saveURL();
  clearError();
  renderResult(result);
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

const METRIC_OPS = ["sum", "avg", "min", "max", "p50", "p95", "p99"];

// metricRows are the rows of the Metrics section; state.aggregates holds only
// those with a field chosen.
let metricRows = [];

function clearExplore() {
  clearTriples();
  metricRows = [];
  $("sb-group-body").replaceChildren();
  $("sb-metrics-body").replaceChildren();
  $("sb-plot-body").replaceChildren();
}

function rebuildExplore() {
  rebuildTriples();
  metricRows = state.aggregates.map((a) => ({ op: a.op, field: a.field || "" }));
  drawGroupBy();
  drawMetrics();
  drawPlot();
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
  state.aggregates = metricRows.filter((r) => r.field).map((r) => ({ op: r.op, field: r.field }));
  // The plotted metric must stay one of the aggregates, or every query fails.
  const p = state.plot;
  if (p && p.op !== "count" && !state.aggregates.some((a) => a.op === p.op && a.field === p.field)) {
    state.plot = { op: "count" };
  }
  drawPlot();
}

const BUCKETS = ["auto", "1s", "5s", "10s", "30s", "1m", "5m", "15m", "1h", "6h", "1d"];

// autoBucket is the size the server chose for "auto" in the last graph.
let autoBucket = "";

const plotKey = (a) => (a && a.op !== "count" ? a.op + "\u0000" + a.field : "count");

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
    state.plot = a ? { op: a.op, field: a.field } : { op: "count" };
    runQuery();
  });
  box.replaceChildren(sel);
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
      const opt = el("option", "", o);
      opt.value = o;
      op.append(opt);
    }
    op.value = r.op;
    op.addEventListener("change", () => {
      r.op = op.value;
      syncAggregates();
      if (r.field) runQuery();
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
      const had = !!r.field;
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

// ---- column editor ----

let colDraft = []; // [{path, label, on}]

function openColumns() {
  const shown = currentColumns();
  const fields = schemaFields();
  colDraft = shown.map((p) => ({ path: p, label: (fieldByPath(p) || {}).label || "", on: true }));
  for (const f of fields) {
    if (!shown.includes(f.path)) colDraft.push({ path: f.path, label: f.label || "", on: false, hidden: f.hidden });
  }
  renderColList();
  $("columns-panel").hidden = false;
}

function closeColumns() {
  $("columns-panel").hidden = true;
}

let dragFrom = -1;

function moveCol(from, to) {
  if (to < 0 || to >= colDraft.length || from === to) return;
  const [c] = colDraft.splice(from, 1);
  colDraft.splice(to, 0, c);
  renderColList();
}

function renderColList() {
  const list = $("col-list");
  list.replaceChildren();
  colDraft.forEach((c, i) => {
    const li = el("li", "col-item");
    li.draggable = true;
    li.dataset.path = c.path;
    li.addEventListener("dragstart", (e) => {
      dragFrom = i;
      e.dataTransfer.effectAllowed = "move";
      e.dataTransfer.setData("text/plain", c.path);
    });
    li.addEventListener("dragover", (e) => e.preventDefault());
    li.addEventListener("drop", (e) => {
      e.preventDefault();
      moveCol(dragFrom, i);
    });
    const label = el("label");
    const cb = el("input");
    cb.type = "checkbox";
    cb.checked = c.on;
    cb.addEventListener("change", () => (c.on = cb.checked));
    label.append(cb, el("span", "fpath", c.path));
    if (c.label) label.append(el("span", "flabel", c.label));
    if (c.hidden) label.append(badge("hidden"));
    const up = el("button", "mv", "↑");
    up.type = "button";
    up.setAttribute("aria-label", "move " + c.path + " up");
    up.disabled = i === 0;
    up.addEventListener("click", () => moveCol(i, i - 1));
    const down = el("button", "mv", "↓");
    down.type = "button";
    down.setAttribute("aria-label", "move " + c.path + " down");
    down.disabled = i === colDraft.length - 1;
    down.addEventListener("click", () => moveCol(i, i + 1));
    li.append(el("span", "grip", "⋮⋮"), label, up, down);
    list.append(li);
  });
}

function wireColumns() {
  $("edit-columns").addEventListener("click", () => {
    if ($("columns-panel").hidden) openColumns();
    else closeColumns();
  });
  $("col-cancel").addEventListener("click", closeColumns);
  $("col-apply").addEventListener("click", () => {
    const cols = colDraft.filter((c) => c.on).map((c) => c.path);
    if (!cols.length) return;
    state.columns = cols;
    closeColumns();
    runQuery();
  });
  $("col-default").addEventListener("click", () => {
    state.columns = [];
    closeColumns();
    runQuery();
  });
  $("col-restore").addEventListener("click", () => {
    state.columns = [];
    saveWidths({});
    closeColumns();
    runQuery();
  });
}

// ---- schema panel ----

const TYPES = ["int", "float", "bool", "string", "duration", "timestamp", "json"];
const VISUALIZERS = ["raw", "number", "local-time", "duration", "severity", "sql", "json"];
const UNITS = ["", "s", "ms", "us", "ns"];
const UNIT_TYPES = new Set(["timestamp", "duration"]);

function defaultVisualizer(path, type) {
  if (path === "severity") return "severity";
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
    if (f.new) badges.append(badge("new", "new"));
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
  $("schema-title").textContent = "Schema · " + state.stream;
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
  const status = $("schema-status");
  status.className = "status";
  status.textContent = "saving…";
  const cols = state.columns.length ? state.columns.slice() : ((schemaData && schemaData.schema.columns) || []);
  const body = {
    stream: state.stream,
    columns: cols,
    fields: schemaDraft.map((f) => ({
      path: f.path, type: f.type, unit: UNIT_TYPES.has(f.type) ? f.unit || "" : "",
      visualizer: f.visualizer, label: f.label || "", hidden: !!f.hidden,
    })),
  };
  try {
    const res = await api("PUT", "/api/schema?stream=" + encodeURIComponent(state.stream), body);
    await loadSchema();
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

async function loadFavorites() {
  const list = $("fav-list");
  list.replaceChildren(el("li", "muted", "loading…"));
  let favs;
  try {
    favs = await api("GET", "/api/favorites");
  } catch (err) {
    list.replaceChildren();
    favStatus(err.detail || err.message, true);
    return;
  }
  list.replaceChildren();
  if (!favs || !favs.length) list.append(el("li", "muted", "no favorites yet"));
  for (const f of favs || []) {
    const li = el("li", "fav");
    const load = el("button", "fav-load", f.name);
    load.type = "button";
    load.title = (f.query && f.query.stream ? f.query.stream + " · " : "") + "load and run";
    load.addEventListener("click", () => loadFavorite(f));
    const del = el("button", "chip-x", "×");
    del.type = "button";
    del.title = "delete";
    del.setAttribute("aria-label", "delete favorite " + f.name);
    del.addEventListener("click", () => deleteFavorite(f.name));
    li.append(load, del);
    list.append(li);
  }
}

function openFavorites() {
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
  const before = state.stream;
  applyQuery(JSON.parse(JSON.stringify(f.query || {})));
  if (!state.stream) state.stream = before;
  closeColumns();
  syncBar();
  syncView();
  clearExplore();
  if (state.stream !== before || !schemaData) await loadSchema();
  rebuildExplore();
  runQuery();
}

async function saveFavorite() {
  const name = prompt("Name for this favorite query:");
  if (name === null || !name.trim()) return;
  const n = name.trim();
  const path = "/api/favorites/" + encodeURIComponent(n);
  const body = JSON.parse(JSON.stringify(state));
  try {
    try {
      await api("PUT", path, body);
    } catch (err) {
      if (err.status !== 409) throw err;
      if (!confirm("A favorite named \"" + n + "\" exists. Overwrite it?")) return;
      await api("PUT", path + "?overwrite=1", body);
    }
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
    await api("DELETE", "/api/favorites/" + encodeURIComponent(name));
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
  $("fav-save-menu").addEventListener("click", saveFavorite);
}

async function init() {
  loadURL();
  wireBar();
  syncView();
  wireColumns();
  wireSchema();
  wireFavorites();
  await refresh();
  rebuildExplore();
}

init();
