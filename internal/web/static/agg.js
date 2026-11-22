// loghorn aggregate operation view: every trace of one operation in the
// page's range, merged into one tree of per-span statistics. Every name
// reaches the DOM through textContent or attributes, never innerHTML. app.js
// passes its helpers to initAggregate, so this module does not import app.js.

import { GROUPS, cancelTrace, leafSteps, openTrace, plural, traceLoading, treeGuides, typeGroup } from "./trace.js";
import { copyTableBar } from "./copy.js";
import { back, navState, syncNav } from "./nav.js";

const $ = (id) => document.getElementById(id);

let d = null; // deps from initAggregate

// av is the view's state. data is the last /api/aggregate answer, kept while
// the view is hidden so Back from a trace shows it again without a fetch; key
// is the (name, service, from, to) it answers.
const av = {
  data: null,
  key: "",
  name: "",
  service: "", // as sent: "*" for every service
  range: { from: "", to: "" },
  collapsed: new Set(), // node ids
  seq: 0, // the newest openAggregate; older answers are dropped
};

let kidsOf = new Map(); // node id -> child nodes, in order
let restoreParams = null; // the page URL's {agg, aggsvc}, until restored

const enc = encodeURIComponent;
const isOpen = () => !$("agg").hidden && av.data !== null;
const fmtMs = (ms) => d.formatDuration(ms / 1000, "ms");
const pct = (x) => (Number(x) || 0).toFixed(1) + "%";

// normService maps "" and "*" (every service) to "*", so the URL always
// names the service and a reload does not fall back to the table's.
const normService = (s) => (s === "" || s === "*" || s == null ? "*" : String(s));

function urlParams() {
  const p = new URLSearchParams(location.search);
  return { agg: p.get("agg") || "", aggsvc: p.get("aggsvc"), trace: p.get("trace") || "", span: p.get("span") || "" };
}

// aggURLParams is the URL suffix of the open view: "&agg=<name>&aggsvc=
// <service>"; "" when the view is closed. Before restoreAggregateFromURL ran
// it keeps the page URL's parameters.
export function aggURLParams() {
  let name = "";
  let svc = null;
  if (isOpen()) {
    name = av.name;
    svc = av.service;
  } else if (restoreParams) {
    name = restoreParams.agg;
    svc = restoreParams.aggsvc;
  }
  if (!name) return "";
  let s = "&agg=" + enc(name);
  if (svc) s += "&aggsvc=" + enc(svc);
  return s;
}

// urlWith is the page URL with the table's q and params (an aggURLParams
// string) and nothing else.
function urlWith(params) {
  const raw = new URLSearchParams(location.search).get("q");
  let s = (raw === null ? "" : "?q=" + enc(raw)) + params;
  if (s.startsWith("&")) s = "?" + s.slice(1);
  return s || location.pathname;
}

// writeURL pushes (push) or replaces the history entry with the view's URL.
// The entry's state carries the view stack (see nav.js).
function writeURL(push) {
  const url = urlWith(aggURLParams());
  const crumb = { kind: "agg", name: av.name, service: av.service };
  if (push) history.pushState(navState(crumb, true), "", url);
  else history.replaceState(navState(crumb, false), "", url);
  syncNav();
}

export function initAggregate(deps) {
  d = deps;
  const p = urlParams();
  if (p.agg && !p.trace && !p.span) restoreParams = p;
  $("agg-close").addEventListener("click", closeAgg);
  $("agg-reload").addEventListener("click", () => {
    if (av.data) openAggregate({ name: av.name, service: av.service }, { push: false, reload: true });
  });
  $("agg-hide-leaves").addEventListener("click", () => {
    if (!av.data) return;
    for (const id of leafSteps(visibleNodes(), kids, av.collapsed).hide) av.collapsed.add(id);
    renderAgg();
  });
  $("agg-expand-leaves").addEventListener("click", () => {
    if (!av.data) return;
    for (const id of leafSteps(visibleNodes(), kids, av.collapsed).expand) av.collapsed.delete(id);
    renderAgg();
  });
  $("agg-copy").replaceWith(copyTableBar(aggTable, "agg-copy"));
  $("agg-expand").addEventListener("click", () => {
    av.collapsed = new Set();
    renderAgg();
  });
  document.addEventListener("keydown", (e) => {
    if (e.key !== "Escape" || !isOpen() || e.defaultPrevented) return;
    if (document.querySelector("dialog[open]")) return;
    const t = e.target;
    if (t instanceof HTMLElement && t.matches("input, select, textarea")) return;
    closeAgg();
  });
  window.addEventListener("popstate", () => {
    const q = urlParams();
    if (q.agg && !q.trace && !q.span) {
      openAggregate({ name: q.agg, service: q.aggsvc ?? d.getService() }, { push: false });
    } else {
      ++av.seq;
      hideAgg();
    }
  });
}

