package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"sync"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// MaxBody caps both the request body on the wire and its decompressed size.
const MaxBody = 16 << 20

const (
	ctProto = "application/x-protobuf"
	ctJSON  = "application/json"
)

// Listen binds the loopback address the receiver serves on.
func Listen() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }

// Receiver is an OTLP/HTTP endpoint. Logs and traces are kept, each mapped to
// lines for its own callback; metrics are validated and discarded.
type Receiver struct {
	logs     func(line []byte)
	spans    func(line []byte) // nil: traces are decoded and discarded
	runLines func(RunLine)     // nil: loghorn.run records are mapped like any other
	now      func() time.Time

	// dmu orders delivery against Drain: handlers hold the read side while
	// they deliver, Drain takes the write side to close, so no delivery can
	// start after Drain returns or overlap its close.
	dmu    sync.RWMutex
	closed bool

	mu       sync.Mutex // guards inflight, last
	inflight int
	last     time.Time

	srv *http.Server
}

// NewReceiver returns a receiver that passes every mapped log line to logs and
// every mapped span line to spans before it answers the request. spans == nil
// means traces are decoded and discarded. now == nil means time.Now.
func NewReceiver(logs, spans func(line []byte), now func() time.Time) *Receiver {
	if now == nil {
		now = time.Now
	}
	return &Receiver{logs: logs, spans: spans, now: now}
}

// SetRunLines diverts log records in the RunScope scope to f, one RunLine per
// record with a string body, before the remaining records are mapped for the
// logs callback. Call it before Serve.
func (r *Receiver) SetRunLines(f func(RunLine)) { r.runLines = f }

// Serve starts serving on ln in a goroutine.
func (r *Receiver) Serve(ln net.Listener) {
	srv := &http.Server{Handler: r, ReadHeaderTimeout: 10 * time.Second}
	r.mu.Lock()
	r.srv = srv
	r.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
}

// Drain blocks until no request has been in flight or started for idle
// (measured from the call or the last request's end, whichever is later), or
// max has passed since the call. It then closes the receiver, so neither the
// logs nor the spans callback is called again once Drain returns, and shuts the
// server down.
//
// Call Drain before closing whatever the callbacks write to (the log writer,
// whose Close also closes the span writer): a batch the SDK flushes as the
// child exits would otherwise arrive after the writers are closed and be lost.
func (r *Receiver) Drain(idle, max time.Duration) {
	start := time.Now()
	r.mu.Lock()
	if r.last.Before(start) {
		r.last = start
	}
	r.mu.Unlock()
	for {
		now := time.Now()
		r.mu.Lock()
		quiet := r.inflight == 0 && now.Sub(r.last) >= idle
		r.mu.Unlock()
		if quiet || now.Sub(start) >= max {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	r.dmu.Lock()
	r.closed = true
	r.dmu.Unlock()

	r.mu.Lock()
	srv := r.srv
	r.mu.Unlock()
	if srv == nil {
		return
	}
	remaining := max - time.Since(start)
	if remaining < 50*time.Millisecond {
		remaining = 50 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), remaining)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
	}
}

func (r *Receiver) begin() {
	r.mu.Lock()
	r.inflight++
	r.last = time.Now()
	r.mu.Unlock()
}

func (r *Receiver) end() {
	r.mu.Lock()
	r.inflight--
	r.last = time.Now()
	r.mu.Unlock()
}

func listenerPort(req *http.Request) string {
	a, ok := req.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || a == nil {
		return ""
	}
	_, port, err := net.SplitHostPort(a.String())
	if err != nil {
		return ""
	}
	return port
}

