# `-otel` Receiver and TUI Removal Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: execute with the `orchestrating-plans` skill (one task-runner subagent per task, each dispatching its own fresh reviewer). Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `loghorn -otel -- <cmd>` receives the child's OTLP/HTTP logs into `.loghorn/` next to its other output, and `loghorn -- <cmd>` becomes a transparent wrapper with the TUI deleted.

**Architecture:** Two standalone libraries come first: `internal/otlp` maps OTLP `LogRecord`s to JSON lines that the existing normaliser reads, and serves OTLP/HTTP on loopback. Then a new child runner (`runner.StartChild`: pty or pipes, no `Setpgid`), a `recorder` (the single, mutex-guarded point where input records and OTLP records enter the log file, the live web store and the alerts) and a `wrap` package (pass-through copy, framing, `-filter`, signal forwarding, drain). Task 5 rewires `main.go` onto them and deletes the TUI; Task 6 adds `-otel` on top.

**Order, and why:** the OTLP mapping and receiver (Tasks 1–2) are libraries with no caller yet, so they cannot break anything. The wrapper pieces (Tasks 3–4) are added beside the old runner, not in place of it, because the TUI still calls `runner.Start`/`Send`/`Terminate` until Task 5. Task 5 switches `main.go` over and deletes the TUI and everything only it used in one step, since a half-removed TUI cannot compile. `-otel` comes last (Task 6) because it depends on the wrapper's lifecycle (exit drain, child environment) and on the recorder's dedupe switch.

**Tech Stack:** Go 1.25; new modules `go.opentelemetry.io/proto/otlp` (v1.11.x; needs `LogRecord.GetEventName`, present since v1.5.0) and `google.golang.org/protobuf` (`proto`, `encoding/protojson`); `golang.org/x/term` for `IsTerminal`; test-only `go.opentelemetry.io/otel/sdk/log` and `go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp`. `github.com/creack/pty` (already present) and `github.com/charmbracelet/x/ansi` (already present; kept for `ansi.Strip`).

**Spec:** `docs/superpowers/specs/2026-11-19-otel-receiver-design.md` (background: `docs/superpowers/specs/2026-11-19-architecture-working-notes.md`, decisions 1, 8, 9, 41).

## Global Constraints

- No cgo. Every task ends with `CGO_ENABLED=0 go build ./...` succeeding. (The OTLP collector packages pull in `google.golang.org/grpc` transitively; that is accepted, it is pure Go.)
- Every task ends with: `gofmt -l .` prints nothing (ignore anything under `.claude/`), `go vet ./...` clean, `go test -race ./...` passes.
- Commit each task directly to `main` (no branch, no merge commit). Commit messages end with the session's attribution lines.
- **Never pipe data into a loghorn binary while exploring** (`… | loghorn`): piped stdin is recorded to the project log. Manual checks run the binary from a scratch directory (e.g. `$TMPDIR/lh-smoke`) with a command after `--` or `< file`.
- **Tests never write into the repo's own `.loghorn`.** A test that runs the built binary sets `cmd.Dir` to a `t.TempDir()` (which is also outside the loghorn source tree, so `logfile.Open` does not refuse it) and sets `HOME` and `XDG_CONFIG_HOME` to fresh temp dirs so the developer's config cannot change framing. Unit tests never call `logfile.Open` on the working directory.
- Startup ordering (CLAUDE.md): every check that can `os.Exit` — flag conflicts, the log-file lock, the `-web` listener, the `-otel` listener — happens **before** the child is launched. After launch, nothing calls `os.Exit` except `main` itself with the child's status once everything has drained.
- After launch, a loghorn-internal failure (log file stopped, web server error, receiver error) is reported once on stderr and never changes the exit status or stops the child.
- Stored OTLP keys are exactly: `timestamp`, `severity`, `message`, `trace_id`, `span_id`, `service.name`, `loghorn.run.id`, `otel.resource`, `otel.scope.name`, `otel.scope.version`, `labels` (an object `{"schema": <stream>}`), `loghorn.source` (`"otlp"`), plus body-map keys and record attributes.
- Environment variables set for the child under `-otel`: `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`, `OTEL_EXPORTER_OTLP_LOGS_PROTOCOL`, `OTEL_LOGS_EXPORTER`, `OTEL_EXPORTER_OTLP_ENDPOINT` (only if unset or empty), `OTEL_RESOURCE_ATTRIBUTES`, `LOGHORN_RUN_ID`.
- Saved-schema stream names match `^[a-z0-9][a-z0-9._-]*$`.
- Copy fixed by the spec, verbatim: `-otel: no OTLP logs received; stdout was recorded instead` (printed as `loghorn: -otel: no OTLP logs received; stdout was recorded instead`).
- Builder decisions this plan makes where the spec leaves room are marked **Decided:** in the task that owns them.

## Review Focus

1. **Ignored signals survive `exec`.** `signal.Ignore(SIGINT)` sets `SIG_IGN`, which the child inherits, so Ctrl-C would never stop it. loghorn must instead `signal.Notify` SIGINT/SIGQUIT and drop them (a Go-handled signal is reset to default in the child). Test: Task 4 `TestChildGetsDefaultSIGINT`.
2. **OTLP/JSON trace and span ids are hex, not base64.** `protojson` alone decodes a 32-char hex `traceId` as base64 without error, producing a wrong 24-byte id. The receiver rewrites hex ids to base64 before `protojson.Unmarshal`. Test: Task 2 `TestJSONHexTraceID`.
3. **A grandchild keeps the output open after the child exits** (`npm run dev` leaving a server, `sh -c 'sleep 30 & exit 4'`). loghorn must return the child's status within the drain window, not hang on the read. Test: Task 4 `TestRunDoesNotWaitForGrandchildOutput`.
4. **A gzip body that inflates past 16 MB** gets 413 and delivers nothing, the same as a large raw body. Test: Task 2 `TestGzipBombIs413`.
5. **A browser page or DNS-rebound name posting to the receiver.** Requests whose `Host` is not `127.0.0.1:<port>`/`localhost:<port>`, or that carry any `Origin` header, get 403 and deliver nothing. Test: Task 2 `TestForeignHostAndOriginRefused`.

---

## File Structure

| Path | Responsibility |
|---|---|
| `internal/otlp/mapping.go` | `MapLogs`, `Stream`: OTLP `LogRecord` → stored JSON line |
| `internal/otlp/receiver.go` | `Listen`, `Receiver` (OTLP/HTTP handler, Host/Origin guard, drain) |
| `internal/otlp/env.go` | `NewRunID`, `ChildEnv` (Task 6) |
| `internal/runner/child.go` | `StartChild`, `Child`: the wrapper's child (pty or pipes, same process group) |
| `internal/runner/runner.go` | old TUI runner; deleted in Task 5 |
| `internal/recorder/recorder.go` | `Recorder`: the one entry point to log file + live store + alerts; `-otel` dedupe switch |
| `internal/wrap/wrap.go` | `Run` (child pass-through, signals, drain), `Pipe` (stdin/replay), `NotifySignals` |
| `internal/schema/saved.go` | stream-name rule widened |
| `internal/config/config.go` | `[display]` removed, accepted with a warning |
| `main.go`, `mode.go`, `wrapper.go`, `replay.go` | flags/usage/dispatch; input-mode checks; `-- cmd` and pipe paths; replay and `-web`-only paths |
| `e2e_test.go` | builds the binary once and runs it in temp dirs |
| deleted | `internal/tui`, `internal/clipboard`, `internal/buffer`, `internal/headless` (its tests move to `internal/wrap`), old `runner.Runner` |

---

### Task 1: OTLP log record → stored line

**Model:** sonnet · **Review:** standard

**Files:**
- Create: `internal/otlp/mapping.go`, `internal/otlp/mapping_test.go`
- Modify: `internal/schema/saved.go:18` (stream regex), `internal/schema/saved_test.go`, `go.mod`, `go.sum`

