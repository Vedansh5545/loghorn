package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/store"
)

type fixture struct {
	srv  *httptest.Server
	repo string // temp repo root holding .git
}

func newFixture(t *testing.T, files ...string) *fixture {
	t.Helper()
	st := store.New(store.ModeHistorical, time.Time{})
	if err := store.Preload(st, files, config.Input{}); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 17, 18, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(New(Options{Store: st, StartDir: repo, Now: func() time.Time { return now }}))
	t.Cleanup(srv.Close)
	return &fixture{srv: srv, repo: repo}
}

const pino = "../../testdata/web/pino.log"

// do sends a request; a write method gets the server's own Origin unless
// origin is given ("-" means no Origin header).
func (f *fixture) do(t *testing.T, method, path, body, origin string) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, f.srv.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case origin == "-":
	case origin != "":
		req.Header.Set("Origin", origin)
	case method != http.MethodGet:
		req.Header.Set("Origin", f.srv.URL)
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (f *fixture) getJSON(t *testing.T, path string, v any) {
	t.Helper()
	code, b := f.do(t, http.MethodGet, path, "", "")
	if code != 200 {
		t.Fatalf("GET %s = %d %s", path, code, b)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("GET %s: %v in %s", path, err, b)
	}
}

func wantError(t *testing.T, code, want int, b []byte) {
	t.Helper()
	if code != want {
		t.Fatalf("status = %d %s; want %d", code, b, want)
	}
	var e struct{ Error string }
	if err := json.Unmarshal(b, &e); err != nil || e.Error == "" {
		t.Fatalf("body %s is not {\"error\": ...}", b)
	}
}

func TestStreams(t *testing.T) {
	f := newFixture(t, pino)
	var got []struct {
		Name  string
		Count int
	}
	f.getJSON(t, "/api/streams", &got)
	names := map[string]int{}
	for _, s := range got {
		names[s.Name] = s.Count
	}
	if names["db"] != 3 || names["api"] != 2 || names["server"] == 0 {
		t.Fatalf("streams = %+v", got)
	}
}

type schemaResp struct {
	Schema struct {
		Stream  string
		Columns []string
		Fields  []struct {
			Path, Type, Visualizer, Inferred string
			Count                            int
			New, Saved                       bool
		}
	}
	Ops map[string][]string
}

func TestSchemaInferred(t *testing.T) {
	f := newFixture(t, pino)
	var got schemaResp
	f.getJSON(t, "/api/schema?stream=db", &got)
	if got.Schema.Stream != "db" || len(got.Schema.Columns) == 0 {
		t.Fatalf("schema = %+v", got.Schema)
	}
	types := map[string]string{}
	for _, fl := range got.Schema.Fields {
		types[fl.Path] = fl.Type
	}
	if types["latency"] != "duration" || types["databaseContext.metadata.rowsAffected"] != "int" {
		t.Fatalf("types = %v", types)
	}
	if len(got.Ops["duration"]) == 0 || len(got.Ops["string"]) == 0 || len(got.Ops["bool"]) == 0 {
		t.Fatalf("ops = %v", got.Ops)
	}
	// Cached per snapshot version: a second call returns the same schema.
	var again schemaResp
	f.getJSON(t, "/api/schema?stream=db", &again)
	if len(again.Schema.Fields) != len(got.Schema.Fields) {
		t.Fatalf("second call differs")
	}
	code, b := f.do(t, http.MethodGet, "/api/schema", "", "")
	wantError(t, code, 400, b)
}

func TestQueryEndToEnd(t *testing.T) {
	f := newFixture(t, pino)
	q := `{"stream":"db","range":{"from":"all"},"filters":[{"field":"latency","op":">","values":["1ms"]}]}`
	code, b := f.do(t, http.MethodPost, "/api/query", q, "")
	if code != 200 {
		t.Fatalf("query = %d %s", code, b)
	}
	var res struct {
		Matched     int
		StreamTotal int
		Rows        []struct{ Seq int }
	}
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	if res.Matched != 2 || res.StreamTotal != 3 || len(res.Rows) != 2 {
		t.Fatalf("result = %s", b)
	}
	// curl-style: no Origin header is allowed for POST /api/query only.
	if code, b := f.do(t, http.MethodPost, "/api/query", q, "-"); code != 200 {
		t.Fatalf("query without Origin = %d %s", code, b)
	}
	code, b = f.do(t, http.MethodPost, "/api/query", q, "http://evil.example")
	wantError(t, code, 403, b)
}

