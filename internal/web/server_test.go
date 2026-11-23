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
	"github.com/jimbeveridge/loghorn/internal/query"
	"github.com/jimbeveridge/loghorn/internal/store"
	"github.com/jimbeveridge/loghorn/internal/svcconf"
)

type fixture struct {
	srv  *httptest.Server
	base string // temp config base (<base>/services/<key>/)
}

func newFixture(t *testing.T, files ...string) *fixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	st := store.New(store.ModeHistorical, time.Time{})
	if err := store.Preload(st, files, config.Input{}); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	now := time.Date(2026, 11, 17, 18, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(New(Options{Store: st, ConfigBase: base, Now: func() time.Time { return now }}))
	t.Cleanup(srv.Close)
	return &fixture{srv: srv, base: base}
}

// svcDir is the config dir of service key under the fixture's base.
func (f *fixture) svcDir(key string) string { return svcconf.Dir(f.base, key) }

// noServices fails when a refused write created any service config.
func (f *fixture) noServices(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(f.base, "services")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused write created config: %v", err)
	}
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
			Saved                            bool
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
	body := `{"fields":[{"path":"latency","type":"duration","unit":"ms","visualizer":"duration"}]}`
	code, b := f.do(t, http.MethodPut, "/api/schema?stream=db", body, "")
	if code != 200 {
		t.Fatalf("PUT = %d %s", code, b)
	}
	want := filepath.Join(f.svcDir("default"), "fields-log.toml")
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
	var sess struct{ ConfigBase string }
	f.getJSON(t, "/api/session", &sess)
	if sess.ConfigBase != f.base {
		t.Fatalf("session = %+v", sess)
	}
}

func TestSchemaPutInvalid400(t *testing.T) {
	f := newFixture(t, pino)
	for _, body := range []string{
		`{"fields":[{"path":"latency","type":"weird","visualizer":"text"}]}`,
		`{"fields":[{"path":"latency","type":"int","unit":"ms","visualizer":"x"}]}`,
		`{"fields":`,
	} {
		code, b := f.do(t, http.MethodPut, "/api/schema", body, "")
		wantError(t, code, 400, b)
	}
	f.noServices(t)
}

