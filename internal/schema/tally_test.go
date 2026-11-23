package schema_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jimbeveridge/loghorn/internal/adapter"
	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/ingest"
	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

func loadFixtures(t *testing.T) []record.Record {
	t.Helper()
	paths, err := filepath.Glob("../../testdata/web/*")
	if err != nil {
		t.Fatal(err)
	}
	in := config.Input{}
	p := adapter.New(in)
	var out []record.Record
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		err = ingest.Stored(f, func(line []byte) {
			if r, ok := record.Normalize(p.ParseLine(bytes.Clone(line)), in); ok {
				out = append(out, r)
			}
		})
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(out) == 0 {
		t.Fatal("no fixture records")
	}
	return out
}

// TestFromTalliesMatchesInfer builds per-path tallies with Classify and
// checks FromTallies gives exactly Infer's schema, for every stream of every
// fixture plus the all-streams key, with and without a service.
func TestFromTalliesMatchesInfer(t *testing.T) {
	recs := loadFixtures(t)
	type key struct{ svc, src, stream string }
	keys := map[key]bool{}
	for _, r := range recs {
		k := r.Key()
		keys[key{"*", k.Source, k.Name}] = true
		keys[key{r.Service, k.Source, k.Name}] = true
		keys[key{"*", k.Source, record.AllStreams}] = true
	}
	for k := range keys {
		tallies := map[string]schema.Tally{}
		for _, r := range recs {
			rk := r.Key()
			if rk.Source != k.src || (k.stream != record.AllStreams && rk.Name != k.stream) {
				continue
			}
			if k.svc != "*" && k.svc != "" && r.Service != k.svc {
				continue
			}
			for path, v := range r.Fields {
				tl := tallies[path]
				tl.Count++
				tl.Kinds |= schema.Classify(v)
				tallies[path] = tl
			}
		}
		want := schema.Infer(k.svc, k.src, k.stream, recs)
		got := schema.FromTallies(k.src, k.stream, tallies)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%v:\n got %+v\nwant %+v", k, got, want)
		}
	}
}

func TestTallyType(t *testing.T) {
	int1 := schema.Classify(jsonNumber("5"))
	flt := schema.Classify(jsonNumber("1.5"))
	str := schema.Classify("x")
	none := schema.Classify(nil)
	cases := []struct {
		path string
		k    schema.KindSet
		want schema.Type
	}{
		{"a", int1, schema.TInt},
		{"a", int1 | flt, schema.TFloat},
		{"a", int1 | str, schema.TString},
		{"a", none, schema.TString},
		{"a", int1 | none, schema.TInt},
		{"timestamp", int1, schema.TTimestamp},
		{"receiveTimestamp", str, schema.TTimestamp},
	}
	for _, c := range cases {
		if got := schema.TallyType(c.path, c.k); got != c.want {
			t.Errorf("TallyType(%s, %b) = %s, want %s", c.path, c.k, got, c.want)
		}
	}
}

func jsonNumber(s string) any { return json.Number(s) }