**Interfaces:**
- Consumes: nothing new.
- Produces (package `github.com/jimbeveridge/loghorn/internal/otlp`; proto imports `collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"`, `logspb "go.opentelemetry.io/proto/otlp/logs/v1"`, `commonpb "go.opentelemetry.io/proto/otlp/common/v1"`):
  - `func MapLogs(req *collogspb.ExportLogsServiceRequest, now time.Time) [][]byte` — one compact JSON object (no trailing newline) per `LogRecord`, in request order. `now` is used only when both timestamps are zero.
  - `func Stream(lr *logspb.LogRecord, scope *commonpb.InstrumentationScope) string`
  - `const Source = "otlp"` (value of `loghorn.source`)
  - `schema.ValidStream` now accepts `^[a-z0-9][a-z0-9._-]*$`.

**Mapping rules** (spec table, plus these **Decided:** points):
- `timestamp`: `time_unix_nano`, else `observed_time_unix_nano`, else `now`; formatted `time.RFC3339Nano` in UTC.
- `severity` (**Decided**, refining "text if set" so common SDK texts like `WARN` don't become `DEFAULT`): `severity_text` when `entry.ParseSeverity(text) != entry.SevDefault` or text equals `DEFAULT` case-insensitively → that canonical name (`entry.Severity.String()`); else `severity_number` 1–8 `DEBUG`, 9–12 `INFO`, 13–16 `WARNING`, 17–20 `ERROR`, 21–24 `CRITICAL`; else non-empty `severity_text` verbatim; else the key is omitted.
- `body`: string → `message`. kvlist → its keys merged as top-level fields; `message` in it becomes `message`, else `msg` becomes `message` (when both exist, `message` wins and `msg` stays a field). Any other body kind (int, double, bool, array, bytes) → stored under `otel.body` as its JSON value (**Decided**).
- Attribute values: string, bool, int (JSON number), double (number; NaN/±Inf as the strings `"NaN"`, `"+Inf"`, `"-Inf"`), array → JSON array, kvlist → nested object, bytes → base64 string, unset → `null`.
- `trace_id`/`span_id`: lowercase hex, only when the bytes are non-empty and not all zero.
- Resource: `service.name` and `loghorn.run.id` promoted to top-level keys; every other resource attribute goes in the `otel.resource` object (omitted when empty).
- `otel.scope.name`/`otel.scope.version`: omitted when empty.
- **Reserved keys (Decided).** A body-map key or record attribute is *reserved* when it equals one of `timestamp time severity message labels trace_id span_id service.name loghorn.run.id loghorn.source trace spanId httpRequest http_request resource textPayload text_payload jsonPayload json_payload receiveTimestamp receive_timestamp insertId insert_id logName log_name sourceLocation source_location operation traceSampled trace_sampled logging.googleapis.com/labels logging.googleapis.com/trace logging.googleapis.com/spanId` or starts with `otel.` or `labels.` (these are the keys the normaliser in `internal/record/record.go` `promote()` reinterprets, or that would be silently dropped behind a promoted field). A reserved body key is stored as `otel.body.<key>`; a reserved attribute, or an attribute that collides with a body key, is stored as `otel.attributes.<key>`.
- Known and accepted: under the default `field-naming = auto`, the normaliser promotes `span_id` to the canonical field `spanId`; no normaliser change.
- `Stream`: `lr.GetEventName()` if non-empty; else the `event.name` string attribute if non-empty; else `scope.GetName()` if non-empty; else `"otel"`. Used verbatim (a scope name like `@scope/pkg` is viewable but its schema cannot be saved, as today for any invalid name).

- [ ] **Step 1: Add the modules**

Run: `go get go.opentelemetry.io/proto/otlp@v1.11.1 google.golang.org/protobuf@latest`
Expected: `go.mod` lists both as direct requirements once Step 3 imports them.

- [ ] **Step 2: Write the failing tests** in `internal/otlp/mapping_test.go` (build requests with a helper `req(resAttrs, scope, records...)`; decode each line with `json.Decoder.UseNumber`):
  - `TestStringBody`: body `"hello"` → `message == "hello"`, `loghorn.source == "otlp"`, `labels == {"schema": "otel"}`.
  - `TestMapBody`: body kvlist `{msg: "m", user: "u", severity: "x"}` → `message == "m"`, `user == "u"`, `otel.body.severity == "x"`, no top-level `msg`. And `{message: "a", msg: "b"}` → `message == "a"`, `msg == "b"`.
  - `TestOtherBodyKind`: int body 7 → `otel.body == 7`, no `message`.
  - `TestAttributeValueTypes`: one attribute of each kind → `s:"x"`, `b:true`, `i:json.Number("9007199254740993")` (exact), `d:1.5`, `nan:"NaN"`, `arr:[1,"a"]`, `kv:{"k":"v"}`, `bytes:"AQI="`, `empty:null`; key `http.response.status_code` stored verbatim at top level.
  - `TestSeverity`: table — text `"WARN"` num 13 → `WARNING`; text `"error"` → `ERROR`; text `""` num 3 → `DEBUG`; 9 → `INFO`; 17 → `ERROR`; 21 → `CRITICAL`; text `"Verbose"` num 0 → `Verbose`; neither → key absent.
  - `TestTimestamp`: `time_unix_nano=1763584812123456789` → `"2025-11-19T20:40:12.123456789Z"`; only observed set → observed; neither → `now` formatted.
  - `TestTraceIDs`: all-zero 16-byte trace id and empty span id → neither key; set ids → lowercase hex of 32 and 16 chars.
  - `TestResourceAndScope`: resource `{service.name: "api", loghorn.run.id: "R", host.name: "h"}`, scope `{name: "lib", version: "1.2"}` → `service.name == "api"`, `loghorn.run.id == "R"`, `otel.resource == {"host.name": "h"}`, `otel.scope.name == "lib"`, `otel.scope.version == "1.2"`; resource with only `service.name` → no `otel.resource` key.
  - `TestCollisions`: attribute `severity: "a"`, attribute `otel.resource: "b"`, attribute `labels.schema: "c"`, and an attribute `user` colliding with body-map `user` → `otel.attributes.severity`, `otel.attributes.otel.resource`, `otel.attributes.labels.schema`, `otel.attributes.user`; promoted values intact.
  - `TestStreamFallbacks`: `event_name: "auction.db"` beats attribute `event.name: "x"` → `auction.db`; attribute only → `x`; scope `lib` only → `lib`; nothing → `otel`; empty `event_name` falls through to the attribute.
  - `TestMappedLineNormalizes`: map a record (event name `auction.db`, severity number 17, attribute `http.response.status_code` int 503, resource `service.name: api`), then `e := adapter.ParseLine(line)`, `r, ok := record.Normalize(e, config.Default().Input)` → `ok`, `r.Stream == "auction.db"`, `r.Fields["service.name"] == "api"`, `r.Fields["http.response.status_code"] == json.Number("503")`, `r.Fields["severity"] == "ERROR"`, `r.Time` equals the record time, and `engine.IsImportant(e)`.
  - `TestMapLogsOrder`: two resources × two scopes × two records → 8 lines in request order.
  - In `internal/schema/saved_test.go`, `TestValidStreamAllowsDots`: true for `auction.db`, `a..b`, `0x`, `a-b_c`; false for `.hidden`, `..`, `.`, `a/b`, `UPPER`, `""`, `-a`. Plus a `SaveSchema`/`LoadSaved` round trip for stream `auction.db` in a temp dir.

- [ ] **Step 3: Run to verify they fail**

Run: `go test ./internal/otlp/ ./internal/schema/`
Expected: FAIL (package `otlp` has no non-test files / `ValidStream("auction.db")` false).

- [ ] **Step 4: Implement** `MapLogs` and `Stream` in `internal/otlp/mapping.go` (build a `map[string]any` per record and `json.Marshal` it; keys come out sorted, which is fine), and change `streamRe` in `internal/schema/saved.go` to `^[a-z0-9][a-z0-9._-]*$`.

- [ ] **Step 5: Verify**

Run: `go test -race ./internal/otlp/ ./internal/schema/ && gofmt -l . | grep -v '^.claude/' ; go vet ./... && CGO_ENABLED=0 go build ./... && go test -race ./...`
Expected: all PASS, gofmt prints nothing.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/otlp/mapping.go internal/otlp/mapping_test.go internal/schema/saved.go internal/schema/saved_test.go
git commit -m "feat(otlp): map OTLP log records to stored lines; allow dots in stream names"
```

---

### Task 2: OTLP/HTTP receiver

**Model:** sonnet · **Review:** full (concurrency: handlers vs. drain; untrusted request bodies)

**Files:**
- Create: `internal/otlp/receiver.go`, `internal/otlp/receiver_test.go`

**Interfaces:**
- Consumes: `MapLogs(req, now) [][]byte` (Task 1).
- Produces:
  - `const MaxBody = 16 << 20`
  - `func Listen() (net.Listener, error)` — `127.0.0.1:0`.
  - `func NewReceiver(deliver func(line []byte), now func() time.Time) *Receiver` — `now == nil` means `time.Now`. `deliver` may be called concurrently from several handler goroutines; it is called for every mapped line **before** the response is written, so an exporter whose request returned has its records stored.
  - `func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request)`
  - `func (r *Receiver) Serve(ln net.Listener)` — starts an `http.Server{Handler: r, ReadHeaderTimeout: 10 * time.Second}` on `ln` in a goroutine.
  - `func (r *Receiver) Drain(idle, max time.Duration)` — blocks until no request has been in flight or started for `idle` (measured from the call or the last request's end, whichever is later), or `max` has passed since the call; then marks the receiver closed and shuts the server down. After `Drain` returns, `deliver` is never called again (a handler that wakes after close delivers nothing and replies 503).

**Behaviour:**
- Guard first (same rule as `internal/web/server.go` `guard`/`listenerPort`): `Host` must be `127.0.0.1:<port>` or `localhost:<port>` where `<port>` comes from `http.LocalAddrContextKey`; any `Origin` header → 403 (**Decided**: OTLP exporters in Node/Go/Python send no `Origin`; a browser always does).
- Routes: `POST /v1/logs`, `POST /v1/traces`, `POST /v1/metrics`. Other methods on those paths → 405; any other path → 404.
- `Content-Type` parsed with `mime.ParseMediaType`: `application/x-protobuf` or `application/json`; anything else → 415.
- `Content-Encoding: gzip` → `gzip.NewReader`; any other non-empty encoding → 415. The raw body goes through `http.MaxBytesReader(MaxBody)` and the decompressed stream through a limit of `MaxBody+1` bytes; exceeding either → 413, nothing delivered.
- Logs, protobuf: `proto.Unmarshal` into `collogspb.ExportLogsServiceRequest`. Logs, JSON: decode generically with `UseNumber`, rewrite every `resourceLogs[].scopeLogs[].logRecords[].traceId`/`spanId` that is valid hex of length 32/16 to base64 (empty strings stay), re-marshal, then `protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal`. Malformed → 400.
- Traces/metrics: protobuf → `proto.Unmarshal` into `coltracepb.ExportTraceServiceRequest`/`colmetricspb.ExportMetricsServiceRequest`; JSON → `json.Valid` only (**Decided**: their hex ids would need the same rewrite for full validation and the data is discarded anyway). Malformed → 400. Valid → discarded.
- Success: 200 with the empty `Export{Logs,Trace,Metrics}ServiceResponse` encoded like the request (`proto.Marshal` or `protojson.Marshal`) and the same `Content-Type`.

- [ ] **Step 1: Write the failing tests** in `internal/otlp/receiver_test.go`, all through `httptest.NewServer(NewReceiver(collect, fixedNow))` (CLAUDE.md: `httptest.NewRequest` has no local address and gets 403), where `collect` appends to a mutex-guarded slice:
  - `TestProtobufLogs`: POST a 2-record request as `application/x-protobuf` → 200, `Content-Type: application/x-protobuf`, body `proto.Unmarshal`s into `ExportLogsServiceResponse`, 2 lines delivered, line 1 has `message == "a"`.
  - `TestJSONLogs`: same request encoded with `protojson.Marshal` but with `traceId` written as hex → 200, `Content-Type: application/json`, 2 lines.
  - `TestJSONHexTraceID`: hand-written JSON `{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"timeUnixNano":"1","traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174","body":{"stringValue":"x"}}]}]}]}` → line has `trace_id == "5b8efff798038103d269b633813fc60c"` and `span_id == "eee19b7ec3c1b174"`.
  - `TestGzip`: protobuf body gzipped with `Content-Encoding: gzip` → 200, lines delivered.
  - `TestTooLargeIs413`: raw body of `MaxBody+1` bytes → 413, nothing delivered.
  - `TestGzipBombIs413`: gzip of `MaxBody+1` zero bytes (small on the wire) → 413, nothing delivered.
  - `TestTracesAndMetricsAccepted`: valid protobuf trace and metrics requests → 200 with the matching empty response; JSON `{}` to both → 200; nothing delivered.
  - `TestBadBodies`: garbage protobuf to `/v1/logs` → 400; invalid JSON → 400; `text/plain` → 415; `Content-Encoding: br` → 415.
  - `TestRoutes`: `GET /v1/logs` → 405; `POST /v1/other` → 404; `GET /` → 404.
  - `TestForeignHostAndOriginRefused`: request with `Host: evil.example:<port>` → 403; valid Host with `Origin: http://127.0.0.1:<port>` → 403; nothing delivered.
  - `TestDrainIdle`: real `Listen` + `Serve`; no traffic; `Drain(200ms, 3s)` returns between 200 ms and 600 ms; a POST afterwards fails to connect or returns non-200, and nothing is delivered.
  - `TestDrainWaitsForTraffic`: a goroutine POSTs every 50 ms for 600 ms; `Drain(200ms, 3s)` returns no earlier than 600 ms after the traffic began and every POST that got 200 was delivered.
  - `TestDrainMax`: POSTs every 50 ms indefinitely; `Drain(200ms, 700ms)` returns within 700 ms–1.2 s.
  - `TestConcurrentPosts`: 8 goroutines × 20 POSTs, run under `-race` → 160×(records per request) lines delivered.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/otlp/ -run 'Protobuf|JSON|Gzip|Large|Traces|Bad|Routes|Foreign|Drain|Concurrent'`
Expected: FAIL (`NewReceiver` undefined).

- [ ] **Step 3: Implement** `internal/otlp/receiver.go` per the Interfaces and Behaviour above. Track in-flight count and last-activity time under one mutex; the `closed` flag is checked under the same mutex immediately before delivering, so `Drain` and a late handler cannot interleave. `Drain` polls at most every 25 ms, then `Shutdown` with a context bounded by the remaining `max`, falling back to `Close`.

- [ ] **Step 4: Verify**

Run: `go test -race -count=3 ./internal/otlp/ && gofmt -l . | grep -v '^.claude/' ; go vet ./... && CGO_ENABLED=0 go build ./... && go test -race ./...`
Expected: PASS three times in a row (drain timing tests are not flaky).

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/otlp/receiver.go internal/otlp/receiver_test.go
git commit -m "feat(otlp): loopback OTLP/HTTP receiver with drain"
```

