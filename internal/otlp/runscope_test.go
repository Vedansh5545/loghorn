package otlp

import (
	"net/http/httptest"
	"sync"
	"testing"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

func strAttr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: sv(v)}
}

// mixedRequest has one loghorn.run resource (two lines) and one ordinary one.
func mixedRequest() *collogspb.ExportLogsServiceRequest {
	return &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{
		{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				strAttr("service.name", "api"),
				strAttr("loghorn.run.id", "r1"),
				strAttr("loghorn.input", `{"format":"auto"}`),
			}},
			ScopeLogs: []*logspb.ScopeLogs{{
				Scope: &commonpb.InstrumentationScope{Name: RunScope},
				LogRecords: []*logspb.LogRecord{
					{TimeUnixNano: 1, Body: sv(`{"msg":"a"}`)},
					{TimeUnixNano: 2, Body: sv("plain b")},
				},
			}},
		},
		{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{strAttr("service.name", "other")}},
			ScopeLogs: []*logspb.ScopeLogs{{
				Scope:      &commonpb.InstrumentationScope{Name: "app"},
				LogRecords: []*logspb.LogRecord{{TimeUnixNano: 3, Body: sv("ordinary")}},
			}},
		},
	}}
}

func TestReceiverRunScopeDiverted(t *testing.T) {
	logs := &sink{}
	var mu sync.Mutex
	var runs []RunLine
	rcv := NewReceiver(logs.collect, nil, fixedNow)
	rcv.SetRunLines(func(l RunLine) {
		mu.Lock()
		defer mu.Unlock()
		runs = append(runs, l)
	})
	srv := httptest.NewServer(rcv)
	defer srv.Close()

	resp, _ := post(t, srv.URL+"/v1/logs", ctProto, mustProto(t, mixedRequest()), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(runs) != 2 {
		t.Fatalf("run lines = %d; want 2", len(runs))
	}
	for i, want := range []string{`{"msg":"a"}`, "plain b"} {
		l := runs[i]
		if string(l.Line) != want || l.Service != "api" || l.RunID != "r1" || string(l.Input) != `{"format":"auto"}` {
			t.Fatalf("run line %d = %+v (line %q)", i, l, l.Line)
		}
	}
	if logs.count() != 1 {
		t.Fatalf("mapped log lines = %d; want only the ordinary one", logs.count())
	}
}

func TestReceiverRunScopeUnsetMapsNormally(t *testing.T) {
	logs := &sink{}
	srv := httptest.NewServer(NewReceiver(logs.collect, nil, fixedNow))
	defer srv.Close()
	post(t, srv.URL+"/v1/logs", ctProto, mustProto(t, mixedRequest()), nil)
	if logs.count() != 3 {
		t.Fatalf("mapped log lines = %d; want 3", logs.count())
	}
}