func TestRecordSQLFormatted(t *testing.T) {
	f := newFixture(t, pino)
	sdir := f.svcDir("default")
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := "[[field]]\npath = \"databaseContext.metadata.query\"\ntype = \"string\"\nvisualizer = \"sql\"\n"
	if err := os.WriteFile(filepath.Join(sdir, "fields-log.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Raw       string
		SQL       map[string]string
		SQLTokens map[string][]struct{ C, T string }
		RawTokens []struct{ C, T string }
	}
	f.getJSON(t, "/api/record?seq=2", &got)
	if !strings.Contains(got.Raw, "\n  \"severity\"") {
		t.Fatalf("raw not pretty-printed: %q", got.Raw)
	}
	var rawBack strings.Builder
	keys := 0
	for _, tk := range got.RawTokens {
		rawBack.WriteString(tk.T)
		if tk.C == "key" {
			keys++
		}
	}
	if rawBack.String() != got.Raw || keys == 0 {
		t.Fatalf("raw tokens (%d keys) do not colour the raw record", keys)
	}
	sql := got.SQL["databaseContext.metadata.query"]
	if !strings.Contains(sql, "\n") || !strings.Contains(strings.ToLower(sql), "from") {
		t.Fatalf("sql not formatted: %q (all %v)", sql, got.SQL)
	}
	var rebuilt strings.Builder
	kw := false
	for _, tk := range got.SQLTokens["databaseContext.metadata.query"] {
		rebuilt.WriteString(tk.T)
		kw = kw || tk.C == "kw"
	}
	if rebuilt.String() != sql || !kw {
		t.Fatalf("sql tokens %v do not colour %q", got.SQLTokens, sql)
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
	f.noServices(t)
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
	f.noServices(t)
}

func TestMissingOriginPut403(t *testing.T) {
	f := newFixture(t, pino)
	code, b := f.do(t, http.MethodPut, "/api/schema?stream=db", `{"stream":"db","fields":[]}`, "-")
	wantError(t, code, 403, b)
	code, b = f.do(t, http.MethodPut, "/api/favorites/x", `{"stream":"db"}`, "-")
	wantError(t, code, 403, b)
	code, b = f.do(t, http.MethodDelete, "/api/favorites/x", "", "-")
	wantError(t, code, 403, b)
	f.noServices(t)
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
	if _, err := os.Stat(filepath.Join(f.svcDir("default"), "queries.toml")); err != nil {
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
		Mode       string
		Count      int
		RunStart   *string
		ConfigBase string
	}
	f.getJSON(t, "/api/session", &sess)
	if sess.Count != 0 || sess.Mode != "historical" || sess.RunStart != nil || sess.ConfigBase != f.base {
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
	q := `{"stream":"db","source":"otel","service":"demo-svc","range":{"from":"all"},"columns":["db.query.text","duration_ms"]}`
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
	f.getJSON(t, "/api/schema?stream=db&source=otel&service=demo-svc", &sch)
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
	code, b := f.do(t, http.MethodGet, "/api/schema?stream=db&source=otel&service=demo-svc", "", "")
	if code != 200 {
		t.Fatalf("GET = %d %s", code, b)
	}
	var resp struct{ Schema json.RawMessage }
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatal(err)
	}
	body := string(resp.Schema)
	code, b = f.do(t, http.MethodPut, "/api/schema?stream=db&source=otel&service=demo-svc", body, "")
	if code != 200 {
		t.Fatalf("PUT = %d %s", code, b)
	}
	var res struct{ Path string }
	if err := json.Unmarshal(b, &res); err != nil || !strings.HasSuffix(res.Path, "fields-otel.toml") {
		t.Fatalf("response %s", b)
	}
	if _, err := os.Stat(filepath.Join(f.svcDir("demo-svc"), "fields-log.toml")); !os.IsNotExist(err) {
		t.Fatalf("log schema file written: %v", err)
	}
	var got schemaResp
	f.getJSON(t, "/api/schema?stream=db&source=otel&service=demo-svc", &got)
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
	code, b = f.do(t, http.MethodPut, "/api/schema?stream=db&source=bogus", body, "")
	wantError(t, code, 400, b)
}

const services = "../../testdata/web/services.jsonl"

type kindsResp struct {
	Kinds []struct {
		Name  string
		Count int
	}
	Services []struct {
		Key   string
		Count int
	}
}

func (k kindsResp) count(name string) (int, bool) {
	for _, c := range k.Kinds {
		if c.Name == name {
			return c.Count, true
		}
	}
	return 0, false
}

func TestKindsFollowRange(t *testing.T) {
	f := newFixture(t, services)
	var all kindsResp
	f.getJSON(t, "/api/kinds?source=otel&service=shop-api&from=all", &all)
	if n, _ := all.count("sweep orders"); n != 12 {
		t.Errorf("sweep orders = %d; want 12 (%+v)", n, all.Kinds)
	}
	if n, _ := all.count("db"); n != 6 {
		t.Errorf("db = %d; want 6 (%+v)", n, all.Kinds)
	}
	if n, _ := all.count("http.server"); n != 4 {
		t.Errorf("http.server = %d; want 4 (%+v)", n, all.Kinds)
	}
	if len(all.Services) != 2 || all.Services[0].Key != "shop-api" || all.Services[0].Count != 88 ||
		all.Services[1].Key != "mail" || all.Services[1].Count != 5 {
		t.Errorf("services = %+v", all.Services)
	}
	var early kindsResp
	f.getJSON(t, "/api/kinds?source=otel&service=shop-api&from=2026-11-17T16:00:00Z&to=2026-11-17T16:30:00Z", &early)
	if _, ok := early.count("db"); ok {
		t.Errorf("db before 16:30: %+v", early.Kinds)
	}
	if n, _ := early.count("sweep orders"); n == 0 || n >= 12 {
		t.Errorf("sweep orders before 16:30 = %d; want some, not all", n)
	}
	var logs kindsResp
	f.getJSON(t, "/api/kinds?source=log&from=all", &logs)
	if n, _ := logs.count("db"); n != 3 || len(logs.Kinds) != 1 {
		t.Errorf("default log kinds = %+v", logs.Kinds)
	}
	var every kindsResp
	f.getJSON(t, "/api/kinds?source=otel&service=*&from=all", &every)
	if n, _ := every.count("send email"); n != 12 {
		t.Errorf("send email over all services = %d; want 12", n)
	}
}

func TestKindsUnknownServiceEmpty(t *testing.T) {
	f := newFixture(t, services)
	code, b := f.do(t, http.MethodGet, "/api/kinds?source=otel&service=nobody&from=all", "", "")
	if code != 200 || !strings.Contains(string(b), `"kinds":[]`) {
		t.Fatalf("kinds = %d %s", code, b)
	}
	var got kindsResp
	if err := json.Unmarshal(b, &got); err != nil || len(got.Services) != 2 {
		t.Fatalf("services = %s (%v)", b, err)
	}
}

func TestKindsBadRange400(t *testing.T) {
	f := newFixture(t, services)
	for _, q := range []string{"from=yesterday", "to=later", "source=bogus"} {
		code, b := f.do(t, http.MethodGet, "/api/kinds?service=shop-api&"+q, "", "")
		wantError(t, code, 400, b)
	}
}

func TestPinsRoundTrip(t *testing.T) {
	f := newFixture(t, services)
	var got struct {
		Pins []struct{ Source, Kind string }
	}
	f.getJSON(t, "/api/pins?service=shop-api", &got)
	if got.Pins == nil || len(got.Pins) != 0 {
		t.Fatalf("empty pins = %#v", got.Pins)
	}
	code, b := f.do(t, http.MethodPut, "/api/pins?service=shop-api", `{"pins":[{"source":"otel","kind":"db"},{"source":"otel","kind":"db"}]}`, "")
	if code != 200 || json.Unmarshal(b, &got) != nil || len(got.Pins) != 1 || got.Pins[0].Kind != "db" {
		t.Fatalf("PUT = %d %s", code, b)
	}
	got.Pins = nil
	f.getJSON(t, "/api/pins?service=shop-api", &got)
	if len(got.Pins) != 1 || got.Pins[0].Source != "otel" || got.Pins[0].Kind != "db" {
		t.Fatalf("pins = %+v", got.Pins)
	}
	if _, err := os.Stat(filepath.Join(f.base, "services", "shop-api", "pins.toml")); err != nil {
		t.Fatal(err)
	}
	code, b = f.do(t, http.MethodPut, "/api/pins?service=*", `{"pins":[]}`, "")
	wantError(t, code, 400, b)
	if !strings.Contains(string(b), "Choose a service to save") {
		t.Errorf("PUT * = %s", b)
	}
	code, b = f.do(t, http.MethodGet, "/api/pins?service=*", "", "")
	wantError(t, code, 400, b)
	code, b = f.do(t, http.MethodPut, "/api/pins?service=mail", `{"pins":[{"source":"bogus","kind":"db"}]}`, "")
	wantError(t, code, 400, b)
	code, b = f.do(t, http.MethodPut, "/api/pins?service=mail", `{"pins":`, "")
	wantError(t, code, 400, b)
	if _, err := os.Stat(f.svcDir("mail")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused pin PUT created config: %v", err)
	}
}

func TestQueryServiceFilter(t *testing.T) {
	f := newFixture(t, services)
	matched := func(body string) int {
		t.Helper()
		code, b := f.do(t, http.MethodPost, "/api/query", body, "")
		var res struct{ Matched int }
		if code != 200 || json.Unmarshal(b, &res) != nil {
			t.Fatalf("query %s = %d %s", body, code, b)
		}
		return res.Matched
	}
	if n := matched(`{"stream":"send email","source":"otel","service":"mail","range":{"from":"all"}}`); n != 3 {
		t.Errorf("mail send email = %d; want 3", n)
	}
	if n := matched(`{"stream":"send email","source":"otel","service":"*","range":{"from":"all"}}`); n != 12 {
		t.Errorf("all send email = %d; want 12", n)
	}
	if n := matched(`{"stream":"db","range":{"from":"all"}}`); n != 3 {
		t.Errorf("default log db = %d; want the 3 pino lines", n)
	}
	if n := matched(`{"stream":"db","service":"nobody","range":{"from":"all"}}`); n != 0 {
		t.Errorf("unknown service db = %d; want 0", n)
	}
}

func TestSchemaPutMergesBySource(t *testing.T) {
	f := newFixture(t, services)
	body := `{"fields":[{"path":"duration_ms","type":"duration","visualizer":"duration","unit":"ms"}]}`
	code, b := f.do(t, http.MethodPut, "/api/schema?source=otel&service=shop-api", body, "")
	if code != 200 {
		t.Fatalf("PUT = %d %s", code, b)
	}
	var res struct{ Path string }
	want := filepath.Join(f.svcDir("shop-api"), "fields-otel.toml")
	if json.Unmarshal(b, &res) != nil || res.Path != want {
		t.Fatalf("response %s; want %s", b, want)
	}
	for _, kind := range []string{"db", url.QueryEscape("sweep orders")} {
		var got schemaResp
		f.getJSON(t, "/api/schema?stream="+kind+"&source=otel&service=shop-api", &got)
		ok := false
		for _, fl := range got.Schema.Fields {
			ok = ok || (fl.Path == "duration_ms" && fl.Saved && fl.Type == "duration")
		}
		if !ok {
			t.Errorf("%s: duration_ms not saved: %+v", kind, got.Schema.Fields)
		}
	}
	// Another service and the all-services view apply nothing.
	for _, svc := range []string{"mail", url.QueryEscape("*")} {
		var got schemaResp
		f.getJSON(t, "/api/schema?stream="+url.QueryEscape("send email")+"&source=otel&service="+svc, &got)
		for _, fl := range got.Schema.Fields {
			if fl.Saved {
				t.Errorf("%s: field saved: %+v", svc, fl)
			}
		}
	}
	data, err := os.ReadFile(want)
	if err != nil || strings.Count(string(data), "[[field]]") != 1 {
		t.Fatalf("file: %v\n%s", err, data)
	}
	code, b = f.do(t, http.MethodPut, "/api/schema?source=otel&service=*", body, "")
	wantError(t, code, 400, b)
	if !strings.Contains(string(b), "Choose a service to save") {
		t.Errorf("PUT * = %s", b)
	}
	code, b = f.do(t, http.MethodPut, "/api/schema?source=bogus&service=shop-api", body, "")
	wantError(t, code, 400, b)
}

func TestSchemaPutConcurrentMerge(t *testing.T) {
	f := newFixture(t, services)
	const n = 20
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			body := fmt.Sprintf(`{"fields":[{"path":"f%02d","type":"string","visualizer":"plain"}]}`, i)
			req, _ := http.NewRequest(http.MethodPut, f.srv.URL+"/api/schema?source=otel&service=shop-api", strings.NewReader(body))
			req.Header.Set("Origin", f.srv.URL)
			resp, err := f.srv.Client().Do(req)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode != 200 {
					err = fmt.Errorf("PUT f%02d = %d", i, resp.StatusCode)
				}
			}
			errs <- err
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(f.svcDir("shop-api"), "fields-otel.toml"))
	if err != nil || strings.Count(string(data), "[[field]]") != n {
		t.Fatalf("file: %v\n%s", err, data)
	}
}