// restoreAggregateFromURL opens the view the page URL names, once, after the
// table's startup. Without aggsvc the service is the table's.
export function restoreAggregateFromURL() {
  const p = restoreParams;
  if (!p) return;
  openAggregate({ name: p.agg, service: p.aggsvc ?? d.getService() }, { push: false }).finally(() => {
    restoreParams = null;
  });
}

// aggRangeChanged aggregates the open view again over the page's new range;
// the view follows the sidebar's From and To. A trace being opened from the
// view wins: re-aggregating would cancel it.
export function aggRangeChanged() {
  if (isOpen() && !traceLoading()) openAggregate({ name: av.name, service: av.service }, { push: false });
}

// cancelAggregate supersedes a pending openAggregate, so its answer is
// dropped (opening a trace does this).
export function cancelAggregate() {
  ++av.seq;
}

// openAggregate shows the aggregate of the operation name (root spans of that
// name) for service ("" or "*": every service) over the page's range, in
// place of the table or the waterfall. push adds a history entry; else the
// current one is replaced. The last answer is reused when its key matches,
// unless reload. A newer call supersedes this one.
export async function openAggregate({ name, service }, { push = true, reload = false } = {}) {
  const mine = ++av.seq;
  cancelTrace(); // a pending trace must not cover this view
  const r = d.getRange() || {};
  const range = { from: String(r.from || ""), to: String(r.to || "") };
  const svc = normService(service);
  const key = JSON.stringify([name, svc, range.from, range.to]);
  let data = !reload && av.data && av.key === key ? av.data : null;
  if (!data) {
    const path = "/api/aggregate?name=" + enc(name) + "&service=" + enc(svc) + "&from=" + enc(range.from) + "&to=" + enc(range.to);
    try {
      data = await d.api("GET", path);
    } catch (err) {
      if (mine !== av.seq) return;
      d.showError(err.detail || err.message);
      // A failed restore (deep link, Back/Forward) drops its parameters; a
      // failed open or reload leaves the current view as it is.
      if (!push && !reload && urlParams().agg) {
        history.replaceState(history.state, "", urlWith(""));
        hideAgg();
      }
      return;
    }
    if (mine !== av.seq) return;
  }
  restoreParams = null;
  d.clearError();
  if (key !== av.key && !reload) av.collapsed = new Set();
  av.data = data;
  av.key = key;
  av.name = name;
  av.service = svc;
  av.range = range;
  kidsOf = new Map();
  for (const n of data.nodes || []) {
    if (!n.parent) continue;
    if (!kidsOf.has(n.parent)) kidsOf.set(n.parent, []);
    kidsOf.get(n.parent).push(n);
  }
  showAgg();
  renderAgg();
  writeURL(push);
}

// closeAgg returns to the view the aggregate was opened from, or the
// results (see nav.js back).
function closeAgg() {
  ++av.seq;
  if (!isOpen()) return;
  back(); // popstate hides the view
}

function showAgg() {
  $("bar").hidden = true;
  $("results").hidden = true;
  $("footer").hidden = true;
  $("trace").hidden = true;
  $("agg").hidden = false;
}

