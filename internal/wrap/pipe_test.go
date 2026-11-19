package wrap

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/ingest"
	"github.com/jimbeveridge/loghorn/internal/recorder"
)

// Ported from the old headless package: Pipe with Filter set is the old headless.Run.

// pipeFilter runs Pipe as a filter with in's framing and field settings and
// returns what it printed and every record the sink saw.
func pipeFilter(t *testing.T, r io.Reader, w io.Writer, in config.Input) []string {
	t.Helper()
	s := &sink{}
	rec := recorder.New(recorder.Options{Sink: s.add, Input: in})
	err := Pipe(r, Options{
		Recorder: rec,
		Filter:   true,
		Frame: func(r io.Reader, emit func([]byte)) error {
			return ingest.Records(r, in.Format, emit)
		},
		Stdout: w,
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("Pipe error: %v", err)
	}
	return s.all()
}

func TestPipeEmitsOnlyImportant(t *testing.T) {
	input := strings.Join([]string{
		`{"severity":"INFO","textPayload":"routine"}`,
		`{"severity":"ERROR","textPayload":"boom"}`,
		`plain benign line`,
		`{"httpRequest":{"status":200}}`,
		`{"httpRequest":{"status":500},"textPayload":"down"}`,
		`something with a Traceback in it`,
	}, "\n") + "\n"

	var out bytes.Buffer
	pipeFilter(t, strings.NewReader(input), &out, config.Default().Input)
	got := out.String()

	for _, s := range []string{`"textPayload":"boom"`, `"textPayload":"down"`, "Traceback"} {
		if !strings.Contains(got, s) {
			t.Errorf("output missing important line %q\ngot:\n%s", s, got)
		}
	}
	if strings.Contains(got, "routine") || strings.Contains(got, "benign") {
		t.Errorf("output leaked a routine line\ngot:\n%s", got)
	}
}

// The log file must see the whole stream, not just what the filter prints, and
// multi-line YAML records must arrive whole.
func TestPipePassesEveryRecordToSink(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        []string
	}{
		{
			name:  "lines",
			input: "{\"severity\":\"INFO\",\"textPayload\":\"routine\"}\nplain benign line\n{\"severity\":\"ERROR\",\"textPayload\":\"boom\"}\n",
			want: []string{
				`{"severity":"INFO","textPayload":"routine"}`,
				`plain benign line`,
				`{"severity":"ERROR","textPayload":"boom"}`,
			},
		},
		{
			name:  "yaml",
			input: "---\nseverity: INFO\n---\nseverity: ERROR\n",
			want:  []string{"---\nseverity: INFO", "---\nseverity: ERROR"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pipeFilter(t, strings.NewReader(tc.input), io.Discard, config.Default().Input)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("sink got %q, want %q", got, tc.want)
			}
		})
	}
}

// A write failure must not stop records from reaching the sink: the log file
// is meant to keep the whole stream even when stdout has failed. Pipe returns
// only the read error, so the write failure is not reported.
func TestPipeKeepsFeedingSinkAfterWriteError(t *testing.T) {
	want := []string{
		`{"severity":"ERROR","textPayload":"first"}`,
		`{"severity":"ERROR","textPayload":"second"}`,
		`{"severity":"ERROR","textPayload":"third"}`,
	}
	input := strings.Join(want, "\n") + "\n"
	got := pipeFilter(t, strings.NewReader(input), failWriter{}, config.Default().Input)
	if !slices.Equal(got, want) {
		t.Fatalf("sink got %q, want %q", got, want)
	}
}

// A stored day whose first run wrote JSON lines and whose second wrote YAML
// documents still reads each YAML document back as one record.
func TestPipeStoredMixedDay(t *testing.T) {
	input := "{\"severity\":\"INFO\",\"textPayload\":\"routine\"}\n" +
		"---\nseverity: ERROR\ntextPayload: boom\n" +
		"---\nseverity: INFO\ntextPayload: fine\n"
	var out bytes.Buffer
	rec := recorder.New(recorder.Options{Input: config.Default().Input})
	if err := Pipe(strings.NewReader(input), Options{Recorder: rec, Filter: true, Frame: ingest.Stored, Stdout: &out}); err != nil {
		t.Fatal(err)
	}
	if want := "---\nseverity: ERROR\ntextPayload: boom\n"; out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}

