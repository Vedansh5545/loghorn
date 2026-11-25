package sock

import (
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tmpSock returns a socket path short enough for sun_path: macOS t.TempDir()
// paths are too long.
func tmpSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "lh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("LOGHORN_SOCKET", "/tmp/other.sock")
	if got := DefaultPath(); got != "/tmp/other.sock" {
		t.Fatalf("DefaultPath = %q", got)
	}
	t.Setenv("LOGHORN_SOCKET", "")
	if got := DefaultPath(); got != "/tmp/loghorn.sock" {
		t.Fatalf("DefaultPath = %q; want /tmp/loghorn.sock", got)
	}
}

func TestListenStaleSocket(t *testing.T) {
	path := tmpSock(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale socket file should remain: %v", err)
	}
	ln2, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	ln2.Close()
}

func TestListenLiveSocket(t *testing.T) {
	path := tmpSock(t)
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, err = Listen(path)
	var live *LiveError
	if !errors.As(err, &live) || live.Path != path {
		t.Fatalf("Listen on a live socket = %v; want *LiveError", err)
	}
}

func TestListenSocketMode(t *testing.T) {
	path := tmpSock(t)
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v; want 0600", fi.Mode().Perm())
	}
}

func TestListenTooLong(t *testing.T) {
	path := "/tmp/" + strings.Repeat("x", 115)
	_, err := Listen(path)
	if err == nil || !strings.Contains(err.Error(), "103") {
		t.Fatalf("Listen(long) = %v; want an error naming the 103-byte limit", err)
	}
}

func TestProbeNotRunning(t *testing.T) {
	_, err := Probe(tmpSock(t))
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Probe(missing) = %v; want ErrNotRunning", err)
	}
}

func TestProbeSession(t *testing.T) {
	path := tmpSock(t)
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/session" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pid":7,"port":4380,"otlpPort":4318,"dataDir":"/d","socket":"/s","version":"v1"}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	s, err := Probe(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.PID != 7 || s.Port != 4380 || s.OTLPPort != 4318 || s.DataDir != "/d" || s.Socket != "/s" || s.Version != "v1" {
		t.Fatalf("session = %+v", s)
	}
	if s.WebURL() != "http://127.0.0.1:4380/" {
		t.Fatalf("WebURL = %q", s.WebURL())
	}
	if s.OTLPEndpoint() != "http://127.0.0.1:4318" {
		t.Fatalf("OTLPEndpoint = %q", s.OTLPEndpoint())
	}
}

func TestListenRefusesNonSocket(t *testing.T) {
	path := tmpSock(t)
	if err := os.WriteFile(path, []byte("notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("Listen over a regular file = %v; want a refusal", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "notes" {
		t.Fatalf("the file was touched: %q, %v", b, err)
	}
}
