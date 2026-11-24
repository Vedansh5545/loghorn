package duckstore

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/record"
	"github.com/jimbeveridge/loghorn/internal/schema"
)

const day0 = 20415 // 2025-11-23

type dayRig struct {
	t     *testing.T
	dir   string
	e     *Engine
	warns []string
	text  map[int]int64
}

func newDayRig(t *testing.T, dir string, write bool, keep int) *dayRig {
	t.Helper()
	g := &dayRig{t: t, dir: dir, text: map[int]int64{}}
	e, err := OpenDays(config.Input{}, func(s string) { g.warns = append(g.warns, s) }, DayOptions{
		Dir: dir, Write: write, KeepDays: keep, TextBytes: func(d int) int64 { return g.text[d] },
	})
	if err != nil {
		t.Fatal(err)
	}
	g.e = e
	return g
}

func (g *dayRig) add(day, seq int, extra string) record.Record {
	r := logRec(g.t, "s", "api", extra)
	r.Seq = day<<32 + seq
	g.e.Add(r)
	if !g.e.Sync(10 * time.Second) {
		g.t.Fatal("sync timed out")
	}
	return r
}

func (g *dayRig) close() {
	if err := g.e.Close(); err != nil {
		g.t.Fatal(err)
	}
}

func TestDayPairWrittenAndStampedOnClose(t *testing.T) {
	dir := t.TempDir()
	g := newDayRig(t, dir, true, 3)
	if err := g.e.AttachDay(day0, true); err != nil {
		t.Fatal(err)
	}
	g.add(day0, 1, `,"n":1`)
	g.add(day0, 2, `,"n":2`)
	g.text[day0] = 42
	g.close()
	for _, p := range []string{"logs-2025-11-23.duckdb", "traces-2025-11-23.duckdb"} {
		fi, err := os.Stat(filepath.Join(dir, p))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", p, fi.Mode().Perm())
		}
	}
	h := newDayRig(t, dir, false, 3)
	defer h.close()
	st, ok := h.e.ReadStamp(day0)
	if !ok || st != (Stamp{Clean: true, TextBytes: 42}) {
		t.Fatalf("stamp %+v ok=%v", st, ok)
	}
	if err := h.e.AttachDay(day0, false); err != nil {
		t.Fatal(err)
	}
	if n := h.e.MaxSeq(day0); n != 2 {
		t.Errorf("MaxSeq = %d, want 2", n)
	}
	recs, err := h.e.Records(bg, "true")
	if err != nil || len(recs) != 2 {
		t.Fatalf("records %d, %v", len(recs), err)
	}
}

// The write-ahead log holds the same records as the day file, so it must be
// owner-only too, whatever the process umask or directory mode.
func TestDayWALIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	g := newDayRig(t, dir, true, 3)
	defer g.close()
	if err := g.e.AttachDay(day0, true); err != nil {
		t.Fatal(err)
	}
	g.add(day0, 1, `,"n":1`)
	g.add(day0, 2, `,"n":2`)
	wals, _ := filepath.Glob(filepath.Join(dir, "*.duckdb.wal"))
	if len(wals) == 0 {
		t.Fatal("no .wal file while the day is open; the test can't check its mode")
	}
	for _, w := range wals {
		fi, err := os.Stat(w)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", filepath.Base(w), fi.Mode().Perm())
		}
	}
}

func TestOpenDayIsStampedUnclean(t *testing.T) {
	g := newDayRig(t, t.TempDir(), true, 3)
	defer g.close()
	if err := g.e.AttachDay(day0, true); err != nil {
		t.Fatal(err)
	}
	var clean bool
	if err := g.e.DB().QueryRow(`SELECT clean FROM "l2025-11-23"._loghorn._day`).Scan(&clean); err != nil {
		t.Fatal(err)
	}
	if clean {
		t.Error("an open day must be stamped unclean, so a crash shows")
	}
}

