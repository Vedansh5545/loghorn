// Package web serves the query view: an embedded page and a JSON API over the
// session store. It listens on loopback only and refuses any request whose
// Host is not the listener's own loopback address (DNS rebinding) and any
// write whose Origin is not the page's own (cross-site requests).
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jimbeveridge/loghorn/internal/favorites"
	"github.com/jimbeveridge/loghorn/internal/query"
	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
	"github.com/jimbeveridge/loghorn/internal/sqlfmt"
	"github.com/jimbeveridge/loghorn/internal/store"
	"github.com/jimbeveridge/loghorn/internal/svcconf"
)

//go:embed static
var embedded embed.FS

// maxBody caps request bodies (schemas, queries, favorites, pins).
const maxBody = 1 << 20

// Options configures the handler.
type Options struct {
	Store *store.Store
	// ConfigBase holds the per-service config: <ConfigBase>/services/<key>/
	// (svcconf.Dir) with pins, favorites and field settings.
	ConfigBase string
	Now        func() time.Time // nil means time.Now
}

// schemaKey identifies a cached inferred schema.
type schemaKey struct{ Service, Source, Name string }

type server struct {
	o       Options
	static  fs.FS
	writeMu sync.Mutex // serialises pins, field settings and favorites writes

	cacheMu sync.Mutex
	cache   map[schemaKey]cachedSchema // inferred schema per (service, source, stream)
}

type cachedSchema struct {
	version uint64
	s       schema.Schema
}

// Listen opens a TCP listener on 127.0.0.1:port (0 picks a free port).
func Listen(port int) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

// New returns the HTTP handler for the query view.
func New(o Options) http.Handler {
	if o.Now == nil {
		o.Now = time.Now
	}
	sub, err := fs.Sub(embedded, "static")
	if err != nil {
		panic(err) // the embed directive guarantees static/ exists
	}
	s := &server{o: o, static: sub, cache: map[schemaKey]cachedSchema{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(sub)))
	mux.HandleFunc("GET /api/session", s.session)
	mux.HandleFunc("GET /api/streams", s.streams)
	mux.HandleFunc("GET /api/kinds", s.kinds)
	mux.HandleFunc("GET /api/pins", s.getPins)
	mux.HandleFunc("PUT /api/pins", s.putPins)
	mux.HandleFunc("GET /api/schema", s.getSchema)
	mux.HandleFunc("PUT /api/schema", s.putSchema)
	mux.HandleFunc("POST /api/query", s.query)
	mux.HandleFunc("GET /api/record", s.record)
	mux.HandleFunc("GET /api/favorites", s.listFavorites)
	mux.HandleFunc("PUT /api/favorites/{name}", s.putFavorite)
	mux.HandleFunc("DELETE /api/favorites/{name}", s.deleteFavorite)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	return s.guard(mux)
}

// guard enforces the loopback Host on every request and the page's own
// Origin on every non-GET request. POST /api/query alone may omit Origin (a
// curl client); a present, foreign Origin is refused everywhere.
func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		port := listenerPort(r)
		if port == "" || (r.Host != "127.0.0.1:"+port && r.Host != "localhost:"+port) {
			writeError(w, http.StatusForbidden, "forbidden host")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			origin := r.Header.Get("Origin")
			exempt := origin == "" && r.Method == http.MethodPost && r.URL.Path == "/api/query"
			if !exempt && origin != "http://"+r.Host {
				writeError(w, http.StatusForbidden, "forbidden origin")
				return
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next.ServeHTTP(w, r)
	})
}

