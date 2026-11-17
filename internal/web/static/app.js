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
    closeFilterAdd();
    closeColumns();
    loadSchema().then(runQuery);
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
  $("footer-text").textContent = foot;
  renderColumnNames();
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
  renderChips();
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

// ---- filter chips ----

function filterText(f) {
  if (f.op === "empty" || f.op === "not-empty") return f.field + " " + f.op;
  if (f.op === "in" || f.op === "not-in") return f.field + " " + f.op + " (" + (f.values || []).join(", ") + ")";
  return f.field + " " + f.op + " " + ((f.values || [])[0] ?? "");
}

function renderChips() {
  const box = $("chips");
  box.replaceChildren();
  state.filters.forEach((f, i) => {
    const chip = el("span", "chip");
    chip.append(el("span", "", filterText(f)));
    const x = el("button", "chip-x", "×");
    x.type = "button";
    x.title = "remove this filter";
    x.setAttribute("aria-label", "remove filter " + filterText(f));
    x.addEventListener("click", () => {
      state.filters.splice(i, 1);
      runQuery();
    });
    chip.append(x);
    box.append(chip);
  });
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

const adder = { field: null, tags: [] };

function openFilterAdd() {
  closeColumns();
  $("filter-add").hidden = false;
  $("fa-pick").hidden = false;
  $("fa-form").hidden = true;
  $("fa-error").textContent = "";
  $("fa-search").value = "";
  renderFieldList();
  $("fa-search").focus();
}

function closeFilterAdd() {
  $("filter-add").hidden = true;
  adder.field = null;
  adder.tags = [];
}

function renderFieldList() {
  const q = $("fa-search").value.trim().toLowerCase();
  const list = $("fa-fields");
  list.replaceChildren();
  for (const f of schemaFields()) {
    if (q && !f.path.toLowerCase().includes(q) && !(f.label || "").toLowerCase().includes(q)) continue;
    const li = el("li");
    const b = el("button", "field-item");
    b.type = "button";
    b.dataset.path = f.path;
    b.append(el("span", "fpath", f.path));
    if (f.label) b.append(el("span", "flabel", f.label));
    b.append(badge(f.type, "type"));
    if (f.new) b.append(badge("new", "new"));
    b.addEventListener("click", () => pickField(f));
    li.append(b);
    list.append(li);
  }
  if (!list.children.length) list.append(el("li", "muted", "no matching fields"));
}

function pickField(f) {
  adder.field = f;
  adder.tags = [];
  $("fa-pick").hidden = true;
  $("fa-form").hidden = false;
  $("fa-error").textContent = "";
  $("fa-field").replaceChildren(el("span", "fpath", f.path), badge(f.type, "type"));
  const op = $("fa-op");
  op.replaceChildren();
  const ops = (schemaData && schemaData.ops && schemaData.ops[f.type]) || [];
  for (const o of ops) {
    const opt = el("option", "", o);
    opt.value = o;
    op.append(opt);
  }
  renderValueInput();
  op.focus();
}

function renderValueInput() {
  const f = adder.field;
  const op = $("fa-op").value;
  const box = $("fa-value");
  box.replaceChildren();
  if (op === "empty" || op === "not-empty") return;
  if (op === "in" || op === "not-in") {
    renderTagInput(box);
    return;
  }
  let input;
  if (f.type === "bool") {
    input = el("select");
    for (const v of ["true", "false"]) {
      const o = el("option", "", v);
      o.value = v;
      input.append(o);
    }
  } else if (f.type === "timestamp") {
    input = el("input");
    input.type = "datetime-local";
    input.step = "0.001";
  } else {
    input = el("input");
    input.type = "text";
    input.placeholder = f.type === "duration" ? "e.g. 50ms" : "value";
    input.addEventListener("keydown", (e) => {
      if (e.key === "Enter") {
        e.preventDefault();
        addFilter();
      }
    });
  }
  input.id = "fa-input";
  box.append(input);
  input.focus();
}

function renderTagInput(box) {
  const tags = el("span", "tags");
  const input = el("input");
  input.type = "text";
  input.id = "fa-input";
  input.placeholder = "value, Enter to add";
  const sugg = el("div", "suggestions");
  const all = distinctValues(adder.field.path);

  const addTag = (v) => {
    v = v.trim();
    if (v && !adder.tags.includes(v)) adder.tags.push(v);
    input.value = "";
    draw();
    input.focus();
  };
  const draw = () => {
    tags.replaceChildren();
    adder.tags.forEach((t, i) => {
      const tag = el("span", "tag", t);
      const x = el("button", "chip-x", "×");
      x.type = "button";
      x.setAttribute("aria-label", "remove " + t);
      x.addEventListener("click", () => {
        adder.tags.splice(i, 1);
        draw();
      });
      tag.append(x);
      tags.append(tag);
    });
    sugg.replaceChildren();
    const q = input.value.trim().toLowerCase();
    for (const v of all) {
      if (adder.tags.includes(v) || (q && !v.toLowerCase().includes(q))) continue;
      const b = el("button", "suggestion", v);
      b.type = "button";
      b.addEventListener("click", () => addTag(v));
      sugg.append(b);
    }
  };
  input.addEventListener("input", draw);
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" || e.key === ",") {
      e.preventDefault();
      if (input.value.trim()) addTag(input.value);
      else if (e.key === "Enter") addFilter();
    } else if (e.key === "Backspace" && !input.value && adder.tags.length) {
      adder.tags.pop();
      draw();
    }
  });
  box.append(tags, input, sugg);
  draw();
  input.focus();
}