func TestRolloverClosesDayAndExpires(t *testing.T) {
	dir := t.TempDir()
	g := newDayRig(t, dir, true, 1)
	if err := g.e.AttachDay(day0, true); err != nil {
		t.Fatal(err)
	}
	g.text[day0], g.text[day0+1] = 7, 8
	g.add(day0, 1, `,"n":1`)
	g.add(day0+1, 1, `,"n":2`) // midnight passed
	g.add(day0+2, 1, `,"n":3`) // day0 is now past KeepDays=1
	if _, err := os.Stat(filepath.Join(dir, "logs-2025-11-23.duckdb")); !os.IsNotExist(err) {
		t.Errorf("day0 not expired: %v", err)
	}
	recs, _ := g.e.Records(bg, "true")
	if len(recs) != 2 {
		t.Errorf("records after expiry = %d, want 2", len(recs))
	}
	g.close()
	h := newDayRig(t, dir, false, 1)
	defer h.close()
	if st, ok := h.e.ReadStamp(day0 + 1); !ok || !st.Clean || st.TextBytes != 8 {
		t.Errorf("day0+1 stamp %+v ok=%v", st, ok)
	}
}

func TestBuildDayFromLoader(t *testing.T) {
	dir := t.TempDir()
	g := newDayRig(t, dir, true, 3)
	g.text[day0] = 99
	err := g.e.BuildDay(day0, func() error {
		r := logRec(t, "s", "api", `,"n":5`)
		r.Seq = day0<<32 + 1
		g.e.Add(r)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := g.e.ReadStamp(day0); !ok || !st.Clean || st.TextBytes != 99 {
		t.Fatalf("stamp %+v ok=%v", st, ok)
	}
	if err := g.e.AttachDay(day0, false); err != nil {
		t.Fatal(err)
	}
	recs, _ := g.e.Records(bg, "true")
	if len(recs) != 1 {
		t.Errorf("records = %d, want 1", len(recs))
	}
	g.close()
	if m, _ := filepath.Glob(filepath.Join(dir, "*.tmp*")); len(m) != 0 {
		t.Errorf("temporary files left: %v", m)
	}
}

func TestWriteFailureKeepsRecordsInMemory(t *testing.T) {
	g := newDayRig(t, t.TempDir(), true, 3)
	defer g.close()
	if err := g.e.AttachDay(day0, true); err != nil {
		t.Fatal(err)
	}
	g.add(day0, 1, `,"n":1`)
	// Break the file under the writer.
	if _, err := g.e.DB().Exec(`DETACH "l2025-11-23"`); err != nil {
		t.Fatal(err)
	}
	g.add(day0, 2, `,"n":2`)
	g.add(day0, 3, `,"n":3`)
	if len(g.warns) != 1 || !strings.Contains(g.warns[0], "memory") {
		t.Errorf("warnings %q, want one about memory", g.warns)
	}
	recs, _ := g.e.Records(bg, "true")
	if len(recs) != 2 {
		t.Errorf("records = %d, want the 2 written after the failure", len(recs))
	}
}

func TestViewsWidenAcrossDays(t *testing.T) {
	g := newDayRig(t, t.TempDir(), true, 3)
	defer g.close()
	if err := g.e.AttachDay(day0, true); err != nil {
		t.Fatal(err)
	}
	g.add(day0, 1, `,"v":5`)
	g.add(day0+1, 1, `,"v":"text"`)
	q, args := g.e.Relation(record.SourceLog, "s", "api", []schema.Field{{Path: "v", Type: schema.TString}})
	rows, err := g.e.DB().Query("SELECT v0 FROM ("+q+") ORDER BY _id", args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		got = append(got, s)
	}
	if want := []string{"5", "text"}; !reflect.DeepEqual(got, want) {
		t.Errorf("values %v, want %v", got, want)
	}
	var vt string
	if err := g.e.DB().QueryRow(`SELECT data_type FROM duckdb_columns() WHERE table_name = 'log/s/api' AND column_name = 'v'`).Scan(&vt); err != nil || vt != "VARCHAR" {
		t.Errorf("view column type %q, %v; want VARCHAR", vt, err)
	}
}