func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	port := listenerPort(req)
	if port == "" || (req.Host != "127.0.0.1:"+port && req.Host != "localhost:"+port) {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}
	if _, has := req.Header["Origin"]; has {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	switch req.URL.Path {
	case "/v1/logs", "/v1/traces", "/v1/metrics":
	default:
		http.NotFound(w, req)
		return
	}
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ct, _, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || (ct != ctProto && ct != ctJSON) {
		http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
		return
	}
	enc := req.Header.Get("Content-Encoding")
	if enc != "" && enc != "gzip" {
		http.Error(w, "unsupported content encoding", http.StatusUnsupportedMediaType)
		return
	}

	r.begin()
	defer r.end()

	var src io.Reader = http.MaxBytesReader(w, req.Body, MaxBody)
	if enc == "gzip" {
		gr, err := gzip.NewReader(src)
		if err != nil {
			readFailed(w, err)
			return
		}
		src = gr
	}
	body, err := io.ReadAll(io.LimitReader(src, MaxBody+1))
	if err != nil {
		readFailed(w, err)
		return
	}
	if len(body) > MaxBody {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}

	isJSON := ct == ctJSON
	var resp proto.Message
	var lines, spanLines [][]byte
	var runLines []RunLine
	switch req.URL.Path {
	case "/v1/logs":
		var lr collogspb.ExportLogsServiceRequest
		if err := unmarshal(body, isJSON, &lr, fixHexIDs); err != nil {
			http.Error(w, "malformed request", http.StatusBadRequest)
			return
		}
		if r.runLines != nil {
			runLines = SplitRunLines(&lr)
		}
		lines = MapLogs(&lr, r.now())
		resp = &collogspb.ExportLogsServiceResponse{}
	case "/v1/traces":
		var tr coltracepb.ExportTraceServiceRequest
		if err := unmarshal(body, isJSON, &tr, fixSpanHexIDs); err != nil {
			http.Error(w, "malformed request", http.StatusBadRequest)
			return
		}
		if r.spans != nil {
			spanLines = MapSpans(&tr)
		}
		resp = &coltracepb.ExportTraceServiceResponse{}
	default:
		if err := validate(body, isJSON, &colmetricspb.ExportMetricsServiceRequest{}); err != nil {
			http.Error(w, "malformed request", http.StatusBadRequest)
			return
		}
		resp = &colmetricspb.ExportMetricsServiceResponse{}
	}

	r.dmu.RLock()
	if r.closed {
		r.dmu.RUnlock()
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	for _, l := range runLines {
		r.runLines(l)
	}
	for _, l := range lines {
		r.logs(l)
	}
	for _, l := range spanLines {
		r.spans(l)
	}
	r.dmu.RUnlock()

	var out []byte
	if isJSON {
		out, err = protojson.Marshal(resp)
	} else {
		out, err = proto.Marshal(resp)
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func readFailed(w http.ResponseWriter, err error) {
	var mb *http.MaxBytesError
	if errors.As(err, &mb) {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "unreadable body", http.StatusBadRequest)
}

// unmarshal decodes a protobuf or OTLP/JSON body into m. pre, when non-nil,
// rewrites the JSON document first.
func unmarshal(body []byte, isJSON bool, m proto.Message, pre func(map[string]any)) error {
	if !isJSON {
		return proto.Unmarshal(body, m)
	}
	if pre != nil {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			return err
		}
		pre(doc)
		var err error
		if body, err = json.Marshal(doc); err != nil {
			return err
		}
	}
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(body, m)
}

// validate checks a body that will be discarded: protobuf must parse, JSON
// need only be well-formed.
func validate(body []byte, isJSON bool, m proto.Message) error {
	if isJSON {
		if !json.Valid(body) {
			return errors.New("invalid json")
		}
		return nil
	}
	return proto.Unmarshal(body, m)
}

// The OTLP/JSON id rewrites below turn the hex the spec uses into the base64
// protojson expects for a bytes field; protojson would otherwise decode a
// 32-char hex string as base64 without error and produce a wrong id.

// eachObject calls f for every object in the JSON array v.
func eachObject(v any, f func(map[string]any)) {
	arr, _ := v.([]any)
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			f(m)
		}
	}
}

// hexToBase64 rewrites m[key] from hex to base64 when it is a hex string of
// exactly want characters, and leaves it alone otherwise.
func hexToBase64(m map[string]any, key string, want int) {
	s, ok := m[key].(string)
	if !ok || len(s) != want {
		return
	}
	if b, err := hex.DecodeString(s); err == nil {
		m[key] = base64.StdEncoding.EncodeToString(b)
	}
}

// fixHexIDs rewrites the traceId/spanId of every log record.
func fixHexIDs(doc map[string]any) {
	eachObject(doc["resourceLogs"], func(rl map[string]any) {
		eachObject(rl["scopeLogs"], func(sl map[string]any) {
			eachObject(sl["logRecords"], func(lr map[string]any) {
				hexToBase64(lr, "traceId", 32)
				hexToBase64(lr, "spanId", 16)
			})
		})
	})
}

// fixSpanHexIDs rewrites every span's traceId, spanId and parentSpanId, and
// each of its links' traceId and spanId.
func fixSpanHexIDs(doc map[string]any) {
	eachObject(doc["resourceSpans"], func(rs map[string]any) {
		eachObject(rs["scopeSpans"], func(ss map[string]any) {
			eachObject(ss["spans"], func(sp map[string]any) {
				hexToBase64(sp, "traceId", 32)
				hexToBase64(sp, "spanId", 16)
				hexToBase64(sp, "parentSpanId", 16)
				eachObject(sp["links"], func(ln map[string]any) {
					hexToBase64(ln, "traceId", 32)
					hexToBase64(ln, "spanId", 16)
				})
			})
		})
	})
}
