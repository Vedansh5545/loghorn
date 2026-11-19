# loghorn — notes for agents

## Startup ordering (`main.go`)

Validate or bind anything that can fail with `os.Exit` (flag conflicts, the
`-web` listener) **before** launching the child command. `os.Exit` skips
deferred cleanup, and the child runs in its own process group, so an exit
after launch leaves it running.

## Web view (`internal/web`)

- The page (`internal/web/static/`) is embedded with `go:embed`. After editing
  any `app.js`, `app.css` or `index.html`, rebuild the binary (or re-run
  `go run ./internal/web/demo <logfile>`) before checking it in a browser.
- Keep the `[hidden]{display:none!important}` rule in `app.css`: a
  `display:flex` rule otherwise overrides the `hidden` attribute and hidden
  panels show.
- The Host guard reads the listener port from `http.LocalAddrContextKey`.
  Handler tests must use `httptest.NewServer`; a request built with
  `httptest.NewRequest` and passed to `ServeHTTP` has no local address and
  gets 403.
- Number values in test records must be `json.Number`: `schema.Convert`
  rejects plain `float64`/`int64` as the wrong type.
- A button inside a control that redraws on its input's blur/change must
  `preventDefault` on `mousedown`, or the redraw swallows the click.
- The server requires `plot` and `order` to be count or one of `aggregates`,
  so any edit of the metrics list in the page must also reconcile
  `state.plot` and `state.order` (see `syncAggregates`).
- Sidebar controls are `.sb-field` rows: a `.sb-l` label in the fixed label
  column, then a `.sb-body`. A new control must use that row shape (with
  `data-views` on the row) or its box won't line up with the others.
- `go:embed` can't reach the repo's `images/`, so
  `internal/web/static/favicon.svg` is a copy of `images/favicon.svg`. Edit
  both, or copy after editing.
- Before checking a page in the browser, make sure the server you start is
  the one listening: a demo left on the port makes the new one exit with
  "address already in use" while the browser keeps showing the old build.
  Find it with `lsof -nP -iTCP:<port> -sTCP:LISTEN` and kill it by PID.
- Saved TOML: BurntSushi/toml's `omitempty` keeps a zero int; add `omitzero`
  to any optional int field (see `schema.Field.Width`).
- Schema edits from the page queue one after another; each must re-apply its
  patch when it runs, because the previous save's reload replaces
  `schemaData` (see `saveFieldFormat`).

## Go modules

After `go get` of a module with generated protobuf code (e.g.
`go.opentelemetry.io/proto/otlp`), run `go mod tidy`: `go get` alone leaves
go.sum without the entries for its transitive grpc/grpc-gateway imports, and
the build fails on a clean checkout.

## Config lookups that walk up the tree

A helper that searches upward for a repo's `.config/loghorn/` must stop at the
git root (`.git` may be a file in a worktree), and outside git must not walk
above its start directory, or it picks up the user-level `~/.config/loghorn`.
Test it with no `.git` and with `HOME` as an ancestor of the start directory
(see `schema.FindConfigDir`).