// hideAgg hides the view, keeping its answer for a later Back. The table
// shows again only when the URL names no other view (the waterfall shows
// itself).
function hideAgg() {
  $("agg").hidden = true;
  $("agg-rows").replaceChildren();
  const p = urlParams();
  if (!(p.trace || p.span || p.agg)) {
    $("bar").hidden = false;
    $("results").hidden = false;
    $("footer").hidden = false;
  }
  syncNav();
}

const kids = (n) => kidsOf.get(n.id) || [];

// AGG_HEADERS are the columns aggTable copies.
const AGG_HEADERS = ["Name", "traces", "calls/trace", "p50", "p95", "total %", "self %", "errors"];

// aggCells are the copied cells of node n, of N traces, without its name.
function aggCells(n, N) {
  const tracePct = N > 0 ? Math.round(((n.traces || 0) / N) * 100) : 0;
  return [
    (n.traces || 0).toLocaleString() + " (" + tracePct + "%)",
    (Number(n.callsPerTrace) || 0).toFixed(1),
    fmtMs(n.p50Ms || 0),
    fmtMs(n.p95Ms || 0),
    pct(n.totalPct),
    pct(n.selfPct),
  ];
}

// aggTable is the metrics tree for copying: every node, expanded or not, its
// name indented two spaces per depth.
function aggTable() {
  const data = av.data;
  if (!data) return { headers: AGG_HEADERS, rows: [] };
  const N = data.traces || 0;
  const rows = (data.nodes || []).map((n) => ["  ".repeat(n.depth || 0) + (n.name || ""), ...aggCells(n, N), String(n.errors || 0)]);
  return { headers: AGG_HEADERS, rows };
}

// visibleNodes are the nodes, in tree order, whose ancestors are all
// expanded.
function visibleNodes() {
  const out = [];
  let skipBelow = Infinity; // depth of a collapsed node whose subtree is skipped
  for (const n of av.data.nodes || []) {
    if (n.depth > skipBelow) continue;
    skipBelow = Infinity;
    out.push(n);
    if (av.collapsed.has(n.id)) skipBelow = n.depth;
  }
  return out;
}

// renderAgg redraws the header, legend and rows.
function renderAgg() {
  const data = av.data;
  if (!data) return;
  const nodes = data.nodes || [];
  const N = data.traces || 0;

  // header
  const head = $("agg-head");
  head.replaceChildren();
  const name = d.el("span", "trace-id", data.name ?? av.name);
  name.title = "operation (root span name)";
  head.append(name);
  // The scope: what the numbers are over, so they are not read as the
  // results' service or a range the sidebar no longer shows.
  const scope = (label, value, title) => {
    const s = d.el("span", "agg-scope");
    s.append(d.el("span", "muted", label + " "), d.el("span", "", value));
    s.title = title;
    return s;
  };
  head.append(scope("service", av.service === "*" ? "all" : av.service, "the service whose root spans are merged"));
  head.append(scope("range", (av.range.from || "…") + " → " + (av.range.to || "…"),
    (data.from || "start") + " → " + (data.to || "end") + "\nfollows the sidebar's From and To"));
  const count = data.capped
    ? N.toLocaleString() + " of " + plural(data.matched || 0, "trace") + " (capped, most recent)"
    : plural(N, "trace");
  head.append(d.el("span", "", count));
  if (nodes.length) head.append(d.el("span", "muted", "p50 " + fmtMs(nodes[0].p50Ms || 0) + " · p95 " + fmtMs(nodes[0].p95Ms || 0)));
  if (data.outsideSpans > 0) {
    head.append(d.el("span", "trace-note",
      plural(data.incompleteTraces || 0, "trace") + " with " + plural(data.outsideSpans, "span") + " outside the root (missing parents)"));
  }
  if (data.truncatedTraces > 0) {
    head.append(d.el("span", "trace-note", plural(data.truncatedTraces, "trace") + " cut at 10000 spans, logs and events"));
  }
  if (data.nodesTruncated) head.append(d.el("span", "trace-note", "showing the first 10000 merged spans"));

  // legend: only the groups present
  const present = new Set(nodes.map((n) => typeGroup(n.type)));
  const legend = $("agg-legend");
  legend.replaceChildren();
  for (const g of GROUPS) {
    if (!present.has(g)) continue;
    const item = d.el("span", "legend-item");
    item.append(d.el("span", "legend-swatch bar-" + g), d.el("span", "", g));
    legend.append(item);
  }

  // rows
  const vis = visibleNodes();
  const guides = treeGuides(vis);
  $("agg-rows").replaceChildren(...vis.map((n, i) => aggRow(n, guides[i], N)));
  const steps = leafSteps(vis, kids, av.collapsed);
  $("agg-hide-leaves").disabled = !steps.hide.length;
  $("agg-expand-leaves").disabled = !steps.expand.length;
  $("agg-expand").disabled = ![...av.collapsed].some((id) => kidsOf.has(id));
}

