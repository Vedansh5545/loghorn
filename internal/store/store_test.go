package store

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/record"
)

var t0 = time.Date(2026, 11, 17, 17, 0, 0, 0, time.UTC)

func rec(stream string, t time.Time) record.Record {
	return record.Record{Stream: stream, Time: t, Fields: map[string]any{}}
}

func TestAddAssignsSeq(t *testing.T) {
	s := New(ModeHistorical, time.Time{})
	for i := 0; i < 3; i++ {
		s.Add(rec("a", t0), time.Time{})
	}
	recs, v := s.Snapshot()
	if v != 3 || len(recs) != 3 {
		t.Fatalf("version=%d len=%d", v, len(recs))
	}
	for i, r := range recs {
		if r.Seq != i+1 {
			t.Errorf("recs[%d].Seq=%d", i, r.Seq)
		}
	}
}

func TestInheritFromPrevious(t *testing.T) {
	s := New(ModeHistorical, time.Time{})
	s.Add(rec("a", t0), time.Time{})
	s.Add(rec("a", time.Time{}), time.Time{})
	recs, _ := s.Snapshot()
	if !recs[1].Time.Equal(t0) || !recs[1].TimeInherited || recs[0].TimeInherited {
		t.Fatalf("got %+v", recs)
	}
}

func TestInheritFromReceived(t *testing.T) {
	s := New(ModeLive, t0)
	rcv := t0.Add(5 * time.Second)
	s.Add(rec("a", time.Time{}), rcv)
	recs, _ := s.Snapshot()
	if !recs[0].Time.Equal(rcv) || !recs[0].TimeInherited {
		t.Fatalf("got %+v", recs[0])
	}
}

func TestBackfillLeading(t *testing.T) {
	s := New(ModeHistorical, time.Time{})
	s.Add(rec("a", time.Time{}), time.Time{})
	s.Add(rec("a", time.Time{}), time.Time{})
	if in := s.Info(); !in.Earliest.IsZero() || !in.Latest.IsZero() {
		t.Fatalf("bounds with no real time: %+v", in)
	}
	s.Add(rec("a", t0), time.Time{})
	recs, _ := s.Snapshot()
	for i, r := range recs {
		if !r.Time.Equal(t0) {
			t.Errorf("recs[%d].Time=%v", i, r.Time)
		}
	}
	if !recs[0].TimeInherited || !recs[1].TimeInherited || recs[2].TimeInherited {
		t.Fatalf("inherited flags wrong: %+v", recs)
	}
}

func TestStreamsSorted(t *testing.T) {
	s := New(ModeHistorical, time.Time{})
	for _, n := range []string{"db", "api", "db", "server"} {
		s.Add(rec(n, t0), time.Time{})
	}
	got := s.Streams()
	want := []StreamCount{{Name: "api", Source: "log", Count: 1}, {Name: "db", Source: "log", Count: 2}, {Name: "server", Source: "log", Count: 1}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestInfoBounds(t *testing.T) {
	s := New(ModeLive, t0)
	s.Add(rec("a", t0.Add(2*time.Second)), time.Time{})
	s.Add(rec("a", time.Time{}), time.Time{})
	s.Add(rec("a", t0.Add(time.Second)), time.Time{})
	s.Add(rec("a", t0.Add(9*time.Second)), time.Time{})
	in := s.Info()
	if in.Mode != ModeLive || !in.RunStart.Equal(t0) || in.Count != 4 ||
		!in.Earliest.Equal(t0.Add(time.Second)) || !in.Latest.Equal(t0.Add(9*time.Second)) {
		t.Fatalf("%+v", in)
	}
}

func TestPreloadReadsFixtures(t *testing.T) {
	s := New(ModeHistorical, time.Time{})
	if err := Preload(s, []string{"../../testdata/web/pino.log"}, config.Input{}); err != nil {
		t.Fatal(err)
	}
	// pino.log: 3 db, 2 api, 2 server records (one server line untimed); the
	// npm banner line has no stream label and is dropped.
	want := []StreamCount{{Name: "api", Source: "log", Count: 2}, {Name: "db", Source: "log", Count: 3}, {Name: "server", Source: "log", Count: 2}}
	if got := s.Streams(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	recs, _ := s.Snapshot()
	last := recs[len(recs)-1]
	if !last.TimeInherited || last.Time.IsZero() {
		t.Fatalf("untimed line should inherit previous time: %+v", last)
	}
}

func TestConcurrentAddAndRead(t *testing.T) {
	s := New(ModeLive, t0)
	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				recs, v := s.Snapshot()
				if uint64(len(recs)) != v {
					t.Errorf("version %d != len %d", v, len(recs))
				}
				s.Streams()
				s.Info()
			}
		}()
	}
	for i := 0; i < 10000; i++ {
		var tm time.Time
		if i%3 != 0 && i > 5 {
			tm = t0.Add(time.Duration(i) * time.Millisecond)
		}
		s.Add(rec("a", tm), time.Time{})
	}
	close(done)
	wg.Wait()
	if s.Info().Count != 10000 {
		t.Fatal("lost records")
	}
}

