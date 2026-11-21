# Interval-aware stream list and per-service config — design

Date: 2026-11-21
Status: awaiting approval
Branch: `interval-streams` (worktree `.claude/worktrees/interval-streams`)
Background: architecture working notes, decisions 36b–36d.

## Intent

The span-streams change (`2026-11-20-span-streams-design.md`) put each span
into a fixed stream by its standard attributes. Using it showed two
problems:

1. **Most kinds of span have no stream of their own.** Internal work (sweeps,
   token refresh, boot steps, timers, email delivery) has no OTEL semantic
   convention, so it all falls into the one catch-all stream `span`. In the
   sample data that is about 1,400 of 5,600 spans, covering roughly 30 kinds.
2. **The list doesn't follow the interval.** The page fetches `/api/streams`
   once at load, and its counts cover the whole store. So you can't tell what
   is actually present in the selected time range.

The user also runs loghorn globally rather than inside one repo, so a repo's
`.config/loghorn/` is the wrong home for pins, favorites and formatting.

This change makes the stream list show what is in the selected interval,
lets users pin kinds, and moves config to a per-service location. It runs
on today's in-memory engine and does its own counting. The DuckDB query
engine redesign is pending and will replace that counting later. So the
counting sits behind one narrow function, which the engine work can swap
out.

## Goals

- The page has a **Logs / Spans switch**. Each mode has its own list.
- **Spans mode:**
  - The list shows the **top 10 kinds** in the selected interval, ranked by
    count, each with its count.
  - A *kind* is the span's classification (`db`, `http.server`,
    `http.client`, `messaging`, `rpc`, `gen_ai`). For a span the
    classification leaves unclassified, the kind is its `name`.
- **Logs mode:** the same list, over log streams (`labels.schema` /
  `http-request`).
- **More** shows the rest of the list, with a type-to-filter box.
- **Pins:**
  - Pinned kinds always come first and don't count toward the 10.
  - A pinned kind with nothing in the interval still shows, greyed out
    with `0`.
- **Counts follow the interval.** They are recomputed whenever the time
  range changes, and refreshed when the user re-runs a query.
- **Service selector:**
  - It lists the services present in the interval.
  - It is selected automatically when only one service is present.
  - **All** shows every service's records, with pins labelled by service.
  - The list, pins, favorites and field formatting follow the selected
    service.
- **Config lives per service**, under
  `~/.config/loghorn/services/<key>/`.
- **Columns:**
  - When the selected kind has a favorite with columns, the page uses
    those.
  - Otherwise the columns are inferred from that kind's records **in the
    interval**, not from all time.

## Non-goals

- **Listing values within a kind.** The list never shows individual
  tables, routes and so on; that is the job of Filters and a group-by in
  Table view.
- **Choosing the grouping by clicking the list's label** (decision 36c).
  The grouping is fixed in this change: classification, then name. A
  chooser can come later.
- **Queries across kinds or across sources,** and the trace view
  (decision 36b).
- **A new query engine.** The counting is a scan over the store's snapshot.
- **Automatic migration of existing repo config** (see Migration).

## Anti-goals

- **Nothing app-specific in loghorn.** Kinds come from standard OTEL
  attributes and span names only.
- **No list rows for values inside a kind.** For example, `db` is one row,
  never one row per table.
- **Interval counts must stay fast.** The list must not make range changes
  feel slower. Budget: list counts ≤ 50 ms over 200k records on this
  machine. A test measures this.
- **No ingestion changes,** as before.

## Decisions (the user's)

- Logs and spans are separate modes behind a switch.
- Config is keyed by `service.namespace` when set, else `service.name`.
- A repo's `.config/loghorn/` is retired. Only the per-service location is
  read.
- When the interval holds several services, a service selector decides
  which service's pins and favorites apply. **All** shows everything.
- The list shows the top 10 kinds, more are one click away, and kinds can
  be pinned.
- Values within a kind are Filters, not list rows.

## Kinds

Every span record gets a new field, **`span.type`**: the stream the
span-streams classifier assigns today (`gen_ai`, `messaging`, `rpc`, `db`,
`http.server`, `http.client`, or `span` for unclassified spans).

A span's **kind** is its `span.type` unless that is `span`; in that case the
kind is the span's `name`.

`record.Record.Stream` for a span becomes its kind, so the existing query
path (one source plus one stream per query) works unchanged. A query for
the kind `sweep autoOpen` is a query for spans whose stream is
`sweep autoOpen`.

For logs, the kind is the existing stream (`labels.schema`, else
`http-request`).