// listenerPort is the port of the local address the request arrived on, or
// "" when the server did not record one (the guard then refuses).
func listenerPort(r *http.Request) string {
	a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || a == nil {
		return ""
	}
	_, port, err := net.SplitHostPort(a.String())
	if err != nil {
		return ""
	}
	return port
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	b, err := fs.ReadFile(s.static, "index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

// allServices is the service value that means every service (read only).
const allServices = "*"

// serviceParam is ?service=, with "" meaning record.DefaultService.
func serviceParam(r *http.Request) string {
	return orDefault(r.URL.Query().Get("service"))
}

func orDefault(svc string) string {
	if svc == "" {
		return record.DefaultService
	}
	return svc
}

// saveService is the ?service= a write applies to. All services can't be
// written: it writes a 400 and returns false.
func saveService(w http.ResponseWriter, r *http.Request) (string, bool) {
	svc := serviceParam(r)
	if svc == allServices {
		writeError(w, http.StatusBadRequest, "Choose a service to save")
		return "", false
	}
	return svc, true
}

// readService is the ?service= of a read that is per service only (pins,
// favorites); the page asks each service in turn for all of them.
func readService(w http.ResponseWriter, r *http.Request) (string, bool) {
	svc := serviceParam(r)
	if svc == allServices {
		writeError(w, http.StatusBadRequest, "choose a service")
		return "", false
	}
	return svc, true
}

// svcDir is the config directory of one service (never allServices).
func (s *server) svcDir(svc string) string {
	return svcconf.Dir(s.o.ConfigBase, svc)
}

// timePtr is nil for the zero time, so JSON shows null rather than year 1.
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

type sessionJSON struct {
	Mode       store.Mode `json:"mode"`
	RunStart   *time.Time `json:"runStart"`
	Earliest   *time.Time `json:"earliest"`
	Latest     *time.Time `json:"latest"`
	Count      int        `json:"count"`
	ConfigBase string     `json:"configBase"`
}

func (s *server) session(w http.ResponseWriter, r *http.Request) {
	in := s.o.Store.Info()
	writeJSON(w, http.StatusOK, sessionJSON{
		Mode: in.Mode, RunStart: timePtr(in.RunStart), Earliest: timePtr(in.Earliest),
		Latest: timePtr(in.Latest), Count: in.Count, ConfigBase: s.o.ConfigBase,
	})
}

type streamJSON struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Count  int    `json:"count"`
}

func (s *server) streams(w http.ResponseWriter, r *http.Request) {
	out := []streamJSON{}
	for _, sc := range s.o.Store.Streams() {
		out = append(out, streamJSON{sc.Name, sc.Source, sc.Count})
	}
	writeJSON(w, http.StatusOK, out)
}

type countJSON struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type serviceCountJSON struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

// kinds counts the kinds of one service (or "*") and the records of every
// service, for one source, within the ?from=&to= range (query range values).
func (s *server) kinds(w http.ResponseWriter, r *http.Request) {
	src, ok := sourceParam(w, r)
	if !ok {
		return
	}
	rng := query.Range{From: r.URL.Query().Get("from"), To: r.URL.Query().Get("to")}
	from, to, err := rng.Resolve(s.o.Now(), s.o.Store.Info().RunStart)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	kc, sc := s.o.Store.Kinds(src, serviceParam(r), from, to)
	out := struct {
		Kinds    []countJSON        `json:"kinds"`
		Services []serviceCountJSON `json:"services"`
	}{make([]countJSON, 0, len(kc)), make([]serviceCountJSON, 0, len(sc))}
	for _, c := range kc {
		out.Kinds = append(out.Kinds, countJSON{c.Name, c.Count})
	}
	for _, c := range sc {
		out.Services = append(out.Services, serviceCountJSON{c.Name, c.Count})
	}
	writeJSON(w, http.StatusOK, out)
}

type pinsJSON struct {
	Pins []svcconf.Pin `json:"pins"`
}

func (s *server) getPins(w http.ResponseWriter, r *http.Request) {
	svc, ok := readService(w, r)
	if !ok {
		return
	}
	pins, err := svcconf.LoadPins(s.svcDir(svc))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pinsJSON{pins})
}

// putPins replaces the service's pins and returns them as saved (sources
// normalised, duplicates dropped).
func (s *server) putPins(w http.ResponseWriter, r *http.Request) {
	svc, ok := saveService(w, r)
	if !ok {
		return
	}
	var body pinsJSON
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad pins: "+err.Error())
		return
	}
	dir := s.svcDir(svc)
	s.writeMu.Lock()
	err := svcconf.SavePins(dir, body.Pins)
	var pins []svcconf.Pin
	if err == nil {
		pins, err = svcconf.LoadPins(dir)
	}
	s.writeMu.Unlock()
	switch {
	case errors.Is(err, svcconf.ErrBadPin):
		writeError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, pinsJSON{pins})
	}
}

// effective returns the snapshot and the effective schema of stream k within
// service svc: the inferred schema (cached per snapshot version) with the
// service's saved field settings applied. For allServices it infers over
// every service and applies no saved settings. k.Source must already be
// parsed (record.ParseSource).
func (s *server) effective(svc string, k record.StreamKey) ([]record.Record, schema.Schema, error) {
	recs, version := s.o.Store.Snapshot()
	ck := schemaKey{svc, k.Source, k.Name}
	s.cacheMu.Lock()
	c, ok := s.cache[ck]
	s.cacheMu.Unlock()
	if !ok || c.version != version {
		c = cachedSchema{version, schema.Infer(svc, k.Source, k.Name, recs)}
		s.cacheMu.Lock()
		s.cache[ck] = c
		s.cacheMu.Unlock()
	}
	if svc == allServices {
		return recs, schema.Effective(c.s, nil), nil
	}
	saved, err := schema.LoadFields(s.svcDir(svc), k.Source)
	if err != nil {
		return nil, schema.Schema{}, err
	}
	return recs, schema.Effective(c.s, saved), nil
}

var allTypes = []schema.Type{schema.TInt, schema.TFloat, schema.TBool, schema.TString, schema.TDuration, schema.TTimestamp, schema.TJSON}

// sourceParam parses the optional ?source= (default log). On an unknown
// source it writes a 400 and returns false.
func sourceParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	v := r.URL.Query().Get("source")
	src, ok := record.ParseSource(v)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown source %q", v))
	}
	return src, ok
}