func TestFavoritesPerService(t *testing.T) {
	f := newFixture(t, services)
	if code, b := f.do(t, http.MethodPut, "/api/favorites/x?service=shop-api", `{"stream":"db","source":"otel","service":"shop-api"}`, ""); code != 200 {
		t.Fatalf("PUT = %d %s", code, b)
	}
	var favs []struct{ Name string }
	f.getJSON(t, "/api/favorites?service=mail", &favs)
	if favs == nil || len(favs) != 0 {
		t.Fatalf("mail favorites = %#v", favs)
	}
	f.getJSON(t, "/api/favorites?service=shop-api", &favs)
	if len(favs) != 1 || favs[0].Name != "x" {
		t.Fatalf("shop-api favorites = %+v", favs)
	}
	data, err := os.ReadFile(filepath.Join(f.svcDir("shop-api"), "queries.toml"))
	if err != nil || strings.Contains(string(data), "service") {
		t.Fatalf("queries.toml: %v\n%s", err, data)
	}
	code, b := f.do(t, http.MethodGet, "/api/favorites?service=*", "", "")
	wantError(t, code, 400, b)
	for _, m := range []string{http.MethodPut, http.MethodDelete} {
		code, b := f.do(t, m, "/api/favorites/x?service=*", `{"stream":"db"}`, "")
		wantError(t, code, 400, b)
		if !strings.Contains(string(b), "Choose a service to save") {
			t.Errorf("%s * = %s", m, b)
		}
	}
	if code, b := f.do(t, http.MethodDelete, "/api/favorites/x?service=shop-api", "", ""); code != 200 {
		t.Fatalf("DELETE = %d %s", code, b)
	}
}

