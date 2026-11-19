package otlp

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type sink struct {
	mu    sync.Mutex
	lines [][]byte
}

func (s *sink) collect(l []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, append([]byte(nil), l...))
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.lines)
}

func fixedNow() time.Time { return now }

func twoRecords() *collogspb.ExportLogsServiceRequest {
	return req(nil, nil,
		&logspb.LogRecord{TimeUnixNano: 1, Body: sv("a")},
		&logspb.LogRecord{TimeUnixNano: 2, Body: sv("b")})
}

// newServer serves a receiver whose log lines go to the first sink and span
// lines to the second.
func newServer(t *testing.T) (*httptest.Server, *sink, *sink) {
	t.Helper()
	s, spans := &sink{}, &sink{}
	srv := httptest.NewServer(NewReceiver(s.collect, spans.collect, fixedNow))
	t.Cleanup(srv.Close)
	return srv, s, spans
}

func post(t *testing.T, url, ct string, body []byte, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	for k, v := range hdr {
		if k == "Host" {
			r.Host = v
			continue
		}
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func mustProto(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func TestProtobufLogs(t *testing.T) {
	srv, s, _ := newServer(t)
	resp, body := post(t, srv.URL+"/v1/logs", ctProto, mustProto(t, twoRecords()), nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != ctProto {
		t.Fatalf("status %d ct %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if err := proto.Unmarshal(body, &collogspb.ExportLogsServiceResponse{}); err != nil {
		t.Fatal(err)
	}
	if s.count() != 2 {
		t.Fatalf("delivered %d", s.count())
	}
	if m := decode(t, s.lines[0]); m["message"] != "a" {
		t.Fatalf("line 1 = %v", m)
	}
}

func TestJSONLogs(t *testing.T) {
	srv, s, _ := newServer(t)
	rq := twoRecords()
	rq.ResourceLogs[0].ScopeLogs[0].LogRecords[0].TraceId = mustHex(t, "5b8efff798038103d269b633813fc60c")
	b, err := protojson.Marshal(rq)
	if err != nil {
		t.Fatal(err)
	}
	// protojson writes the id as base64; swap in the hex the OTLP spec uses.
	var g map[string]any
	json.Unmarshal(b, &g)
	lr := g["resourceLogs"].([]any)[0].(map[string]any)["scopeLogs"].([]any)[0].(map[string]any)["logRecords"].([]any)[0].(map[string]any)
	lr["traceId"] = "5b8efff798038103d269b633813fc60c"
	b, _ = json.Marshal(g)
	resp, _ := post(t, srv.URL+"/v1/logs", ctJSON, b, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != ctJSON {
		t.Fatalf("status %d ct %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if s.count() != 2 {
		t.Fatalf("delivered %d", s.count())
	}
}

func mustHex(t *testing.T, h string) []byte {
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestJSONHexTraceID(t *testing.T) {
	srv, s, _ := newServer(t)
	body := `{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"timeUnixNano":"1","traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174","body":{"stringValue":"x"}}]}]}]}`
	resp, _ := post(t, srv.URL+"/v1/logs", ctJSON, []byte(body), nil)
	if resp.StatusCode != 200 || s.count() != 1 {
		t.Fatalf("status %d delivered %d", resp.StatusCode, s.count())
	}
	m := decode(t, s.lines[0])
	if m["trace_id"] != "5b8efff798038103d269b633813fc60c" || m["span_id"] != "eee19b7ec3c1b174" {
		t.Fatalf("ids = %v / %v", m["trace_id"], m["span_id"])
	}
}

func TestGzip(t *testing.T) {
	srv, s, _ := newServer(t)
	resp, _ := post(t, srv.URL+"/v1/logs", ctProto, gz(t, mustProto(t, twoRecords())), map[string]string{"Content-Encoding": "gzip"})
	if resp.StatusCode != 200 || s.count() != 2 {
		t.Fatalf("status %d delivered %d", resp.StatusCode, s.count())
	}
}

func TestTooLargeIs413(t *testing.T) {
	srv, s, _ := newServer(t)
	resp, _ := post(t, srv.URL+"/v1/logs", ctProto, make([]byte, MaxBody+1), nil)
	if resp.StatusCode != 413 || s.count() != 0 {
		t.Fatalf("status %d delivered %d", resp.StatusCode, s.count())
	}
}

func TestGzipBombIs413(t *testing.T) {
	srv, s, _ := newServer(t)
	resp, _ := post(t, srv.URL+"/v1/logs", ctProto, gz(t, make([]byte, MaxBody+1)), map[string]string{"Content-Encoding": "gzip"})
	if resp.StatusCode != 413 || s.count() != 0 {
		t.Fatalf("status %d delivered %d", resp.StatusCode, s.count())
	}
}

func oneSpanReq(name string) *coltracepb.ExportTraceServiceRequest {
	return &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
			TraceId: mustHexB("5b8efff798038103d269b633813fc60c"), SpanId: mustHexB("eee19b7ec3c1b174"), Name: name,
		}}}},
	}}}
}

func mustHexB(h string) []byte {
	b, err := hex.DecodeString(h)
	if err != nil {
		panic(err)
	}
	return b
}

func TestTracesStored(t *testing.T) {
	srv, logs, spans := newServer(t)
	jsonBody, err := protojson.Marshal(oneSpanReq("json"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		ct   string
		body []byte
		hdr  map[string]string
	}{
		{"proto", ctProto, mustProto(t, oneSpanReq("proto")), nil},
		{"json", ctJSON, jsonBody, nil},
		{"gzip", ctProto, gz(t, mustProto(t, oneSpanReq("gzip"))), map[string]string{"Content-Encoding": "gzip"}},
	}
	for i, c := range cases {
		resp, body := post(t, srv.URL+"/v1/traces", c.ct, c.body, c.hdr)
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != c.ct {
			t.Fatalf("%s: status %d ct %q", c.name, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		var out coltracepb.ExportTraceServiceResponse
		if c.ct == ctJSON {
			err = protojson.Unmarshal(body, &out)
		} else {
			err = proto.Unmarshal(body, &out)
		}
		if err != nil {
			t.Errorf("%s: reply %q: %v", c.name, body, err)
		}
		if spans.count() != i+1 {
			t.Fatalf("%s: span sink has %d lines, want %d", c.name, spans.count(), i+1)
		}
		if got := decode(t, spans.lines[i])["name"]; got != c.name {
			t.Errorf("%s: stored name %v", c.name, got)
		}
	}
	if logs.count() != 0 {
		t.Fatalf("log sink got %d lines", logs.count())
	}
}

func TestMetricsDiscarded(t *testing.T) {
	srv, logs, spans := newServer(t)
	resp, body := post(t, srv.URL+"/v1/metrics", ctProto, mustProto(t, &colmetricspb.ExportMetricsServiceRequest{}), nil)
	if resp.StatusCode != 200 || proto.Unmarshal(body, &colmetricspb.ExportMetricsServiceResponse{}) != nil {
		t.Fatalf("metrics: %d", resp.StatusCode)
	}
	resp, _ = post(t, srv.URL+"/v1/metrics", ctJSON, []byte(`{}`), nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != ctJSON {
		t.Fatalf("metrics json: %d", resp.StatusCode)
	}
	if logs.count() != 0 || spans.count() != 0 {
		t.Fatalf("delivered %d logs, %d spans", logs.count(), spans.count())
	}
}

func TestJSONHexSpanIDs(t *testing.T) {
	srv, _, spans := newServer(t)
	body := `{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174","parentSpanId":"0102030405060708","name":"s",` +
		`"links":[{"traceId":"0f0e0d0c0b0a09080706050403020100","spanId":"a1a2a3a4a5a6a7a8"}]}]}]}]}`
	resp, _ := post(t, srv.URL+"/v1/traces", ctJSON, []byte(body), nil)
	if resp.StatusCode != 200 || spans.count() != 1 {
		t.Fatalf("status %d delivered %d", resp.StatusCode, spans.count())
	}
	m := decode(t, spans.lines[0])
	if m["trace_id"] != "5b8efff798038103d269b633813fc60c" || m["span_id"] != "eee19b7ec3c1b174" || m["parent_span_id"] != "0102030405060708" {
		t.Errorf("ids = %v / %v / %v", m["trace_id"], m["span_id"], m["parent_span_id"])
	}
	links, _ := m["links"].([]any)
	if len(links) != 1 {
		t.Fatalf("links = %v", m["links"])
	}
	l := links[0].(map[string]any)
	if l["trace_id"] != "0f0e0d0c0b0a09080706050403020100" || l["span_id"] != "a1a2a3a4a5a6a7a8" {
		t.Errorf("link ids = %v / %v", l["trace_id"], l["span_id"])
	}
}

func TestTracesNilSinkDiscards(t *testing.T) {
	logs := &sink{}
	srv := httptest.NewServer(NewReceiver(logs.collect, nil, fixedNow))
	t.Cleanup(srv.Close)
	resp, _ := post(t, srv.URL+"/v1/traces", ctProto, mustProto(t, oneSpanReq("x")), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	resp, _ = post(t, srv.URL+"/v1/traces", ctJSON, []byte(`{"resourceSpans": 5}`), nil)
	if resp.StatusCode != 400 {
		t.Errorf("malformed traces with nil sink: status %d, want 400", resp.StatusCode)
	}
	if logs.count() != 0 {
		t.Fatalf("log sink got %d", logs.count())
	}
}

func TestTracesTooLargeIs413(t *testing.T) {
	srv, logs, spans := newServer(t)
	resp, _ := post(t, srv.URL+"/v1/traces", ctProto, make([]byte, MaxBody+1), nil)
	if resp.StatusCode != 413 || logs.count() != 0 || spans.count() != 0 {
		t.Fatalf("status %d delivered %d/%d", resp.StatusCode, logs.count(), spans.count())
	}
}

func TestBadBodies(t *testing.T) {
	srv, s, spans := newServer(t)
	cases := []struct {
		name string
		path string
		ct   string
		body string
		hdr  map[string]string
		want int
	}{
		{"garbage protobuf", "/v1/logs", ctProto, "\xff\xff\xff\xff\xff", nil, 400},
		{"invalid json", "/v1/logs", ctJSON, "{nope", nil, 400},
		{"text/plain", "/v1/logs", "text/plain", "hi", nil, 415},
		{"brotli", "/v1/logs", ctProto, "", map[string]string{"Content-Encoding": "br"}, 415},
		{"bad gzip", "/v1/logs", ctProto, "not gzip", map[string]string{"Content-Encoding": "gzip"}, 400},
		{"bad trace json", "/v1/traces", ctJSON, "{nope", nil, 400},
		{"trace json of the wrong shape", "/v1/traces", ctJSON, `{"resourceSpans": 5}`, nil, 400},
		{"garbage trace protobuf", "/v1/traces", ctProto, "\xff\xff\xff\xff\xff", nil, 400},
	}
	for _, c := range cases {
		resp, _ := post(t, srv.URL+c.path, c.ct, []byte(c.body), c.hdr)
		if resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
	if s.count() != 0 || spans.count() != 0 {
		t.Fatalf("delivered %d logs, %d spans", s.count(), spans.count())
	}
}

func TestRoutes(t *testing.T) {
	srv, _, _ := newServer(t)
	resp, err := http.Get(srv.URL + "/v1/logs")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("GET /v1/logs: %d", resp.StatusCode)
	}
	r2, _ := post(t, srv.URL+"/v1/other", ctProto, nil, nil)
	if r2.StatusCode != 404 {
		t.Errorf("POST /v1/other: %d", r2.StatusCode)
	}
	resp, err = http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("GET /: %d", resp.StatusCode)
	}
}

func TestForeignHostAndOriginRefused(t *testing.T) {
	srv, s, _ := newServer(t)
	port := srv.Listener.Addr().String()[strings.LastIndex(srv.Listener.Addr().String(), ":")+1:]
	b := mustProto(t, twoRecords())
	resp, _ := post(t, srv.URL+"/v1/logs", ctProto, b, map[string]string{"Host": "evil.example:" + port})
	if resp.StatusCode != 403 {
		t.Errorf("foreign host: %d", resp.StatusCode)
	}
	resp, _ = post(t, srv.URL+"/v1/logs", ctProto, b, map[string]string{"Origin": "http://127.0.0.1:" + port})
	if resp.StatusCode != 403 {
		t.Errorf("origin: %d", resp.StatusCode)
	}
	resp, _ = post(t, srv.URL+"/v1/logs", ctProto, b, map[string]string{"Host": "localhost:" + port})
	if resp.StatusCode != 200 {
		t.Errorf("localhost host: %d", resp.StatusCode)
	}
	if s.count() != 2 {
		t.Fatalf("delivered %d, want only the localhost request's 2", s.count())
	}
}

// startReal serves a receiver on a real listener and returns its log sink,
// span sink and base URL (http://127.0.0.1:<port>).
func startReal(t *testing.T) (*Receiver, *sink, *sink, string) {
	t.Helper()
	ln, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	s, spans := &sink{}, &sink{}
	r := NewReceiver(s.collect, spans.collect, fixedNow)
	r.Serve(ln)
	t.Cleanup(func() { r.Drain(0, 100*time.Millisecond) })
	return r, s, spans, "http://" + ln.Addr().String()
}

func TestDrainIdle(t *testing.T) {
	r, s, _, base := startReal(t)
	url := base + "/v1/logs"
	start := time.Now()
	r.Drain(200*time.Millisecond, 3*time.Second)
	if d := time.Since(start); d < 200*time.Millisecond || d > 600*time.Millisecond {
		t.Fatalf("Drain took %v", d)
	}
	c := http.Client{Timeout: time.Second}
	resp, err := c.Post(url, ctProto, bytes.NewReader(mustProto(t, twoRecords())))
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Fatal("POST after Drain succeeded")
		}
	}
	if s.count() != 0 {
		t.Fatalf("delivered %d after Drain", s.count())
	}
}

func TestDrainWaitsForTraffic(t *testing.T) {
	r, s, spans, base := startReal(t)
	url := base + "/v1/logs"
	b := mustProto(t, twoRecords())
	tb := mustProto(t, oneSpanReq("late"))
	began := time.Now()
	var ok, spanOK atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for time.Since(began) < 600*time.Millisecond {
			resp, err := http.Post(url, ctProto, bytes.NewReader(b))
			if err == nil {
				if resp.StatusCode == 200 {
					ok.Add(1)
				}
				resp.Body.Close()
			}
			time.Sleep(50 * time.Millisecond)
		}
		// A span batch flushed as the child exits, inside the drain window.
		resp, err := http.Post(base+"/v1/traces", ctProto, bytes.NewReader(tb))
		if err == nil {
			if resp.StatusCode == 200 {
				spanOK.Add(1)
			}
			resp.Body.Close()
		}
	}()
	time.Sleep(25 * time.Millisecond)
	r.Drain(200*time.Millisecond, 3*time.Second)
	if d := time.Since(began); d < 600*time.Millisecond {
		t.Fatalf("Drain returned %v after traffic began", d)
	}
	<-done
	if got := s.count(); int64(got) != 2*ok.Load() {
		t.Fatalf("delivered %d lines for %d OK posts", got, ok.Load())
	}
	if spanOK.Load() != 1 || spans.count() != 1 {
		t.Fatalf("span post OK %d, span lines %d; want the drain-window span delivered", spanOK.Load(), spans.count())
	}
}

func TestNoSpanDeliveryAfterDrain(t *testing.T) {
	r, logs, spans, _ := startReal(t)
	r.Drain(0, 100*time.Millisecond)
	// Drain shut the real listener; serve the same, closed receiver again to
	// reach its closed path.
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	resp, _ := post(t, srv.URL+"/v1/traces", ctProto, mustProto(t, oneSpanReq("after")), nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", resp.StatusCode)
	}
	if spans.count() != 0 || logs.count() != 0 {
		t.Fatalf("delivered %d spans, %d logs after Drain", spans.count(), logs.count())
	}
}

func TestDrainMax(t *testing.T) {
	r, _, _, base := startReal(t)
	url := base + "/v1/logs"
	b := mustProto(t, twoRecords())
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if resp, err := http.Post(url, ctProto, bytes.NewReader(b)); err == nil {
				resp.Body.Close()
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	start := time.Now()
	r.Drain(200*time.Millisecond, 700*time.Millisecond)
	d := time.Since(start)
	close(stop)
	<-done
	if d < 700*time.Millisecond || d > 1200*time.Millisecond {
		t.Fatalf("Drain took %v", d)
	}
}

func TestConcurrentPosts(t *testing.T) {
	srv, s, _ := newServer(t)
	b := mustProto(t, twoRecords())
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				resp, _ := post(t, srv.URL+"/v1/logs", ctProto, b, nil)
				if resp.StatusCode != 200 {
					t.Errorf("status %d", resp.StatusCode)
				}
			}
		}()
	}
	wg.Wait()
	if s.count() != 160*2 {
		t.Fatalf("delivered %d", s.count())
	}
}