func (s *server) getSchema(w http.ResponseWriter, r *http.Request) {
	stream := r.URL.Query().Get("stream")
	if stream == "" {
		writeError(w, http.StatusBadRequest, "stream is required")
		return
	}
	src, ok := sourceParam(w, r)
	if !ok {
		return
	}
	_, sch, err := s.effective(serviceParam(r), record.StreamKey{Source: src, Name: stream})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ops := map[schema.Type][]string{}
	for _, t := range allTypes {
		ops[t] = query.OpsFor(t)
	}
	writeJSON(w, http.StatusOK, struct {
		Schema schema.Schema            `json:"schema"`
		Ops    map[schema.Type][]string `json:"ops"`
	}{sch, ops})
}

// putSchema merges the body's fields, by path, into the service's saved
// field settings for the source; other saved fields are kept. The stream is
// not needed: field settings apply to every kind of the source.
func (s *server) putSchema(w http.ResponseWriter, r *http.Request) {
	src, ok := sourceParam(w, r)
	if !ok {
		return
	}
	svc, ok := saveService(w, r)
	if !ok {
		return
	}
	var body struct {
		Fields []schema.Field `json:"fields"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad fields: "+err.Error())
		return
	}
	s.writeMu.Lock()
	path, err := schema.MergeFields(s.svcDir(svc), src, body.Fields)
	s.writeMu.Unlock()
	if err != nil {
		writeError(w, saveStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": path})
}

// saveStatus maps a MergeFields error to 500 for file-system failures and 400
// for everything else (unsavable stream, invalid field).
func saveStatus(err error) int {
	var pe *fs.PathError
	var le *os.LinkError
	var se *os.SyscallError
	if errors.As(err, &pe) || errors.As(err, &le) || errors.As(err, &se) {
		return http.StatusInternalServerError
	}
	return http.StatusBadRequest
}

func (s *server) query(w http.ResponseWriter, r *http.Request) {
	var q query.Query
	if err := decode(r, &q); err != nil {
		writeError(w, http.StatusBadRequest, "bad query: "+err.Error())
		return
	}
	if q.Stream == "" {
		writeError(w, http.StatusBadRequest, "stream is required")
		return
	}
	src, ok := record.ParseSource(q.Source)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown source %q", q.Source))
		return
	}
	q.Service = orDefault(q.Service)
	recs, sch, err := s.effective(q.Service, record.StreamKey{Source: src, Name: q.Stream})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := q.Validate(sch); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := query.Run(recs, sch, q, s.o.Now(), s.o.Store.Info().RunStart)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) record(w http.ResponseWriter, r *http.Request) {
	seq, err := strconv.Atoi(r.URL.Query().Get("seq"))
	if err != nil || seq < 1 {
		writeError(w, http.StatusBadRequest, "seq must be a positive integer")
		return
	}
	recs, _ := s.o.Store.Snapshot()
	var rec *record.Record
	if seq <= len(recs) && recs[seq-1].Seq == seq {
		rec = &recs[seq-1]
	} else {
		for i := range recs {
			if recs[i].Seq == seq {
				rec = &recs[i]
				break
			}
		}
	}
	if rec == nil {
		writeError(w, http.StatusNotFound, "no record with that seq")
		return
	}
	raw := string(rec.Raw)
	var buf bytes.Buffer
	if json.Indent(&buf, rec.Raw, "", "  ") == nil {
		raw = buf.String()
	}
	_, sch, err := s.effective(orDefault(rec.Service), rec.Key())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sql := map[string]string{}
	for _, f := range sch.Fields {
		if f.Visualizer != schema.VSQL {
			continue
		}
		v, ok := rec.Fields[f.Path].(string)
		if !ok {
			continue
		}
		if out, err := sqlfmt.Format(v); err == nil {
			sql[f.Path] = out
		} else {
			sql[f.Path] = v
		}
	}
	writeJSON(w, http.StatusOK, struct {
		Raw string            `json:"raw"`
		SQL map[string]string `json:"sql"`
	}{raw, sql})
}

func (s *server) listFavorites(w http.ResponseWriter, r *http.Request) {
	svc, ok := readService(w, r)
	if !ok {
		return
	}
	favs, err := favorites.Load(s.svcDir(svc))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, favs)
}

// putFavorite stores the body (a query.Query) under the path's name, in the
// ?service= favorites (the query's own service is not saved).
func (s *server) putFavorite(w http.ResponseWriter, r *http.Request) {
	svc, ok := saveService(w, r)
	if !ok {
		return
	}
	var q query.Query
	if err := decode(r, &q); err != nil {
		writeError(w, http.StatusBadRequest, "bad query: "+err.Error())
		return
	}
	f := favorites.Favorite{Name: r.PathValue("name"), Query: q}
	s.writeMu.Lock()
	err := favorites.Put(s.svcDir(svc), f, r.URL.Query().Get("overwrite") == "1")
	s.writeMu.Unlock()
	switch {
	case errors.Is(err, favorites.ErrExists):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, favorites.ErrBadName):
		writeError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, f)
	}
}

func (s *server) deleteFavorite(w http.ResponseWriter, r *http.Request) {
	svc, ok := saveService(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	s.writeMu.Lock()
	err := favorites.Delete(s.svcDir(svc), name)
	s.writeMu.Unlock()
	switch {
	case errors.Is(err, favorites.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]string{"deleted": name})
	}
}
