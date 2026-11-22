// loghorn trace waterfall: one trace's spans as a tree of timed bars. Every
// piece of log data reaches the DOM through textContent or attributes, never
// innerHTML. app.js passes its helpers to initTrace, so this module does not
// import app.js.

const $ = (id) => document.getElementById(id);

let d = null; // deps from initTrace

// tv is the view's state. data is the /api/trace answer on show, else null.
const tv = {
  data: null,
  selected: null, // node id
  collapsed: new Set(), // node ids
  logs: "tree",
  critical: false,
  sort: { col: -1, key: "", dir: 1 }, // self-time table; key "" is the order served
  find: { q: "", matches: [], i: -1, cur: null }, // cur: the matched node id last gone to
};

let traceSeq = 0; // the newest openTrace; older answers are dropped
let byId = new Map(); // node id -> node of tv.data
let kidsOf = new Map(); // node id -> child nodes, in order
let restoreParams = null; // the page URL's {trace, span}, until restored

// HISTORY_MARK is the history.state of an entry openTrace pushed, so Close
// can go Back to the table instead of adding another entry.
const HISTORY_MARK = { loghornTrace: 1 };

const GROUPS = ["db", "http", "rpc", "messaging", "gen_ai", "span"];

// typeGroup maps a span.type to its colour group.
function typeGroup(type) {
  const t = String(type || "");
  if (t === "db" || t.startsWith("db.")) return "db";
  if (t === "http" || t.startsWith("http.")) return "http";
  if (t === "rpc" || t.startsWith("rpc.")) return "rpc";
  if (t === "messaging" || t.startsWith("messaging.")) return "messaging";
  if (t === "gen_ai" || t.startsWith("gen_ai.")) return "gen_ai";
  return "span";
}

const isOpen = () => !$("trace").hidden && tv.data !== null;

const LOGS_KEY = "loghorn.traceLogs";

// isLeaf reports whether a node is a log line or a span event: a point in
// time that the Logs & events toggle shows or hides.
const isLeaf = (n) => n.kind === "log" || n.kind === "event";

// shown reports whether a node gets a row: logs and events only when they are
// in the tree.
const shown = (n) => !isLeaf(n) || tv.logs === "tree";

function selectedNode() {
  return tv.selected ? byId.get(tv.selected) || null : null;
}

// traceURLParams is the URL suffix of the open view: "&trace=<id>", plus
// "&span=<id>" when a span is selected; "" when the view is closed. Before
// restoreTraceFromURL ran it keeps the page URL's parameters.
export function traceURLParams() {
  let trace = "";
  let span = "";
  if (isOpen()) {
    trace = tv.data.traceId;
    const n = selectedNode();
    if (n && n.kind === "span") span = n.id;
  } else if (restoreParams) {
    trace = restoreParams.trace || "";
    span = restoreParams.span || "";
  }
  if (!trace) return "";
  let s = "&trace=" + encodeURIComponent(trace);
  if (span) s += "&span=" + encodeURIComponent(span);
  return s;
}

// urlWith is the page URL with the table's q and params (a traceURLParams
// string) and nothing else.
function urlWith(params) {
  const raw = new URLSearchParams(location.search).get("q");
  let s = (raw === null ? "" : "?q=" + encodeURIComponent(raw)) + params;
  if (s.startsWith("&")) s = "?" + s.slice(1);
  return s || location.pathname;
}

// writeURL pushes (push) or replaces the history entry with the view's URL.
function writeURL(push) {
  const url = urlWith(traceURLParams());
  if (push) history.pushState(HISTORY_MARK, "", url);
  else if (location.search !== url) history.replaceState(history.state, "", url);
}

function urlParams() {
  const p = new URLSearchParams(location.search);
  return { trace: p.get("trace") || "", span: p.get("span") || "" };
}