---

### Task 3: Wrapper child runner and the recorder

**Model:** sonnet · **Review:** full (child process lifecycle and signals; recorder shared by concurrent goroutines)

**Files:**
- Create: `internal/runner/child.go`, `internal/runner/child_test.go`, `internal/recorder/recorder.go`, `internal/recorder/recorder_test.go`
- Leave `internal/runner/runner.go` untouched (the TUI still uses it until Task 5).

**Interfaces:**
- Produces (package `runner`):
  ```go
  type Options struct {
      PTY   bool     // child's stdout and stderr share one pty; else two separate pipes
      Stdin *os.File // child's stdin, passed directly; nil means os.Stdin
      Env   []string // child's environment; nil means os.Environ()
  }
  func StartChild(argv []string, o Options) (*Child, error)
  func (c *Child) Stdout() io.Reader            // pty master in PTY mode (both streams); else the stdout pipe. EIO from a pty master reads as io.EOF.
  func (c *Child) Stderr() io.Reader            // nil in PTY mode
  func (c *Child) Pid() int
  func (c *Child) Exited() <-chan struct{}      // closed once reaped
  func (c *Child) ExitStatus() int              // after Exited: the exit code, or 128+signal number if killed by a signal
  func (c *Child) Signal(sig syscall.Signal) error // to the child process only; nil once it has exited
  func (c *Child) InheritSize(tty *os.File) error  // PTY mode: copy tty's window size to the pty (pty.InheritSize); no-op otherwise
  func (c *Child) Close()                       // closes loghorn's ends of the pty/pipes
  ```
  No `SysProcAttr` at all: no `Setpgid`, no `Setsid`, no `Setctty` — the child keeps the terminal as its controlling terminal and stays in loghorn's (the terminal's foreground) process group. In PTY mode open with `pty.Open()`, set `cmd.Stdout = cmd.Stderr = pts`, close `pts` in the parent after `Start`. A start failure returns an error wrapping the `exec` error (so `errors.Is(err, exec.ErrNotFound)` works).
