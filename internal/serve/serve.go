// Package serve is loghorn serve: the one long-running process that receives
// OTLP from apps and the lines loghorn run sends, stores them in the per-user
// data folder, and serves the web page and its API on TCP and on the unix
// socket clients find it by.
//
// Start takes the data folder's lock, binds every listener, and only then
// starts serving, so a second serve, a port in use or a live socket fails
// before anything is served or written there. serve never forks: whoever
// started it (a terminal, launchd, systemd) owns its lifetime and stops it
// with SIGTERM, which ends in Close.
package serve

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/ingest"
	"github.com/jimbeveridge/loghorn/internal/logfile"
	"github.com/jimbeveridge/loghorn/internal/otlp"
	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/sock"
	"github.com/jimbeveridge/loghorn/internal/store"
	"github.com/jimbeveridge/loghorn/internal/web"
)

// Options configures a Server. Ports are used as given: 0 picks a free one.
type Options struct {
	DataDir string
	Socket  string
	WebPort int
	// OTLPPort is the OTLP/HTTP port.
	OTLPPort int
	// Config is the user-level config; its Input frames and parses what
	// arrives without a run's own settings (OTLP, stored days).
	Config     config.Config
	ConfigBase string // svcconf base for the page's saved settings
	Version    string
	Log        io.Writer        // serve's own log; nil = stderr
	Now        func() time.Time // nil = time.Now

	// AdoptEvery is how often runs recorded while serve was down are adopted
	// and idle runs forgotten; 0 = a minute. SocketCheckEvery is how often the
	// socket file is checked and re-created if something deleted it; 0 = five
	// minutes.
	AdoptEvery       time.Duration
	SocketCheckEvery time.Duration
}

// PortError is a listener that could not be bound. Which is "web" or
// "otlp-http".
type PortError struct {
	Addr, Which string
	Err         error
}

func (e *PortError) Error() string {
	return fmt.Sprintf("can't listen on %s for %s: %v", e.Addr, e.Which, e.Err)
}

func (e *PortError) Unwrap() error { return e.Err }

// Server is a running serve.
type Server struct {
	o       Options
	logs    *logfile.Writer
	spans   *logfile.Writer // nil when the span file couldn't open
	st      *store.Store
	rcv     *otlp.Receiver
	router  *router
	sink    func(rec []byte)  // appends a record to loghorn.log
	spanFn  func(line []byte) // appends a span and stores it
	handler http.Handler
	webSrv  *http.Server
	session sock.Session

	sockMu   sync.Mutex
	sockSrvs []*http.Server

	stop      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

// Start opens the data folder, binds the socket and both ports, and serves.
// Its errors are *logfile.LockedError (another serve has the data folder),
// *sock.LiveError (another serve answers on the socket), *PortError, or a
// plain error; in every case nothing is left running, bound or locked.
func Start(o Options) (*Server, error) {
	if o.Log == nil {
		o.Log = os.Stderr
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.AdoptEvery == 0 {
		o.AdoptEvery = time.Minute
	}
	if o.SocketCheckEvery == 0 {
		o.SocketCheckEvery = 5 * time.Minute
	}
	s := &Server{o: o, stop: make(chan struct{})}

	// Lock and open storage first: the lock is what makes this the only serve
	// on the folder. Everything after it is undone on failure.
	if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
		return nil, err
	}
	logs, err := logfile.Open(o.DataDir, o.Now, time.Local)
	if err != nil {
		return nil, err
	}
	s.logs = logs
	undo := func() {
		logs.Close()
	}

	sockLn, err := sock.Listen(o.Socket)
	if err != nil {
		undo()
		return nil, err
	}
	undo = func() {
		sockLn.Close()
		logs.Close()
	}
	webLn, err := web.Listen(o.WebPort)
	if err != nil {
		undo()
		return nil, &PortError{Addr: addr(o.WebPort), Which: "web", Err: err}
	}
	otlpLn, err := net.Listen("tcp", addr(o.OTLPPort))
	if err != nil {
		webLn.Close()
		undo()
		return nil, &PortError{Addr: addr(o.OTLPPort), Which: "otlp-http", Err: err}
	}

	if w, err := logs.Companion(logfile.SpansBase, logfile.SpansSuffix); err != nil {
		s.logf("span file: %v; spans are kept in memory only", err)
	} else {
		s.spans = w
	}
	s.openStore()

	s.sink, s.spanFn = s.logSink(), s.spanSink()
	s.router = newRouter(s.sink, s.st, o.Config.Input, o.Now)
	s.rcv = otlp.NewReceiver(s.router.otlpLog, s.spanFn, o.Now)
	s.adopt()
	s.rcv.SetRunLines(s.router.runLine)

	webPort := webLn.Addr().(*net.TCPAddr).Port
	otlpPort := otlpLn.Addr().(*net.TCPAddr).Port
	started := o.Now().Round(0) // wall clock only, as /api/session reports it
	s.session = sock.Session{
		PID: os.Getpid(), Started: started, Version: o.Version,
		DataDir: o.DataDir, Socket: o.Socket, Port: webPort, OTLPPort: otlpPort,
	}
	s.handler = web.New(web.Options{
		Store: s.st, ConfigBase: o.ConfigBase,
		PID: os.Getpid(), Port: webPort, Started: started,
		DataDir: o.DataDir, Socket: o.Socket, Version: o.Version, OTLPPort: otlpPort,
	})
	s.webSrv = &http.Server{Handler: s.handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.webSrv.Serve(webLn) }()
	s.serveSocket(sockLn)
	s.rcv.Serve(otlpLn)

	s.wg.Add(1)
	go s.background()
	return s, nil
}

func addr(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

// Session describes this serve as /api/session reports it.
func (s *Server) Session() sock.Session { return s.session }

// Close stops serving, waits for in-flight OTLP deliveries, closes the files
// and releases the data folder. The socket file goes with its listener. Safe
// to call twice.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
		s.wg.Wait()
		s.rcv.Drain(500*time.Millisecond, 3*time.Second)
		s.webSrv.Close()
		s.sockMu.Lock()
		for _, srv := range s.sockSrvs {
			srv.Close() // closing the listener unlinks the socket file
		}
		s.sockMu.Unlock()
		if s.spans != nil {
			s.spans.Close()
		}
		s.closeErr = closeOwned(s.st, s.logs)
	})
	return s.closeErr
}

