package serve

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/logfile"
	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/shipper"
	"github.com/jimbeveridge/loghorn/internal/sock"
)

// syncBuf is a goroutine-safe log writer for Options.Log.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// testOpts returns options with a temp HOME, data folder and a short socket
// path, ephemeral ports and fast timers.
func testOpts(t *testing.T) Options {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	sdir, err := os.MkdirTemp("/tmp", "lh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sdir) })
	return Options{
		DataDir:          t.TempDir(),
		Socket:           filepath.Join(sdir, "s.sock"),
		Config:           config.Default(),
		ConfigBase:       t.TempDir(),
		Version:          "test",
		Log:              &syncBuf{},
		AdoptEvery:       50 * time.Millisecond,
		SocketCheckEvery: 50 * time.Millisecond,
	}
}

func start(t *testing.T, o Options) *Server {
	t.Helper()
	s, err := Start(o)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// waitFor polls cond for up to 3 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func records(s *Server) []record.Record {
	recs, _ := s.st.Snapshot()
	return recs
}

func logFile(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "loghorn.log"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

func ship(s *Server, svc, run string, lines ...string) {
	sh := shipper.New(s.Session().OTLPEndpoint(), shipper.Resource{Service: svc, RunID: run, Input: config.Default().Input}, nil)
	for _, l := range lines {
		sh.Line([]byte(l))
	}
	sh.Close(3 * time.Second)
}

func TestStartServesSessionAndWeb(t *testing.T) {
	o := testOpts(t)
	s := start(t, o)
	sess, err := sock.Probe(o.Socket)
	if err != nil {
		t.Fatal(err)
	}
	if sess.DataDir != o.DataDir || sess.Socket != o.Socket || sess.Port == 0 || sess.OTLPPort == 0 || sess.Version != "test" {
		t.Fatalf("session = %+v", sess)
	}
	if sess != s.Session() {
		t.Fatalf("Probe %+v != Session %+v", sess, s.Session())
	}
	resp, err := http.Get(sess.WebURL() + "api/streams")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/streams = %d", resp.StatusCode)
	}
}

func TestRunLinesUseRunInputAndService(t *testing.T) {
	o := testOpts(t)
	s := start(t, o)
	ship(s, "api", "r1", `{"labels":{"schema":"web"},"message":"a1"}`, "plain a2")
	ship(s, "web", "r2", `{"labels":{"schema":"web"},"message":"b1"}`, "plain b2")
	waitFor(t, "4 lines in loghorn.log", func() bool { return strings.Count(logFile(t, o.DataDir), "\n") == 4 })
	got := map[string]string{}
	for _, r := range records(s) {
		got[string(r.Raw)] = r.Service
	}
	if got[`{"labels":{"schema":"web"},"message":"a1"}`] != "api" || got[`{"labels":{"schema":"web"},"message":"b1"}`] != "web" {
		t.Fatalf("services = %v", got)
	}
	log := logFile(t, o.DataDir)
	for _, l := range []string{"plain a2", "plain b2", `"a1"`, `"b1"`} {
		if !strings.Contains(log, l) {
			t.Fatalf("loghorn.log lacks %q:\n%s", l, log)
		}
	}
}

func postOTLPLog(t *testing.T, s *Server, runID, body string) {
	t.Helper()
	req := &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "api"}}},
			{Key: "loghorn.run.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: runID}}},
		}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
			TimeUnixNano: uint64(time.Now().UnixNano()),
			Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}},
		}}}},
	}}}
	b, _ := proto.Marshal(req)
	resp, err := http.Post(s.Session().OTLPEndpoint()+"/v1/logs", "application/x-protobuf", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func TestOTLPDedupePerRun(t *testing.T) {
	o := testOpts(t)
	s := start(t, o)
	postOTLPLog(t, s, "r1", "from otlp")
	ship(s, "api", "r1", `{"labels":{"schema":"web"},"message":"dup"}`)
	ship(s, "web", "r2", `{"labels":{"schema":"web"},"message":"kept"}`)
	waitFor(t, "two lines in loghorn.log", func() bool { return strings.Count(logFile(t, o.DataDir), "\n") >= 2 })
	time.Sleep(100 * time.Millisecond)
	log := logFile(t, o.DataDir)
	if strings.Contains(log, `"dup"`) {
		t.Fatalf("run r1's JSON line was stored after its OTLP:\n%s", log)
	}
	if !strings.Contains(log, `"kept"`) || !strings.Contains(log, "from otlp") {
		t.Fatalf("loghorn.log = %s", log)
	}
}

func TestSecondServeFailsOnLock(t *testing.T) {
	o := testOpts(t)
	start(t, o)
	o2 := o
	sdir, _ := os.MkdirTemp("/tmp", "lh")
	defer os.RemoveAll(sdir)
	o2.Socket = filepath.Join(sdir, "s.sock")
	_, err := Start(o2)
	var locked *logfile.LockedError
	if !errors.As(err, &locked) {
		t.Fatalf("second Start = %v; want *logfile.LockedError", err)
	}
	if _, err := sock.Probe(o.Socket); err != nil {
		t.Fatalf("first serve stopped answering: %v", err)
	}
}

func TestSecondServeFailsOnLiveSocket(t *testing.T) {
	o := testOpts(t)
	start(t, o)
	o2 := o
	o2.DataDir = t.TempDir()
	_, err := Start(o2)
	var live *sock.LiveError
	if !errors.As(err, &live) {
		t.Fatalf("second Start = %v; want *sock.LiveError", err)
	}
	if _, err := logfile.TryLock(o2.DataDir); err != nil {
		t.Fatalf("failed Start kept the lock: %v", err)
	}
	if _, err := sock.Probe(o.Socket); err != nil {
		t.Fatalf("first serve stopped answering: %v", err)
	}
}

func TestPortInUse(t *testing.T) {
	o := testOpts(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	o.OTLPPort = ln.Addr().(*net.TCPAddr).Port
	_, err = Start(o)
	var pe *PortError
	if !errors.As(err, &pe) || pe.Which != "otlp-http" {
		t.Fatalf("Start = %v; want *PortError for otlp-http", err)
	}
	if _, err := os.Lstat(o.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket left behind: %v", err)
	}
	release, err := logfile.TryLock(o.DataDir)
	if err != nil {
		t.Fatalf("lock still held: %v", err)
	}
	release()
}

func TestCloseRemovesSocketAndFlushes(t *testing.T) {
	o := testOpts(t)
	s, err := Start(o)
	if err != nil {
		t.Fatal(err)
	}
	ship(s, "api", "r1", "one", "two")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := os.Lstat(o.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket not removed: %v", err)
	}
	release, err := logfile.TryLock(o.DataDir)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	release()
	if log := logFile(t, o.DataDir); !strings.Contains(log, "one\ntwo\n") {
		t.Fatalf("loghorn.log = %q", log)
	}
}

func TestSocketRecreated(t *testing.T) {
	o := testOpts(t)
	start(t, o)
	if err := os.Remove(o.Socket); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the socket to answer again", func() bool {
		_, err := sock.Probe(o.Socket)
		return err == nil
	})
}

func TestStartFromRoot(t *testing.T) {
	o := testOpts(t)
	wd, _ := os.Getwd()
	if err := os.Chdir("/"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })
	s := start(t, o)
	ship(s, "api", "r1", "x")
	waitFor(t, "the line in loghorn.log", func() bool { return strings.Contains(logFile(t, o.DataDir), "x\n") })
	if _, err := os.Stat("/.loghorn"); err == nil {
		t.Fatal("serve wrote /.loghorn")
	}
}
