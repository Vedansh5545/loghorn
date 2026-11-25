package shipper

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/jimbeveridge/loghorn/internal/config"
)

type collector struct {
	mu    sync.Mutex
	reqs  []*collogspb.ExportLogsServiceRequest
	lines []string
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/logs" || r.Header.Get("Content-Type") != "application/x-protobuf" {
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	b, _ := io.ReadAll(r.Body)
	var req collogspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(b, &req); err != nil {
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.reqs = append(c.reqs, &req)
	for _, rl := range req.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				c.lines = append(c.lines, lr.GetBody().GetStringValue())
			}
		}
	}
	c.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func TestShipperSendsBatches(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	in := config.Default().Input
	s := New(srv.URL, Resource{Service: "api", RunID: "r1", Input: in}, func(err error) { t.Errorf("onError: %v", err) })
	for i := range 1000 {
		s.Line([]byte("line " + strconv.Itoa(i)))
	}
	if dropped := s.Close(2 * time.Second); dropped != 0 {
		t.Fatalf("dropped = %d", dropped)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.lines) != 1000 {
		t.Fatalf("received %d lines; want 1000", len(c.lines))
	}
	for i, l := range c.lines {
		if l != "line "+strconv.Itoa(i) {
			t.Fatalf("line %d = %q; out of order", i, l)
		}
	}
	wantInput, _ := json.Marshal(in)
	rl := c.reqs[0].GetResourceLogs()[0]
	attrs := map[string]string{}
	for _, kv := range rl.GetResource().GetAttributes() {
		attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
	}
	if attrs["service.name"] != "api" || attrs["loghorn.run.id"] != "r1" || attrs["loghorn.input"] != string(wantInput) {
		t.Fatalf("resource attrs = %v", attrs)
	}
	if got := rl.GetScopeLogs()[0].GetScope().GetName(); got != "loghorn.run" {
		t.Fatalf("scope = %q", got)
	}
	if rl.GetScopeLogs()[0].GetLogRecords()[0].GetTimeUnixNano() == 0 {
		t.Fatal("record has no time")
	}
}

func TestShipperNeverBlocks(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens: every batch is refused
	var errs atomic.Int32
	s := New("http://"+addr, Resource{RunID: "r"}, func(error) { errs.Add(1) })
	start := time.Now()
	for range 10000 {
		s.Line([]byte("x"))
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Line calls took %v; they must not block", d)
	}
	if dropped := s.Close(500 * time.Millisecond); dropped != 10000 {
		t.Fatalf("dropped = %d; want 10000", dropped)
	}
	if n := errs.Load(); n != 1 {
		t.Fatalf("onError called %d times; want 1", n)
	}
}

func TestShipperLineAfterCloseIsDropped(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	s := New(srv.URL, Resource{RunID: "r"}, nil)
	s.Line([]byte("before"))
	s.Close(time.Second)
	s.Line([]byte("late")) // a grandchild still writing after the command exits
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.lines) != 1 || c.lines[0] != "before" {
		t.Fatalf("lines = %q", c.lines)
	}
}