// filterValues reads the value input for the chosen operator.
function filterValues(f, op) {
  if (op === "empty" || op === "not-empty") return [];
  const input = $("fa-input");
  if (op === "in" || op === "not-in") {
    const vals = adder.tags.slice();
    const pending = input ? input.value.trim() : "";
    if (pending && !vals.includes(pending)) vals.push(pending);
    return vals;
  }
  const v = input ? input.value : "";
  if (f.type === "timestamp") {
    if (!v) return [""];
    const d = new Date(v); // datetime-local is local time
    return [isNaN(d.getTime()) ? v : d.toISOString()];
  }
  return [v];
}

async function addFilter() {
  const f = adder.field;
  if (!f) return;
  const op = $("fa-op").value;
  const filter = { field: f.path, op, values: filterValues(f, op) };
  const q = Object.assign({}, state, { filters: state.filters.concat([filter]) });
  const mine = ++querySeq;
  $("fa-error").textContent = "";
  try {
    const result = await api("POST", "/api/query", q);
    state.filters = q.filters;
    closeFilterAdd();
    saveURL();
    renderChips();
    if (mine !== querySeq) return;
    clearError();
    render(result);
  } catch (err) {
    $("fa-error").textContent = err.detail || err.message;
  }
}

function wireFilters() {
  $("add-filter").addEventListener("click", () => {
    if ($("filter-add").hidden) openFilterAdd();
    else closeFilterAdd();
  });
  $("fa-cancel").addEventListener("click", closeFilterAdd);
  $("fa-search").addEventListener("input", renderFieldList);
  $("fa-op").addEventListener("change", () => {
    $("fa-error").textContent = "";
    renderValueInput();
  });
  $("fa-add").addEventListener("click", addFilter);
  $("filter-add").addEventListener("keydown", (e) => {
    if (e.key === "Escape") closeFilterAdd();
  });
}

// ---- column editor ----

let colDraft = []; // [{path, label, on}]

function openColumns() {
  closeFilterAdd();
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
  closeFilterAdd();
  closeColumns();
  syncBar();
  if (state.stream !== before || !schemaData) await loadSchema();
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
  wireFilters();
  wireColumns();
  wireSchema();
  wireFavorites();
  await refresh();
}

init();
