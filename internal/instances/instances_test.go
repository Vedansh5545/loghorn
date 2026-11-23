package instances

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/store"
	"github.com/jimbeveridge/loghorn/internal/web"
)

// shortDir is a 0700 temp dir with a path short enough for sun_path.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "lh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// serve starts a web handler for pid on <dir>/<pid>.sock.
func serve(t *testing.T, dir string, pid, port int, cwd string) {
	t.Helper()
	h := web.New(web.Options{
		Store: store.New(store.ModeLive, time.Time{}), ConfigBase: t.TempDir(),
		PID: pid, Cwd: cwd, Port: port, Started: time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC),
	})
	ln, _, err := web.ListenSocket(dir, pid)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
}

// staleSocket leaves a socket file with no listener behind it, as a killed
// instance does.
func staleSocket(t *testing.T, dir string, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("stale socket gone before the test: %v", err)
	}
	return path
}

func TestInstancesListLive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := shortDir(t)
	serve(t, dir, 4002, 5002, "/work/b")
	serve(t, dir, 4001, 5001, "/work/a")
	got, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("List = %+v; want 2", got)
	}
	a := got[0]
	if a.PID != 4001 || a.Port != 5001 || a.Cwd != "/work/a" || a.Socket != filepath.Join(dir, "4001.sock") || a.Started.IsZero() {
		t.Fatalf("first = %+v", a)
	}
	if got[1].PID != 4002 {
		t.Fatalf("second = %+v", got[1])
	}
}

func TestInstancesListRemovesStale(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := shortDir(t)
	serve(t, dir, 4001, 5001, "/work/a")
	stale := staleSocket(t, dir, deadPIDSock)
	// Not sockets, or not *.sock: never touched.
	plain := filepath.Join(dir, "888.sock")
	if err := os.WriteFile(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := staleSocket(t, dir, "other.socket")

	got, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PID != 4001 {
		t.Fatalf("List = %+v; want only 4001", got)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale socket kept: %v", err)
	}
	for _, p := range []string{plain, other} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s removed: %v", p, err)
		}
	}
}

func TestInstancesListSlowSocketTimesOut(t *testing.T) {
	dir := shortDir(t)
	// A listener that accepts but never answers: List must give up, not hang,
	// and must keep the socket (it did not refuse).
	path := filepath.Join(dir, "777.sock")
	ln, err := net.Listen("unix", path)
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
			defer c.Close()
		}
	}()
	start := time.Now()
	got, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("List = %+v; want none", got)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("List took %v", d)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("slow socket removed: %v", err)
	}
}

// deadPIDSock names a socket after a pid above every OS's pid limit, so no
// process has it.
const deadPIDSock = "1073741824.sock"

func TestInstancesListKeepsRefusingLiveSocket(t *testing.T) {
	// A live instance with a full listen backlog refuses connections too: a
	// refusing socket whose pid is alive is kept. So is one not named a pid.
	dir := shortDir(t)
	live := staleSocket(t, dir, strconv.Itoa(os.Getpid())+".sock")
	named := staleSocket(t, dir, "x.sock")
	got, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("List = %+v; want none", got)
	}
	for _, p := range []string{live, named} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s removed: %v", p, err)
		}
	}
}

func TestClientResponseTimeout(t *testing.T) {
	old := ResponseTimeout
	ResponseTimeout = 100 * time.Millisecond
	t.Cleanup(func() { ResponseTimeout = old })
	dir := shortDir(t)
	path := filepath.Join(dir, "1.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release })}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { close(release); srv.Close() })
	start := time.Now()
	resp, err := Client(path).Post("http://loghorn/api/query", "application/json", nil)
	if err == nil {
		resp.Body.Close()
		t.Fatal("a wedged instance answered")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("gave up after %v", d)
	}
}

func TestInstancesListMissingDir(t *testing.T) {
	if _, err := List(filepath.Join(shortDir(t), "nope")); err == nil {
		t.Fatal("List of a missing dir: no error")
	}
}
