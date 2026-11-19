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

func newServer(t *testing.T) (*httptest.Server, *sink) {
	t.Helper()
	s := &sink{}
	srv := httptest.NewServer(NewReceiver(s.collect, fixedNow))
	t.Cleanup(srv.Close)
	return srv, s
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
	srv, s := newServer(t)
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
	srv, s := newServer(t)
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
	srv, s := newServer(t)
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
	srv, s := newServer(t)
	resp, _ := post(t, srv.URL+"/v1/logs", ctProto, gz(t, mustProto(t, twoRecords())), map[string]string{"Content-Encoding": "gzip"})
	if resp.StatusCode != 200 || s.count() != 2 {
		t.Fatalf("status %d delivered %d", resp.StatusCode, s.count())
	}
}

func TestTooLargeIs413(t *testing.T) {
	srv, s := newServer(t)
	resp, _ := post(t, srv.URL+"/v1/logs", ctProto, make([]byte, MaxBody+1), nil)
	if resp.StatusCode != 413 || s.count() != 0 {
		t.Fatalf("status %d delivered %d", resp.StatusCode, s.count())
	}
}

func TestGzipBombIs413(t *testing.T) {
	srv, s := newServer(t)
	resp, _ := post(t, srv.URL+"/v1/logs", ctProto, gz(t, make([]byte, MaxBody+1)), map[string]string{"Content-Encoding": "gzip"})
	if resp.StatusCode != 413 || s.count() != 0 {
		t.Fatalf("status %d delivered %d", resp.StatusCode, s.count())
	}
}

func TestTracesAndMetricsAccepted(t *testing.T) {
	srv, s := newServer(t)
	resp, body := post(t, srv.URL+"/v1/traces", ctProto, mustProto(t, &coltracepb.ExportTraceServiceRequest{}), nil)
	if resp.StatusCode != 200 || proto.Unmarshal(body, &coltracepb.ExportTraceServiceResponse{}) != nil {
		t.Fatalf("traces: %d", resp.StatusCode)
	}
	resp, body = post(t, srv.URL+"/v1/metrics", ctProto, mustProto(t, &colmetricspb.ExportMetricsServiceRequest{}), nil)
	if resp.StatusCode != 200 || proto.Unmarshal(body, &colmetricspb.ExportMetricsServiceResponse{}) != nil {
		t.Fatalf("metrics: %d", resp.StatusCode)
	}
	for _, p := range []string{"/v1/traces", "/v1/metrics"} {
		resp, _ := post(t, srv.URL+p, ctJSON, []byte(`{}`), nil)
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != ctJSON {
			t.Fatalf("%s json: %d", p, resp.StatusCode)
		}
	}
	if s.count() != 0 {
		t.Fatalf("delivered %d", s.count())
	}
}

func TestBadBodies(t *testing.T) {
	srv, s := newServer(t)
	cases := []struct {
		name string
		ct   string
		body string
		hdr  map[string]string
		want int
	}{
		{"garbage protobuf", ctProto, "\xff\xff\xff\xff\xff", nil, 400},
		{"invalid json", ctJSON, "{nope", nil, 400},
		{"text/plain", "text/plain", "hi", nil, 415},
		{"brotli", ctProto, "", map[string]string{"Content-Encoding": "br"}, 415},
		{"bad gzip", ctProto, "not gzip", map[string]string{"Content-Encoding": "gzip"}, 400},
		{"bad trace json", ctJSON, "{nope", nil, 400},
	}
	for _, c := range cases {
		path := "/v1/logs"
		if c.name == "bad trace json" {
			path = "/v1/traces"
		}
		resp, _ := post(t, srv.URL+path, c.ct, []byte(c.body), c.hdr)
		if resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
	if s.count() != 0 {
		t.Fatalf("delivered %d", s.count())
	}
}

func TestRoutes(t *testing.T) {
	srv, _ := newServer(t)
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
	srv, s := newServer(t)
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

func startReal(t *testing.T) (*Receiver, *sink, string) {
	t.Helper()
	ln, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	s := &sink{}
	r := NewReceiver(s.collect, fixedNow)
	r.Serve(ln)
	t.Cleanup(func() { r.Drain(0, 100*time.Millisecond) })
	return r, s, "http://" + ln.Addr().String() + "/v1/logs"
}

func TestDrainIdle(t *testing.T) {
	r, s, url := startReal(t)
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
	r, s, url := startReal(t)
	b := mustProto(t, twoRecords())
	began := time.Now()
	var ok atomic.Int64
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
}

func TestDrainMax(t *testing.T) {
	r, _, url := startReal(t)
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
	srv, s := newServer(t)
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
