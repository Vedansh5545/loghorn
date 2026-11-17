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
- The server requires `plot` to be count or one of `aggregates`, so any edit
  of the metrics list in the page must also reconcile `state.plot`.

## Config lookups that walk up the tree

A helper that searches upward for a repo's `.config/loghorn/` must stop at the
git root (`.git` may be a file in a worktree), and outside git must not walk
above its start directory, or it picks up the user-level `~/.config/loghorn`.
Test it with no `.git` and with `HOME` as an ancestor of the start directory
(see `schema.FindConfigDir`).