func TestPipePassThrough(t *testing.T) {
	input := "{\"severity\":\"INFO\",\"textPayload\":\"routine\"}\r\nplain \033[1mbold\033[0m\n\xff raw\nlast"
	var out bytes.Buffer
	s := &sink{}
	rec := recorder.New(recorder.Options{Sink: s.add, Input: config.Default().Input})
	if err := Pipe(strings.NewReader(input), Options{Recorder: rec, StripANSI: true, Stdout: &out}); err != nil {
		t.Fatal(err)
	}
	if out.String() != input {
		t.Fatalf("stdout %q, want %q", out.String(), input)
	}
	want := []string{`{"severity":"INFO","textPayload":"routine"}`, "plain bold", "\xff raw", "last"}
	if got := s.all(); !slices.Equal(got, want) {
		t.Fatalf("sink %q, want %q", got, want)
	}
}

// pipeFixture runs a committed gcloud sample through Pipe as a filter.
func pipeFixture(t *testing.T, name string, in config.Input) (out string, records []string) {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	var buf strings.Builder
	records = pipeFilter(t, f, &buf, in)
	return buf.String(), records
}

// The JSON array fixture: four objects (the last truncated) plus one
// interleaved plain-text line.
func TestGcloudTailJSONFixture(t *testing.T) {
	out, records := pipeFixture(t, "gcloud-tail.json", config.Default().Input)

	if len(records) != 5 {
		t.Fatalf("got %d records, want 5 (4 objects + 1 plain-text line):\n%q", len(records), records)
	}
	if records[1] != "WARNING: gcloud notice interleaved on the same stream" {
		t.Errorf("record 1 = %q, want the plain-text line passed through", records[1])
	}
	if !strings.Contains(records[0], "not-a-brace") {
		t.Errorf("record 0 lost the braced string:\n%s", records[0])
	}
	if !strings.Contains(out, "Database: QUERY failed") {
		t.Errorf("severity 500 record should be important:\n%s", out)
	}
	if !strings.Contains(strings.Join(records, "\n"), "run.googleapis.com%2Frequests") {
		t.Errorf("the severity 200 record should still have been ingested")
	}
	if strings.Contains(out, "run.googleapis.com%2Frequests") {
		t.Errorf("severity 200 record should not be important:\n%s", out)
	}
	if !strings.Contains(out, `"status": 503`) {
		t.Errorf("http_request.status 503 should be important:\n%s", out)
	}
	if strings.Contains(out, "cut off by ctrl-c") {
		t.Errorf("the truncated record should not be important:\n%s", out)
	}
}

func TestGcloudReadYAMLFixture(t *testing.T) {
	out, records := pipeFixture(t, "gcloud-read.yaml", config.Default().Input)

	if len(records) != 2 {
		t.Fatalf("got %d records, want 2 documents:\n%q", len(records), records)
	}
	for i, r := range records {
		if !strings.HasPrefix(r, "---") {
			t.Errorf("record %d should keep its leading separator: %q", i, r)
		}
	}
	if !strings.Contains(out, "upstream unavailable") {
		t.Errorf("severity 500 document should be important:\n%s", out)
	}
	if !strings.Contains(strings.Join(records, "\n"), "returned 1 row") {
		t.Errorf("the severity 200 document should still have been ingested")
	}
	if strings.Contains(out, "returned 1 row") {
		t.Errorf("severity 200 document should not be important:\n%s", out)
	}
}

// Forcing text framing turns grouping off: the fixture's 40 lines become 40
// records.
func TestForcedTextFramingDisablesGrouping(t *testing.T) {
	in := config.Default().Input
	in.Format = config.FormatText
	_, records := pipeFixture(t, "gcloud-tail.json", in)
	if len(records) != 40 {
		t.Errorf("forced text framing emitted %d records, want 40 (one per line)", len(records))
	}
}
