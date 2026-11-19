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

// Receiver is an OTLP/HTTP endpoint. Only logs are kept; traces and metrics
// are validated and discarded.
type Receiver struct {
	deliver func(line []byte)
	now     func() time.Time

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

// NewReceiver returns a receiver that passes every mapped log line to
// deliver before it answers the request. now == nil means time.Now.
func NewReceiver(deliver func(line []byte), now func() time.Time) *Receiver {
	if now == nil {
		now = time.Now
	}
	return &Receiver{deliver: deliver, now: now}
}

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
// max has passed since the call. It then closes the receiver, so deliver is
// never called again, and shuts the server down.
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
	var lines [][]byte
	switch req.URL.Path {
	case "/v1/logs":
		var lr collogspb.ExportLogsServiceRequest
		if err := unmarshal(body, isJSON, &lr, fixHexIDs); err != nil {
			http.Error(w, "malformed request", http.StatusBadRequest)
			return
		}
		lines = MapLogs(&lr, r.now())
		resp = &collogspb.ExportLogsServiceResponse{}
	case "/v1/traces":
		if err := validate(body, isJSON, &coltracepb.ExportTraceServiceRequest{}); err != nil {
			http.Error(w, "malformed request", http.StatusBadRequest)
			return
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
	for _, l := range lines {
		r.deliver(l)
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

// fixHexIDs rewrites the hex traceId/spanId of every log record to base64,
// which is what protojson expects for a bytes field. OTLP/JSON specifies hex;
// protojson would otherwise decode a 32-char hex string as base64 without
// error and produce a wrong id.
func fixHexIDs(doc map[string]any) {
	each := func(v any, f func(map[string]any)) {
		arr, _ := v.([]any)
		for _, e := range arr {
			if m, ok := e.(map[string]any); ok {
				f(m)
			}
		}
	}
	each(doc["resourceLogs"], func(rl map[string]any) {
		each(rl["scopeLogs"], func(sl map[string]any) {
			each(sl["logRecords"], func(lr map[string]any) {
				for _, k := range []string{"traceId", "spanId"} {
					want := 32
					if k == "spanId" {
						want = 16
					}
					s, ok := lr[k].(string)
					if !ok || len(s) != want {
						continue
					}
					if b, err := hex.DecodeString(s); err == nil {
						lr[k] = base64.StdEncoding.EncodeToString(b)
					}
				}
			})
		})
	})
}