- Produces (package `recorder`, `github.com/jimbeveridge/loghorn/internal/recorder`):
  ```go
  type Options struct {
      Sink            func(rec []byte)    // copies a record to the log file (main's recordSink); nil = none
      Store           *store.Store        // live web store; nil = none
      Input           config.Input
      Live            bool                // stamp store records with arrival time (false for replays)
      Alert           func(e entry.Entry) // called with each *stored* important entry; nil = none
      DedupeAfterOTLP bool                // -otel: once an OTLP record arrives, stop storing input lines that are JSON objects
      Now             func() time.Time    // nil = time.Now
  }
  func New(o Options) *Recorder
  func (r *Recorder) Line(rec []byte) entry.Entry // one framed input record: parse, set Important, store unless deduped; returns the entry either way
  func (r *Recorder) OTLP(rec []byte)              // one mapped OTLP line: store it, mark OTLP seen
  func (r *Recorder) SawOTLP() bool
  ```
  "Store" means: `Sink(rec)`; then `adapter.Parser.ParseLine`; then `record.Normalize` and `Store.Add(r, received)` with `received = Now()` when `Live`, else zero; then `Alert(e)` when `engine.IsImportant(e)`. One mutex covers all of `Line` and `OTLP` (the adapter parser and an alert coalescer are not safe for concurrent use). A "JSON object" line is one whose trimmed bytes start with `{` and satisfy `json.Valid`. `rec` is only valid during the call (same contract as `ingest`).

- [ ] **Step 1: Write the failing runner tests** in `internal/runner/child_test.go` (children are `sh -c '…'`):
  - `TestPipesKeepStreamsSeparate`: `echo out; echo err >&2` → `Stdout()` reads exactly `"out\n"`, `Stderr()` exactly `"err\n"`.
  - `TestPipesAreNotATerminal`: `test -t 1 || echo notty1; test -t 2 || echo notty2 >&2` → both printed.
  - `TestPTYIsATerminal`: `PTY: true`, `test -t 1 && echo tty1; test -t 2 && echo tty2 >&2` → `Stdout()` contains `tty1` and `tty2`; `Stderr() == nil`; reading to the end returns `io.EOF`, not an error.
  - `TestStdinPassedDirectly`: `Stdin` = read end of an `os.Pipe` holding `"abc\n"`; child `read x; echo got-$x` → `got-abc`.
  - `TestSameProcessGroup`: child `ps -o pgid= -p $$` (or `syscall.Getpgid(c.Pid())` after start, while the child sleeps) equals `syscall.Getpgrp()`.
  - `TestEnvPassed`: `Env: append(os.Environ(), "LH_X=1")`, child `echo $LH_X` → `1`.
  - `TestExitStatus`: `exit 3` → 3; `kill -KILL $$` → 137; `kill -TERM $$` → 143.
  - `TestSignalReachesChild`: child `trap 'echo got; exit 7' TERM; echo ready; while :; do sleep 0.05; done`; after `ready`, `Signal(SIGTERM)` → output contains `got`, status 7. `Signal` after exit returns nil.
  - `TestInheritSize`: `PTY: true`; open a second pty pair, `pty.Setsize(that, &pty.Winsize{Rows: 40, Cols: 100})`, `InheritSize(thatTTY)` before the child (`sleep 0.2; stty size`) prints → `40 100`.
  - `TestStartChildMissingBinary`: `[]string{"no-such-binary-xyz"}` → error with `errors.Is(err, exec.ErrNotFound)`; `StartChild(nil, …)` → error.
- [ ] **Step 2: Write the failing recorder tests** in `internal/recorder/recorder_test.go` (use a slice-collecting `Sink` and a real `store.New(store.ModeLive, t0)`; number values in assertions are `json.Number`):
  - `TestLineStoresEverywhere`: a pino-style JSON line with `labels.schema` and a plain-text line → both in the sink in order; the JSON one in the store with its stream; returned entries parsed (`Severity`, `Important` set for an `"severity":"ERROR"` line).
  - `TestReplayHasNoReceivedTime`: `Live: false`, a labelled JSON line without a time, then one with a time → the first record's `Time` is backfilled from the second (store's existing inheritance), not `Now()`.
  - `TestDedupeAfterOTLP`: `DedupeAfterOTLP: true`; JSON line A, plain line B, `OTLP(otlpLine)`, JSON line C, plain line D, a line `{not json` E → sink holds A, B, otlpLine, D, E (C absent); `SawOTLP()` true; `Line(C)` still returned a parsed entry.
  - `TestNoDedupeWithoutOption`: same sequence with `DedupeAfterOTLP: false` → C stored.
  - `TestAlertOnlyForStoredImportant`: `Alert` collects; an ERROR JSON line before OTLP → alerted; an OTLP line with `"severity":"ERROR"` → alerted; an ERROR JSON line after OTLP (deduped) → not alerted; an INFO line → not alerted.
  - `TestConcurrentLineAndOTLP`: 4 goroutines × 500 `Line` + 2 goroutines × 500 `OTLP` under `-race` → sink count 3000 (no dedupe option), store count equals the labelled ones.
- [ ] **Step 3: Run to verify they fail**

Run: `go test ./internal/runner/ ./internal/recorder/`
Expected: FAIL (`StartChild`, package `recorder` undefined).

- [ ] **Step 4: Implement** `internal/runner/child.go` and `internal/recorder/recorder.go` per Interfaces. `ExitStatus` reads `ExitError.Sys().(syscall.WaitStatus)`: `Signaled()` → `128 + int(Signal())`, else `ExitStatus()`; a `Wait` error that is not an `ExitError` → 1.

- [ ] **Step 5: Verify**

Run: `go test -race -count=3 ./internal/runner/ ./internal/recorder/ && gofmt -l . | grep -v '^.claude/' ; go vet ./... && CGO_ENABLED=0 go build ./... && go test -race ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/runner/child.go internal/runner/child_test.go internal/recorder
git commit -m "feat(runner,recorder): wrapper child (pty or pipes, same process group) and shared recorder"
```

---

