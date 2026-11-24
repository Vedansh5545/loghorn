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

// A pair whose two files were stamped for different text (a crash between
// BuildDay's two renames leaves a new logs file beside an old traces one)
// is in doubt, so the next writer rebuilds it.
func TestStampsDisagreeingOnTextAreInDoubt(t *testing.T) {
	dir := t.TempDir()
	g := newDayRig(t, dir, true, 3)
	if err := g.e.AttachDay(day0, true); err != nil {
		t.Fatal(err)
	}
	g.add(day0, 1, `,"n":1`)
	g.text[day0] = 42
	g.close()
	h := newDayRig(t, dir, true, 3)
	defer h.close()
	traces := filepath.Join(dir, "traces-2025-11-23.duckdb")
	for _, q := range []string{
		"ATTACH '" + traces + "' AS t",
		"UPDATE t._loghorn._day SET text_bytes = 41",
		"DETACH t",
	} {
		if _, err := h.e.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if st, ok := h.e.ReadStamp(day0); ok && st.Clean {
		t.Errorf("stamp %+v accepted, though logs says 42 bytes and traces 41", st)
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

// Rollover and expiry detach catalogs the views name; a read holding
// ReadLock must never see them half done.
func TestReadsDuringRolloverDontFail(t *testing.T) {
	g := newDayRig(t, t.TempDir(), true, 2)
	defer g.close()
	if err := g.e.AttachDay(day0, true); err != nil {
		t.Fatal(err)
	}
	g.add(day0, 1, `,"n":1`)
	stop := make(chan struct{})
	errs := make(chan error, 1)
	go func() {
		defer close(errs)
		for {
			select {
			case <-stop:
				return
			default:
			}
			unlock := g.e.ReadLock()
			var n int
			err := g.e.DB().QueryRow("SELECT count(*) FROM all_records").Scan(&n)
			unlock()
			if err != nil {
				errs <- err
				return
			}
		}
	}()
	for d := 1; d <= 40; d++ {
		g.add(day0+d, 1, `,"n":1`) // a midnight each, and an expiry
	}
	close(stop)
	if err := <-errs; err != nil {
		t.Errorf("a read failed during rollover: %v", firstLine(err))
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

// A batch that fails partway through a day file must leave nothing of
// itself there: the whole batch goes to memory instead, so each record shows
// once, not once from the file and again from memory.
func TestFailedBatchLeavesNoPartialRows(t *testing.T) {
	g := newDayRig(t, t.TempDir(), true, 3)
	defer g.close()
	if err := g.e.AttachDay(day0, true); err != nil {
		t.Fatal(err)
	}
	g.add(day0, 1, `,"n":1`) // stream api exists in the file
	w0 := logRec(t, "s", "web", `,"n":10`)
	w0.Seq = day0<<32 + 2
	g.e.Add(w0) // so does stream web
	if !g.e.Sync(10 * time.Second) {
		t.Fatal("sync timed out")
	}
	// A column the writer doesn't know about, so adding field m to web fails
	// after stream api's rows of the same batch were appended.
	if _, err := g.e.DB().Exec(`ALTER TABLE "l2025-11-23"."s"."web" ADD COLUMN m INTEGER`); err != nil {
		t.Fatal(err)
	}
	api := logRec(t, "s", "api", `,"n":3`)
	api.Seq = day0<<32 + 3
	web := logRec(t, "s", "web", `,"m":4`)
	web.Seq = day0<<32 + 4
	g.e.wmu.Lock()
	g.e.write([]record.Record{api, web})
	g.e.wmu.Unlock()

	recs, err := g.e.Records(bg, "true")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]int{}
	for _, r := range recs {
		seen[r.Seq-day0<<32]++
	}
	if want := map[int]int{1: 1, 2: 1, 3: 1, 4: 1}; !reflect.DeepEqual(seen, want) {
		t.Errorf("records by sequence %v, want each once %v", seen, want)
	}
	var n int
	if err := g.e.DB().QueryRow(`SELECT count(*) FROM "l2025-11-23"._loghorn._records WHERE _id = ?`, api.Seq).Scan(&n); err != nil || n != 0 {
		t.Errorf("failed batch left %d index rows in the day file (%v), want 0", n, err)
	}
}

// Widening a day file's column to string rewrites its values from _raw
// inside the batch's transaction (it uses a temp table), and must still
// commit.
func TestDayColumnWidensToStringInTransaction(t *testing.T) {
	g := newDayRig(t, t.TempDir(), true, 3)
	defer g.close()
	if err := g.e.AttachDay(day0, true); err != nil {
		t.Fatal(err)
	}
	g.add(day0, 1, `,"v":1.50`)
	g.add(day0, 2, `,"v":"text"`)
	if len(g.warns) != 0 {
		t.Fatalf("warnings %q", g.warns)
	}
	var got []string
	rows, err := g.e.DB().Query(`SELECT v FROM "l2025-11-23"."s"."api" ORDER BY _id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		rows.Scan(&s)
		got = append(got, s)
	}
	if want := []string{"1.50", "text"}; !reflect.DeepEqual(got, want) {
		t.Errorf("values %v, want %v (as received, in the file)", got, want)
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
