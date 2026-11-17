# Web Columns, URLs, LogEntry Defaults, Links Implementation Plan

> **For agentic workers:** Execute with the `orchestrating-plans` skill (one task-runner subagent per task, each dispatching its own fresh reviewer). Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Column menus (format/hide/sort/Add-Move/Move to start), A→Z, schema-stored widths, complete written URLs, largest-stream default, built-in LogEntry formats, clickable links.

**Architecture:** `schema.Field` gains `Width`; visualizers `plain` and `link` join the set; `DefaultVisualizer` checks a built-in LogEntry table first. The page (`internal/web/static/app.js`) renders links, writes full URLs, and replaces the column editor with per-column menus that save formatting through the existing `PUT /api/schema`.

**Tech Stack:** Go standard library; vanilla ES modules, no build step.

**Spec:** `docs/superpowers/specs/2026-11-17-web-columns-design.md`

## Global Constraints

- No new Go modules, no third-party JS, no CDN; log data reaches the DOM only via `textContent`/attributes/`createElement` — never `innerHTML`.
- Links: only `http:` and `https:` become `<a>`, with `target="_blank"` and `rel="noopener noreferrer"`.
- Keep `[hidden]{display:none!important}` in `app.css`.
- After editing `internal/web/static/*`, rebuild/re-run the demo before a browser check (files are `go:embed`ed).
- Never pipe input into loghorn to test. Browser checks: `go run ./internal/web/demo -port 8797 testdata/web/gcloud.json testdata/web/pino.log` in the background; stop it afterwards. Do not write the demo's schema saves into the loghorn repo's `.config/loghorn/` — run the demo from a scratch directory (`cd` to a temp dir and pass absolute testdata paths) so saves land there.
- Handler tests use `httptest.NewServer`.
- Full checks before each commit: `test -z "$(gofmt -l .)" && go vet ./... && go test ./... && for f in internal/web/static/*.js; do node --check "$f" || exit 1; done`.
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **Link injection**: a value like `javascript:alert(1)` or `"><img src=x onerror=…>` must render as text; a URL followed by `).,"'` must not swallow the punctuation. Test: browser check in Task 1 with a crafted record.
2. **Old schema files** without `width` and old URLs/favorites without `columns`/`stream` load unchanged; a saved schema written before `plain`/`link` existed still validates. Test: `TestLoadSavedWithoutWidth` (Task 1).
3. **Saved formatting must not be lost**: a column-menu format change or width drag on an unsaved (inferred-only) schema saves the whole effective schema, and a second edit keeps the first. Browser check in Task 2 (reload, both edits present).
4. **Add/Move onto itself / last column / single column**: moving a column to the right of itself is not offered; hiding the last remaining column is refused. Browser check in Task 2.
5. **Stream switch** while a column menu is open closes it and does not write the old stream's schema into the new one. Browser check in Task 2.

---

### Task 1: Schema width + visualizers + LogEntry defaults; page links, plain/link, full URLs, largest stream

**Review: full** (output-escaping). **Model: sonnet.**

**Files:**
- Modify: `internal/schema/types.go` (Field.Width, `VPlain`, `VLink`), `internal/schema/saved.go` (validVisualizers; Width validation ≥ 0), `internal/schema/infer.go` (`DefaultVisualizer`)
- Modify: `internal/web/static/app.js` (visualize, detail view, saveURL, default stream)
- Test: `internal/schema/*_test.go` (existing test files for infer/saved)

**Interfaces:**
- Produces (Go): `Field.Width int \`toml:"width,omitempty" json:"width,omitempty"\`` (pixels; 0 = automatic). `VPlain = "plain"`, `VLink = "link"`. `DefaultVisualizer(path, t)` returns, before its type switch: `pid`, `httpRequest.status` → `plain`; `insertId`, `trace`, `spanId`, `hostname`, any `labels.` prefix → `raw`; `timestamp`, `receiveTimestamp` → `local-time`; `severity` → `severity`; `httpRequest.latency` → `duration`; `httpRequest.requestUrl` → `link`. `Effective` must carry `Width` from saved fields.
- Produces (JS, used by Task 2): `function linkify(text): Node` (a `<span>` of text nodes and `<a>`s); `visualize` handles `plain` (number as `String(v)`, no grouping) and `link` (whole value as one link when it is http(s), else text); every text-showing visualizer (`raw`, default, `sql` preview, `json` preview) passes its text through `linkify`; the expanded record's `<pre>` content is built with `linkify`. `saveURL()` writes `stream` and `columns` explicitly (columns = `currentColumns()` when `state.columns` is empty). `loadSession` picks the stream with the highest `count` when the URL names none or names an unknown stream.

