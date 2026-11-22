// Heatmap view: span count by time bucket (x) and log-duration band (y), as
// an SVG grid of cells. Every label reaches the DOM through textContent or
// attributes, never innerHTML.

import { xTicks } from "./graph.js";

const SVG = "http://www.w3.org/2000/svg";
const PAD = { top: 8, right: 52, bottom: 22, left: 4 };
const MIN_PLOT = 60; // px: below this the plot is not drawn
// STEPS is the number of colour steps, --heat-1 (fewest spans) to --heat-5;
// a cell with no spans is left empty.
const STEPS = 5;

// observers holds each host's ResizeObserver, so a redraw replaces it.
const observers = new WeakMap();

function svgEl(tag, attrs) {
  const e = document.createElementNS(SVG, tag);
  for (const k in attrs) e.setAttribute(k, attrs[k]);
  return e;
}

function div(cls, text) {
  const e = document.createElement("div");
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

// formatMs labels a duration in ms with two significant digits: 160us,
// 1.6ms, 25ms, 1.6s.
export function formatMs(ms) {
  const v = Number(ms.toPrecision(6)); // 0.99999… from 10^(k/5) reads as 1
  const sig = (x) => String(Number(x.toPrecision(2)));
  if (v < 1) return sig(v * 1000) + "us";
  if (v < 1000) return sig(v) + "ms";
  return sig(v / 1000) + "s";
}

// bandLabel names band bi: "1.6ms – 2.5ms", or "< 160us" for the lowest
// band, which also holds zero and negative durations.
export function bandLabel(bands, bi) {
  const b = bands[bi];
  return bi === 0 ? "< " + formatMs(b.hi) : formatMs(b.lo) + " – " + formatMs(b.hi);
}

const count = (h, ti, bi) => ((h.counts && h.counts[ti]) || [])[bi] || 0;

// cellTime is a bucket start as "Nov 21, 2026, 5:21:30 PM" in local time.
export function cellTime(heatmap, ti) {
  return new Date(heatmap.starts[ti]).toLocaleString(undefined, {
    month: "short", day: "numeric", year: "numeric", hour: "numeric", minute: "2-digit", second: "2-digit",
  });
}

// cellLabel describes cell (ti, bi): its time slice, duration band and count,
// "Nov 21, 2026, 5:21:30 PM · 1.6ms – 2.5ms · 43 spans".
export function cellLabel(heatmap, ti, bi) {
  const n = count(heatmap, ti, bi);
  return cellTime(heatmap, ti) + " · " + bandLabel(heatmap.bands, bi) + " · " + n.toLocaleString() + (n === 1 ? " span" : " spans");
}

// bucketEnd is the end (ms) of bucket i: the next bucket's start, or start +
// size for the last one (buckets of a day are not all the same length).
function bucketEnd(h, starts, i) {
  return i + 1 < starts.length ? starts[i + 1] : starts[i] + h.bucketSeconds * 1000;
}

// step is the colour step (1..STEPS) of a non-zero count. The square root
// keeps cells of a few spans visible next to a busy peak.
function step(n, max) {
  return Math.min(STEPS, Math.max(1, Math.ceil(Math.sqrt(n / Math.max(max, 1)) * STEPS)));
}

const isDecade = (v) => v > 0 && Math.abs(Math.log10(v) - Math.round(Math.log10(v))) < 1e-6;

// zoneLabel names the local time zone: "America/Los_Angeles, UTC-8".
function zoneLabel(at) {
  const off = -new Date(at).getTimezoneOffset();
  const abs = Math.abs(off);
  let utc = "UTC" + (off < 0 ? "-" : "+") + Math.floor(abs / 60);
  if (abs % 60) utc += ":" + String(abs % 60).padStart(2, "0");
  let name = "";
  try {
    name = Intl.DateTimeFormat().resolvedOptions().timeZone || "";
  } catch {}
  return "Local time (" + (name ? name + ", " : "") + utc + ")";
}

// renderHeatmap draws heatmap (a query result's `heatmap`) into svgHost: the
// grid of cells, a time axis below, decade labels on the right, a legend and
// a hover readout. A click on a cell with spans calls onSelect({ti, bi}), or
// onSelect(null) on the selected cell; selected ({ti, bi}) is outlined and the
// other cells dimmed. The drawing follows svgHost's size.
export function renderHeatmap(svgHost, heatmap, { onSelect, selected } = {}) {
  const old = observers.get(svgHost);
  if (old) old.disconnect();
  observers.delete(svgHost);
  svgHost.replaceChildren();
  const h = heatmap || {};
  const starts = (h.starts || []).map((s) => new Date(s).getTime());
  const bands = h.bands || [];
  if (!starts.length || !bands.length || !h.total) {
    let msg = "No spans with a duration in this range.";
    if (h.skipped) msg += " " + h.skipped.toLocaleString() + " without a duration or time.";
    svgHost.append(div("empty", msg));
    return;
  }

  const plotBox = div("heat-plot");
  svgHost.append(plotBox, legend(h, starts[0]));

  let lastW = -1, lastH = -1;
  const draw = () => {
    const width = Math.floor(plotBox.clientWidth);
    const height = Math.floor(plotBox.clientHeight);
    if (width <= 0 || height <= 0 || (width === lastW && height === lastH)) return;
    lastW = width;
    lastH = height;
    plot(plotBox, width, height, h, starts, bands, onSelect, selected);
  };
  const ro = new ResizeObserver(draw);
  ro.observe(plotBox);
  observers.set(svgHost, ro);
  draw();
}

// legend is "Number of spans 0 [ramp] max", then the totals and time zone.
function legend(h, t0) {
  const box = div("heat-legend");
  const key = div("heat-key");
  const ramp = div("heat-ramp");
  for (let s = 1; s <= STEPS; s++) {
    const sw = div("heat-swatch");
    sw.style.background = "var(--heat-" + s + ")";
    ramp.append(sw);
  }
  ramp.title = "colour grows with the square root of the count";
  key.append(div("heat-key-title", "Number of spans"), div("heat-key-n", "0"), ramp, div("heat-key-n", h.max.toLocaleString()));
  let info = h.total.toLocaleString() + " spans";
  if (h.bucket) info += " · buckets of " + h.bucket;
  if (h.skipped) info += " · " + h.skipped.toLocaleString() + " without a duration";
  box.append(key, div("heat-info", info), div("heat-zone", zoneLabel(t0)));
  return box;
}

function plot(box, width, height, h, starts, bands, onSelect, selected) {
  box.replaceChildren();
  const plotW = width - PAD.left - PAD.right;
  const plotH = height - PAD.top - PAD.bottom;
  if (plotW < MIN_PLOT || plotH < MIN_PLOT / 2) return;
  const t0 = starts[0];
  const t1 = bucketEnd(h, starts, starts.length - 1);
  const x = (t) => PAD.left + ((t - t0) / Math.max(t1 - t0, 1)) * plotW;
  const nb = bands.length;
  const bandH = plotH / nb;
  const yEdge = (b) => PAD.top + plotH - b * bandH; // bottom edge of band b
  const svg = svgEl("svg", { width, height, role: "img", "aria-label": "latency heatmap of " + h.total + " spans" });

  // Decade rules and labels on the right, at band edges that are powers of 10.
  const grid = svgEl("g", { class: "heat-grid" });
  for (let b = 0; b <= nb; b++) {
    const v = b < nb ? bands[b].lo : bands[nb - 1].hi;
    if (!isDecade(v)) continue;
    const y = Math.round(yEdge(b)) + 0.5;
    grid.append(svgEl("line", { x1: PAD.left, x2: PAD.left + plotW + 4, y1: y, y2: y }));
    const label = svgEl("text", { x: PAD.left + plotW + 7, y, "dominant-baseline": "middle" });
    label.textContent = formatMs(v);
    grid.append(label);
  }
  // Time ticks below.
  const baseY = PAD.top + plotH;
  grid.append(svgEl("line", { class: "heat-base", x1: PAD.left, x2: PAD.left + plotW, y1: baseY + 0.5, y2: baseY + 0.5 }));
  for (const tk of xTicks(t0, t1, plotW)) {
    const xx = Math.round(x(tk.t)) + 0.5;
    grid.append(svgEl("line", { x1: xx, x2: xx, y1: baseY, y2: baseY + 4 }));
    const label = svgEl("text", { x: xx, y: baseY + 16, "text-anchor": "middle" });
    label.textContent = tk.label;
    grid.append(label);
  }
  svg.append(grid);

  // Cells with spans; a 1px gap of surface between neighbours when they are
  // wide enough to spare it.
  const sel = selected && count(h, selected.ti, selected.bi) > 0 ? selected : null;
  const cells = svgEl("g", { class: "heat-cells" + (sel ? " heat-dim" : "") });
  const gapY = bandH >= 4 ? 1 : 0;
  const cellBox = (ti, bi) => {
    const xa = x(starts[ti]);
    const xb = x(bucketEnd(h, starts, ti));
    const gapX = xb - xa >= 4 ? 1 : 0;
    return { x: xa + gapX / 2, y: yEdge(bi + 1) + gapY / 2, w: Math.max(1, xb - xa - gapX), h: Math.max(1, bandH - gapY) };
  };
  for (let ti = 0; ti < starts.length; ti++) {
    for (let bi = 0; bi < nb; bi++) {
      const n = count(h, ti, bi);
      if (!n) continue;
      const c = cellBox(ti, bi);
      const r = svgEl("rect", { x: c.x, y: c.y, width: c.w, height: c.h, fill: "var(--heat-" + step(n, h.max) + ")" });
      if (sel && sel.ti === ti && sel.bi === bi) r.setAttribute("class", "heat-on");
      cells.append(r);
    }
  }
  svg.append(cells);
  if (sel) {
    const c = cellBox(sel.ti, sel.bi);
    svg.append(svgEl("rect", { class: "heat-sel", x: c.x - 1, y: c.y - 1, width: c.w + 2, height: c.h + 2 }));
  }
  const hover = svgEl("rect", { class: "heat-hover", visibility: "hidden" });
  const hit = svgEl("rect", { class: "heat-hit", x: PAD.left, y: PAD.top, width: plotW, height: plotH });
  svg.append(hover, hit);
  box.append(svg);
  const tip = div("graph-tip heat-tip");
  tip.hidden = true;
  box.append(tip);

  // at finds the cell under a pointer event, or null outside the grid.
  const at = (e) => {
    const r = svg.getBoundingClientRect();
    const px = e.clientX - r.left;
    const py = e.clientY - r.top;
    const t = t0 + ((px - PAD.left) / plotW) * (t1 - t0);
    let lo = 0, hi = starts.length - 1; // last start <= t
    while (lo < hi) {
      const mid = (lo + hi + 1) >> 1;
      if (starts[mid] <= t) lo = mid;
      else hi = mid - 1;
    }
    const bi = Math.floor((PAD.top + plotH - py) / bandH);
    if (bi < 0 || bi >= nb || t < t0 || t >= t1) return null;
    return { ti: lo, bi, px, py };
  };
  hit.addEventListener("pointermove", (e) => {
    const c = at(e);
    if (!c) {
      hover.setAttribute("visibility", "hidden");
      tip.hidden = true;
      return;
    }
    const b = cellBox(c.ti, c.bi);
    hover.setAttribute("x", b.x - 0.5);
    hover.setAttribute("y", b.y - 0.5);
    hover.setAttribute("width", b.w + 1);
    hover.setAttribute("height", b.h + 1);
    hover.setAttribute("visibility", "visible");
    hit.style.cursor = onSelect && count(h, c.ti, c.bi) ? "pointer" : "";
    tip.textContent = cellLabel(h, c.ti, c.bi);
    tip.hidden = false;
    // Keep the readout inside the chart: right of and below the pointer, or
    // flipped when near an edge.
    const tw = tip.offsetWidth, th = tip.offsetHeight;
    tip.style.left = (c.px + 14 + tw > width ? Math.max(0, c.px - 14 - tw) : c.px + 14) + "px";
    tip.style.top = (c.py + 14 + th > height ? Math.max(0, c.py - 14 - th) : c.py + 14) + "px";
  });
  hit.addEventListener("pointerleave", () => {
    hover.setAttribute("visibility", "hidden");
    tip.hidden = true;
  });
  hit.addEventListener("click", (e) => {
    if (!onSelect) return;
    const c = at(e);
    if (!c || !count(h, c.ti, c.bi)) return;
    onSelect(sel && sel.ti === c.ti && sel.bi === c.bi ? null : { ti: c.ti, bi: c.bi });
  });
}