### Task 4: The wrapper loop (`internal/wrap`)

**Model:** opus · **Review:** full (signal handling, child lifecycle, drain, goroutine ordering)

**Files:**
- Create: `internal/wrap/wrap.go`, `internal/wrap/wrap_test.go`, `internal/wrap/pipe_test.go` (ported from `internal/headless/headless_test.go` and `internal/headless/gcloud_test.go`; the originals stay until Task 5 deletes the package)

**Interfaces:**
- Consumes: `runner.StartChild`, `*runner.Child` methods, `recorder.New`/`Line` (Task 3).
- Produces (package `github.com/jimbeveridge/loghorn/internal/wrap`):
  ```go
  const DefaultDrain = 500 * time.Millisecond
  type Options struct {
      Recorder  *recorder.Recorder                            // required
      Frame     func(r io.Reader, emit func(rec []byte)) error // framing of the recorded copy; nil = ingest.Records(r, config.FormatAuto, emit)
      StripANSI bool          // strip ANSI escape sequences from each framed record before recording (ansi.Strip)
      Filter    bool          // print only important records (raw + "\n") to Stdout instead of passing bytes through
      Stdout, Stderr io.Writer
      Grace     time.Duration // after a forwarded SIGTERM, SIGKILL the child this much later
      Signals   <-chan os.Signal // from NotifySignals; nil = no forwarding (tests)
      Terminal  *os.File      // a PTY child's window-size source; nil = none
      Drain     time.Duration // how long output readers get after the child is reaped; 0 = DefaultDrain
  }
  func NotifySignals() (sigs <-chan os.Signal, stop func()) // signal.Notify for SIGINT, SIGQUIT, SIGTERM, SIGHUP, SIGWINCH; buffered 8
  func Run(c *runner.Child, o Options) int                 // returns c.ExitStatus()
  func Pipe(r io.Reader, o Options) error                  // a finished or live stream with no child: same copy/record/filter, returns the read error
  ```