- [ ] **Step 1: Failing Go tests**: `TestDefaultVisualizerLogEntry` (each listed path → its visualizer; `labels.schema` → raw; unlisted int → `number`), `TestSavedWidthRoundTrip` (save with Width 140, load → 140), `TestLoadSavedWithoutWidth` (old TOML without width loads, Width 0), `TestValidateRejectsNegativeWidth`, `TestPlainAndLinkVisualizersValid`.
- [ ] **Step 2: Run** `go test ./internal/schema/` — FAIL.
- [ ] **Step 3: Implement** the Go side.
- [ ] **Step 4: Run** `go test ./internal/schema/ ./internal/query/ ./internal/web/` — PASS.
- [ ] **Step 5: Page.** Implement the JS interfaces above. Link regex stops at whitespace and does not include trailing `)` `]` `,` `.` `"` `'`.
- [ ] **Step 6: Browser check** (demo from a scratch dir; add a scratch copy of `testdata/web/gcloud.json` with one extra record whose message contains `see https://example.com/a?b=1).` and `javascript:alert(1)` and `<img src=x onerror=alert(1)>`): the https URL is a link without the `).`; the others are plain text; `pid` shows without separators; the URL after load contains `"columns":[…]` with the real list and the largest stream; an old URL with no columns still loads.
- [ ] **Step 7: Full checks, commit** `feat(web): LogEntry default formats, clickable links, complete URLs`.

---

### Task 2: Column menus, Add/Move, A→Z, widths in the schema

**Review: full** (schema writes). **Model: opus.**

**Files:**
- Modify: `internal/web/static/app.js`, `app.css`, `index.html`
- Modify: `README.md` (web view: column menu, A→Z, widths saved in schema)

**Interfaces:**
- Consumes: Task 1's `Field.Width`, `plain`/`link` visualizers, `saveURL` full-columns behaviour; the existing `saveSchema`/`PUT /api/schema?stream=` body shape (see `saveSchema` in `app.js`), `schemaFields()`, `currentColumns()`, `headerCell`, `startResize`, `applyWidths`.
- Produces:
  - `async function saveFieldFormat(path, patch)` — merges `patch` (any of `visualizer`, `unit`, `label`, `width`, `type`) into that field of the current effective schema and PUTs the whole schema (same body as the Schema dialog), then reloads `schemaData` and re-renders. Fields not yet in the schema (column for an unseen path) are added with type `string`.
  - `function openColumnMenu(path, anchorEl)`; `function moveColumnAfter(path, afterPath)`; `function moveColumnToStart(path)`; `function alphabetizeColumns()` (key: last `.`-segment lower-cased, then full path).
  - `applyWidths` reads widths from `schemaFields()` (`f.width`) instead of `localStorage`; drag end calls `saveFieldFormat(path, {width})`; double-click on the resize grip saves `{width: 0}`. `loadWidths`/`saveWidths`/`widthsKey` and the localStorage use are removed.
  - `hiddenThisQuery` — the page remembers paths hidden in this query (session-only, in memory) to list them first under Add.

- [ ] **Step 1: Menu.** Clicking the label part of a Samples header (not the trash icon or the resize grip) or a name in the sidebar column list opens the column menu anchored to it: visualizer `select` (all visualizers), unit (only for timestamp/duration types), label text (saved on Enter/blur), Hide, Sort ascending, Sort descending, Add / Move ▸ (submenu with "Add" and "Move" lists; Move omits the column itself), Move to start (disabled for the first column). Escape or an outside click closes it; switching stream closes it. Remove the sidebar `edit` button, `#columns-panel`, `openColumns`/`renderColList` and their CSS. Keep `restore defaults`; add an `A→Z` button beside it.
- [ ] **Step 2: Widths in schema** as in Interfaces; remove localStorage width code.
- [ ] **Step 3: Browser check** (demo from a scratch dir): change `message`'s label via the menu → header updates and survives reload; drag a width → reload keeps it (schema file in the scratch `.config/loghorn/` has `width`), then change a visualizer → width still there; hide a column, reopen any column's Add/Move → the hidden one is listed first, adding puts it right of that column; Move puts a column right of the chosen one; Move to start; A→Z orders `httpRequest.status` by `status`; hiding the last column is refused; sort descending on a column reorders rows and the URL's `sort` changes; switching stream with a menu open closes it.
- [ ] **Step 4: README** web view section.
- [ ] **Step 5: Full checks, commit** `feat(web): column menus, add/move, A→Z, widths saved in the schema`.