export function initTrace(deps) {
  d = deps;
  const p = urlParams();
  if (p.trace || p.span) restoreParams = p;
  $("trace-close").addEventListener("click", closeTrace);
  $("trace-reload").addEventListener("click", reloadTrace);
  try {
    if (localStorage.getItem(LOGS_KEY) === "hidden") tv.logs = "hidden";
  } catch {}
  $("trace-logs").addEventListener("click", () => {
    tv.logs = tv.logs === "tree" ? "hidden" : "tree";
    try {
      localStorage.setItem(LOGS_KEY, tv.logs);
    } catch {}
    const sel = selectedNode();
    if (tv.logs === "hidden" && sel && isLeaf(sel)) {
      tv.selected = null;
      $("trace-detail").replaceChildren();
      writeURL(false);
    }
    renderTrace();
  });
  const find = $("trace-find");
  find.addEventListener("input", () => {
    tv.find.q = find.value;
    tv.find.cur = null;
    refreshFind();
    if (tv.find.matches.length) goToMatch(0);
    else renderTrace();
  });
  find.addEventListener("keydown", (e) => {
    if (e.key === "Enter") {
      e.preventDefault();
      stepMatch(e.shiftKey ? -1 : 1);
    } else if (e.key === "Escape" && find.value) {
      e.preventDefault(); // the document handler then leaves the view open
      find.value = "";
      tv.find.q = "";
      tv.find.cur = null;
      renderTrace();
    }
  });
  for (const [id, dir] of [["trace-prev", -1], ["trace-next", 1]]) {
    const b = $(id);
    b.addEventListener("mousedown", (e) => e.preventDefault());
    b.addEventListener("click", () => stepMatch(dir));
  }
  $("trace-critical").addEventListener("click", () => {
    tv.critical = !tv.critical;
    renderTrace();
  });
  $("trace-collapse").addEventListener("click", () => {
    tv.collapsed = new Set();
    for (const n of tv.data ? tv.data.nodes : []) if (shownKids(n).length) tv.collapsed.add(n.id);
    renderTrace();
  });
  $("trace-expand").addEventListener("click", () => {
    tv.collapsed = new Set();
    renderTrace();
  });
  document.addEventListener("keydown", (e) => {
    if (e.key !== "Escape" || !isOpen() || e.defaultPrevented) return;
    if (document.querySelector("dialog[open]")) return;
    const find = $("trace-find");
    if (find && find.value) return; // Escape clears the find box first
    const t = e.target;
    if (t !== find && t instanceof HTMLElement && t.matches("input, select, textarea")) return;
    closeTrace();
  });
  window.addEventListener("popstate", () => {
    const q = urlParams();
    if (q.trace || q.span) {
      openTrace(q, { push: false });
    } else {
      ++traceSeq;
      hideView();
    }
  });
}

// restoreTraceFromURL opens the view the page URL names, once, after the
// table's startup.
export function restoreTraceFromURL() {
  const p = restoreParams;
  if (!p) return;
  openTrace(p, { push: false }).finally(() => {
    restoreParams = null;
  });
}

// openTrace loads a trace by trace ID, or by span ID (which also selects that
// span), and shows it in place of the table. push adds a history entry; else
// the current one is replaced. A newer call supersedes this one.
export async function openTrace({ trace, span }, { push = true } = {}) {
  const mine = ++traceSeq;
  const q = trace ? "trace=" + encodeURIComponent(trace) : "span=" + encodeURIComponent(span || "");
  let data;
  try {
    data = await d.api("GET", "/api/trace?" + q);
  } catch (err) {
    if (mine !== traceSeq) return;
    d.showError(err.message);
    hideView();
    if (!push && (urlParams().trace || urlParams().span)) history.replaceState(history.state, "", urlWith(""));
    return;
  }
  if (mine !== traceSeq) return;
  restoreParams = null;
  d.clearError();
  const same = tv.data && tv.data.traceId === data.traceId;
  const prev = tv.selected;
  setData(data);
  if (!same) tv.collapsed = new Set();
  let sel = null;
  if (span) {
    const norm = String(span).trim().toLowerCase();
    sel = byId.get(norm) || byId.get(span) || byId.get("missing:" + norm) || null;
  }
  else if (same && prev && byId.has(prev)) sel = byId.get(prev);
  tv.selected = sel ? sel.id : null;
  if (sel) expandAncestors(sel);
  showView();
  renderTrace();
  writeURL(push);
  if (sel) selectNode(sel.id, { scroll: true });
  else $("trace-detail").replaceChildren();
}

