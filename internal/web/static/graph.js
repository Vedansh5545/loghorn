// Graph view: an SVG line or stacked-area chart of a query's time series. Every label reaches
// the DOM through textContent or attributes, never innerHTML.

const SVG = "http://www.w3.org/2000/svg";
const HEIGHT = 300;
const PAD = { top: 12, right: 16, bottom: 26 };
const SERIES_COLORS = 10;

// observers holds each container's ResizeObserver, so a redraw replaces it.
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

// seriesColor is the CSS colour of the i-th series; "other" is grey.
function seriesColor(s, i) {
  if (s.other) return "var(--series-other)";
  return "var(--series-" + ((i % SERIES_COLORS) + 1) + ")";
}

// niceStep rounds a raw step up to 1, 2, 2.5 or 5 times a power of ten.
function niceStep(raw) {
  if (!(raw > 0)) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(raw)));
  for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= raw) return m * p;
  return 10 * p;
}

// yTicks returns ascending tick values covering [lo, hi].
function yTicks(lo, hi, count, integer) {
  let step = niceStep((hi - lo) / count);
  if (integer) step = Math.max(1, Math.ceil(step));
  const start = Math.floor(lo / step) * step;
  const ticks = [];
  for (let i = 0; ; i++) {
    const v = Number(((Math.round(start / step) + i) * step).toPrecision(12));
    ticks.push(v);
    if (i >= 1 && v >= hi) break;
  }
  return ticks;
}

const pad2 = (n) => String(n).padStart(2, "0");
const SEC = 1000, MIN = 60 * SEC, HOUR = 60 * MIN, DAY = 24 * HOUR;
const TIME_STEPS = [SEC, 2 * SEC, 5 * SEC, 10 * SEC, 15 * SEC, 30 * SEC, MIN, 2 * MIN, 5 * MIN, 10 * MIN,
  15 * MIN, 30 * MIN, HOUR, 2 * HOUR, 3 * HOUR, 6 * HOUR, 12 * HOUR, DAY, 2 * DAY, 7 * DAY, 14 * DAY, 30 * DAY];

function dateLabel(d) {
  return pad2(d.getMonth() + 1) + "-" + pad2(d.getDate());
}

function clockLabel(d, seconds) {
  const t = pad2(d.getHours()) + ":" + pad2(d.getMinutes());
  return seconds ? t + ":" + pad2(d.getSeconds()) : t;
}

