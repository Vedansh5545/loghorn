// Package shipper sends the lines loghorn run records to loghorn serve, as
// OTLP/HTTP log records in the loghorn.run scope (see otlp.RunScope). It never
// blocks the caller: the wrapped command's output must keep flowing whether
// serve is fast, slow or gone, so a full queue drops lines and counts them.
package shipper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/otlp"
)

// Queue and batch limits.
const (
	queueLen   = 4096
	batchLines = 512
	batchWait  = 200 * time.Millisecond
	postTimout = 5 * time.Second
)

// Resource identifies the run on every batch.
type Resource struct {
	Service, RunID string
	Input          config.Input
}

type item struct {
	line []byte
	at   time.Time
}

// Shipper batches lines to one OTLP/HTTP endpoint.
type Shipper struct {
	url     string
	res     *resourcepb.Resource
	onError func(error)
	client  *http.Client

	q    chan item
	done chan struct{}

	mu      sync.Mutex
	dropped int
	failed  bool
}

// New starts a shipper posting to endpoint+"/v1/logs". onError is called once,
// for the first failed batch; nil ignores failures.
func New(endpoint string, r Resource, onError func(error)) *Shipper {
	input, _ := json.Marshal(r.Input)
	s := &Shipper{
		url: endpoint + "/v1/logs",
		res: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
			strKV(otlp.AttrService, r.Service),
			strKV(otlp.AttrRunID, r.RunID),
			strKV(otlp.AttrInput, string(input)),
		}},
		onError: onError,
		client:  &http.Client{Timeout: postTimout},
		q:       make(chan item, queueLen),
		done:    make(chan struct{}),
	}
	go s.loop()
	return s
}

func strKV(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

// Line queues a copy of rec. It never blocks: when the queue is full the line
// is dropped and counted.
func (s *Shipper) Line(rec []byte) {
	it := item{line: append([]byte(nil), rec...), at: time.Now()}
	select {
	case s.q <- it:
	default:
		s.mu.Lock()
		s.dropped++
		s.mu.Unlock()
	}
}

// Close sends what is queued, waiting at most timeout, and returns how many
// lines were not delivered (queue full, failed batches, or still queued at the
// deadline). Line must not be called after Close.
func (s *Shipper) Close(timeout time.Duration) (dropped int) {
	close(s.q)
	select {
	case <-s.done:
	case <-time.After(timeout):
		s.mu.Lock()
		s.dropped += len(s.q)
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

func (s *Shipper) loop() {
	defer close(s.done)
	var batch []item
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	flush := func() {
		if len(batch) > 0 {
			s.send(batch)
			batch = nil
		}
		timer.Stop()
	}
	for {
		select {
		case it, ok := <-s.q:
			if !ok {
				flush()
				return
			}
			if len(batch) == 0 {
				timer.Reset(batchWait)
			}
			batch = append(batch, it)
			if len(batch) >= batchLines {
				flush()
			}
		case <-timer.C:
			flush()
		}
	}
}

func (s *Shipper) send(batch []item) {
	recs := make([]*logspb.LogRecord, len(batch))
	for i, it := range batch {
		recs[i] = &logspb.LogRecord{
			TimeUnixNano: uint64(it.at.UnixNano()),
			Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: string(it.line)}},
		}
	}
	req := &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: s.res,
		ScopeLogs: []*logspb.ScopeLogs{{
			Scope:      &commonpb.InstrumentationScope{Name: otlp.RunScope},
			LogRecords: recs,
		}},
	}}}
	body, err := proto.Marshal(req)
	if err == nil {
		err = s.post(body)
	}
	if err != nil {
		s.mu.Lock()
		s.dropped += len(batch)
		first := !s.failed
		s.failed = true
		s.mu.Unlock()
		if first && s.onError != nil {
			s.onError(err)
		}
	}
}

func (s *Shipper) post(body []byte) error {
	resp, err := s.client.Post(s.url, "application/x-protobuf", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", s.url, resp.Status)
	}
	return nil
}