function reloadTrace() {
  if (!tv.data) return;
  const n = selectedNode();
  openTrace({ trace: tv.data.traceId, span: n && n.kind === "span" ? n.id : "" }, { push: false });
}

// closeTrace returns to the table: Back when openTrace pushed this entry,
// else the URL loses its trace parameters in place.
function closeTrace() {
  ++traceSeq;
  if (!isOpen()) return;
  if (history.state && history.state.loghornTrace) {
    history.back(); // popstate hides the view
    return;
  }
  hideView();
  history.replaceState(history.state, "", urlWith(""));
}

function showView() {
  $("bar").hidden = true;
  $("results").hidden = true;
  $("footer").hidden = true;
  $("trace").hidden = false;
}

function hideView() {
  $("trace").hidden = true;
  $("bar").hidden = false;
  $("results").hidden = false;
  $("footer").hidden = false;
  tv.data = null;
  tv.selected = null;
  byId = new Map();
  kidsOf = new Map();
  $("trace-rows").replaceChildren();
  $("trace-detail").replaceChildren();
}

function setData(data) {
  tv.data = data;
  tv.sort = { col: -1, key: "", dir: 1 };
  byId = new Map();
  kidsOf = new Map();
  for (const n of data.nodes || []) {
    byId.set(n.id, n);
    if (!n.parent) continue;
    if (!kidsOf.has(n.parent)) kidsOf.set(n.parent, []);
    kidsOf.get(n.parent).push(n);
  }
}

// shownKids are a node's children that get rows.
function shownKids(n) {
  return (kidsOf.get(n.id) || []).filter(shown);
}

function expandAncestors(n) {
  for (let p = byId.get(n.parent); p; p = byId.get(p.parent)) tv.collapsed.delete(p.id);
}

// visibleNodes are the nodes, in tree order, that get a row: shown kinds
// whose ancestors are all expanded.
function visibleNodes() {
  const out = [];
  let skipBelow = Infinity; // depth of a collapsed node whose subtree is skipped
  for (const n of tv.data.nodes || []) {
    if (n.depth > skipBelow) continue;
    skipBelow = Infinity;
    if (!shown(n)) continue;
    out.push(n);
    if (tv.collapsed.has(n.id)) skipBelow = n.depth;
  }
  return out;
}

const fmtMs = (ms) => d.formatDuration(ms / 1000, "ms");

// axisStep is the smallest 1/2/5 × 10ⁿ ms step giving at most 10 intervals.
function axisStep(D) {
  let p = Math.pow(10, Math.floor(Math.log10(D / 10)));
  for (;;) {
    for (const m of [1, 2, 5]) if (D / (m * p) <= 10) return m * p;
    p *= 10;
  }
}

function plural(n, word) {
  return n.toLocaleString() + " " + word + (n === 1 ? "" : "s");
}

// findMatches are the ids, in pre-order, of the spans whose name contains the
// find text (case-insensitive) and, with logs and events in the tree, the logs
// whose message does and the events whose name or exception message does.
function findMatches() {
  const q = tv.find.q.trim().toLowerCase();
  if (!q || !tv.data) return [];
  const out = [];
  for (const n of tv.data.nodes || []) {
    let text = "";
    if (n.kind === "span") text = n.name;
    else if (n.kind === "log" && tv.logs === "tree") text = n.message;
    else if (n.kind === "event" && tv.logs === "tree") text = (n.name || "") + "\n" + (n.message || "");
    if (text && text.toLowerCase().includes(q)) out.push(n.id);
  }
  return out;
}

