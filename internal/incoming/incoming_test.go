package incoming

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/logfile"
)

func meta(id, svc string, started time.Time) Meta {
	return Meta{RunID: id, Service: svc, Input: config.Default().Input, Started: started}
}

func TestCreateListAdopt(t *testing.T) {
	data := t.TempDir()
	t0 := time.Date(2026, 11, 23, 8, 12, 0, 0, time.UTC)
	logs, spans, err := Create(data, meta("r1", "api", t0))
	if err != nil {
		t.Fatal(err)
	}
	logs.Write([]byte("one"))
	logs.Write([]byte(`{"a":1}`))
	spans.Write([]byte(`{"span_id":"s1"}`))

	list, err := List(data)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %+v, %v", list, err)
	}
	p := list[0]
	if !p.Locked || p.Meta.RunID != "r1" || p.Meta.Service != "api" || !p.Meta.Started.Equal(t0) {
		t.Fatalf("pending = %+v", p)
	}
	var locked *logfile.LockedError
	if err := Adopt(p, func([]byte) {}, func([]byte) {}); !errors.As(err, &locked) {
		t.Fatalf("Adopt while held = %v; want *logfile.LockedError", err)
	}

	spans.Close()
	logs.Close()
	list, _ = List(data)
	if len(list) != 1 || list[0].Locked {
		t.Fatalf("after Close: %+v", list)
	}
	var lines, spanLines []string
	if err := Adopt(list[0], func(b []byte) { lines = append(lines, string(b)) }, func(b []byte) { spanLines = append(spanLines, string(b)) }); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0] != "one" || lines[1] != `{"a":1}` {
		t.Fatalf("lines = %q", lines)
	}
	if len(spanLines) != 1 || spanLines[0] != `{"span_id":"s1"}` {
		t.Fatalf("spans = %q", spanLines)
	}
	if _, err := os.Stat(p.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("folder not removed: %v", err)
	}
	if list, _ := List(data); len(list) != 0 {
		t.Fatalf("List after Adopt = %+v", list)
	}
}

func TestListOldestFirst(t *testing.T) {
	data := t.TempDir()
	t0 := time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC)
	for _, m := range []Meta{meta("b", "x", t0.Add(time.Hour)), meta("a", "x", t0)} {
		l, s, err := Create(data, m)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
		l.Close()
	}
	list, _ := List(data)
	if len(list) != 2 || list[0].Meta.RunID != "a" || list[1].Meta.RunID != "b" {
		t.Fatalf("order = %+v", list)
	}
}

func TestAdoptMissingMeta(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, "incoming", "orphan")
	w, err := logfile.Open(dir, time.Now, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("lost"))
	w.Close()
	list, err := List(data)
	if err != nil || len(list) != 1 || list[0].Meta.RunID != "orphan" || list[0].Meta.Service != "" {
		t.Fatalf("List = %+v, %v", list, err)
	}
	var got []string
	if err := Adopt(list[0], func(b []byte) { got = append(got, string(b)) }, func([]byte) {}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "lost" {
		t.Fatalf("lines = %q", got)
	}
}

func TestListNoIncoming(t *testing.T) {
	list, err := List(t.TempDir())
	if err != nil || len(list) != 0 {
		t.Fatalf("List = %+v, %v", list, err)
	}
}