Consequences:

- **Stream names can now be any span name,** such as `SELECT invites` or
  `sweep autoOpen`. Stream names are no longer used as file names (see
  Config), so `schema.ValidStream` no longer limits them. Every URL and
  request carries the stream as a value, never in a path.
- **The schema cache** (keyed by source and stream) gets one entry per
  kind that is queried, not per kind that exists.

## Service

A record's **service key** is:

- `otel.resource.service.namespace` (or a top-level `service.namespace`)
  when present.
- Otherwise `service.name`, wherever the record carries it:
  - spans: the top-level `service.name`;
  - OTLP log records: the top-level `service.name`;
  - GCP `LogEntry`: `resource.labels.service_name`.
- Otherwise `default`.

Child stdout lines under `-- cmd` have no `service.name`. If the child's
environment has `OTEL_SERVICE_NAME` set, loghorn uses that. Otherwise they
go to `default`. Reading the child's environment is a small change in
`wrapper.go`, next to where `-otel` already builds the child's environment.

`record.Record` gains a **`Service`** field, set when the record is
normalised. The store counts by (service, source, kind).

## API

New:

- `GET /api/kinds?source=<log|otel>&service=<key|*>&from=<rfc3339>&to=<rfc3339>`
  returns `{"kinds":[{"name","count"}…], "services":[{"key","count"}…]}`.
  - `kinds` lists every kind with records in the range, sorted by count
    descending; the page applies the top 10 and More.
  - `services` lists the services present in the range for that source.
  - `from` and `to` are the query's resolved range. A relative range is
    resolved the same way `/api/query` resolves it.
- `GET /api/pins?service=<key>` and `PUT /api/pins?service=<key>` (body:
  `{"pins":[{"source","kind"}…]}`) read and replace the service's pins.

Changed:

- `/api/query`, `/api/schema` and `/api/favorites` take `service`. A
  missing value means the selected service (the page always sends it) and
  defaults to `default`. Records are filtered to that service unless it is
  `*`.
- `/api/streams` stays, for compatibility. The page stops using it.

## Counting (the in-house analysis)

`store.Kinds(source, service string, from, to time.Time) (kinds, services []Count)`
scans the snapshot once.

- Records are appended roughly in time order, but not strictly: span files
  are preloaded after log files, and spans arrive at their end time. So the
  scan can't binary-search on time without an index.
- Budget: ≤ 50 ms over 200k records, measured by a test with a generated
  store.

If the plain scan misses the budget, the builder adds a per-(service,
source, kind) list of record indexes sorted by time. It is maintained in
`Store.Add`, and counts become two binary searches per kind. Either way the
function signature stays the same, so the DuckDB engine can replace it.

## Columns

When a kind is selected:

1. **A favorite for (service, source, kind) with `Columns`:** the page uses
   them.
2. **Otherwise:** the **default column list** comes from fields present in
   that kind's records **in the query's range**. The rule is `timestamp`,
   `severity` and `message` for logs (or `name` and `duration_ms` for
   spans), then the fields present in the most matching rows, capped as
   today.

The **schema itself** (field types, used by validation, filters, the
filter picker and `/api/record`) is still inferred from **all** of the
kind's records, and cached per (service, source, kind) as today. So field
types never change with the range, and a favorite that filters on a field
absent from the interval still validates. The interval only decides which
columns show by default. That is a cheap per-query count of which fields
are present, not a second inference.

## Config

Location: `~/.config/loghorn/services/<escaped key>/`.

- **Directory base:** `$XDG_CONFIG_HOME/loghorn` when that is set,
  otherwise `~/.config/loghorn`.
- **Escaping the key:** letters, digits, `.`, `_` and `-` are kept; every
  other byte becomes `%XX`. An empty key becomes `default`.

Files:

- **`queries.toml`:** favorites, in the same format as today.
- **`pins.toml`:** `[[pin]] source = "otel" kind = "db"`.
- **`fields-log.toml` and `fields-otel.toml`:** field settings per source,
  keyed by field path.
  - They hold the `[[field]]` part of today's saved schema: a type
    override, plus visualizer, width and format.
  - They are split by source because the same path can mean different
    things in logs and spans (e.g. `status`). Within one source, a path
    formats the same in every kind.
  - A save **merges by path**: it writes only the fields the user changed,
    never the inferred types of the rest. The page's `saveFieldFormat` is
    changed to send just the edited field.
  - Today's per-stream `columns` list is dropped. Columns now come from
    favorites or inference.