// refreshFind recomputes the matches and the count label.
function refreshFind() {
  const f = tv.find;
  f.matches = findMatches();
  f.i = f.cur ? f.matches.indexOf(f.cur) : -1;
  const count = $("trace-count");
  if (!f.q.trim()) count.textContent = "";
  else if (!f.matches.length) count.textContent = "no matches";
  else count.textContent = (f.i < 0 ? "–" : f.i + 1) + " of " + f.matches.length;
  $("trace-find").placeholder = tv.logs === "tree" ? "find spans, logs and events" : "find spans";
}

function goToMatch(i) {
  const f = tv.find;
  if (!f.matches.length) return;
  f.i = i;
  f.cur = f.matches[i];
  selectNode(f.cur, { scroll: true });
  renderTrace(); // marks the matches and the count
  const row = [...$("trace-rows").children].find((r) => r.dataset.id === f.cur);
  if (row) row.scrollIntoView({ block: "center" });
}

// stepMatch goes to the next (dir 1) or previous (-1) match, wrapping.
function stepMatch(dir) {
  const f = tv.find;
  if (!f.matches.length) return;
  const m = f.matches.length;
  goToMatch(f.i < 0 ? (dir > 0 ? 0 : m - 1) : (f.i + dir + m) % m);
}

// renderTrace redraws the header, legend, axis and rows.
function renderTrace() {
  const data = tv.data;
  if (!data) return;
  refreshFind();
  $("trace").classList.toggle("critical-on", tv.critical);
  $("trace-critical").setAttribute("aria-pressed", tv.critical ? "true" : "false");
  $("trace-logs").textContent = tv.logs === "tree" ? "Logs & events: in tree" : "Logs & events: hidden";
  const matched = new Set(tv.find.matches);
  const runOf = new Map(); // span id -> its run; first span id -> its run
  const runFirst = new Map();
  for (const r of data.serialRuns || []) {
    runFirst.set(r.first, r);
    for (const id of r.spans || []) runOf.set(id, r);
  }
  const D = Math.max(data.durationMs || 0, 0.001);
  const services = data.services || [];
  const multi = services.length > 1;

  // header
  const head = $("trace-head");
  head.replaceChildren();
  const id = d.el("span", "trace-id", data.traceId);
  id.title = "trace ID";
  head.append(id);
  const start = new Date(data.start);
  if (!isNaN(start.getTime())) {
    const t = d.el("span", "", d.localTime(start));
    t.title = start.toISOString();
    head.append(t);
  }
  head.append(d.el("span", "", fmtMs(data.durationMs || 0)));
  head.append(d.el("span", "muted",
    plural(data.spanCount || 0, "span") + " · " + plural(data.logCount || 0, "log") +
    (data.eventCount ? " · " + plural(data.eventCount, "event") : "") + " · " + plural(services.length, "service")));
  if (data.truncated) head.append(d.el("span", "trace-note", "showing the first 10000 spans, logs and events"));

  // legend: only the groups present
  const present = new Set();
  for (const n of data.nodes || []) if (n.kind === "span") present.add(typeGroup(n.type));
  const legend = $("trace-legend");
  legend.replaceChildren();
  for (const g of GROUPS) {
    if (!present.has(g)) continue;
    const item = d.el("span", "legend-item");
    item.append(d.el("span", "legend-swatch bar-" + g), d.el("span", "", g));
    legend.append(item);
  }

  // grid columns and gridlines
  const grid = $("trace-grid");
  grid.style.gridTemplateColumns = multi ? "minmax(240px, 35%) max-content 1fr" : "minmax(240px, 35%) 1fr";
  const step = axisStep(D);
  grid.style.setProperty("--grid-step", (step / D) * 100 + "%");

  // axis
  const axis = $("trace-axis");
  axis.replaceChildren(d.el("div"));
  if (multi) axis.append(d.el("div"));
  const track = d.el("div", "ax-track");
  // A zero-length trace (one log line) gets only the 0 label.
  const end = data.durationMs > 0 ? D * (1 + 1e-9) : 0;
  for (let i = 0; i * step <= end; i++) {
    const pct = ((i * step) / D) * 100;
    const lab = d.el("span", "ax-label", fmtMs(i * step));
    if (i === 0) lab.classList.add("first");
    else if (pct > 94) lab.classList.add("last");
    lab.style.left = pct + "%";
    track.append(lab);
  }
  axis.append(track);

  // rows
  const vis = visibleNodes();
  const last = new Set(); // ids with no later visible sibling
  const seen = new Set();
  for (let i = vis.length - 1; i >= 0; i--) {
    if (!seen.has(vis[i].parent)) last.add(vis[i].id);
    seen.add(vis[i].parent);
  }
  const lineAt = []; // lineAt[k]: the ancestor at depth k has a later visible sibling
  const rows = [];
  for (const n of vis) {
    const guides = [];
    for (let k = 1; k < n.depth; k++) guides.push(lineAt[k] ? "v" : "");
    if (n.depth > 0) guides.push(last.has(n.id) ? "end" : "tee");
    lineAt[n.depth] = !last.has(n.id);
    const row = traceRow(n, guides, { D, multi, run: runFirst.get(n.id), inRun: runOf.has(n.id) });
    if (matched.has(n.id)) row.classList.add("match");
    rows.push(row);
  }
  $("trace-rows").replaceChildren(...rows);
  renderSelfTable();
}

