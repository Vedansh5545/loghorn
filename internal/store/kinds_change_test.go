package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/query"
)

// TestQueriesWhileKindsChange: queries keep answering while the fields they
// read change from numbers to text in the records being written.
func TestQueriesWhileKindsChange(t *testing.T) {
	s := New(ModeHistorical, time.Time{})
	defer s.Close()
	const fields = 40
	line := func(v string) string {
		l := `{"timestamp":"2026-11-23T10:00:00Z","severity":"INFO","message":"m","labels":{"schema":"api"}`
		for f := 0; f < fields; f++ {
			l += fmt.Sprintf(`,"f%d":%s`, f, v)
		}
		return l + "}"
	}
	for i := 0; i < 5000; i++ {
		s.Add(logRecord(t, line("1")), time.Time{})
	}
	s.Flush()
	sch, err := s.Schema("*", "log", "api")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, 100)
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				q := query.Query{Stream: "api", Filters: []query.Filter{{Field: fmt.Sprintf("f%d", (i+r)%fields), Op: "!=", Values: []string{"2"}}}}
				if _, err := s.Query(context.Background(), sch, q, time.Now()); err != nil {
					select {
					case errs <- err:
					default:
					}
				}
			}
		}(r)
	}
	for round := 0; round < 3; round++ { // every field widens in one batch
		s.Add(logRecord(t, line(`"x"`)), time.Time{})
		s.Flush()
		time.Sleep(20 * time.Millisecond)
		s.Add(logRecord(t, line(fmt.Sprint(round))), time.Time{})
		s.Flush()
	}
	close(stop)
	wg.Wait()
	close(errs)
	n := 0
	for err := range errs {
		if n == 0 {
			t.Errorf("query failed during a type change: %v", err)
		}
		n++
	}
}