const traceFile = "../../testdata/web/trace.jsonl"

const traceT1 = "4bf92f3577b34da6a3ce929d0e0e4736"

func TestTraceAPIByTrace(t *testing.T) {
	f := newFixture(t, traceFile)
	var got struct {
		TraceID string
		Nodes   []json.RawMessage
	}
	f.getJSON(t, "/api/trace?trace="+traceT1, &got)
	if got.TraceID != traceT1 || len(got.Nodes) != 24 {
		t.Errorf("traceId %q, %d nodes", got.TraceID, len(got.Nodes))
	}
}

func TestTraceAPIBySpan(t *testing.T) {
	f := newFixture(t, traceFile)
	for _, id := range []string{"1000000000000013", "ffffffffffffff02"} {
		var got struct{ TraceID string }
		f.getJSON(t, "/api/trace?span="+id, &got)
		if got.TraceID != traceT1 {
			t.Errorf("span %s: traceId %q", id, got.TraceID)
		}
	}
}

func TestTraceAPIByCloudTraceForm(t *testing.T) {
	f := newFixture(t, traceFile)
	var got struct{ TraceID string }
	f.getJSON(t, "/api/trace?trace="+url.QueryEscape("projects/example-project/traces/"+strings.ToUpper(traceT1)), &got)
	if got.TraceID != traceT1 {
		t.Errorf("traceId %q", got.TraceID)
	}
}