// renderSelfTable draws the per-name self-time table below the detail box.
function renderSelfTable() {
  const box = $("trace-self");
  const data = tv.data;
  box.replaceChildren();
  const rows = (data && data.selfTime) || [];
  if (!rows.length) return;
  const D = Math.max(data.durationMs || 0, 0.001);
  const cols = [
    ["name", "name", false],
    ["count", "count", true],
    ["totalMs", "total", true],
    ["selfMs", "self", true],
    ["selfMs", "self %", true],
  ];
  const sorted = rows.slice();
  const { key, dir } = tv.sort;
  if (key) {
    sorted.sort((a, b) => {
      const x = a[key], y = b[key];
      return (typeof x === "string" ? x.localeCompare(y) : x - y) * dir;
    });
  }
  const table = d.el("table", "self-table");
  const tr = d.el("tr");
  cols.forEach(([k, label, num], i) => {
    const th = d.el("th", num ? "num" : "", label);
    th.tabIndex = 0;
    if (tv.sort.col === i) th.append(d.el("span", "sort-ind", dir > 0 ? " ▲" : " ▼"));
    const go = () => {
      const same = tv.sort.col === i;
      tv.sort = { col: i, key: k, dir: same ? -tv.sort.dir : k === "name" ? 1 : -1 };
      renderSelfTable();
    };
    th.addEventListener("click", go);
    th.addEventListener("keydown", (e) => {
      if (e.key === "Enter") go();
    });
    tr.append(th);
  });
  const head = d.el("thead");
  head.append(tr);
  table.append(head);
  const body = d.el("tbody");
  for (const r of sorted) {
    const row = d.el("tr");
    const name = d.el("td", "st-name", r.name);
    name.title = "find " + r.name;
    name.addEventListener("click", () => {
      const f = $("trace-find");
      f.value = r.name;
      tv.find.q = r.name;
      tv.find.cur = null;
      refreshFind();
      if (tv.find.matches.length) goToMatch(0);
      else renderTrace();
    });
    row.append(
      name,
      d.el("td", "num", String(r.count)),
      d.el("td", "num", fmtMs(r.totalMs)),
      d.el("td", "num", fmtMs(r.selfMs)),
      d.el("td", "num", ((r.selfMs / D) * 100).toFixed(1) + "%"),
    );
    body.append(row);
  }
  table.append(body);
  box.append(table);
}