func TestQueryTableAndGraphOverHTTP(t *testing.T) {
	f := newFixture(t, pino)
	for _, view := range []string{"table", "graph"} {
		q := `{"stream":"db","view":"` + view + `","range":{"from":"all"},"groupBy":["level"]}`
		code, b := f.do(t, http.MethodPost, "/api/query", q, "")
		if code != 200 {
			t.Fatalf("%s query = %d %s", view, code, b)
		}
		var res struct {
			View  string
			Rows  []json.RawMessage
			Graph *struct{ Series []json.RawMessage }
		}
		if err := json.Unmarshal(b, &res); err != nil || res.View != view {
			t.Fatalf("%s result = %s (%v)", view, b, err)
		}
		if view == "table" && len(res.Rows) == 0 {
			t.Errorf("table has no rows: %s", b)
		}
		if view == "graph" && (res.Graph == nil || len(res.Graph.Series) == 0) {
			t.Errorf("graph has no series: %s", b)
		}
	}
}

func TestQueryValidation400(t *testing.T) {
	f := newFixture(t, pino)
	for _, body := range []string{
		`{"stream":"db","filters":[{"field":"latency","op":"contains","values":["x"]}]}`,
		`{"stream":"db","filters":[{"field":"latency","op":">","values":["soon"]}]}`,
		`{"stream":"db","range":{"from":"yesterday-ish"}}`,
		`{not json`,
	} {
		code, b := f.do(t, http.MethodPost, "/api/query", body, "")
		wantError(t, code, 400, b)
	}
}

func TestSchemaPutWritesFile(t *testing.T) {
	f := newFixture(t, pino)
	body := `{"stream":"db","columns":["latency"],"fields":[{"path":"latency","type":"duration","unit":"ms","visualizer":"duration"}]}`
	code, b := f.do(t, http.MethodPut, "/api/schema?stream=db", body, "")
	if code != 200 {
		t.Fatalf("PUT = %d %s", code, b)
	}
	want := filepath.Join(f.repo, ".config", "loghorn", "fields-log.toml")
	var res struct{ Path string }
	if err := json.Unmarshal(b, &res); err != nil || res.Path != want {
		t.Fatalf("response %s; want path %s", b, want)
	}
	data, err := os.ReadFile(want)
	if err != nil || !strings.Contains(string(data), "latency") {
		t.Fatalf("file: %v %s", err, data)
	}
	var got schemaResp
	f.getJSON(t, "/api/schema?stream=db", &got)
	for _, fl := range got.Schema.Fields {
		if fl.Path == "latency" && !fl.Saved {
			t.Fatalf("saved field not marked saved: %+v", fl)
		}
	}
	var sess struct {
		ConfigDir   string
		ConfigFound bool
	}
	f.getJSON(t, "/api/session", &sess)
	if !sess.ConfigFound || sess.ConfigDir != filepath.Join(f.repo, ".config", "loghorn") {
		t.Fatalf("session = %+v", sess)
	}
}

func TestSchemaPutBadStream400(t *testing.T) {
	f := newFixture(t, pino)
	cases := []struct{ stream, body string }{
		{"", `{"fields":[]}`},
		{"db", `{"stream":"api","fields":[]}`}, // body names another stream
		{"db", `{"fields":[{"path":"latency","type":"weird","visualizer":"text"}]}`},        // validation
		{"db", `{"fields":[{"path":"latency","type":"int","unit":"ms","visualizer":"x"}]}`}, // validation
		{"db", `{"fields":`},
	}
	for _, c := range cases {
		code, b := f.do(t, http.MethodPut, "/api/schema?stream="+c.stream, c.body, "")
		wantError(t, code, 400, b)
	}
	if _, err := os.Stat(filepath.Join(f.repo, ".config")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused PUT created .config: %v", err)
	}
}