func TestStreamsBySource(t *testing.T) {
	s := New(ModeHistorical, time.Time{})
	for _, r := range []record.Record{
		{Stream: "db"},
		{Stream: "db", Source: "otel"},
		{Stream: "db", Source: "otel"},
		{Stream: "api", Source: "log"},
	} {
		r.Time, r.Fields = t0, map[string]any{}
		s.Add(r, time.Time{})
	}
	got := s.Streams()
	want := []StreamCount{{Name: "api", Source: "log", Count: 1}, {Name: "db", Source: "log", Count: 1}, {Name: "db", Source: "otel", Count: 2}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func svcRec(service, kind string, t time.Time) record.Record {
	return record.Record{Source: record.SourceOtel, Service: service, Stream: kind, Time: t, Fields: map[string]any{"k": "v"}}
}

func TestKindsRangeEdges(t *testing.T) {
	s := New(ModeHistorical, time.Time{})
	t10 := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	s.Add(svcRec("a", "db", t10), time.Time{})
	s.Add(svcRec("a", "db", t10.Add(5*time.Minute)), time.Time{})
	s.Add(svcRec("a", "x", t10.Add(10*time.Minute)), time.Time{})
	k, _ := s.Kinds("otel", "a", t10, t10.Add(10*time.Minute))
	if !reflect.DeepEqual(k, []Count{{"db", 2}}) {
		t.Fatalf("bounded: %v", k)
	}
	k, _ = s.Kinds("otel", "a", time.Time{}, time.Time{})
	if !reflect.DeepEqual(k, []Count{{"db", 2}, {"x", 1}}) {
		t.Fatalf("unbounded: %v", k)
	}
}

func TestKindsZeroTime(t *testing.T) {
	s := New(ModeHistorical, time.Time{})
	s.Add(svcRec("a", "db", time.Time{}), time.Time{})
	if k, _ := s.Kinds("otel", "a", time.Time{}, time.Time{}); len(k) != 1 {
		t.Fatalf("zero from should count: %v", k)
	}
	if k, _ := s.Kinds("otel", "a", t0, time.Time{}); len(k) != 0 {
		t.Fatalf("bounded from should skip: %v", k)
	}
}

func TestKindsServiceAll(t *testing.T) {
	s := New(ModeHistorical, time.Time{})
	s.Add(svcRec("a", "db", t0), time.Time{})
	s.Add(svcRec("a", "db", t0), time.Time{})
	s.Add(svcRec("a", "rpc", t0), time.Time{})
	s.Add(svcRec("b", "http.server", t0), time.Time{})
	k, sv := s.Kinds("otel", "*", time.Time{}, time.Time{})
	if !reflect.DeepEqual(k, []Count{{"db", 2}, {"http.server", 1}, {"rpc", 1}}) {
		t.Fatalf("kinds: %v", k)
	}
	if !reflect.DeepEqual(sv, []Count{{"a", 3}, {"b", 1}}) {
		t.Fatalf("services: %v", sv)
	}
	k, sv = s.Kinds("otel", "b", time.Time{}, time.Time{})
	if !reflect.DeepEqual(k, []Count{{"http.server", 1}}) || len(sv) != 2 {
		t.Fatalf("b: %v %v", k, sv)
	}
	if k, _ := s.Kinds("", "*", time.Time{}, time.Time{}); len(k) != 0 {
		t.Fatalf("log source should be empty: %v", k)
	}
}

func TestKindsEmpty(t *testing.T) {
	s := New(ModeHistorical, time.Time{})
	k, sv := s.Kinds("otel", "*", time.Time{}, time.Time{})
	if k == nil || sv == nil || len(k) != 0 || len(sv) != 0 {
		t.Fatalf("empty store: %#v %#v", k, sv)
	}
	s.Add(svcRec("a", "db", t0), time.Time{})
	k, sv = s.Kinds("otel", "nope", time.Time{}, time.Time{})
	if k == nil || len(k) != 0 || len(sv) != 1 {
		t.Fatalf("unknown service: %#v %#v", k, sv)
	}
	k, sv = s.Kinds("bogus", "*", time.Time{}, time.Time{})
	if k == nil || sv == nil {
		t.Fatalf("bogus source: %#v %#v", k, sv)
	}
}

func TestKindsBudget(t *testing.T) {
	if raceEnabled {
		t.Skip("wall-clock budget is not checked under the race detector")
	}
	s := New(ModeHistorical, time.Time{})
	const n = 200000
	for i := 0; i < n; i++ {
		r := record.Record{
			Source:  []string{record.SourceLog, record.SourceOtel}[i%2],
			Service: fmt.Sprintf("svc%d", i%3),
			Stream:  fmt.Sprintf("kind%d", i%30),
			Time:    t0.Add(time.Duration(i) * time.Millisecond),
			Fields:  map[string]any{"a": "b", "c": "d"},
		}
		s.Add(r, time.Time{})
	}
	from, to := t0.Add(n*time.Millisecond/10), t0.Add(n*time.Millisecond*9/10)
	best := time.Hour
	for i := 0; i < 5; i++ {
		start := time.Now()
		s.Kinds("otel", "*", from, to)
		if d := time.Since(start); d < best {
			best = d
		}
	}
	if best > 50*time.Millisecond {
		t.Fatalf("Kinds over %d records took %v, budget 50ms", n, best)
	}
}

func TestKindsConcurrentAdd(t *testing.T) {
	s := New(ModeLive, time.Time{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10000; i++ {
			s.Add(svcRec("a", "db", t0.Add(time.Duration(i)*time.Millisecond)), time.Time{})
		}
	}()
	for {
		s.Kinds("otel", "*", time.Time{}, time.Time{})
		select {
		case <-done:
			if k, _ := s.Kinds("otel", "*", time.Time{}, time.Time{}); k[0].Count != 10000 {
				t.Fatalf("final count %v", k)
			}
			return
		default:
		}
	}
}