// traceRow is one grid row of node n. guides are the connector cells, one per
// depth level below the root: "v" (a line), "" (blank), and last "tee" or
// "end" for the node's own elbow.
function traceRow(n, guides, { D, multi, run, inRun }) {
  const row = d.el("div", "trace-row");
  row.dataset.id = n.id;
  if (n.kind === "missing") row.classList.add("missing");
  if (n.id === tv.selected) row.classList.add("selected");
  if (inRun) row.classList.add("serial");
  if (n.critical) row.classList.add("crit");

  const name = d.el("div", "tr-name");
  for (const g of guides) name.append(d.el("span", g ? "tg " + g : "tg"));
  if (shownKids(n).length) {
    const open = !tv.collapsed.has(n.id);
    const tw = d.el("button", "twisty", open ? "▾" : "▸");
    tw.type = "button";
    tw.title = open ? "collapse" : "expand";
    tw.setAttribute("aria-expanded", open ? "true" : "false");
    tw.addEventListener("click", (e) => {
      e.stopPropagation();
      if (tv.collapsed.has(n.id)) tv.collapsed.delete(n.id);
      else tv.collapsed.add(n.id);
      renderTrace();
    });
    name.append(tw);
    row.addEventListener("dblclick", (e) => {
      e.preventDefault();
      tw.click();
    });
  } else {
    name.append(d.el("span", "tw-pad"));
  }
  const isLog = n.kind === "log";
  const isEvent = n.kind === "event";
  const msg = String(n.message || "");
  let text = n.kind === "missing" ? "(Missing span " + n.name + ")" : n.name || n.id;
  if (isLog) text = msg.split("\n")[0];
  // An exception event shows "type: message"; other events their name.
  else if (isEvent) text = (n.error && msg ? msg.split("\n")[0] : n.name) || "(event)";
  if (isLog) {
    row.classList.add("log");
    name.append(d.el("span", "sev sev-" + d.severityClass(n.severity), n.severity || ""));
  }
  if (isEvent) {
    row.classList.add("event");
    if (n.error) row.classList.add("event-error");
    name.append(d.el("span", "ev-mark", "◆"));
  }
  const label = d.el("span", "tr-label", text);
  label.title = isLog ? msg : isEvent ? (n.name || "") + (msg ? "\n" + msg : "") : text;
  name.append(label);
  if (run) {
    const chip = d.el("span", "serial-chip", run.count + " serial queries, " + fmtMs(run.wallMs));
    chip.title = "these " + run.count + " queries ran one after another, not at the same time";
    name.append(chip);
  }
  row.append(name);

  if (multi) row.append(d.el("div", "tr-svc", n.service || ""));

  const cell = d.el("div", "tr-bar");
  if (isLog || isEvent) {
    const mark = d.el("span", isEvent ? "tmark emark" + (n.error ? " err" : "") : "tmark");
    mark.style.left = (Math.max(n.startMs || 0, 0) / D) * 100 + "%";
    mark.title = (isEvent ? n.name || "event" : n.severity || "") + " · " + fmtMs(n.startMs || 0);
    cell.append(mark);
    row.append(cell);
    row.addEventListener("click", () => selectNode(n.id, { scroll: false }));
    return row;
  }
  const ticks = (n.logTicks || []).map((ms) => [ms, "ttick"]);
  for (const t of n.eventTicks || []) ticks.push([t.ms, "ttick etick" + (t.error ? " err" : "")]);
  for (const [ms, cls] of ticks) {
    const tick = d.el("span", cls);
    tick.style.left = (Math.max(ms, 0) / D) * 100 + "%";
    cell.append(tick);
  }
  const start = Math.max(n.startMs || 0, 0);
  const dur = Math.max(n.durationMs || 0, 0);
  const left = (start / D) * 100;
  const bar = d.el("span", "tbar " + (n.kind === "missing" ? "bar-missing" : "bar-" + typeGroup(n.type)));
  if (n.error) bar.classList.add("bar-error");
  bar.style.left = left + "%";
  bar.style.width = "max(2px, " + (dur / D) * 100 + "%)";
  bar.title = text + " · " + fmtMs(dur);
  const lab = d.el("span", "bar-label", fmtMs(dur));
  // After the bar; before it when the bar ends in the right fifth, or inside
  // its right end when it also starts in the left fifth (no room either side).
  if (start + dur <= 0.8 * D) lab.style.left = "calc(" + ((start + dur) / D) * 100 + "% + 4px)";
  else if (start >= 0.2 * D) lab.style.right = "calc(" + (100 - left) + "% + 4px)";
  else {
    lab.classList.add("inside");
    lab.style.right = "calc(" + (100 - ((start + dur) / D) * 100) + "% + 4px)";
  }
  cell.append(bar, lab);
  row.append(cell);

  row.addEventListener("click", () => selectNode(n.id, { scroll: false }));
  return row;
}