func TestTraceAPIErrors(t *testing.T) {
	f := newFixture(t, traceFile)
	for _, tc := range []struct {
		path string
		code int
		msg  string
	}{
		{"/api/trace", 400, "trace or span is required"},
		{"/api/trace?trace=" + strings.Repeat("a", 300), 400, "id too long"},
		{"/api/trace?span=" + strings.Repeat("a", 300), 400, "id too long"},
		{"/api/trace?trace=deadbeef", 404, "no spans or logs with that trace id"},
		{"/api/trace?span=deadbeef", 404, "no record with that span id"},
	} {
		code, b := f.do(t, http.MethodGet, tc.path, "", "")
		wantError(t, code, tc.code, b)
		var e struct{ Error string }
		_ = json.Unmarshal(b, &e)
		if e.Error != tc.msg {
			t.Errorf("%s: error %q; want %q", tc.path, e.Error, tc.msg)
		}
	}
	var got struct{ Nodes []json.RawMessage }
	f.getJSON(t, "/api/trace?service=pricing&trace="+traceT1, &got)
	if len(got.Nodes) != 24 {
		t.Errorf("service filter changed the trace: %d nodes", len(got.Nodes))
	}
}

const aggFile = "../../testdata/web/aggregate.jsonl"

func TestAggregateAPI(t *testing.T) {
	f := newFixture(t, aggFile)
	var got struct {
		Traces int
		Nodes  []struct {
			Name        string
			SerialCalls int
		}
	}
	f.getJSON(t, "/api/aggregate?name=GET%20%2Forders&from=all", &got)
	if got.Traces != 5 || len(got.Nodes) != 6 {
		t.Fatalf("traces %d, %d nodes", got.Traces, len(got.Nodes))
	}
	if n := got.Nodes[2]; n.Name != "SELECT orders" || n.SerialCalls != 9 {
		t.Errorf("nodes[2] = %+v", n)
	}
}

