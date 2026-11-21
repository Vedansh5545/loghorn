package store

import (
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/record"
)

func TestStoreFallbackService(t *testing.T) {
	s := New(ModeHistorical, time.Time{})
	s.Add(rec("a", t0), time.Time{})
	s.SetFallbackService("shop-api")
	s.Add(rec("a", t0), time.Time{})
	named := rec("a", t0)
	named.Service = "x"
	s.Add(named, time.Time{})
	recs, _ := s.Snapshot()
	want := []string{record.DefaultService, "shop-api", "x"}
	for i, w := range want {
		if recs[i].Service != w {
			t.Errorf("recs[%d].Service = %q, want %q", i, recs[i].Service, w)
		}
	}
	(*Store)(nil).SetFallbackService("a")
}
