package recorder

import "testing"

func TestRecorderServiceFillsEmpty(t *testing.T) {
	g := newRig(Options{Live: true, Service: "api"})
	g.r.Line([]byte(lineA))
	g.r.Line([]byte(`{"labels":{"schema":"web"},"service.name":"own","message":"x"}`))
	recs, _ := g.st.Snapshot()
	if len(recs) != 2 {
		t.Fatalf("stored %d records; want 2", len(recs))
	}
	if recs[0].Service != "api" {
		t.Fatalf("line without a service = %q; want api", recs[0].Service)
	}
	if recs[1].Service != "own" {
		t.Fatalf("line naming its service = %q; want own", recs[1].Service)
	}
}

func TestRecorderMarkOTLP(t *testing.T) {
	g := newRig(Options{Live: true, DedupeAfterOTLP: true})
	g.r.MarkOTLP()
	if !g.r.SawOTLP() {
		t.Fatal("MarkOTLP did not set SawOTLP")
	}
	g.r.Line([]byte(lineA)) // JSON: dropped
	g.r.Line([]byte(lineB)) // plain: kept
	eq(t, g.sink, []string{lineB})
}