func TestAggregateAPIErrors(t *testing.T) {
	f := newFixture(t, aggFile)
	for _, tc := range []struct {
		path string
		code int
		msg  string
	}{
		{"/api/aggregate", 400, "name is required"},
		{"/api/aggregate?name=" + strings.Repeat("a", 1025), 400, "name too long"},
		{"/api/aggregate?name=GET%20%2Forders&from=yesterday", 400, "range from"},
		{"/api/aggregate?name=nope", 404, "no root spans with that name in range"},
		{"/api/aggregate?service=pricing&name=GET%20%2Forders", 404, "no root spans with that name in range"},
	} {
		code, b := f.do(t, http.MethodGet, tc.path, "", "")
		wantError(t, code, tc.code, b)
		var e struct{ Error string }
		_ = json.Unmarshal(b, &e)
		if !strings.Contains(e.Error, tc.msg) {
			t.Errorf("%s: error %q; want %q", tc.path, e.Error, tc.msg)
		}
	}
	// 1024 bytes is not too long.
	code, b := f.do(t, http.MethodGet, "/api/aggregate?name="+strings.Repeat("a", 1024), "", "")
	wantError(t, code, 404, b)
}

func TestAggregateAPIRange(t *testing.T) {
	f := newFixture(t, aggFile)
	var got struct {
		Traces int
		From   string
	}
	f.getJSON(t, "/api/aggregate?name=GET%20%2Forders&from=2026-11-17T17:02:30Z", &got)
	if got.Traces != 2 || got.From != "2026-11-17T17:02:30Z" {
		t.Errorf("traces %d from %q", got.Traces, got.From)
	}
}

func TestServerQueryAllKinds(t *testing.T) {
	f := newFixture(t, "../../testdata/web/aggregate.jsonl")
	body := `{"stream":"*","source":"otel","service":"*","view":"table","range":{"from":"all"},"groupBy":["name"],"aggregates":[{"op":"p90","field":"duration_ms"},{"op":"error_rate"}]}`
	code, b := f.do(t, http.MethodPost, "/api/query", body, "")
	if code != 200 {
		t.Fatalf("query = %d %s", code, b)
	}
	var res struct {
		Groups int
		Rows   []json.RawMessage
	}
	if err := json.Unmarshal(b, &res); err != nil || res.Groups < 2 || len(res.Rows) != res.Groups {
		t.Fatalf("result = %s (%v)", b, err)
	}
}

