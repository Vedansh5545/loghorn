// nav.js keeps the stack of views opened over the query results: a trace
// waterfall, an aggregate, a trace opened from that aggregate, and so on.
//
// The page URL names only the top view, so the stack lives in history.state:
// each entry the trace or aggregate view writes carries { crumbs }, the views
// open in that entry, bottom first; the last one is the view shown. A crumb
// is { kind: "trace", trace, span } or { kind: "agg", name, service }, plus
// pushed: whether its view added a history entry, so going down to a lower
// crumb can use Back.
//
// syncNav draws what follows from the stack: the breadcrumb, the back
// buttons, the sidebar's lock and the layer edges.

const $ = (id) => document.getElementById(id);

let d = { rootLabel: () => "Results" };
let going = false; // a history.go is on its way; a second click must not go further


export function initNav(deps) {
  d = deps;
  $("crumbs-close").addEventListener("click", () => goTo(-1));
  $("sb-note-back").addEventListener("click", () => goTo(-1));
  window.addEventListener("popstate", () => {
    going = false;
  });
}

// crumbFromURL is the crumb of the view the page URL names; null when it
// names none.
function crumbFromURL() {
  const p = new URLSearchParams(location.search);
  if (p.get("trace") || p.get("span")) return { kind: "trace", trace: p.get("trace") || "", span: p.get("span") || "" };
  if (p.get("agg")) return { kind: "agg", name: p.get("agg"), service: p.get("aggsvc") || "" };
  return null;
}

// currentCrumbs is the stack of the current entry; [] when no view is open.
// An entry the views didn't write (a deep link, a reload of an older build)
// gives the URL's view alone.
export function currentCrumbs() {
  const top = crumbFromURL();
  if (!top) return [];
  const s = history.state;
  if (s && Array.isArray(s.crumbs) && s.crumbs.length) return s.crumbs;
  return [{ ...top, pushed: !!(s && (s.loghornTrace || s.loghornAgg)) }];
}

// navState is the history.state for showing crumb: on top of the current
// stack when push, else in place of its top. Call it before the URL changes.
export function navState(crumb, push) {
  const below = currentCrumbs();
  if (push) return { crumbs: [...below, { ...crumb, pushed: true }] };
  if (!below.length) return { crumbs: [{ ...crumb, pushed: false }] };
  const top = below[below.length - 1];
  return { crumbs: [...below.slice(0, -1), { ...crumb, pushed: !!top.pushed }] };
}

// crumbParams is the URL suffix that names crumb's view.
function crumbParams(c) {
  const enc = encodeURIComponent;
  if (!c) return "";
  if (c.kind === "trace") return (c.trace ? "&trace=" + enc(c.trace) : "") + (c.span ? "&span=" + enc(c.span) : "");
  return "&agg=" + enc(c.name || "") + (c.service ? "&aggsvc=" + enc(c.service) : "");
}

function urlFor(c) {
  const raw = new URLSearchParams(location.search).get("q");
  let s = (raw === null ? "" : "?q=" + encodeURIComponent(raw)) + crumbParams(c);
  if (s.startsWith("&")) s = "?" + s.slice(1);
  return s || location.pathname;
}

// goTo shows crumb k of the stack (-1: the results under every view). When
// every entry above it was pushed, that is Back; else (a deep link is in the
// way) the current entry is rewritten to name it, and the views follow the
// URL as they do on Back.
export function goTo(k) {
  const cs = currentCrumbs();
  const top = cs.length - 1;
  if (going || top < 0 || k >= top) return;
  let n = 0;
  for (let i = top; i > k && cs[i].pushed; i--) n++;
  if (n === top - k) {
    going = true;
    history.go(-n);
    return;
  }
  const state = k < 0 ? null : { crumbs: cs.slice(0, k + 1).map((c) => ({ ...c, pushed: false })) };
  history.replaceState(state, "", urlFor(k < 0 ? null : cs[k]));
  window.dispatchEvent(new PopStateEvent("popstate", { state }));
}