func TestRecordSQLFormatted(t *testing.T) {
	f := newFixture(t, pino)
	sdir := filepath.Join(f.repo, ".config", "loghorn")
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := "[[field]]\npath = \"databaseContext.metadata.query\"\ntype = \"string\"\nvisualizer = \"sql\"\n"
	if err := os.WriteFile(filepath.Join(sdir, "fields-log.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Raw string
		SQL map[string]string
	}
	f.getJSON(t, "/api/record?seq=2", &got)
	if !strings.Contains(got.Raw, "\n  \"severity\"") {
		t.Fatalf("raw not pretty-printed: %q", got.Raw)
	}
	sql := got.SQL["databaseContext.metadata.query"]
	if !strings.Contains(sql, "\n") || !strings.Contains(strings.ToLower(sql), "from") {
		t.Fatalf("sql not formatted: %q (all %v)", sql, got.SQL)
	}
	// Big ints keep their digits in raw.
	f.getJSON(t, "/api/record?seq=4", &got)
	if !strings.Contains(got.Raw, "1763400000123456789") {
		t.Fatalf("raw lost big int: %s", got.Raw)
	}
	code, b := f.do(t, http.MethodGet, "/api/record?seq=999", "", "")
	wantError(t, code, 404, b)
	code, b = f.do(t, http.MethodGet, "/api/record?seq=x", "", "")
	wantError(t, code, 400, b)
}

func TestForeignHost403(t *testing.T) {
	f := newFixture(t, pino)
	port := f.srv.Listener.Addr().(interface{ String() string }).String()
	port = port[strings.LastIndex(port, ":")+1:]
	for _, host := range []string{"evil.example:" + port, "evil.example", "127.0.0.1:1", "localhost.evil.example:" + port, "127.0.0.1"} {
		for _, path := range []string{"/", "/api/streams", "/api/session", "/api/favorites", "/static/index.html"} {
			req, _ := http.NewRequest(http.MethodGet, f.srv.URL+path, nil)
			req.Host = host
			resp, err := f.srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 403 {
				t.Fatalf("Host %s GET %s = %d %s; want 403", host, path, resp.StatusCode, b)
			}
		}
	}
	// localhost:<port> is the server's own name too.
	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/api/streams", nil)
	req.Host = "localhost:" + port
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("localhost Host = %d", resp.StatusCode)
	}
}

// TestForeignHostWrites403 pins the Host guard on the write routes: a foreign
// Host is refused even when the Origin matches the server or the Host.
func TestForeignHostWrites403(t *testing.T) {
	f := newFixture(t, pino)
	port := f.srv.Listener.Addr().String()
	port = port[strings.LastIndex(port, ":")+1:]
	reqs := []struct{ method, path, body string }{
		{http.MethodPut, "/api/schema?stream=db", `{"stream":"db","fields":[]}`},
		{http.MethodPut, "/api/favorites/x", `{"stream":"db"}`},
		{http.MethodDelete, "/api/favorites/x", ""},
		{http.MethodPost, "/api/query", `{"stream":"db"}`},
	}
	for _, host := range []string{"evil.example:" + port, "evil.example", "127.0.0.1:1"} {
		for _, origin := range []string{"", f.srv.URL, "http://" + host} {
			for _, rq := range reqs {
				req, _ := http.NewRequest(rq.method, f.srv.URL+rq.path, strings.NewReader(rq.body))
				req.Host = host
				if origin != "" {
					req.Header.Set("Origin", origin)
				}
				resp, err := f.srv.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 403 {
					t.Fatalf("Host %s Origin %q %s %s = %d %s; want 403", host, origin, rq.method, rq.path, resp.StatusCode, b)
				}
			}
		}
	}
	if _, err := os.Stat(filepath.Join(f.repo, ".config")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused write created .config: %v", err)
	}
}

func TestForeignOrigin403(t *testing.T) {
	f := newFixture(t, pino)
	body := `{"stream":"db","fields":[]}`
	for _, o := range []string{"http://evil.example", "null", "https" + strings.TrimPrefix(f.srv.URL, "http")} {
		code, b := f.do(t, http.MethodPut, "/api/schema?stream=db", body, o)
		wantError(t, code, 403, b)
		code, b = f.do(t, http.MethodPut, "/api/favorites/x", `{"stream":"db"}`, o)
		wantError(t, code, 403, b)
		code, b = f.do(t, http.MethodDelete, "/api/favorites/x", "", o)
		wantError(t, code, 403, b)
	}
	if _, err := os.Stat(filepath.Join(f.repo, ".config")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused write created .config: %v", err)
	}
}

