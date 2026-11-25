// loghorn copy helpers: table serializers, the clipboard call and the small
// copy buttons the page puts next to SQL and under tables. Text reaches the
// DOM through textContent only.

const SVG = "http://www.w3.org/2000/svg";

// toCSV serializes headers and rows (arrays of strings) as RFC 4180 CSV: a
// field holding a comma, quote, CR or LF is quoted with inner quotes doubled;
// lines end in CRLF. Each cell is neutralized first (see neutralize).
export function toCSV(headers, rows) {
  const field = (v) => {
    const s = neutralize(v === null || v === undefined ? "" : String(v));
    return /[",\r\n]/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s;
  };
  return [headers, ...rows].map((r) => r.map(field).join(",")).join("\r\n") + "\r\n";
}

// neutralize keeps a spreadsheet from running a log value as a formula: a
// cell starting with =, +, @, a tab or a CR, or with - unless the whole cell
// is a plain number, gets a leading single quote.
export function neutralize(s) {
  if (/^[=+@\t\r]/.test(s)) return "'" + s;
  if (s.startsWith("-") && !/^-(\d+\.?\d*|\.\d+)(e[+-]?\d+)?$/i.test(s)) return "'" + s;
  return s;
}

// toMarkdown serializes headers and rows as a GitHub table: | escaped as \|,
// line breaks in a cell replaced with a space.
export function toMarkdown(headers, rows) {
  const cell = (v) => {
    const s = v === null || v === undefined ? "" : String(v);
    // A backslash before a pipe or at the end would escape that pipe.
    return s.replace(/\\(?=\||$)/g, "\\\\").replace(/\|/g, "\\|").replace(/\r\n|\r|\n/g, " ");
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
      b.setAttribute("aria-label", "Copied");
    } catch (err) {
      b.classList.add("copy-failed");
      b.title = "Copy failed: " + (err && err.message ? err.message : err);
      b.setAttribute("aria-label", b.title);
    }
    timer = setTimeout(() => {
      b.replaceChildren(copyIcon());
      b.classList.remove("copied", "copy-failed");
      b.title = title;
      b.setAttribute("aria-label", title);
    }, 1500);
  });
  return b;
}

// toTSV serializes headers and rows as tab-separated text: a tab, newline,
// carriage return or backslash in a cell is written as \t, \n, \r or \\.
export function toTSV(headers, rows) {
  const field = (v) =>
    (v === null || v === undefined ? "" : String(v))
      .replace(/\\/g, "\\\\")
      .replace(/\t/g, "\\t")
      .replace(/\n/g, "\\n")
      .replace(/\r/g, "\\r");
  return [headers, ...rows].map((r) => r.map(field).join("\t")).join("\n") + "\n";
}

export const COPY_FORMATS = {
  md: "Markdown",
  csv: "CSV",
  tsv: "TSV",
  json: "JSON",
  jsonl: "JSONL",
  yaml: "YAML",
  toon: "TOON",
};
const LOCAL_FORMATS = { md: toMarkdown, csv: toCSV, tsv: toTSV };
const FORMAT_KEY = "loghorn.copyFormat";

function storedFormat(formats) {
  try {
    const f = localStorage.getItem(FORMAT_KEY);
    if (f && formats.includes(f)) return f;
  } catch {
    // no storage: use the default
  }
  return formats.includes("md") ? "md" : formats[0];
}

function storeFormat(f) {
  try {
    localStorage.setItem(FORMAT_KEY, f);
  } catch {
    // the choice lasts until reload
  }
}