// aggRow is one grid row of merged node n; guides are its connector cells
// (see treeGuides) and N the number of traces aggregated.
function aggRow(n, guides, N) {
  const row = d.el("div", "trace-row");
  row.dataset.id = n.id;

  const name = d.el("div", "tr-name");
  for (const g of guides) name.append(d.el("span", g ? "tg " + g : "tg"));
  if (kidsOf.has(n.id)) {
    const open = !av.collapsed.has(n.id);
    const tw = d.el("button", "twisty", open ? "▾" : "▸");
    tw.type = "button";
    tw.title = open ? "collapse" : "expand";
    tw.setAttribute("aria-expanded", open ? "true" : "false");
    tw.addEventListener("click", (e) => {
      e.stopPropagation();
      if (av.collapsed.has(n.id)) av.collapsed.delete(n.id);
      else av.collapsed.add(n.id);
      renderAgg();
    });
    name.append(tw);
    row.addEventListener("dblclick", (e) => {
      e.preventDefault();
      tw.click();
    });
  } else {
    name.append(d.el("span", "tw-pad"));
  }
  const label = d.el("span", "tr-label", n.name || "");
  const svcs = n.services || [];
  label.title = (n.name || "") + (svcs.length ? "\n" + svcs.join(", ") : "") + (n.type ? "\n" + n.type : "");
  name.append(label);
  if (n.slowest && n.slowest.traceId) {
    const a = d.el("a", "agg-slowest", "slowest " + fmtMs(n.slowest.durationMs || 0));
    a.href = "#";
    a.title = "open the slowest trace containing this span";
    a.addEventListener("click", (e) => {
      e.preventDefault();
      e.stopPropagation();
      openTrace({ trace: n.slowest.traceId, span: n.slowest.spanId });
    });
    name.append(a);
  }
  if ((n.serialCalls || 0) > 0) {
    const chip = d.el("span", "serial-chip",
      (Number(n.serialPerTrace) || 0).toFixed(1) + " serial queries/trace, " + (Number(n.serialPct) || 0).toFixed(1) + "%");
    chip.title = "average queries per trace that ran one after another inside a serial run, and their share of the operation's time";
    name.append(chip);
  }
  row.append(name);

  row.append(...aggCells(n, N).map((t) => d.el("div", "agg-num", t)));

  // share bar: total % in the type colour, self % darker inside it; capped at
  // 100 % (parallel children can sum past their parent), the cells show the
  // real numbers.
  const share = d.el("div", "agg-share");
  const track = d.el("span", "agg-track");
  const bar = d.el("span", "abar bar-" + typeGroup(n.type));
  bar.style.width = Math.min(Math.max(Number(n.totalPct) || 0, 0), 100) + "%";
  const self = d.el("span", "abar-self");
  self.style.width = Math.min(Math.max(Number(n.selfPct) || 0, 0), 100) + "%";
  bar.append(self);
  track.append(bar);
  track.title = "total " + pct(n.totalPct) + " · self " + pct(n.selfPct) + " of the operation's time";
  share.append(track);
  if (n.errors > 0) {
    const badge = d.el("span", "agg-err", n.errors.toLocaleString());
    badge.title = plural(n.errors, "call") + " with status error";
    share.append(badge);
  }
  row.append(share);
  return row;
}