// selectNode highlights node id's row, shows its record below the grid and
// puts it in the URL. scroll expands its ancestors and scrolls it into view.
function selectNode(id, { scroll = false } = {}) {
  const n = byId.get(id);
  if (!n) return;
  tv.selected = id;
  if (scroll) {
    const hidden = [];
    for (let p = byId.get(n.parent); p; p = byId.get(p.parent)) if (tv.collapsed.has(p.id)) hidden.push(p.id);
    if (hidden.length) {
      expandAncestors(n);
      renderTrace();
    }
  }
  let rowEl = null;
  for (const r of $("trace-rows").children) {
    const on = r.dataset.id === id;
    r.classList.toggle("selected", on);
    if (on) rowEl = r;
  }
  writeURL(false);

  // A fresh box per selection: a slow fill for an earlier row writes into a
  // detached box instead of over this one.
  const detailBox = d.el("div");
  $("trace-detail").replaceChildren(detailBox);
  if (n.kind === "missing") {
    detailBox.append(
      d.el("p", "ph-title", "This span is not in the data"),
      d.el("p", "ph-line", "ID: " + n.name),
      d.el("p", "ph-line", "Children: " + (n.children || 0)),
    );
  } else if (n.kind === "event") {
    eventDetail(detailBox, n);
  } else if (n.seq) {
    detailBox.append(d.el("span", "muted", "loading…"));
    d.fillRecordDetail(detailBox, n.seq);
  }
  if (scroll && rowEl) rowEl.scrollIntoView({ block: "center" });
}

// eventDetail fills box with a span event: its name, time, parent span (a
// link that selects it) and attributes.
function eventDetail(box, n) {
  box.append(d.el("p", "ph-title", "Event: " + (n.name || "(no name)")));
  const at = new Date(n.time);
  const when = n.time && !isNaN(at.getTime()) ? d.localTime(at) + " · " : "";
  const time = d.el("p", "ph-line", "Time: " + when + "+" + fmtMs(n.startMs || 0));
  if (n.time) time.title = n.time;
  box.append(time);
  const span = byId.get(n.parent);
  const line = d.el("p", "ph-line", "Span: ");
  const link = d.el("button", "ev-span", n.parent + (span && span.name ? " (" + span.name + ")" : ""));
  link.type = "button";
  link.title = "select this span";
  link.addEventListener("click", () => selectNode(n.parent, { scroll: true }));
  line.append(link);
  box.append(line);
  const keys = Object.keys(n.attributes || {}).sort();
  if (!keys.length) return;
  box.append(d.el("p", "ph-line ev-attrs-head", "Attributes:"));
  for (const k of keys) {
    const v = n.attributes[k];
    box.append(d.el("p", "ph-line ev-attr", k + ": " + (typeof v === "string" ? v : JSON.stringify(v))));
  }
}