// copyTableBar is a split button: "Copy as <format>" copies in the remembered
// format and a ▾ opens the list of formats; picking one copies and becomes the
// new choice (kept per browser in localStorage). formats lists the ids shown
// (default Markdown, CSV, TSV). With fetchText(format) → Promise<string> the
// text comes from the server; without it source() returns {headers, rows} at
// click time and the bar formats that table itself. The status line shows
// "Copied N rows" (client-side) or "Copied"; a failure shows "Copy failed: …".
export function copyTableBar(source, cls = "", { formats = ["md", "csv", "tsv"], fetchText } = {}) {
  const bar = document.createElement("span");
  bar.className = "copy-bar" + (cls ? " " + cls : "");
  let fmt = storedFormat(formats);
  const main = document.createElement("button");
  main.type = "button";
  main.className = "copy-main";
  const caret = document.createElement("button");
  caret.type = "button";
  caret.className = "copy-caret";
  caret.textContent = "▾";
  caret.title = "Choose the copy format";
  caret.setAttribute("aria-label", "Choose the copy format");
  caret.setAttribute("aria-haspopup", "listbox");
  caret.setAttribute("aria-expanded", "false");
  const group = document.createElement("span");
  group.className = "copy-split";
  group.append(main, caret);
  const list = document.createElement("ul");
  list.className = "copy-list";
  list.setAttribute("role", "listbox");
  list.hidden = true;
  const status = document.createElement("span");
  status.className = "copy-status muted";
  status.setAttribute("role", "status");
  let timer = 0;

  const label = () => {
    main.textContent = "Copy as " + COPY_FORMATS[fmt];
    for (const li of list.children) li.setAttribute("aria-selected", String(li.dataset.format === fmt));
  };
  const show = (msg) => {
    clearTimeout(timer);
    status.textContent = msg;
    timer = setTimeout(() => (status.textContent = ""), 2500);
  };
  // The document listeners exist only while the list is open, so a bar that
  // is redrawn away (the trace view does) leaves nothing behind.
  const onDocClick = () => close();
  const onDocKey = (e) => {
    if (e.key === "Escape") close();
  };
  function close() {
    list.hidden = true;
    caret.setAttribute("aria-expanded", "false");
    document.removeEventListener("click", onDocClick);
    document.removeEventListener("keydown", onDocKey);
  }
  const copy = async () => {
    const f = fmt;
    try {
      if (fetchText) {
        if (typeof ClipboardItem !== "undefined" && navigator.clipboard && navigator.clipboard.write) {
          // Hand the clipboard a promise so the click's permission outlives the fetch.
          const blob = fetchText(f).then((t) => new Blob([t], { type: "text/plain" }));
          blob.catch(() => {}); // reported below; no unhandled rejection
          try {
            await navigator.clipboard.write([new ClipboardItem({ "text/plain": blob })]);
          } catch (err) {
            // A failed fetch rejects the write with a generic DOMException;
            // the fetch's own error (the server's text) says what went wrong.
            await blob;
            throw err;
          }
        } else {
          await copyText(await fetchText(f));
        }
        show("Copied");
      } else {
        const t = source();
        await copyText(LOCAL_FORMATS[f](t.headers, t.rows));
        show("Copied " + t.rows.length.toLocaleString() + " row" + (t.rows.length === 1 ? "" : "s"));
      }
    } catch (err) {
      show("Copy failed: " + (err && err.message ? err.message : err));
    }
  };
  for (const id of formats) {
    const li = document.createElement("li");
    li.setAttribute("role", "option");
    li.tabIndex = -1;
    li.dataset.format = id;
    li.textContent = COPY_FORMATS[id];
    // A press must not blur the page's inputs: a redraw would swallow the click.
    li.addEventListener("mousedown", (e) => e.preventDefault());
    const pick = () => {
      fmt = id;
      storeFormat(id);
      label();
      close();
      copy();
    };
    li.addEventListener("click", pick);
    li.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        pick();
      }
    });
    list.append(li);
  }
  main.addEventListener("click", () => {
    close();
    copy();
  });
  caret.addEventListener("mousedown", (e) => e.preventDefault());
  caret.addEventListener("click", (e) => {
    e.stopPropagation();
    if (!list.hidden) return close();
    list.hidden = false;
    caret.setAttribute("aria-expanded", "true");
    document.addEventListener("click", onDocClick);
    document.addEventListener("keydown", onDocKey);
  });
  list.addEventListener("click", (e) => e.stopPropagation());
  label();
  bar.append(group, list, status);
  return bar;
}