func TestServerHeatmap(t *testing.T) {
	f := newFixture(t, "../../testdata/web/aggregate.jsonl")
	body := `{"stream":"*","source":"otel","service":"*","view":"heatmap","range":{"from":"all"}}`
	code, b := f.do(t, http.MethodPost, "/api/query", body, "")
	if code != 200 {
		t.Fatalf("query = %d %s", code, b)
	}
	var res struct {
		Heatmap struct {
			BucketSeconds float64
			Starts        []time.Time
			Bands         []struct{ Lo, Hi float64 }
			Counts        [][]int
			Total         int
		}
	}
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	h := res.Heatmap
	if len(h.Starts) == 0 || len(h.Counts) != len(h.Starts) || h.Total == 0 {
		t.Fatalf("heatmap = %s", b)
	}
	// The first non-empty cell, selected as the page does, lists its count.
	for ti, row := range h.Counts {
		for bi, n := range row {
			if n == 0 {
				continue
			}
			to := h.Starts[ti].Add(time.Duration(h.BucketSeconds * float64(time.Second)))
			cell := fmt.Sprintf(`"cell":{"from":%q,"to":%q,"hi":%v`, h.Starts[ti].Format(time.RFC3339Nano), to.Format(time.RFC3339Nano), h.Bands[bi].Hi)
			if bi > 0 {
				cell += fmt.Sprintf(`,"lo":%v`, h.Bands[bi].Lo)
			}
			body := `{"stream":"*","source":"otel","service":"*","range":{"from":"all"},"limit":1000,` + cell + `}}`
			code, b := f.do(t, http.MethodPost, "/api/query", body, "")
			var list struct{ Matched int }
			if code != 200 || json.Unmarshal(b, &list) != nil || list.Matched != n {
				t.Fatalf("cell query = %d %s, want %d matched", code, b, n)
			}
			return
		}
	}
}

func TestServerHeatmapLogs(t *testing.T) {
	f := newFixture(t, "../../testdata/web/aggregate.jsonl")
	code, _ := f.do(t, http.MethodPost, "/api/query", `{"stream":"app","view":"heatmap"}`, "")
	if code != http.StatusBadRequest {
		t.Fatalf("log heatmap = %d, want 400", code)
	}
}

func TestQueryFormatCSV(t *testing.T) {
	f := newFixture(t, pino)
	post := func(body string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, f.srv.URL+"/api/query", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := f.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, body := post(`{"stream":"db","range":{"from":"all"},"columns":["severity","message"],"format":"csv"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("csv = %d %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.HasPrefix(body, "severity,message\nWARNING,slow query\n") {
		t.Errorf("body = %q", body)
	}
	if n := resp.Header.Get("X-Loghorn-Note"); n != "" {
		t.Errorf("note = %q", n)
	}
	resp, body = post(`{"stream":"db","range":{"from":"all"},"limit":1,"format":"csv"}`)
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("X-Loghorn-Note"), "3") {
		t.Errorf("limited = %d note %q", resp.StatusCode, resp.Header.Get("X-Loghorn-Note"))
	}
	resp, body = post(`{"stream":"db","range":{"from":"all"},"format":"bogus"}`)
	wantError(t, resp.StatusCode, http.StatusBadRequest, []byte(body))
}

func TestQueryFormatSQL(t *testing.T) {
	f := newFixture(t, "../../testdata/web/aggregate.jsonl")
	post := func(body string) (*http.Response, string) {
		t.Helper()
		resp, err := f.srv.Client().Post(f.srv.URL+"/api/query", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, body := post(`{"stream":"app","service":"*","range":{"from":"24h"},"search":"x' OR 1=1","format":"sql"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("sql = %d %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/sql; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	for _, want := range []string{query.SQLHeader, `FROM logs."app"`, `raw ILIKE '%x'' or 1=1%' ESCAPE '\'`, `ORDER BY "time" DESC, seq DESC`} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	resp, body = post(`{"stream":"app","range":{"from":"all"},"view":"graph","format":"sql"}`)
	if resp.StatusCode != 200 || !strings.Contains(body, "time_bucket(") && !strings.Contains(body, "date_trunc('day'") {
		t.Errorf("graph sql = %d %s", resp.StatusCode, body)
	}
	resp, body = post(`{"stream":"app","range":{"from":"all"},"view":"table","groupBy":["method"],"format":"sql"}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `GROUP BY "method"`) {
		t.Errorf("table sql = %d %s", resp.StatusCode, body)
	}
}