// back shows the view under the top one, or the results.
export function back() {
  goTo(currentCrumbs().length - 2);
}

function shortId(id) {
  const s = String(id || "");
  return s.length > 10 ? s.slice(0, 8) + "…" : s;
}

function clip(s, n) {
  s = String(s ?? "");
  return s.length > n ? s.slice(0, n - 1) + "…" : s;
}

// crumbLabel is the short name of crumb c (-1: the results).
function crumbLabel(c) {
  if (!c) return d.rootLabel();
  if (c.kind === "trace") return c.trace ? "Trace " + shortId(c.trace) : "Span " + shortId(c.span);
  return "Σ " + clip(c.name || "(unnamed)", 40);
}

function crumbTitle(c) {
  if (!c) return "the query results; the sidebar's controls apply to them";
  if (c.kind === "trace") return c.trace ? "trace " + c.trace : "span " + c.span;
  return "aggregate of operation " + (c.name || "(unnamed)") + (c.service ? " in " + (c.service === "*" ? "every service" : c.service) : "");
}

// shownView is the view on screen: "trace", "agg" or "" (the results).
function shownView() {
  if (!$("trace").hidden) return "trace";
  if (!$("agg").hidden) return "agg";
  return "";
}

// LOCK_KEEP are the sidebar rows each view still uses; the others are
// locked while it is open.
const LOCK_KEEP = { trace: [], agg: ["sb-from", "sb-to"] };

const NOTES = {
  trace: "A trace is the same whatever these controls say; they apply to ",
  agg: "Only From and To apply to this aggregate (it follows them); the rest apply to ",
};

// syncNav draws the breadcrumb, the back buttons' labels, the sidebar lock
// and the layer edges for the view on screen and the current stack.
export function syncNav() {
  const view = shownView();
  const cs = view ? currentCrumbs() : [];
  const nav = $("crumbs");
  nav.hidden = !view;
  document.body.classList.toggle("in-view", !!view);
  lockSidebar(view);
  if (!view) return;
  nav.dataset.depth = String(Math.min(cs.length, 3)); // the layer edges drawn

  const list = $("crumbs-list");
  list.replaceChildren();
  const all = [null, ...cs];
  all.forEach((c, i) => {
    const li = document.createElement("li");
    const last = i === all.length - 1;
    const label = crumbLabel(c);
    if (last) {
      const s = document.createElement("span");
      s.className = "crumb current";
      s.setAttribute("aria-current", "page");
      s.textContent = label;
      s.title = crumbTitle(c);
      li.append(s);
    } else {
      const b = document.createElement("button");
      b.type = "button";
      b.className = "crumb";
      b.textContent = label;
      b.title = "back to " + crumbTitle(c);
      b.addEventListener("click", () => goTo(i - 1));
      li.append(b);
    }
    list.append(li);
  });

  const parent = all[all.length - 2];
  const backBtn = $(view === "trace" ? "trace-close" : "agg-close");
  backBtn.textContent = "← " + crumbLabel(parent);
  backBtn.title = "back to " + crumbTitle(parent) + " (Esc)";
  $("crumbs-close").title = "close " + (cs.length > 1 ? "every view" : "this view") + " and go back to " + d.rootLabel();
}

// lockSidebar makes the sidebar rows the view doesn't use inert and dim, and
// says why; the view tabs and the Favorites list are locked too, since they change
// only the results.
function lockSidebar(view) {
  const keep = new Set(view ? LOCK_KEEP[view] : []);
  const rows = [
    $("sb-view"),
    ...document.querySelectorAll(".sb-form > .sb-field"),
    $("sb-filters"),
    $("sb-columns"),
    $("sb-favs"),
  ];
  for (const r of rows) {
    if (!r) continue;
    const locked = !!view && !keep.has(r.id);
    r.inert = locked;
    r.classList.toggle("locked", locked);
  }
  const note = $("sb-note");
  note.hidden = !view;
  if (view) {
    $("sb-note-text").textContent = NOTES[view];
    $("sb-note-back").textContent = d.rootLabel();
  }
}
