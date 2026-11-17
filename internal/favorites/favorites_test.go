package favorites

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jimbeveridge/loghorn/internal/query"
)

func TestPutLoadDelete(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".config", "loghorn") // not yet created
	favs, err := Load(dir)
	if err != nil || len(favs) != 0 || favs == nil {
		t.Fatalf("Load absent = %#v, %v; want empty non-nil", favs, err)
	}
	a := Favorite{Name: "slow", Query: query.Query{Stream: "db", Search: "x"}}
	b := Favorite{Name: "errors", Query: query.Query{Stream: "api"}}
	for _, f := range []Favorite{a, b} {
		if err := Put(dir, f, false); err != nil {
			t.Fatalf("Put %s: %v", f.Name, err)
		}
	}
	favs, err = Load(dir)
	if err != nil || len(favs) != 2 || favs[0].Name != "slow" || favs[1].Name != "errors" {
		t.Fatalf("Load = %#v, %v", favs, err)
	}
	if err := Delete(dir, "slow"); err != nil {
		t.Fatal(err)
	}
	favs, _ = Load(dir)
	if len(favs) != 1 || favs[0].Name != "errors" {
		t.Fatalf("after delete = %#v", favs)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 || ents[0].Name() != "queries.toml" {
		t.Fatalf("dir holds %v; want only queries.toml (no temp files)", ents)
	}
}

func TestPutExistsWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	if err := Put(dir, Favorite{Name: "n", Query: query.Query{Stream: "a"}}, false); err != nil {
		t.Fatal(err)
	}
	if err := Put(dir, Favorite{Name: "n", Query: query.Query{Stream: "b"}}, false); !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v; want ErrExists", err)
	}
	favs, _ := Load(dir)
	if favs[0].Query.Stream != "a" {
		t.Fatalf("refused put changed the file: %#v", favs)
	}
	if err := Put(dir, Favorite{Name: "n", Query: query.Query{Stream: "b"}}, true); err != nil {
		t.Fatal(err)
	}
	favs, _ = Load(dir)
	if len(favs) != 1 || favs[0].Query.Stream != "b" {
		t.Fatalf("overwrite = %#v", favs)
	}
}

func TestPutBadName(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"", strings.Repeat("é", MaxNameLen+1)} {
		if err := Put(dir, Favorite{Name: n}, true); !errors.Is(err, ErrBadName) {
			t.Fatalf("Put(%q) = %v; want ErrBadName", n, err)
		}
	}
	if err := Put(dir, Favorite{Name: strings.Repeat("é", MaxNameLen)}, true); err != nil {
		t.Fatalf("100-char name: %v", err)
	}
}

func TestDeleteMissing(t *testing.T) {
	dir := t.TempDir()
	if err := Delete(dir, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v; want ErrNotFound", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "queries.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Delete of a missing favorite wrote a file: %v", err)
	}
}

func TestRoundTripPreservesQuery(t *testing.T) {
	dir := t.TempDir()
	q := query.Query{
		Stream: "db",
		Range:  query.Range{From: "15m", To: "now"},
		Filters: []query.Filter{
			{Field: "latency", Op: ">", Values: []string{"1ms"}},
			{Field: "op", Op: "in", Values: []string{"SELECT", "UPDATE"}},
		},
		Search:     `quote " and \ back`,
		Columns:    []string{"timestamp", "message"},
		Sort:       query.Sort{Field: "latency", Desc: true},
		Limit:      250,
		GroupBy:    []string{"op"},
		Aggregates: []query.Aggregate{{Op: "count"}, {Op: "p95", Field: "latency"}},
		Bucket:     "1m",
		View:       "table",
	}
	if err := Put(dir, Favorite{Name: "everything", Query: q}, false); err != nil {
		t.Fatal(err)
	}
	favs, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(favs[0].Query, q) {
		t.Fatalf("round trip:\n got %#v\nwant %#v", favs[0].Query, q)
	}
}