**Behaviour:**
- Pass-through (not `Filter`): for each child stream, `o.Frame(io.TeeReader(eofReader(stream), passWriter{dst}), emit)` where `dst` is `Stdout` for `c.Stdout()` and `Stderr` for `c.Stderr()`; `emit` strips ANSI when asked and calls `Recorder.Line`. `passWriter` forwards bytes unchanged and, after its first write error, swallows every later write (returns `len(p), nil`) so recording continues and the child never blocks on a full pipe. (Go's default SIGPIPE behaviour on a closed stdout is left alone: loghorn then dies like any pipeline stage — **Decided**.)
- `Filter`: no tee. Both streams are framed and recorded; each returned entry with `Important` set is written to `Stdout` as `e.Raw` + `"\n"` (the old `headless.Run` contract). A shared mutex serialises the two streams' writes. Nothing goes to `Stderr`.
- Signals (`Run` only): SIGINT and SIGQUIT are received and dropped — **never** `signal.Ignore`, whose `SIG_IGN` the child would inherit across exec. SIGTERM → `c.Signal(SIGTERM)` and, on the first one, a timer that sends `SIGKILL` after `Grace` unless the child has exited. SIGHUP → `c.Signal(SIGHUP)`. SIGWINCH → `c.InheritSize(o.Terminal)` when `Terminal != nil`. `Run` also calls `InheritSize` once at the start.
- Exit: wait for `c.Exited()`; then wait for the stream goroutines at most `Drain` (a grandchild may hold the output open forever — those goroutines are abandoned, and `os.Exit` in `main` ends them); return `c.ExitStatus()`. `Run` does not close the recorder or the log file.
- `Pipe`: one stream (`r` → `Stdout`), same tee/filter/record rules, no signals; returns the framer's error.

- [ ] **Step 1: Write the failing tests** in `internal/wrap/wrap_test.go` (helper `newRec(t)` returns a recorder with a collecting `Sink`; children via `runner.StartChild` with `sh -c`; `Stdout`/`Stderr` are `bytes.Buffer`s behind a mutex):
  - `TestPassThroughBytesExact`: child `printf 'a\033[31mred\033[0m\n\377\376 raw\nno-newline'` (pipes) → `Stdout` bytes equal exactly what the same `printf` produces; sink holds `"ared"`, `"\xff\xfe raw"`, `"no-newline"` with `StripANSI: true`.
  - `TestStdoutStderrSeparate`: `echo o; echo e >&2` → `Stdout == "o\n"`, `Stderr == "e\n"`; sink holds both.
  - `TestPTYPassThrough`: `PTY: true`, `printf 'x\ny\n'; echo e >&2` → `strings.ReplaceAll(stdout, "\r\n", "\n")` contains `x\ny\n` and `e\n`; sink holds `x`, `y`, `e` with no `\r`.
  - `TestRecordingMatchesPlainFraming`: feed the bytes of `testdata/gcloud-tail.json` through a child (`cat <file>`) → the sink equals what `ingest.Records(file, config.FormatAuto, …)` emits over the same file (the old recording path).
  - `TestFilterWithChild`: `Filter: true`, child prints an INFO JSON line, an ERROR JSON line to stderr, a `panic: boom` line → `Stdout` is exactly the ERROR line + `"\n"` + `"panic: boom\n"`; `Stderr` empty; sink has all three.
  - `TestExitStatusPassesThrough`: `exit 5` → `Run` returns 5.
  - `TestSIGINTDropped`: send `SIGINT` on an injected `Signals` channel → child (`sleep 0.3; exit 0`) unaffected, `Run` returns 0.
  - `TestChildGetsDefaultSIGINT`: `sigs, stop := NotifySignals(); defer stop()` **before** starting `sh -c 'kill -INT $$; sleep 2; exit 0'` → `Run` returns 130 within 1 s.
  - `TestSIGTERMForwarded`: child `trap 'echo got; exit 7' TERM; echo ready; while :; do sleep 0.05; done`; after `ready` appears, inject `SIGTERM` → returns 7, `Stdout` contains `got`.
  - `TestSIGTERMEscalates`: child `trap '' TERM; echo ready; while :; do sleep 0.05; done`, `Grace: 200ms`; inject `SIGTERM` → returns 137 within 200 ms–1.5 s.
  - `TestSIGHUPForwarded`: trap HUP → `exit 9` → returns 9.
  - `TestRunDoesNotWaitForGrandchildOutput`: child `sleep 30 & echo hi; exit 4` (pipes; the background sleep keeps stdout open) → returns 4 within 1.5 s, `Stdout` has `hi`.
  - `TestStdoutWriteFailureKeepsRecording`: `Stdout` is a writer that always errors; child prints 3 lines → sink has all 3, `Run` returns the child's status.
  - In `internal/wrap/pipe_test.go`: port every test of `internal/headless/headless_test.go` and `internal/headless/gcloud_test.go` to `Pipe(r, Options{Filter: true, Frame: …, Recorder: …})` with identical expected output (use `ingest.Stored` as `Frame` for the `RunStored` case and `ingest.Records(r, format, emit)` for explicit-format cases). Add `TestPipePassThrough`: `Filter: false` copies the input byte-for-byte to `Stdout` and records every line.
- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/wrap/`
Expected: FAIL (package has no non-test files).

- [ ] **Step 3: Implement** `internal/wrap/wrap.go` per Interfaces and Behaviour.

- [ ] **Step 4: Verify**

Run: `go test -race -count=3 ./internal/wrap/ && gofmt -l . | grep -v '^.claude/' ; go vet ./... && CGO_ENABLED=0 go build ./... && go test -race ./...`
Expected: PASS three times.

- [ ] **Step 5: Commit**

```bash
git add internal/wrap
git commit -m "feat(wrap): transparent pass-through, -filter, signal forwarding and drain"
```

---

### Task 5: `loghorn -- <cmd>` as a transparent wrapper; delete the TUI

**Model:** opus · **Review:** full (startup ordering, child lifecycle, deleting packages)

**Files:**
- Modify: `main.go` (flags, usage, dispatch), `main_test.go`, `internal/config/config.go`, `internal/config/config_test.go`, `README.md`, `go.mod`, `go.sum`
- Create: `mode.go`, `wrapper.go`, `replay.go`, `mode_test.go`, `e2e_test.go`
- Delete: `internal/tui/`, `internal/clipboard/`, `internal/buffer/`, `internal/headless/`, `internal/runner/runner.go`, `internal/runner/runner_test.go` (keep `TestMain` if `child_test.go` needs one)
- Delete from `main.go`: `childControl`, `parseTheme`, `terminalBackground`, `scrollbackFor`, `historicalScrollback`, `webFlagError`, `pickRunJob`'s picker, the Bubble Tea program; from `main_test.go`: `TestParseTheme`, `TestScrollbackFor`, `TestWebRejectsFilter`.

**Interfaces:**
- Consumes: `runner.StartChild`/`Options`, `recorder.New`/`Options`, `wrap.Run`/`Pipe`/`NotifySignals`/`Options` (Tasks 3–4).
- Produces (package `main`, used by Task 6):
  ```go
  type inputKind int
  const ( kindNone inputKind = iota; kindCommand; kindPipe; kindFile; kindHistorical; kindGHJob; kindGHRun )
  type mode struct { kind inputKind; web, filter, otel bool }
  func (m mode) check() error                       // Task 6 adds the -otel rules
  func detectInput(args []string, stdin *os.File, historical bool, ghJob, ghRun string) inputKind
  func removedFlagError(args []string) error
  func describeJobs(w io.Writer, run string, jobs []ghjob.Job, chosen int)
  func runCommand(o opts, cfg config.Config, cwd string) int // the `-- cmd` path; returns the exit status
  type opts struct { /* every parsed flag value plus args */ }
  // e2e_test.go
  func buildLoghorn(t *testing.T) string // go build -o <shared temp>/loghorn . once per test binary (sync.Once)
  type result struct { stdout, stderr string; code int }
  func runLoghorn(t *testing.T, dir string, stdin io.Reader, args ...string) result // cmd.Dir = dir; HOME/XDG_CONFIG_HOME = fresh temp dirs; 20 s timeout
  ```

**Behaviour:**
- `main()` order: `removedFlagError(os.Args[1:])` → exit 2; `flag.Parse()`; `m := mode{kind: detectInput(...), web, filter}`; `m.check()` → exit 2; config load (exit 2 on a bad file); `-config`; then by kind. Every `os.Exit` before the child's launch is unchanged in meaning; after launch `main` only does `os.Exit(runCommand(...))`.
- `detectInput`: `-historical` → kindHistorical; `-gh-job` → kindGHJob; `-gh-run` → kindGHRun; args after `--` → kindCommand; stdin a regular file → kindFile; stdin not a character device (pipe, socket) → kindPipe; else kindNone. (Existing `refuseOtherInput` still rejects a replay flag combined with a command or stdin, and the one-of check for the three replay flags stays.)
- `mode.check()` messages (prefixed `loghorn: ` when printed, exit 2):

  | condition | error text |
  |---|---|
  | kindNone, no `-web` | `nothing to read: give a command after --, pipe input into loghorn, or pass -web to browse the stored logs` |
  | kindFile, neither `-web` nor `-filter` | `reading a file (loghorn < file) needs -web or -filter` |
  | kindHistorical, neither | `-historical needs -web or -filter` |
  | kindGHJob, neither | `-gh-job needs -web or -filter` |
  | kindGHRun, neither | `-gh-run needs -web or -filter` |

  kindNone with `-web` (± `-filter`) then runs exactly as kindHistorical.
- `removedFlagError`: scans args up to the first `--`; `-scrollback`, `--scrollback`, `-scrollback=…`, `--scrollback=…` → `-scrollback was removed along with the terminal UI`; same shapes for `-theme` → `-theme was removed along with the terminal UI`.
- **kindCommand** (`runCommand`), in this order: open the log file (`openLogFile`, as today; a lock held elsewhere prints `loghorn: <locked>; running without a log file`); with `-web`, live store + preload + `web.Listen` + serve, then print `loghorn: web view at http://127.0.0.1:<port>/` to stderr; build the recorder (`Sink: recordSink(logs, onFail)` where `onFail` prints `loghorn: log file stopped: <err>` once; `Live: true`; `Alert` from `-notify` via `alert.NewCoalescer`/`alert.BeeepNotifier`, nil without it); `sigs, stop := wrap.NotifySignals()`; `pty := term.IsTerminal(int(os.Stdout.Fd()))`; `runner.StartChild(args, runner.Options{PTY: pty})` — on error print `loghorn: <err>` and return 127 if `errors.Is(err, exec.ErrNotFound)`, else 1 (**Decided**, shell convention); `status := wrap.Run(child, wrap.Options{Frame: ingest.Records with cfg.Input.Format, StripANSI: true, Filter: -filter, Stdout: os.Stdout, Stderr: os.Stderr, Grace: -shutdown-grace, Signals: sigs, Terminal: os.Stdout if pty})`; then `stop()`, close the web server, `child.Close()`, close the log file, return `status`.
- **kindPipe**: same log file, `-web` and recorder setup (no signals, no child); `wrap.Pipe(os.Stdin, {StripANSI: true, Filter: -filter, Stdout: os.Stdout, …})`; exit 0, or 1 with `loghorn: input error: <err>`. `-web` stops at EOF.
- **Replays** (kindFile, kindHistorical/kindNone+`-web`, kindGHJob, kindGHRun) in `replay.go`: never open the log file (as today). Recorder with `Sink: nil`, `Live: false`, `Store` only with `-web`. Read once through `wrap.Pipe` with `Filter: -filter`, `Stdout: os.Stdout` when filtering else `io.Discard`, `Frame: ingest.Stored` for stored files, the configured format otherwise, `StripANSI: false`. Store modes from the existing `webStoreMode` (kept, with `TestWebStoreMode`). With `-web`, after reading print the URL and serve until SIGINT/SIGTERM (`signal.NotifyContext`), then exit 0.
- **`-gh-run`**: no picker. One job → that job. Several → `chosen := ghjob.FirstFailed(jobs)` and `describeJobs(os.Stderr, run.Run, jobs, chosen)` prints:
  ```
  loghorn: run <run> has <n> jobs; showing <the first failed one|the first one> (pick another with -gh-job <id>):
    * <id>  <name padded>  <outcome>
      <id>  <name padded>  <outcome>
  ```
  (`*` marks `chosen`; reuse `jobItems` for the padded name/outcome.)
- **`[display]`** (`internal/config`): delete `Display`, `KeywordCase` and its constants, the `display.keyword-case` validation, and `[display]` from `Config.String()`. In `parse`, if `md.IsDefined("display")`, warn once `<path>: [display] is ignored: it configured the terminal UI, which has been removed`, and skip `Undecoded()` keys whose first component is `display`.
- `usage()` and README: rewrite to describe the wrapper. README: delete "Keys"; rewrite "Running it" (the wrapper and what passes through, piped input, replays needing `-web` or `-filter`, `loghorn -web` alone, `-filter` with every input, `-web` with `-filter`, `-gh-run` picking the first failed job); remove `--scrollback` and `--theme` from "Flags"; remove `[display]` from "Configuration"; in "Web query view" drop "cannot be combined with -filter" and say the URL is printed to stderr.
- `go mod tidy` drops bubbletea, bubbles, lipgloss, chroma, cellbuf, go-colorful, termenv and their indirects; it keeps `charmbracelet/x/ansi` (used by `wrap`), `creack/pty`, `golang.org/x/term`.

- [ ] **Step 1: Write the failing unit tests** in `mode_test.go`:
  - `TestRemovedFlags`: `[]string{"-theme", "dark"}`, `{"--scrollback=9"}`, `{"-web", "--theme=light"}` → the exact messages above; `{"--", "prog", "-theme"}` and `{"-web"}` → nil.
  - `TestModeCheck`: one row per table entry above (exact text), plus nil for: command alone; pipe alone; file+filter; file+web; historical+web+filter; gh-job+filter; none+web; none+web+filter.
  - `TestDetectInput`: args → kindCommand; regular temp file → kindFile; `os.Pipe` read end → kindPipe; `/dev/null` (a char device) → kindNone; `historical=true` → kindHistorical; `ghRun="1"` → kindGHRun.
  - `TestDescribeJobs`: three jobs, second failed, `chosen=1` → exact text with `*` on the second and "the first failed one"; no failures, `chosen=0` → "the first one".
  - In `internal/config/config_test.go`: `TestDisplayIgnoredWithWarning`: a file with `[display]\nformat-sql = false\nkeyword-case = "upper"` loads without error, `Warn` called exactly once with the `[display] is ignored` text; a file with `[display]\nkeyword-case = "bogus"` also loads (no validation). Update `String()` expectations. Remove tests of `Display` defaults/validation.
- [ ] **Step 2: Write the failing binary tests** in `e2e_test.go` (each in its own `t.TempDir()`; `sh -c` children; `loghorn.log` read from `<dir>/.loghorn/loghorn.log`):
  - `TestWrapperPassThroughAndRecord`: `-- sh -c "printf 'a\033[31mred\033[0m\nplain\n'; echo err >&2; exit 3"` → code 3, stdout exactly `"a\x1b[31mred\x1b[0m\nplain\n"`, stderr contains `err` and no `loghorn:` line, the log file holds `ared`, `plain`, `err`.
  - `TestWrapperPTY`: run the binary with stdout and stderr on a pty slave (`pty.Open`), `-- sh -c 'test -t 1 && echo out-tty; test -t 2 && echo err-tty >&2'` → the master reads contain `out-tty` and `err-tty`; exit 0.
  - `TestWrapperSIGTERMEscalates`: start `-shutdown-grace 200ms -- sh -c "trap '' TERM; echo ready; while :; do sleep 0.05; done"`, wait for `ready` on stdout, `SIGTERM` the loghorn process → exits 137 within 2 s.
  - `TestWrapperIgnoresSIGINTToItself`: start `-- sh -c 'echo ready; sleep 1; echo done'`, after `ready` send `SIGINT` to loghorn only → loghorn still running 300 ms later; final stdout has `done`, code 0.
  - `TestWrapperFilter`: `-filter -- sh -c 'echo "{\"severity\":\"INFO\",\"message\":\"i\"}"; echo "{\"severity\":\"ERROR\",\"message\":\"e\"}"; exit 2'` → stdout is exactly the ERROR line + `\n`, code 2, the log file has both lines.
  - `TestPipedStdinPassesThrough`: stdin `strings.NewReader("one\ntwo\n")`, no args → stdout `"one\ntwo\n"`, code 0, log file has both lines.
  - `TestReplaysNeedWebOrFilter`: stdin a temp file, no flags → code 2, stderr has `reading a file (loghorn < file) needs -web or -filter`; `-historical` with `/dev/null` stdin → code 2, `-historical needs -web or -filter`; no args with `/dev/null` stdin → code 2, the `nothing to read` text.
  - `TestRemovedFlagsExit2`: `-theme dark -- true` → code 2, stderr `loghorn: -theme was removed along with the terminal UI`.
  - `TestFileWithFilter`: stdin a file with an ERROR JSON line and an INFO one, `-filter` → stdout the ERROR line, no `.loghorn` directory created.
  - `TestWebWithFilterAllowed`: `-web -filter -- sh -c 'echo hi'` → code 0, stderr contains `web view at http://127.0.0.1:`.
  - `TestMissingCommand`: `-- no-such-binary-xyz` → code 127, stderr starts `loghorn:`.
  - `TestDisplayWarns`: `<dir>/.config/loghorn/config.toml` with `[display]\nformat-sql = true`, `-- true` → stderr contains `[display] is ignored`, code 0.
- [ ] **Step 3: Run to verify they fail**

Run: `go test -run 'Removed|ModeCheck|DetectInput|DescribeJobs|Wrapper|Piped|Replays|FileWith|WebWith|Missing|DisplayWarns' . ./internal/config/`
Expected: FAIL (undefined `mode`, `removedFlagError`, … and old behaviour).

- [ ] **Step 4: Implement** per Behaviour: split `main.go` into `main.go`/`mode.go`/`wrapper.go`/`replay.go`; delete the listed packages and files; update `internal/config`; rewrite `usage()` and README; `go mod tidy`.

- [ ] **Step 5: Confirm nothing still uses what was deleted**

Run: `git grep -n 'internal/tui\|internal/clipboard\|internal/buffer\|internal/headless\|runner\.Start(\|config\.Display\|KeywordCase' -- '*.go'; grep -n 'scrollback\|theme\|\[display\]\|^## Keys' README.md`
Expected: no output from either.

- [ ] **Step 6: Verify**

Run: `gofmt -l . | grep -v '^.claude/' ; go vet ./... && CGO_ENABLED=0 go build ./... && go test -race ./...`
Expected: PASS. Then a manual smoke run from a scratch dir (never piping into loghorn):
```bash
go build -o "$TMPDIR/lh-smoke/loghorn" . && cd "$TMPDIR/lh-smoke" && ./loghorn -- sh -c 'echo hi; exit 3'; echo "status=$?"
```
Expected: `hi` then `status=3`, and `.loghorn/loghorn.log` there contains `hi`.

- [ ] **Step 7: Commit**

```bash
git add -A main.go main_test.go mode.go mode_test.go wrapper.go replay.go e2e_test.go internal/config internal/runner internal/tui internal/clipboard internal/buffer internal/headless README.md go.mod go.sum
git commit -m "feat!: loghorn -- <cmd> is a transparent wrapper; remove the TUI"
```

---

### Task 6: `-otel`

**Model:** sonnet · **Review:** full (child environment, receiver/recorder lifecycle at exit)

**Files:**
- Create: `internal/otlp/env.go`, `internal/otlp/env_test.go`, `otel_e2e_test.go`
- Modify: `main.go` (flag, usage), `mode.go` + `mode_test.go` (`-otel` rules), `wrapper.go` (wiring), `e2e_test.go` (`TestMain` helper dispatch), `README.md`, `go.mod`, `go.sum`

**Interfaces:**
- Consumes: `otlp.Listen`, `otlp.NewReceiver`, `(*Receiver).Serve`, `(*Receiver).Drain` (Task 2); `recorder.Options.DedupeAfterOTLP`, `(*Recorder).OTLP`, `(*Recorder).SawOTLP` (Task 3); `runner.Options.Env` (Task 3); `mode`, `runCommand`, `runLoghorn`, `buildLoghorn` (Task 5).
- Produces:
  - `func NewRunID(now time.Time) string` — `now.UTC().Format("20060102T150405Z") + "-" + 4 lowercase hex chars from crypto/rand`, e.g. `20261119T204012Z-3fa2`.
  - `func ChildEnv(environ []string, endpoint, runID string) []string` — `endpoint` is `http://127.0.0.1:<port>`. Returns a copy of `environ` with each key replaced in place (or appended): `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=<endpoint>/v1/logs`, `OTEL_EXPORTER_OTLP_LOGS_PROTOCOL=http/protobuf`, `OTEL_LOGS_EXPORTER=otlp`, `OTEL_EXPORTER_OTLP_ENDPOINT=<endpoint>` only when absent or empty, `OTEL_RESOURCE_ATTRIBUTES=<existing>,loghorn.run.id=<id>` (just `loghorn.run.id=<id>` when absent or empty), `LOGHORN_RUN_ID=<id>`. No key appears twice in the result.
  - Flag: `-otel` (bool) — usage: `receive the command's OTLP/HTTP logs on 127.0.0.1 and record them (needs -- <command>)`.

**Behaviour:**
- `mode.check()` gains, before the existing rules: `otel && kind == kindNone` → `-otel needs a command after --`; `otel && kind != kindCommand` → `-otel needs a command after --; it can't be used with <what>` where `<what>` is `piped input` (kindPipe), `a file on stdin` (kindFile), `-historical`, `-gh-job`, `-gh-run`. `-otel` with `-web` and/or `-filter` is allowed.
- **Decided:** the run id and the OTEL variables exist only with `-otel`; without it the child's environment is untouched and stdout lines are stored verbatim (they carry no run id).
- `runCommand` with `-otel`, inserted before `StartChild` (startup ordering): `ln, err := otlp.Listen()` (failure: `loghorn: -otel: <err>`, exit 1, nothing launched); `id := otlp.NewRunID(time.Now())`; recorder built with `DedupeAfterOTLP: true`; `rcv := otlp.NewReceiver(rec.OTLP, nil)`; `rcv.Serve(ln)`; print `loghorn: -otel: receiving OTLP at http://127.0.0.1:<port> (run <id>)` to stderr; `runner.Options.Env = otlp.ChildEnv(os.Environ(), endpoint, id)`. After `wrap.Run` returns: `rcv.Drain(500*time.Millisecond, 3*time.Second)` **before** closing the web server and the log file; then, if `!rec.SawOTLP()`, print `loghorn: -otel: no OTLP logs received; stdout was recorded instead`; return the child's status.
- OTLP records do not appear in `-filter` output (it shows the child's output); they do feed `-notify` through the recorder's `Alert` (**Decided**).
- README: a "Receiving OTEL logs (`-otel`)" subsection under "Running it" (the command, the variables set, where records go, stream = `event.name` → scope name → `otel`, the dedupe rule and the warning) and a `--otel` row in "Flags".

- [ ] **Step 1: Write the failing unit tests**
  - `internal/otlp/env_test.go`:
    - `TestNewRunID`: `NewRunID(time.Date(2026, 11, 19, 20, 40, 12, 0, time.UTC))` matches `^20261119T204012Z-[0-9a-f]{4}$`; a non-UTC time is converted to UTC.
    - `TestChildEnvFresh`: `ChildEnv([]string{"PATH=/bin"}, "http://127.0.0.1:4000", "R")` contains exactly `PATH=/bin` plus the six variables with the values above.
    - `TestChildEnvKeepsResourceAttributes`: existing `OTEL_RESOURCE_ATTRIBUTES=service.name=api,team=x` → `service.name=api,team=x,loghorn.run.id=R`.
    - `TestChildEnvLeavesGenericEndpoint`: existing `OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4318` kept; existing `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=http://x/v1/logs` replaced; empty `OTEL_EXPORTER_OTLP_ENDPOINT=` replaced; no key duplicated.
  - `mode_test.go` `TestModeCheckOtel`: each `-otel` row (exact text); nil for command+otel, command+otel+web+filter.
- [ ] **Step 2: Write the failing end-to-end tests** in `otel_e2e_test.go`. Add to `e2e_test.go` a `TestMain` that, when `LOGHORN_TEST_HELPER` is set, runs that helper instead of the tests and exits with its code. Helper `otel-child` (uses `otlploghttp.New(ctx)`, which reads `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`, and `sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))`, whose default resource reads `OTEL_RESOURCE_ATTRIBUTES` and `OTEL_SERVICE_NAME`):
  1. prints `{"severity":"INFO","message":"before","labels":{"schema":"app"}}` and `plain before` to stdout;
  2. polls `./.loghorn/loghorn.log` (≤ 5 s) until it contains `plain before` — this removes the race between loghorn reading the stdout pipe and the OTLP record arriving;
  3. emits one record: body `"hello otel"`, severity `ERROR`, attributes `event.name=test.event` and `http.response.status_code=200`, then `ForceFlush` and `Shutdown`;
  4. prints `{"severity":"INFO","message":"after","labels":{"schema":"app"}}` and `plain after`;
  5. exits 3.

  Helper `json-only`: prints `{"severity":"INFO","message":"lonely","labels":{"schema":"app"}}`, exits 0.

  Tests (run as `runLoghorn(t, dir, nil, "-otel", "--", os.Args[0])` with `LOGHORN_TEST_HELPER` and `OTEL_SERVICE_NAME=e2e` in the binary's environment):
  - `TestOtelEndToEnd`: code 3; stdout contains `before`, `plain before`, `after`, `plain after` in that order; stderr has `loghorn: -otel: receiving OTLP at http://127.0.0.1:` and a run id matching `[0-9]{8}T[0-9]{6}Z-[0-9a-f]{4}`; the log file has the `before` JSON line, `plain before`, `plain after`, and exactly one line with `"loghorn.source":"otlp"` whose decoded `labels.schema == "test.event"`, `message == "hello otel"`, `severity == "ERROR"`, `service.name == "e2e"`, `loghorn.run.id` equal to the run id printed on stderr, `http.response.status_code == 200`; the log file does **not** contain `"message":"after"`; stderr does not contain `no OTLP logs received`.
  - `TestOtelNoLogsWarns`: helper `json-only` → code 0; stderr contains `loghorn: -otel: no OTLP logs received; stdout was recorded instead`; the log file contains `lonely`.
  - `TestOtelChildEnvironment`: `-otel -- sh -c 'echo "$OTEL_EXPORTER_OTLP_LOGS_ENDPOINT|$OTEL_LOGS_EXPORTER|$LOGHORN_RUN_ID|$OTEL_RESOURCE_ATTRIBUTES"'` with `OTEL_RESOURCE_ATTRIBUTES=team=x` in loghorn's env → stdout matches `^http://127\.0\.0\.1:\d+/v1/logs\|otlp\|<id>\|team=x,loghorn\.run\.id=<id>\n$`.
  - `TestOtelRejectsPipe`: stdin `strings.NewReader("x\n")`, `-otel` with no command → code 2, stderr `-otel needs a command after --; it can't be used with piped input`.
- [ ] **Step 3: Run to verify they fail**

Run: `go get go.opentelemetry.io/otel/sdk/log@latest go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp@latest && go test -run 'RunID|ChildEnv|ModeCheckOtel|Otel' . ./internal/otlp/`
Expected: FAIL (`NewRunID`, `ChildEnv` undefined; `-otel` flag unknown).

- [ ] **Step 4: Implement** `internal/otlp/env.go`, the `-otel` flag and usage line, the `mode.check()` rules, the wiring in `runCommand`, and the README section.

- [ ] **Step 5: Verify**

Run: `go test -race -count=3 -run 'Otel' . && gofmt -l . | grep -v '^.claude/' ; go vet ./... && CGO_ENABLED=0 go build ./... && go test -race ./...`
Expected: PASS (the end-to-end test passes three runs in a row). Then a manual smoke run from a scratch dir:
```bash
go build -o "$TMPDIR/lh-smoke/loghorn" . && cd "$TMPDIR/lh-smoke" && ./loghorn -otel -- sh -c 'echo $OTEL_EXPORTER_OTLP_LOGS_ENDPOINT'
```
Expected: the endpoint printed by the child, then `loghorn: -otel: no OTLP logs received; stdout was recorded instead`.

- [ ] **Step 6: Commit**

```bash
git add main.go mode.go mode_test.go wrapper.go e2e_test.go otel_e2e_test.go internal/otlp/env.go internal/otlp/env_test.go README.md go.mod go.sum
git commit -m "feat: -otel receives the command's OTLP logs into .loghorn"
```
