# OTLP over gRPC for `-otel` — design

Date: 2026-11-19
Status: **deferred.** Written so it can be picked up later; not scheduled.

## Intent

`loghorn -otel -- <cmd>` receives OTLP over HTTP today, in both encodings:
protobuf (`application/x-protobuf`) and JSON (`internal/otlp/receiver.go`).
It points the child at itself through `OTEL_EXPORTER_OTLP_*` variables with
the protocol set to `http/protobuf` (`internal/otlp/env.go`).

OTLP has a second transport, gRPC. An app reaches loghorn over HTTP when its
SDK picks the exporter from the environment, or when it constructs an
HTTP exporter. An app that constructs a **gRPC** exporter (for example
Node's `@opentelemetry/exporter-*-otlp-grpc`, or Go's `otlploggrpc` /
`otlptracegrpc`) ignores the protocol variables and cannot reach loghorn.
This spec adds a gRPC receiver for those apps.

**Not needed yet (checked 2026-11-19):** the app loghorn is being tested
with constructs `@opentelemetry/exporter-{trace,logs,metrics}-otlp-proto`
exporters (HTTP protobuf) and a reporter that posts OTLP JSON; both already
work with loghorn. Build this only when an app that hard-codes gRPC needs
it.

## Goals

- `-otel` accepts OTLP/gRPC for logs, traces and metrics, storing logs and
  spans exactly as the HTTP receiver does (same mapping, same files, same
  stdout de-duplication switch) and discarding metrics.
- An app configured from loghorn's environment keeps using HTTP; nothing
  changes for it.
- An app with a hard-coded gRPC exporter can be pointed at loghorn with no
  guesswork about which port speaks which protocol.

## Non-goals

- gRPC streaming, OTLP/gRPC over TLS, or auth.
- Changing the protocol loghorn asks env-configured SDKs to use (stays
  `http/protobuf`).
- A gRPC receiver for `loghorn serve` — it will inherit this one when `serve`
  exists.

## Design

### One port for both transports

The receiver keeps its single loopback port and serves both on it:
HTTP/1.1 and cleartext HTTP/2 (h2c, `golang.org/x/net/http2/h2c`) for OTLP
HTTP, and gRPC for requests with `Content-Type: application/grpc`, routed to
`grpc.Server.ServeHTTP`.

Why one port: loghorn sets `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` and friends
for the child, and gRPC exporters read those same variables. With two ports,
a gRPC exporter would read the HTTP port from them and fail with a protocol
error. With one port, whichever transport the exporter speaks, the endpoint
it was given works.

The alternative — a separate gRPC listener plus a
`LOGHORN_OTLP_GRPC_ENDPOINT` variable the app must read — is kept as the
fallback if `ServeHTTP` proves unsuitable (see problems 1 and 2).

### Services

Register the three OTLP collector services from
`go.opentelemetry.io/proto/otlp/collector/{logs,trace,metrics}/v1`:

- `LogsService.Export` → the existing log mapping and delivery.
- `TraceService.Export` → the existing span mapping and delivery.
- `MetricsService.Export` → validate, discard, return success. It must be
  registered: an unregistered service returns `Unimplemented`, and SDKs log
  an export error on every interval.

Replies are empty `Export*ServiceResponse` messages (no partial success),
matching the HTTP receiver.

### Limits, compression, shutdown

- `grpc.MaxRecvMsgSize(16 << 20)`, matching the HTTP 16 MB limit (gRPC's
  default is 4 MB).
- Import `google.golang.org/grpc/encoding/gzip` so gzip-compressed requests
  decode.
- An interceptor counts in-flight calls and feeds the same idle clock as
  HTTP requests, so `Drain(idle, max)` covers both transports. After `Drain`
  closes the receiver, calls fail with `codes.Unavailable` and nothing is
  delivered — the same rule the HTTP side follows (see the doc comment on
  `Receiver.Drain`).

### Startup output and environment

No new variables are set for the child. The startup line names the one
endpoint and says it accepts OTLP over HTTP and gRPC. README's `-otel`
section explains how to point a hard-coded gRPC exporter at it (endpoint
`127.0.0.1:<port>`, insecure).

## Potential problems

1. **`grpc.Server.ServeHTTP` is marked experimental** in grpc-go, is slower
   than the native server, and doesn't support every server option
   (keepalive enforcement among them). Loopback traffic from one app is
   light, so speed doesn't matter, but an option we need could turn out to be
   missing. Fallback: a separate gRPC listener (see "One port").
2. **h2c on the existing HTTP server.** Turning on cleartext HTTP/2 changes
   the HTTP receiver too: HTTP/2 clients may now upgrade. The HTTP tests must
   run over both HTTP/1.1 and h2c, including the 413 limit and the drain
   rules.
3. **Endpoint paths.** loghorn's per-signal variables carry a path
   (`/v1/logs`, `/v1/traces`). gRPC exporters are expected to use only host
   and port, but SDKs differ: some reject an endpoint with a path, some
   reject an `http://` scheme, some require it to choose insecure mode. Each
   exporter we care about needs an explicit test (Go `otlploggrpc` and
   `otlptracegrpc`; Node `exporter-*-otlp-grpc` checked by hand).
4. **TLS by default.** Several gRPC exporters use TLS unless told otherwise
   (`OTEL_EXPORTER_OTLP_INSECURE=true`, an `http://` endpoint, or a code
   option). loghorn is plaintext on loopback. Setting
   `OTEL_EXPORTER_OTLP_INSECURE` for the child is tempting but would also
   affect any exporter the app sends elsewhere; v1 documents it instead.
5. **Retries after shutdown.** gRPC exporters retry `Unavailable` with
   backoff. A batch refused after `Drain` is retried until the exporter's
   timeout; since the child has exited by then this only delays nothing in
   loghorn, but a long-lived grandchild could keep retrying against a closed
   port. Same behaviour as HTTP's 503 today.
6. **Host checking.** The HTTP receiver relies on loopback-only listening.
   gRPC's `:authority` header is not checked either. Browsers can't speak
   raw gRPC, so DNS rebinding isn't a route in; keep loopback-only and note
   it.
7. **Binary size.** grpc is in go.mod only indirectly today; linking the
   server adds several MB to the binary. Measure before and after.
8. **Test dependencies.** End-to-end tests need the OTEL Go SDK's gRPC
   exporters, which pull in more of grpc for tests.
9. **Two transports, one de-dup switch.** The stdout de-duplication switch
   flips on the first OTLP log record from either transport; a test must
   cover a gRPC-only app.

## Testing

- Unit: each service's Export with a valid, empty, oversize (>16 MB) and
  gzip request; metrics accepted and discarded; `Unimplemented` never
  returned for the three services.
- Drain: an in-flight gRPC call delays `Drain`; a call after `Drain` gets
  `Unavailable` and is not stored.
- Both transports on one port: HTTP/1.1 protobuf, HTTP/1.1 JSON, h2c
  protobuf and gRPC against the same `Receiver`.
- End-to-end: a Go test child using `otlploggrpc` and `otlptracegrpc`
  configured only from loghorn's environment (plus insecure), checking
  `.loghorn/loghorn.log` and `.loghorn/loghorn.traces.jsonl`.

## Effort

One plan task: roughly 200–300 changed non-test lines in `internal/otlp`
(server wiring, three service handlers, interceptor, h2c routing) plus
README, with `Review: full` (concurrency and the drain contract).
