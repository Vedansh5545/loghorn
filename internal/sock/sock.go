// Package sock is loghorn serve's unix socket: where serve listens and how
// every other command finds it. One fixed path per machine user
// (/tmp/loghorn.sock unless $LOGHORN_SOCKET says otherwise) means no client
// needs to know the data folder or the ports; /api/session on the socket
// reports them.
package sock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

// EnvVar points every command at another serve's socket.
const EnvVar = "LOGHORN_SOCKET"

// MaxPath is the longest unix socket path that fits sun_path on macOS (104
// bytes including the terminating NUL); Linux allows 108.
const MaxPath = 103

// ErrNotRunning is wrapped by Probe's error when nothing answers on the
// socket: no file, or a file nobody listens on.
var ErrNotRunning = errors.New("loghorn serve isn't running")

// LiveError is Listen's refusal to take over a socket another process is
// serving.
type LiveError struct{ Path string }

func (e *LiveError) Error() string { return "another loghorn serve is answering on " + e.Path }

// DefaultPath is $LOGHORN_SOCKET, else /tmp/loghorn.sock (on Windows,
// %LOCALAPPDATA%\loghorn\loghorn.sock).
func DefaultPath() string {
	if p := os.Getenv(EnvVar); p != "" {
		return p
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("LOCALAPPDATA"), "loghorn", "loghorn.sock")
	}
	return "/tmp/loghorn.sock"
}

// dialTimeout bounds a connect; probeTimeout bounds a whole /api/session
// exchange, so a wedged serve can't hang a client.
const (
	dialTimeout  = 500 * time.Millisecond
	probeTimeout = 2 * time.Second
)

// Listen listens on path, mode 0600. A socket file something answers on is a
// live serve: Listen returns *LiveError and leaves it alone. One that refuses
// connections was left by a crash and is removed first.
func Listen(path string) (net.Listener, error) {
	if len(path) > MaxPath {
		return nil, fmt.Errorf("socket path %s is %d bytes; the limit is %d", path, len(path), MaxPath)
	}
	if _, err := os.Lstat(path); err == nil {
		if c, err := net.DialTimeout("unix", path, dialTimeout); err == nil {
			c.Close()
			return nil, &LiveError{Path: path}
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("removing stale socket %s: %w", path, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// ResponseTimeout bounds the wait for a response's headers through Client, so
// a wedged serve can't hang the CLI. The server writes its headers only once
// the answer is built, so this bounds compute time and never cuts off a long
// body. A var so tests can shorten it.
var ResponseTimeout = 60 * time.Second

// Client is an HTTP client whose every request goes to the unix socket at
// path, whatever the URL's host. It uses no proxy.
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

// Session is what serve's /api/session reports about itself.
type Session struct {
	PID      int       `json:"pid"`
	Started  time.Time `json:"started"`
	Version  string    `json:"version"`
	DataDir  string    `json:"dataDir"`
	Socket   string    `json:"socket"`
	Port     int       `json:"port"`
	OTLPPort int       `json:"otlpPort"`
}

// WebURL is the web page's address.
func (s Session) WebURL() string { return "http://127.0.0.1:" + strconv.Itoa(s.Port) + "/" }

// OTLPEndpoint is the OTLP/HTTP base URL, as OTEL_EXPORTER_OTLP_ENDPOINT takes
// it.
func (s Session) OTLPEndpoint() string { return "http://127.0.0.1:" + strconv.Itoa(s.OTLPPort) }

// Probe asks the serve on path for its session. Nothing answering is
// ErrNotRunning (errors.Is); a serve that answers badly is another error.
func Probe(path string) (Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://loghorn/api/session", nil)
	if err != nil {
		return Session{}, err
	}
	resp, err := Client(path).Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return Session{}, fmt.Errorf("%w (no answer on %s)", ErrNotRunning, path)
		}
		return Session{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Session{}, fmt.Errorf("%s: /api/session answered %s", path, resp.Status)
	}
	var s Session
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return Session{}, fmt.Errorf("%s: /api/session: %w", path, err)
	}
	return s, nil
}