func TestMissingOriginPut403(t *testing.T) {
	f := newFixture(t, pino)
	code, b := f.do(t, http.MethodPut, "/api/schema?stream=db", `{"stream":"db","fields":[]}`, "-")
	wantError(t, code, 403, b)
	code, b = f.do(t, http.MethodPut, "/api/favorites/x", `{"stream":"db"}`, "-")
	wantError(t, code, 403, b)
	code, b = f.do(t, http.MethodDelete, "/api/favorites/x", "", "-")
	wantError(t, code, 403, b)
	if _, err := os.Stat(filepath.Join(f.repo, ".config")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused write created .config: %v", err)
	}
}

func TestFavoritesRoutes(t *testing.T) {
	f := newFixture(t, pino)
	var favs []struct {
		Name  string
		Query struct{ Stream string }
	}
	f.getJSON(t, "/api/favorites", &favs)
	if favs == nil || len(favs) != 0 {
		t.Fatalf("empty favorites = %#v", favs)
	}
	name := url.PathEscape("slow db / errors")
	if code, b := f.do(t, http.MethodPut, "/api/favorites/"+name, `{"stream":"db","search":"x"}`, ""); code != 200 {
		t.Fatalf("PUT = %d %s", code, b)
	}
	code, b := f.do(t, http.MethodPut, "/api/favorites/"+name, `{"stream":"api"}`, "")
	wantError(t, code, 409, b)
	if code, b := f.do(t, http.MethodPut, "/api/favorites/"+name+"?overwrite=1", `{"stream":"api"}`, ""); code != 200 {
		t.Fatalf("overwrite = %d %s", code, b)
	}
	f.getJSON(t, "/api/favorites", &favs)
	if len(favs) != 1 || favs[0].Name != "slow db / errors" || favs[0].Query.Stream != "api" {
		t.Fatalf("favorites = %+v", favs)
	}
	if _, err := os.Stat(filepath.Join(f.repo, ".config", "loghorn", "queries.toml")); err != nil {
		t.Fatal(err)
	}
	code, b = f.do(t, http.MethodPut, "/api/favorites/"+strings.Repeat("n", 101), `{}`, "")
	wantError(t, code, 400, b)
	if code, b := f.do(t, http.MethodDelete, "/api/favorites/"+name, "", ""); code != 200 {
		t.Fatalf("DELETE = %d %s", code, b)
	}
	code, b = f.do(t, http.MethodDelete, "/api/favorites/"+name, "", "")
	wantError(t, code, 404, b)
}

func TestEmptyStore(t *testing.T) {
	f := newFixture(t)
	code, b := f.do(t, http.MethodGet, "/api/streams", "", "")
	if code != 200 || strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("streams = %d %s; want []", code, b)
	}
	var sess struct {
		Mode        string
		Count       int
		RunStart    *string
		ConfigFound bool
	}
	f.getJSON(t, "/api/session", &sess)
	if sess.Count != 0 || sess.Mode != "historical" || sess.RunStart != nil || sess.ConfigFound {
		t.Fatalf("session = %+v", sess)
	}
}

func TestIndexServed(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{"/", "/static/index.html"} {
		code, b := f.do(t, http.MethodGet, p, "", "")
		if code != 200 || !bytes.Contains(b, []byte("<title>loghorn</title>")) {
			t.Fatalf("GET %s = %d %.200s", p, code, b)
		}
	}
	code, _ := f.do(t, http.MethodGet, "/nope", "", "")
	if code != 404 {
		t.Fatalf("GET /nope = %d; want 404", code)
	}
}

func TestListenLoopback(t *testing.T) {
	l, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if !strings.HasPrefix(l.Addr().String(), "127.0.0.1:") {
		t.Fatalf("addr = %s", l.Addr())
	}
}

func TestStaticAssetsServed(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct{ path, ctype, want string }{
		{"/static/app.js", "javascript", "function visualize("},
		{"/static/app.css", "text/css", "prefers-color-scheme"},
	} {
		resp, err := f.srv.Client().Get(f.srv.URL + c.path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s = %d", c.path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, c.ctype) {
			t.Fatalf("GET %s Content-Type = %q; want %s", c.path, ct, c.ctype)
		}
		if !bytes.Contains(b, []byte(c.want)) {
			t.Fatalf("GET %s body lacks %q", c.path, c.want)
		}
	}
}

const spans = "../../testdata/web/spans.jsonl"

