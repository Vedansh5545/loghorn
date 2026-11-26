package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// sqlLine is a log line of stream app from service svc.
func sqlLine(i int, svc, extra string) string {
	return fmt.Sprintf(`{"time":"2026-11-17T17:%02d:%02d.000Z","severity":"INFO","message":"m%d","labels":{"schema":"app"},"service":{"name":%q},"code":%d%s}`,
		i/60%60, i%60, i, svc, 200+i%3*100, extra)
}

func sqlStore(t *testing.T, n int) *Store {
	t.Helper()
	s := New(ModeLive, t0)
	for i := 0; i < n; i++ {
		svc := "api"
		if i%2 == 1 {
			svc = "web"
		}
		s.Add(logRecord(t, sqlLine(i, svc, "")), time.Time{})
	}
	return s
}

func sqlRows(t *testing.T, s *Store, q string, max int) SQLResult {
	t.Helper()
	res, err := s.SQL(context.Background(), q, max)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return res
}

func TestStoreSQL(t *testing.T) {
	s := sqlStore(t, 6)
	res := sqlRows(t, s, `SELECT service, code, count(*) AS n FROM logs."app" GROUP BY ALL ORDER BY ALL`, 100)
	if strings.Join(res.Columns, ",") != "service,code,n" {
		t.Fatalf("columns %v", res.Columns)
	}
	if got := fmt.Sprint(res.Rows); got != "[[api 200 1] [api 300 1] [api 400 1] [web 200 1] [web 300 1] [web 400 1]]" {
		t.Fatalf("rows %s", got)
	}
	if res.Truncated {
		t.Fatal("truncated")
	}
}

func TestStoreSQLCapsRows(t *testing.T) {
	s := sqlStore(t, 25)
	res := sqlRows(t, s, `SELECT seq FROM logs."app"`, 10)
	if len(res.Rows) != 10 || !res.Truncated {
		t.Fatalf("%d rows, truncated %v", len(res.Rows), res.Truncated)
	}
	res = sqlRows(t, s, `SELECT seq FROM logs."app"`, 25)
	if len(res.Rows) != 25 || res.Truncated {
		t.Fatalf("%d rows, truncated %v", len(res.Rows), res.Truncated)
	}
}

func TestStoreSQLRefusesWrites(t *testing.T) {
	s := sqlStore(t, 2)
	if _, err := s.SQL(context.Background(), `DELETE FROM loghorn_db.main.records`, 10); err == nil {
		t.Fatal("DELETE allowed")
	}
	if n := sqlRows(t, s, `SELECT count(*) FROM logs."app"`, 10).Rows[0][0]; n != int64(2) {
		t.Fatalf("count after refused DELETE = %v", n)
	}
}

func TestStoreSQLEmpty(t *testing.T) {
	s := New(ModeLive, t0)
	sqlRows(t, s, `SHOW ALL TABLES`, 10)
	_, err := s.SQL(context.Background(), `FROM logs."app"`, 10)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("error %v", err)
	}
}

func TestStoreSQLSeesNewFields(t *testing.T) {
	s := sqlStore(t, 2)
	sqlRows(t, s, `SELECT count(*) FROM logs."app"`, 10)
	s.Add(logRecord(t, sqlLine(3, "api", `,"region":"eu"`)), time.Time{})
	res := sqlRows(t, s, `SELECT region FROM logs."app" WHERE region IS NOT NULL`, 10)
	if fmt.Sprint(res.Rows) != "[[eu]]" {
		t.Fatalf("rows %v", res.Rows)
	}
}

func TestStoreSQLThisRun(t *testing.T) {
	s := sqlStore(t, 1)
	s.Add(logRecord(t, sqlLine(10, "api", `,"loghorn.run.id":"r1"`)), time.Time{})
	s.Add(logRecord(t, sqlLine(20, "api", `,"loghorn.run.id":"r2"`)), time.Time{})
	s.Add(logRecord(t, sqlLine(30, "api", `,"loghorn.run.id":"r2"`)), time.Time{})
	res := sqlRows(t, s, `SELECT loghorn.this_run_id(), loghorn.this_run_start() = TIMESTAMPTZ '2026-11-17 17:00:20+00'`, 10)
	if fmt.Sprint(res.Rows) != "[[r2 true]]" {
		t.Fatalf("rows %v", res.Rows)
	}
	res = sqlRows(t, s, `SELECT count(*) FROM logs."app" WHERE "time" >= loghorn.this_run_start() AND ("loghorn.run.id" = loghorn.this_run_id() OR "loghorn.run.id" IS NULL)`, 10)
	if fmt.Sprint(res.Rows) != "[[2]]" {
		t.Fatalf("this run's records: %v", res.Rows)
	}
}

func TestStoreSQLConcurrent(t *testing.T) {
	s := sqlStore(t, 10)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			s.Add(logRecord(t, sqlLine(100+i, "api", fmt.Sprintf(`,"f%d":%d`, i, i))), time.Time{})
		}(i)
		go func() {
			defer wg.Done()
			if _, err := s.SQL(context.Background(), `SELECT count(*) FROM logs."app"`, 10); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
