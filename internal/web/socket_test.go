package web

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/store"
)

// shortTemp is a temp dir with a short path: a unix socket path must fit
// sun_path (104 bytes on macOS), which t.TempDir() can exceed.
func shortTemp(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "lh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestSocketDirCreates0700(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	dir, err := SocketDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(tmp, "loghorn") {
		t.Fatalf("dir = %s", dir)
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("stat = %v %v", fi, err)
	}
	// A second call accepts the directory it made.
	if again, err := SocketDir(); err != nil || again != dir {
		t.Fatalf("second call = %s, %v", again, err)
	}
}

func TestSocketDirFallbacks(t *testing.T) {
	run := t.TempDir()
	t.Setenv("TMPDIR", "")
	t.Setenv("XDG_RUNTIME_DIR", run)
	dir, err := SocketDir()
	if err != nil || dir != filepath.Join(run, "loghorn") {
		t.Fatalf("XDG_RUNTIME_DIR: %s, %v", dir, err)
	}
}

func TestSocketDirRejectsLoose(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	dir := filepath.Join(tmp, "loghorn")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // beat the umask
		t.Fatal(err)
	}
	if _, err := SocketDir(); err == nil {
		t.Fatal("mode 0755 accepted")
	}
	// Group-only access is just as loose.
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := SocketDir(); err == nil {
		t.Fatal("mode 0770 accepted")
	}

	// A regular file in the directory's place.
	other := t.TempDir()
	t.Setenv("TMPDIR", other)
	if err := os.WriteFile(filepath.Join(other, "loghorn"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SocketDir(); err == nil {
		t.Fatal("regular file accepted")
	}

	// A symlink to a good directory is not a directory either.
	linked := t.TempDir()
	t.Setenv("TMPDIR", linked)
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(linked, "loghorn")); err != nil {
		t.Fatal(err)
	}
	if _, err := SocketDir(); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestListenSocketReplacesStale(t *testing.T) {
	dir := shortTemp(t)
	stale := filepath.Join(dir, "4242.sock")
	if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "4243.sock")
	if err := os.WriteFile(other, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, path, err := ListenSocket(dir, 4242)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if path != stale {
		t.Fatalf("path = %s", path)
	}
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o700 {
		t.Fatalf("lstat = %v %v", fi, err)
	}
	if b, err := os.ReadFile(other); err != nil || string(b) != "other" {
		t.Fatalf("a neighbour was touched: %q %v", b, err)
	}
	done := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
		done <- err
	}()
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestListenSocketPathTooLong(t *testing.T) {
	dir := filepath.Join(shortTemp(t), strings.Repeat("d", 100))
	if _, _, err := ListenSocket(dir, 1); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("err = %v", err)
	}
}

// unixClient dials path for every request, whatever the URL says.
func unixClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}
}

func TestGuardUnixSkipsHost(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st := store.New(store.ModeHistorical, time.Time{})
	h := New(Options{Store: st, ConfigBase: t.TempDir()})
	ln, path, err := ListenSocket(shortTemp(t), 1)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	defer srv.Close()
	c := unixClient(path)

	req, _ := http.NewRequest(http.MethodGet, "http://example.com/api/session", nil)
	req.Header.Set("Origin", "http://evil.example")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/session = %d", resp.StatusCode)
	}

	body := `{"stream":"app","range":{"from":"all"}}`
	req, _ = http.NewRequest(http.MethodPost, "http://example.com/api/query", strings.NewReader(body))
	req.Header.Set("Origin", "http://evil.example")
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("POST /api/query = %d %s", resp.StatusCode, b)
	}
}

func TestGuardTCPUnchanged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st := store.New(store.ModeHistorical, time.Time{})
	srv := httptest.NewServer(New(Options{Store: st, ConfigBase: t.TempDir()}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/session", nil)
	req.Host = "example.com"
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Host = %d; want 403", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/api/query", strings.NewReader(`{}`))
	req.Header.Set("Origin", "http://evil.example")
	resp, err = srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Origin = %d; want 403", resp.StatusCode)
	}
}

func TestSessionInstanceFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st := store.New(store.ModeLive, time.Time{})
	started := time.Date(2026, 11, 23, 9, 30, 0, 0, time.UTC)
	srv := httptest.NewServer(New(Options{
		Store: st, ConfigBase: t.TempDir(),
		PID: 4321, Cwd: "/work/proj", Port: 5555, Started: started,
		DataDir: "/data", Socket: "/tmp/x.sock", Version: "v9", OTLPPort: 4318,
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/session")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		PID      int       `json:"pid"`
		Cwd      string    `json:"cwd"`
		Port     int       `json:"port"`
		Started  time.Time `json:"started"`
		DataDir  string    `json:"dataDir"`
		Socket   string    `json:"socket"`
		Version  string    `json:"version"`
		OTLPPort int       `json:"otlpPort"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.DataDir != "/data" || got.Socket != "/tmp/x.sock" || got.Version != "v9" || got.OTLPPort != 4318 {
		t.Fatalf("session serve fields = %+v", got)
	}
	if got.PID != 4321 || got.Cwd != "/work/proj" || got.Port != 5555 || !got.Started.Equal(started) {
		t.Fatalf("session = %+v", got)
	}
}