func TestSpanAndLogStreamsApart(t *testing.T) {
	f := newFixture(t, pino, spans)
	var got []struct {
		Name, Source string
		Count        int
	}
	f.getJSON(t, "/api/streams", &got)
	have := map[string]bool{}
	for _, s := range got {
		have[fmt.Sprintf("%s %s %d", s.Name, s.Source, s.Count)] = true
	}
	for _, w := range []string{"db log 3", "db otel 2", "gen_ai otel 1", "db open otel 1", "job.run otel 1"} {
		if !have[w] {
			t.Errorf("streams missing {%s}: %+v", w, got)
		}
	}

	type result struct {
		Matched, StreamTotal int
		Columns              []struct{ Path string }
	}
	q := `{"stream":"db","source":"otel","range":{"from":"all"},"columns":["db.query.text","duration_ms"]}`
	code, b := f.do(t, http.MethodPost, "/api/query", q, "")
	if code != 200 {
		t.Fatalf("otel query = %d %s", code, b)
	}
	var res result
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	hasCol := false
	for _, c := range res.Columns {
		hasCol = hasCol || c.Path == "db.query.text"
	}
	if res.Matched != 2 || res.StreamTotal != 2 || !hasCol {
		t.Fatalf("otel result = %s", b)
	}
	code, b = f.do(t, http.MethodPost, "/api/query", `{"stream":"db","range":{"from":"all"}}`, "")
	res = result{}
	if code != 200 || json.Unmarshal(b, &res) != nil || res.StreamTotal != 3 {
		t.Fatalf("log query = %d %s", code, b)
	}
	code, b = f.do(t, http.MethodPost, "/api/query", `{"stream":"db","source":"bogus","range":{"from":"all"}}`, "")
	wantError(t, code, 400, b)

	var sch struct {
		Schema struct {
			Source string
			Fields []struct{ Path string }
		}
	}
	f.getJSON(t, "/api/schema?stream=db&source=otel", &sch)
	hasField := false
	for _, fl := range sch.Schema.Fields {
		hasField = hasField || fl.Path == "db.query.text"
	}
	if !hasField || sch.Schema.Source != "otel" {
		t.Fatalf("otel schema = %+v", sch)
	}
	code, b = f.do(t, http.MethodGet, "/api/schema?stream=db&source=bogus", "", "")
	wantError(t, code, 400, b)
}

func TestSchemaPutOtelWritesOtelDir(t *testing.T) {
	f := newFixture(t, pino, spans)
	code, b := f.do(t, http.MethodGet, "/api/schema?stream=db&source=otel", "", "")
	if code != 200 {
		t.Fatalf("GET = %d %s", code, b)
	}
	var resp struct{ Schema json.RawMessage }
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatal(err)
	}
	body := string(resp.Schema)
	code, b = f.do(t, http.MethodPut, "/api/schema?stream=db&source=otel", body, "")
	if code != 200 {
		t.Fatalf("PUT = %d %s", code, b)
	}
	var res struct{ Path string }
	if err := json.Unmarshal(b, &res); err != nil || !strings.HasSuffix(res.Path, "fields-otel.toml") {
		t.Fatalf("response %s", b)
	}
	if _, err := os.Stat(filepath.Join(f.repo, ".config", "loghorn", "fields-log.toml")); !os.IsNotExist(err) {
		t.Fatalf("log schema file written: %v", err)
	}
	var got schemaResp
	f.getJSON(t, "/api/schema?stream=db&source=otel", &got)
	saved := 0
	for _, fl := range got.Schema.Fields {
		if fl.Saved {
			saved++
		}
	}
	if saved == 0 {
		t.Fatalf("otel fields not saved: %+v", got.Schema.Fields)
	}
	got = schemaResp{}
	f.getJSON(t, "/api/schema?stream=db", &got)
	for _, fl := range got.Schema.Fields {
		if fl.Saved {
			t.Fatalf("log field marked saved: %+v", fl)
		}
	}
	code, b = f.do(t, http.MethodPut, "/api/schema?stream=db&source=otel",
		`{"stream":"db","source":"log","columns":[],"fields":[]}`, "")
	wantError(t, code, 400, b)
	code, b = f.do(t, http.MethodPut, "/api/schema?stream=db&source=bogus", body, "")
	wantError(t, code, 400, b)
}