function sameDay(a, b) {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

// xTicks picks local-time ticks between t0 and t1 (ms), about one per
// `pixels / 90` and labelled at the precision the step needs.
function xTicks(t0, t1, pixels) {
  const want = Math.max(2, Math.floor(pixels / 90));
  const span = Math.max(t1 - t0, SEC);
  const step = TIME_STEPS.find((s) => span / s <= want) || TIME_STEPS[TIME_STEPS.length - 1];
  // Align ticks to local time, so hour and day ticks land on the hour and midnight.
  const off = -new Date(t0).getTimezoneOffset() * MIN;
  let t = Math.ceil((t0 + off) / step) * step - off;
  const ticks = [];
  let prev = null;
  for (; t <= t1; t += step) {
    const d = new Date(t);
    let label;
    if (step >= DAY) label = dateLabel(d);
    else {
      label = clockLabel(d, step < MIN);
      // Date-prefix the first tick when it is not today, and each day change.
      if (prev ? !sameDay(d, prev) : !sameDay(d, new Date())) {
        label = dateLabel(d) + " " + label;
      }
    }
    ticks.push({ t, label });
    prev = d;
  }
  return ticks;
}

// fullTime labels a bucket start in the hover readout.
function fullTime(d) {
  let s = clockLabel(d, true);
  if (d.getMilliseconds()) s += "." + String(d.getMilliseconds()).padStart(3, "0");
  if (!sameDay(d, new Date())) s = d.getFullYear() + "-" + dateLabel(d) + " " + s;
  return s;
}

// stackSeries returns each series' lower and upper edges when stacked in
// order, the first at the bottom. A gap (null) adds nothing.
export function stackSeries(series, n) {
  const base = new Array(n).fill(0);
  return series.map((s) => {
    const lower = base.slice();
    for (let j = 0; j < n; j++) {
      const v = s.values[j];
      if (v !== null && v !== undefined) base[j] += v;
    }
    return { lower, upper: base.slice() };
  });
}

// drawGraph renders graph (the server's Graph JSON) into container. format(v)
// labels a metric value; stacked draws stacked areas instead of lines. It
// redraws itself when the container is resized.
export function drawGraph(container, graph, format, stacked) {
  const old = observers.get(container);
  if (old) old.disconnect();
  observers.delete(container);
  container.replaceChildren();
  const starts = ((graph && graph.starts) || []).map((s) => new Date(s).getTime());
  const series = (graph && graph.series) || [];
  if (!series.length || !starts.length) {
    container.append(div("empty", "Nothing to plot in this range."));
    return;
  }

  const chart = div("graph-chart");
  const legend = div("graph-legend");
  container.append(chart, legend);
  for (const [i, s] of series.entries()) {
    const item = div("graph-key");
    const sw = div("graph-swatch");
    sw.style.background = seriesColor(s, i);
    item.append(sw, div("graph-key-label", s.label), div("graph-key-count", s.count.toLocaleString()));
    item.title = s.label + " · " + s.count.toLocaleString() + " records";
    legend.append(item);
  }

  let lastWidth = -1;
  const draw = () => {
    const width = Math.floor(chart.clientWidth);
    if (width <= 0 || width === lastWidth) return;
    lastWidth = width;
    plot(chart, width, starts, series, format, graph.metric && graph.metric.type === "int", !!stacked);
  };
  const ro = new ResizeObserver(draw);
  ro.observe(chart);
  observers.set(container, ro);
  draw();
}

function plot(chart, width, starts, series, format, integer, stacked) {
  chart.replaceChildren();
  const stack = stacked ? stackSeries(series, starts.length) : null;
  let lo = 0, hi = -Infinity;
  if (stack) {
    for (const v of stack[stack.length - 1].upper) if (v > hi) hi = v;
  } else {
    for (const s of series) {
      for (const v of s.values) {
        if (v === null || v === undefined) continue;
        if (v > hi) hi = v;
        if (v < lo) lo = v;
      }
    }
  }
  if (!isFinite(hi)) hi = 1;
  if (hi === lo) hi = lo + 1;
  const ticks = yTicks(lo, hi, 5, integer);
  const yMin = ticks[0], yMax = ticks[ticks.length - 1];
  const labels = ticks.map(format);
  const left = Math.min(120, 10 + 7 * Math.max(...labels.map((l) => l.length)));
  const plotW = Math.max(10, width - left - PAD.right);
  const plotH = HEIGHT - PAD.top - PAD.bottom;
  const t0 = starts[0], t1 = starts[starts.length - 1];
  const x = (t) => left + (t1 === t0 ? plotW / 2 : ((t - t0) / (t1 - t0)) * plotW);
  const y = (v) => PAD.top + plotH - ((v - yMin) / (yMax - yMin)) * plotH;

  const svg = svgEl("svg", { width, height: HEIGHT, viewBox: "0 0 " + width + " " + HEIGHT, role: "img" });
  svg.setAttribute("aria-label", (stack ? "stacked area graph of " : "graph of ") + series.length + " series over " + starts.length + " time buckets");

  const grid = svgEl("g", { class: "graph-grid" });
  ticks.forEach((v, i) => {
    const yy = Math.round(y(v)) + 0.5;
    grid.append(svgEl("line", { x1: left, x2: left + plotW, y1: yy, y2: yy, class: v === 0 ? "graph-base" : "" }));
    const t = svgEl("text", { x: left - 6, y: yy, "text-anchor": "end", "dominant-baseline": "middle" });
    t.textContent = labels[i];
    grid.append(t);
  });
  for (const tk of xTicks(t0, t1, plotW)) {
    const xx = Math.round(x(tk.t)) + 0.5;
    grid.append(svgEl("line", { x1: xx, x2: xx, y1: PAD.top + plotH, y2: PAD.top + plotH + 4 }));
    const t = svgEl("text", { x: xx, y: HEIGHT - 6, "text-anchor": "middle" });
    t.textContent = tk.label;
    grid.append(t);
  }
  svg.append(grid);

  if (stack) drawStack(svg, starts, series, stack, x, y);
  // Draw "other" first and the largest series last, so the top series is on top.
  const order = stack ? [] : series.map((s, i) => i).reverse();
  for (const i of order) {
    const s = series[i];
    const g = svgEl("g", { class: "graph-series" });
    g.style.stroke = seriesColor(s, i);
    g.style.fill = seriesColor(s, i);
    let d = "";
    let run = 0;
    s.values.forEach((v, j) => {
      const isNull = v === null || v === undefined;
      const lonely = !isNull && run === 0 && (j + 1 >= s.values.length || s.values[j + 1] === null || s.values[j + 1] === undefined);
      if (isNull) {
        run = 0;
        return;
      }
      const px = x(starts[j]).toFixed(1), py = y(v).toFixed(1);
      if (lonely) g.append(svgEl("circle", { cx: px, cy: py, r: 2.5, class: "graph-dot" }));
      else d += (run === 0 ? "M" : "L") + px + " " + py;
      run++;
    });
    if (d) g.append(svgEl("path", { d, class: "graph-line" }));
    svg.append(g);
  }

  // Hover: a rule at the nearest bucket and a readout of every series.
  const rule = svgEl("line", { y1: PAD.top, y2: PAD.top + plotH, class: "graph-rule", visibility: "hidden" });
  const hit = svgEl("rect", { x: left, y: PAD.top, width: plotW, height: plotH, class: "graph-hit" });
  svg.append(rule, hit);
  chart.append(svg);
  const box = div("graph-tip");
  box.hidden = true;
  chart.append(box);

  const nearest = (px) => {
    let best = 0, bd = Infinity;
    starts.forEach((t, j) => {
      const dd = Math.abs(x(t) - px);
      if (dd < bd) { bd = dd; best = j; }
    });
    return best;
  };
  hit.addEventListener("pointermove", (e) => {
    const r = svg.getBoundingClientRect();
    const j = nearest(e.clientX - r.left);
    const xx = Math.round(x(starts[j])) + 0.5;
    rule.setAttribute("x1", xx);
    rule.setAttribute("x2", xx);
    rule.setAttribute("visibility", "visible");
    box.replaceChildren(div("graph-tip-time", fullTime(new Date(starts[j]))));
    series.forEach((s, i) => {
      const v = s.values[j];
      const line = div("graph-tip-row");
      const sw = div("graph-swatch");
      sw.style.background = seriesColor(s, i);
      line.append(sw, div("graph-tip-label", s.label),
        div("graph-tip-value", v === null || v === undefined ? "—" : format(v)));
      box.append(line);
    });
    if (stack) {
      const line = div("graph-tip-row graph-tip-total");
      line.append(div(""), div("graph-tip-label", "total"), div("graph-tip-value", format(stack[stack.length - 1].upper[j])));
      box.append(line);
    }
    box.hidden = false;
    // Keep the box inside the chart: right of the rule, or left when near the edge.
    const bw = box.offsetWidth;
    box.style.left = (xx + 12 + bw > width ? Math.max(0, xx - 12 - bw) : xx + 12) + "px";
    box.style.top = PAD.top + "px";
  });
  hit.addEventListener("pointerleave", () => {
    rule.setAttribute("visibility", "hidden");
    box.hidden = true;
  });
}

// drawStack draws each series as the band between its lower and upper edges,
// with a line along its top. One bucket has no width, so it gets dots.
function drawStack(svg, starts, series, stack, x, y) {
  const n = starts.length;
  series.forEach((s, i) => {
    const g = svgEl("g", { class: "graph-series" });
    g.style.stroke = seriesColor(s, i);
    g.style.fill = seriesColor(s, i);
    const { lower, upper } = stack[i];
    if (n === 1) {
      g.append(svgEl("circle", { cx: x(starts[0]).toFixed(1), cy: y(upper[0]).toFixed(1), r: 2.5, class: "graph-dot" }));
      svg.append(g);
      return;
    }
    let top = "";
    for (let j = 0; j < n; j++) top += (j ? "L" : "M") + x(starts[j]).toFixed(1) + " " + y(upper[j]).toFixed(1);
    let area = top;
    for (let j = n - 1; j >= 0; j--) area += "L" + x(starts[j]).toFixed(1) + " " + y(lower[j]).toFixed(1);
    g.append(svgEl("path", { d: area + "Z", class: "graph-area" }), svgEl("path", { d: top, class: "graph-line graph-edge" }));
    svg.append(g);
  });
}