// closeOwned closes the store, then the log writer. The writer holds the
// data folder's lock, so it goes last: the store drains its queue and
// checkpoints loghorn.duckdb while no other serve can take the folder.
func closeOwned(st, logs io.Closer) error {
	err := st.Close()
	return errors.Join(err, logs.Close())
}

func (s *Server) serveSocket(ln net.Listener) {
	srv := &http.Server{Handler: s.handler, ReadHeaderTimeout: 10 * time.Second}
	s.sockMu.Lock()
	s.sockSrvs = append(s.sockSrvs, srv)
	s.sockMu.Unlock()
	go func() { _ = srv.Serve(ln) }()
}

// background runs the periodic work: adopting runs recorded while serve was
// down, forgetting idle runs, and putting the
// socket back if something (systemd-tmpfiles cleaning /tmp) deleted it.
func (s *Server) background() {
	defer s.wg.Done()
	adopt := time.NewTicker(s.o.AdoptEvery)
	defer adopt.Stop()
	check := time.NewTicker(s.o.SocketCheckEvery)
	defer check.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-adopt.C:
			s.adopt()
			s.router.evict(s.o.Now().Add(-idleRun))
		case <-check.C:
			s.checkSocket()
		}
	}
}

func (s *Server) checkSocket() {
	if _, err := os.Lstat(s.o.Socket); !errors.Is(err, os.ErrNotExist) {
		return
	}
	ln, err := sock.Listen(s.o.Socket)
	if err != nil {
		s.logf("socket %s was deleted and can't be re-created: %v", s.o.Socket, err)
		return
	}
	s.logf("socket %s was deleted; re-created it", s.o.Socket)
	s.serveSocket(ln)
}

// openStore opens the DuckDB files in the data folder: serve is their one
// writer. The first open fills them from the stored days' text; later opens
// read what they hold. If they can't be opened, the stored days are loaded
// into a store in memory instead.
func (s *Server) openStore() {
	st, err := store.Open(store.Options{
		Mode: store.ModeLive, RunStart: s.o.Now(), Input: s.o.Config.Input,
		Dir: s.o.DataDir, Now: s.o.Now,
		Warn: func(m string) { fmt.Fprintln(s.o.Log, m) },
	})
	if err == nil {
		s.st = st
		return
	}
	s.logf("DuckDB: %v; records are kept in memory only", err)
	s.st = store.New(store.ModeLive, s.o.Now())
	s.preload()
}

// preload loads the stored days, then the stored spans, into the store.
func (s *Server) preload() {
	if paths, err := logfile.HistoricalFiles(s.o.DataDir); err == nil {
		if err := store.Preload(s.st, paths, s.o.Config.Input); err != nil {
			s.logf("preload: %v", err)
		}
	}
	if paths, err := logfile.HistoricalSpanFiles(s.o.DataDir); err == nil {
		if err := store.Preload(s.st, paths, s.o.Config.Input); err != nil {
			s.logf("preload spans: %v", err)
		}
	}
}

// logSink appends a record to loghorn.log, one record per line. The writer
// reports only its first failure; that is logged once.
func (s *Server) logSink() func(rec []byte) {
	var once sync.Once
	return func(rec []byte) {
		if err := s.logs.Write(ingest.CompactRecord(rec)); err != nil {
			once.Do(func() { s.logf("log file stopped: %v", err) })
		}
	}
}

// spanSink appends a span line to the span file and adds it to the store.
func (s *Server) spanSink() func(line []byte) {
	var once sync.Once
	return func(line []byte) {
		if s.spans != nil {
			if err := s.spans.Write(line); err != nil {
				once.Do(func() { s.logf("span file stopped: %v", err) })
			}
		}
		if r, ok := record.NormalizeSpan(line); ok {
			s.st.Add(r, s.o.Now())
		}
	}
}

func (s *Server) logf(format string, a ...any) {
	fmt.Fprintf(s.o.Log, "loghorn serve: "+format+"\n", a...)
}
