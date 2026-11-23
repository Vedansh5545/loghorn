// Package instances finds the running loghorn -web instances through their
// unix sockets (<pid>.sock in the per-user socket dir, see web.SocketDir).
package instances

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// dialTimeout bounds the connect to one socket; probeTimeout bounds the whole
// /api/session exchange, so a wedged instance cannot hang the CLI.
const (
	dialTimeout  = time.Second
	probeTimeout = 2 * time.Second
)

// Instance is one live -web instance, as its /api/session describes it.
type Instance struct {
	PID, Port int
	Cwd       string
	Started   time.Time
	Socket    string // path of its unix socket
}

// ResponseTimeout bounds the wait for a query's response headers, so a
// wedged instance cannot hang the CLI. The server writes its headers only
// once the whole answer is built, so this bounds its compute time and never
// cuts off a long body. A var so tests can shorten it.
var ResponseTimeout = 60 * time.Second

// Client is an HTTP client whose every request goes to the unix socket at
// path, whatever the URL's host. It uses no proxy, and a request fails when
// the response headers take longer than ResponseTimeout.
func Client(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: dialTimeout}
			return d.DialContext(ctx, "unix", path)
		},
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: ResponseTimeout,
	}}
}

// List asks every *.sock socket in dir for its /api/session and returns the
// instances that answered, by PID. dir should come from web.SocketDir, which
// checks that only this user can reach it. A socket that refuses the
// connection and whose pid has exited belongs to an instance that is gone
// and is removed; one that
// answers badly or too slowly is skipped and kept. Entries that are not
// sockets are left alone.
func List(dir string) ([]Instance, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Instance
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sock") || e.Type()&fs.ModeSocket == 0 {
			continue
		}
		path := filepath.Join(dir, e.Name())
		in, err := probe(path)
		if err != nil {
			if errors.Is(err, syscall.ECONNREFUSED) && gone(e.Name()) {
				_ = os.Remove(path)
			}
			continue
		}
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, nil
}

// gone reports whether the process a <pid>.sock is named after has exited.
// A live instance whose listen backlog is full also refuses connections
// (macOS), so a refusal alone is no proof the socket is stale. A name that is
// not a pid is not gone: it is left alone.
func gone(name string) bool {
	pid, err := strconv.Atoi(strings.TrimSuffix(name, ".sock"))
	if err != nil || pid <= 0 {
		return false
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// probe reads the session of the instance listening on path.
func probe(path string) (Instance, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://loghorn/api/session", nil)
	if err != nil {
		return Instance{}, err
	}
	resp, err := Client(path).Do(req)
	if err != nil {
		return Instance{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Instance{}, fmt.Errorf("%s: /api/session answered %s", path, resp.Status)
	}
	var s struct {
		PID     int       `json:"pid"`
		Cwd     string    `json:"cwd"`
		Port    int       `json:"port"`
		Started time.Time `json:"started"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return Instance{}, fmt.Errorf("%s: /api/session: %w", path, err)
	}
	return Instance{PID: s.PID, Port: s.Port, Cwd: s.Cwd, Started: s.Started, Socket: path}, nil
}
