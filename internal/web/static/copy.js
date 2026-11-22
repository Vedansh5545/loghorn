// loghorn copy helpers: table serializers, the clipboard call and the small
// copy buttons the page puts next to SQL and under tables. Text reaches the
// DOM through textContent only.

const SVG = "http://www.w3.org/2000/svg";

// toCSV serializes headers and rows (arrays of strings) as RFC 4180 CSV: a
// field holding a comma, quote, CR or LF is quoted with inner quotes doubled;
// lines end in CRLF.
export function toCSV(headers, rows) {
  const field = (v) => {
    const s = v === null || v === undefined ? "" : String(v);
    return /[",\r\n]/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s;
  };
  return [headers, ...rows].map((r) => r.map(field).join(",")).join("\r\n") + "\r\n";
}

// toMarkdown serializes headers and rows as a GitHub table: | escaped as \|,
// line breaks in a cell replaced with a space.
export function toMarkdown(headers, rows) {
  const cell = (v) => {
    const s = v === null || v === undefined ? "" : String(v);
    return s.replace(/\|/g, "\\|").replace(/\r\n|\r|\n/g, " ");
  };
  const line = (r) => "| " + r.map(cell).join(" | ") + " |";
  const out = [line(headers), "|" + headers.map(() => " --- |").join("")];
  for (const r of rows) out.push(line(headers.map((_, i) => r[i])));
  return out.join("\n") + "\n";
}

// copyText puts text on the clipboard; it throws when the browser has no
// clipboard access (an insecure origin) or refuses it.
export async function copyText(text) {
  if (!navigator.clipboard || !navigator.clipboard.writeText) throw new Error("clipboard not available");
  await navigator.clipboard.writeText(text);
}

const PATHS = {
  copy: ["M5.5 5.5h8v8h-8z", "M10.5 5.5v-3h-8v8h3"],
  check: ["M3 8.5l3.2 3.2L13 4.8"],
};

// copyIcon is a 16×16 stroke icon, in the style of the column menu's icons.
export function copyIcon(name = "copy") {
  const svg = document.createElementNS(SVG, "svg");
  svg.setAttribute("viewBox", "0 0 16 16");
  svg.setAttribute("width", "14");
  svg.setAttribute("height", "14");
  svg.setAttribute("aria-hidden", "true");
  for (const d of PATHS[name]) {
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

// copyButton is an icon button that copies text() and shows a check for a
// moment (the error as its title on failure). A click on it neither toggles
// an enclosing <details> nor reaches an enclosing row.
export function copyButton(text, { title = "copy SQL", cls = "" } = {}) {
  const b = document.createElement("button");
  b.type = "button";
  b.className = "copy-btn" + (cls ? " " + cls : "");
  b.title = title;
  b.setAttribute("aria-label", title);
  b.append(copyIcon());
  let timer = 0;
  b.addEventListener("click", async (e) => {
    e.preventDefault(); // a <summary> parent would toggle its <details>
    e.stopPropagation(); // a table row would open its detail
    clearTimeout(timer);
    try {
      await copyText(text());
      b.replaceChildren(copyIcon("check"));
      b.classList.add("copied");
      b.title = "Copied";
    } catch (err) {
      b.classList.add("copy-failed");
      b.title = "Copy failed: " + (err && err.message ? err.message : err);
    }
    timer = setTimeout(() => {
      b.replaceChildren(copyIcon());
      b.classList.remove("copied", "copy-failed");
      b.title = title;
    }, 1500);
  });
  return b;
}

// copyTableBar is a row of "Copy as Markdown" and "Copy as CSV" buttons and
// a status line. table() returns {headers, rows} at click time.
export function copyTableBar(table, cls = "") {
  const bar = document.createElement("span");
  bar.className = "copy-bar" + (cls ? " " + cls : "");
  const status = document.createElement("span");
  status.className = "copy-status muted";
  status.setAttribute("role", "status");
  let timer = 0;
  for (const [label, fmt] of [["Copy as Markdown", toMarkdown], ["Copy as CSV", toCSV]]) {
    const b = document.createElement("button");
    b.type = "button";
    b.textContent = label;
    b.addEventListener("click", async () => {
      clearTimeout(timer);
      const t = table();
      try {
        await copyText(fmt(t.headers, t.rows));
        status.textContent = "Copied " + t.rows.length.toLocaleString() + " row" + (t.rows.length === 1 ? "" : "s");
      } catch (err) {
        status.textContent = "Copy failed: " + (err && err.message ? err.message : err);
      }
      timer = setTimeout(() => (status.textContent = ""), 2500);
    });
    bar.append(b);
  }
  bar.append(status);
  return bar;
}