**`FindConfigDir` is no longer used by the web view.** It is kept only if
something else still calls it; otherwise it is deleted. Per the CLAUDE.md
note about config lookups, nothing may walk up into `~/.config/loghorn`.
The per-service directory is reached directly, never found by walking.

### Migration

**No automatic move.**
- The first time loghorn starts and finds a repo `.config/loghorn/` with
  saved schemas or favorites, it prints one line to stderr. The line says
  the repo config is no longer read and names the per-service directory.
- A short section in `README.md` shows the manual copy.

My call: a repo's config can't be mapped to a service key automatically,
and moving files the user checked in is destructive.

## Service selection rules

- **Initial choice:**
  - The service named in the URL, if any.
  - Otherwise the last one used, kept in localStorage (a per-browser
    convenience, read in try/catch).
  - Otherwise the service with the most records in the interval.
  - With only one service present, it is that one.
- **When the selected service leaves the interval:** it stays selected. Its
  pinned kinds show greyed with `0`, and the selector still lists it,
  marked as having no records in range. The page never switches service
  on its own when the range changes.
- **All (`*`) is read-only for config:**
  - Pins show as the union of the pins of every service **present in the
    interval**, labelled by service.
  - Favorites show as a union, labelled by service.
  - Field settings fall back to defaults.
  - Pinning, saving a favorite and saving a field setting are disabled,
    with the tooltip "Choose a service to save".
- **Stored child stdout:** when replayed, child stdout lines in stored log
  files have no `OTEL_SERVICE_NAME`, so they fall into `default`. Only the
  live `-- cmd` session knows the child's service. This is accepted for now:
  writing the service into stored lines would be an ingestion change, and
  the planned log/span file merge will revisit it.

## Page

```
[Logs | Spans]   Service: shop-api ▾
Stream ▾                       (counts for the selected interval)
──────────────────────────
📌 http.server          320
📌 db                  3,910
   jwt.refresh          187
   sweep autoOpen        14
   …                           (top 10, after pins)
   More (14) ▸                 (expands in place, with a filter box)
```

- **The switch and the service selector** are `.sb-field` rows (see the
  CLAUDE.md sidebar note).
- **Pinning:**
  - Each row has a pin toggle that shows on hover. Toggling it saves at
    once through `PUT /api/pins`.
  - The pin toggle's button must `preventDefault` on `mousedown` (see the
    CLAUDE.md note), because the list redraws on blur.
- **Labels when names clash:** a kind name that exists under both sources
  can't appear twice in one mode, so the `db (log)` / `db (otel)` labels
  from the span-streams change are no longer needed. They are removed.
- **The page URL** carries the mode, service and kind. Old URLs with
  `stream` and `source` still open the same kind.

## Approaches considered

1. **Kind becomes the record's stream, and a store scan does the counting
   (chosen).** The existing per-stream query, schema and favorites path
   works unchanged. The scan is small and easy to swap for DuckDB.
2. **Add a `kind` filter and query across one source.** This needs the
   query path to drop its per-stream scope, which is the query-engine
   redesign. Too early.
3. **An incremental count index in the store from the start.** Faster, but
   it adds concurrency-sensitive code that DuckDB will throw away. It is
   kept only as the fallback if the scan misses the budget.

## Testing

- **Kinds:** a classified span's kind is its type; an unclassified span's
  kind is its name; a log record's kind is its stream; and `span.type` is
  set on every span.
- **Service key:** cover namespace over name, each record shape (span, OTLP
  log record, GCP `LogEntry`), `OTEL_SERVICE_NAME` for child stdout, and the
  `default` bucket.
- **`store.Kinds`:** range edges (inclusive `from`, exclusive `to`), service
  `*`, and empty results. A benchmark-style test enforces the 50 ms budget
  over 200k generated records.
- **API** (with `httptest.NewServer`): `/api/kinds` follows a changed range;
  pins round-trip through PUT and GET; `service` filtering on
  `/api/query`.
- **Config:**
  - The directory resolves with and without `XDG_CONFIG_HOME`, and nothing
    walks the tree.
  - The escaping round-trips.
  - Tests use a temp `HOME` and never write to the real one.
- **Page check in the browser:**
  - The switch, the service selector and the top 10 plus More.
  - Pinning, including a pinned kind that reads 0 in the interval.
  - Changing the range changes the counts.
  - Columns come from a favorite, or are inferred over the interval.

## Left to the builder

- The look of the More panel and the pin toggle, within the existing
  sidebar styles.
- How many services the selector shows before it scrolls.
- Whether the scan alone meets the 50 ms budget, or the time-sorted index
  is needed.
