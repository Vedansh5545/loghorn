# Web view: column menus, complete URLs, LogEntry defaults, links — design

Date: 2026-11-17
Status: approved in conversation ("go ahead")
Builds on: `2026-11-17-web-query-design.md`, `2026-11-17-web-explore-design.md`

## Intent

Make the page's columns easy to arrange and format in place, make a copied
link reproduce the screen, give Cloud Logging LogEntry fields sensible
formatting out of the box, and make URLs in log data clickable.

## Decided by the user

- **The URL represents what is on screen.** Listing every column is an
  *output* rule: URLs the page writes always carry the stream and the exact
  column list. URLs and favorites without them are still accepted (defaults
  fill in), and the page then writes the full form.
- **Formatting is per schema, not per query**: type, visualizer, unit, label
  and **width** live in the stream's saved schema (`.config/loghorn/`), never
  in the query or URL. Dragging a width saves it to the schema (replaces the
  browser-local widths).
- **Click a column name** (table header in Samples, or the sidebar list) to
  open a column menu; the sidebar "edit" button and its checkbox panel go.
  Menu items:
  - format: visualizer, unit, label — saved to the schema immediately
  - hide (removes it from this query's columns; the header trash icon stays as
    a shortcut for the same)
  - sort ascending / sort descending (sets the query's sort)
  - **Add / Move ▸** submenu, two lists; picking an item places it
    immediately right of this column:
    - Add: fields not shown — those hidden in this query first, then the rest
    - Move: the other shown columns
  - **Move to start**
- **A→Z** button in the sidebar Columns section: sorts shown columns by the
  final path element (`userAuth.detail.code` → `code`), case-insensitive,
  ties by full path. `timestamp` is not pinned.
- **restore defaults** stays: the stream's default column list.
- **Default stream** when the URL names none: the stream with the most
  records (not the alphabetically first). "All streams" stays deferred.
- **Built-in defaults for LogEntry fields** (when nothing is saved;
  overridable like any formatting):
  - `pid`, `httpRequest.status` → new `plain` visualizer (number, no
    thousands separators)
  - `insertId`, `trace`, `spanId`, `hostname`, `labels.*` → `raw`
  - `timestamp`, `receiveTimestamp` → `local-time`
  - `severity` → `severity`
  - `httpRequest.latency` → `duration`
  - `httpRequest.requestUrl` → new `link` visualizer
- **Active links**: any `http://` or `https://` URL inside a cell (any
  visualizer that shows text) or in the expanded record becomes an `<a>`
  opening in a new tab (`target="_blank" rel="noopener noreferrer"`). Other
  schemes stay text. Built with DOM nodes, never `innerHTML`.

## Non-goals

- "All streams"; cross-stream formatting.
- Column menus on Table and Graph results (their columns come from group-by
  and metrics, edited in the sidebar).
- Drag-and-drop reordering of headers.

## Approaches

One sensible way: extend the existing schema model (`schema.Field` gains
`Width`; two visualizers added; `DefaultVisualizer` consults a built-in
LogEntry table) and the existing page. Keeping widths in browser storage was
rejected by the user's "formatting is per schema". A separate
per-field-path formatting store across streams was rejected ("global" means
per schema).

## Left to the builder

Menu/submenu styling and keyboard handling (Escape closes; menu closes on
outside click); exact link regex (must stop at whitespace and trailing
`)`, `]`, `,`, `.`, `"`, `'` punctuation); how the page batches schema saves
during a drag (save on drag end only).
