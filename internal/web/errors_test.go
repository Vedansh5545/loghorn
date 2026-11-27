package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/store"
)

// TestStoreErrorsAreServerErrors: when DuckDB fails (here, a closed
// database), each endpoint answers 500 with the error, not 404 "no record
// with that seq", an empty list, or 400 (review §3).
func TestStoreErrorsAreServerErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	st := store.New(store.ModeHistorical, time.Time{})
	if err := store.Preload(st, []string{pino}, config.Input{}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(Options{Store: st, ConfigBase: t.TempDir()}))
	defer srv.Close()
	st.Close()
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/session", ""},
		{"GET", "/api/streams", ""},
		{"GET", "/api/kinds?source=log", ""},
		{"GET", "/api/record?seq=1", ""},
		{"GET", "/api/trace?trace=0af7651916cd43dd8448eb211c80319c", ""},
		{"GET", "/api/trace?span=a000000000000001", ""},
		{"GET", "/api/aggregate?name=chat", ""},
		{"POST", "/api/query", `{"stream":"*","source":"log"}`},
	} {
		req, _ := http.NewRequest(c.method, srv.URL+c.path, strings.NewReader(c.body))
		req.Header.Set("Origin", srv.URL)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("%s %s: %d, want 500", c.method, c.path, resp.StatusCode)
		}
	}
}
