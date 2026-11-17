package record

import (
	"bufio"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jimbeveridge/loghorn/internal/adapter"
	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/entry"
)

func norm(t *testing.T, line string, in config.Input) (Record, bool) {
	t.Helper()
	return Normalize(adapter.New(in).ParseLine([]byte(line)), in)
}

func fixture(t *testing.T, name string) []string {
	t.Helper()
	f, err := os.Open("../../testdata/web/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}

var auto = config.Default().Input

func TestNormalizePinoDB(t *testing.T) {
	r, ok := norm(t, fixture(t, "pino.log")[0], auto)
	if !ok {
		t.Fatal("not ok")
	}
	if r.Stream != "db" {
		t.Errorf("Stream = %q", r.Stream)
	}
	if got := r.Fields["databaseContext.metadata.operation"]; got != "CREATE" {
		t.Errorf("operation = %v", got)
	}
	if _, ok := r.Fields["timestamp"]; !ok {
		t.Error("no timestamp")
	}
	if _, ok := r.Fields["time"]; ok {
		t.Error("time key should be promoted to timestamp")
	}
	if r.Fields["labels.schema"] != "db" || r.Fields["severity"] != "INFO" {
		t.Errorf("labels.schema=%v severity=%v", r.Fields["labels.schema"], r.Fields["severity"])
	}
	if !r.Time.Equal(time.Date(2026, 11, 17, 17, 0, 1, 123e6, time.UTC)) {
		t.Errorf("Time = %v", r.Time)
	}
}

func paths(r Record, skip ...string) []string {
	var out []string
nextKey:
	for k := range r.Fields {
		for _, s := range skip {
			if k == s {
				continue nextKey
			}
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestNormalizeGcloudSameFieldsAsPino(t *testing.T) {
	pino, ok := norm(t, fixture(t, "pino.log")[1], auto)
	if !ok {
		t.Fatal("pino not ok")
	}
	gc, ok := norm(t, fixture(t, "gcloud.json")[0], auto)
	if !ok {
		t.Fatal("gcloud not ok")
	}
	for _, k := range paths(pino, "labels.schema", "severity", "timestamp") {
		if !reflect.DeepEqual(pino.Fields[k], gc.Fields[k]) {
			t.Errorf("field %q: pino %v, gcloud %v", k, pino.Fields[k], gc.Fields[k])
		}
	}
	for _, k := range paths(gc) {
		if strings.HasPrefix(k, "jsonPayload.") {
			t.Errorf("unexpected prefixed path %q", k)
		}
	}
	for _, k := range []string{"receiveTimestamp", "insertId", "resource.type", "logName", "resource.labels.service_name"} {
		if _, ok := gc.Fields[k]; !ok {
			t.Errorf("gcloud missing %q", k)
		}
	}
}

func TestNormalizeSnakeCase(t *testing.T) {
	line := fixture(t, "gcloud.json")[2]
	for _, naming := range []config.FieldNaming{config.NamingAuto, config.NamingSnake} {
		in := auto
		in.FieldNaming = naming
		r, ok := norm(t, line, in)
		if !ok {
			t.Fatalf("%s: not ok", naming)
		}
		if r.Fields["message"] != "upstream failed" || r.Fields["httpRequest.status"] != json.Number("503") || r.Fields["insertId"] != "i3" {
			t.Errorf("%s: fields = %v", naming, r.Fields)
		}
	}
	in := auto
	in.FieldNaming = config.NamingCamel
	r, ok := norm(t, line, in)
	if !ok {
		t.Fatal("camel: not ok")
	}
	if _, ok := r.Fields["message"]; ok {
		t.Error("camel: json_payload must not be unwrapped")
	}
	if _, ok := r.Fields["httpRequest.status"]; ok {
		t.Error("camel: http_request must not be promoted")
	}
	// Ruling R2: nothing is dropped; the keys stay payload.
	if r.Fields["json_payload.message"] != "upstream failed" || r.Fields["http_request.status"] != json.Number("503") {
		t.Errorf("camel: data hidden: %v", r.Fields)
	}
}

func TestNormalizeUnlabelled(t *testing.T) {
	for _, line := range []string{
		"> app@1.0.0 start",
		`{"severity":"INFO","message":"no labels"}`,
		`{"labels":{"schema":""},"message":"empty"}`,
	} {
		if _, ok := norm(t, line, auto); ok {
			t.Errorf("%q: want ok=false", line)
		}
	}
}

func TestNormalizeActionsPrefix(t *testing.T) {
	lines := fixture(t, "actions.log")
	r, ok := norm(t, lines[3], auto)
	if !ok {
		t.Fatal("prefixed pino line not ok")
	}
	if len(r.Raw) == 0 || r.Raw[0] != '{' {
		t.Errorf("Raw = %q", r.Raw)
	}
	if !r.Time.Equal(time.Date(2026, 11, 17, 17, 0, 31, 500e6, time.UTC)) {
		t.Errorf("Time = %v, want the JSON time", r.Time)
	}
	r, ok = norm(t, lines[4], auto)
	if !ok {
		t.Fatal("untimed prefixed line not ok")
	}
	if !r.Time.Equal(time.Date(2026, 11, 17, 17, 0, 32, 400000000, time.UTC)) {
		t.Errorf("Time = %v, want the prefix time", r.Time)
	}
	if r.Fields["severity"] != "WARNING" {
		t.Errorf("severity = %v (recomputed from stripped bytes)", r.Fields["severity"])
	}
	for _, i := range []int{0, 1, 2, 6} {
		if _, ok := norm(t, lines[i], auto); ok {
			t.Errorf("line %d is not a labelled record", i)
		}
	}
}

func TestNormalizeActionsSeverityNumeric(t *testing.T) {
	in := auto
	in.Severity = config.SeverityNumeric
	r, ok := norm(t, `2026-11-17T17:00:30.1234567Z {"severity":500,"labels":{"schema":"x"}}`, in)
	if !ok || r.Fields["severity"] != "ERROR" {
		t.Errorf("ok=%v severity=%v", ok, r.Fields["severity"])
	}
}

func TestNormalizeBigInt(t *testing.T) {
	r, ok := norm(t, `{"x":1763400000123456789,"labels":{"schema":"a"}}`, auto)
	if !ok {
		t.Fatal("not ok")
	}
	if got := r.Fields["x"]; got != json.Number("1763400000123456789") {
		t.Errorf("x = %#v", got)
	}
}

func TestNormalizeYAMLEntry(t *testing.T) {
	yaml := "---\nseverity: INFO\nlabels:\n  schema: db\njsonPayload:\n  rows: 3\n  big: 1763400000123456789\n  ratio: 0.5\n"
	r, ok := norm(t, yaml, auto)
	if !ok {
		t.Fatal("not ok")
	}
	if r.Fields["rows"] != json.Number("3") || r.Fields["ratio"] != json.Number("0.5") || r.Fields["big"] != json.Number("1763400000123456789") {
		t.Errorf("fields = %#v", r.Fields)
	}
}

func TestNormalizeArrayNotFlattened(t *testing.T) {
	r, ok := norm(t, fixture(t, "pino.log")[3], auto)
	if !ok {
		t.Fatal("not ok")
	}
	arr, isArr := r.Fields["tags"].([]any)
	if !isArr || len(arr) != 2 {
		t.Errorf("tags = %#v", r.Fields["tags"])
	}
	if _, ok := r.Fields["tags.0"]; ok {
		t.Error("array was flattened")
	}
}

func TestNormalizeCollision(t *testing.T) {
	r, ok := norm(t, `{"severity":"ERROR","labels":{"schema":"a"},"jsonPayload":{"severity":"high","labels":{"x":1},"k":2}}`, auto)
	if !ok {
		t.Fatal("not ok")
	}
	if r.Fields["severity"] != "ERROR" || r.Fields["jsonPayload.severity"] != "high" {
		t.Errorf("severity=%v jsonPayload.severity=%v", r.Fields["severity"], r.Fields["jsonPayload.severity"])
	}
	if r.Fields["jsonPayload.labels.x"] != json.Number("1") || r.Fields["k"] != json.Number("2") {
		t.Errorf("fields = %v", r.Fields)
	}
}

func TestNormalizeNoObject(t *testing.T) {
	if _, ok := Normalize(entry.Entry{Raw: []byte("plain")}, auto); ok {
		t.Error("plain text must not normalize")
	}
}

func TestStripActionsPrefix(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		rest string
		ns   int
	}{
		{"2026-11-17T17:00:30.3832868Z {\"a\":1}", true, `{"a":1}`, 383286800},
		{"2026-11-17T17:00:30Z hello", true, "hello", 0},
		{"2026-11-17T17:00:30.1Z x", true, "x", 100000000},
		{"2026-11-17T17:00:30.123456789Z x", true, "x", 123456789},
		{"2026-11-17T17:00:30.3832868 {}", false, "", 0},
		{"2026-11-17T17:00:30.Z x", false, "", 0},
		{"2026-11-17T17:00:30.1234567890Z x", false, "", 0},
		{"2026-11-17T17:00:30Zx", false, "", 0},
		{"abc 2026-11-17T17:00:30Z x", false, "", 0},
		{"2026-13-45T17:00:30Z x", false, "", 0},
		{"", false, "", 0},
	}
	for _, c := range cases {
		rest, tm, ok := StripActionsPrefix([]byte(c.in))
		if ok != c.ok {
			t.Errorf("%q: ok = %v", c.in, ok)
			continue
		}
		if !ok {
			if string(rest) != c.in {
				t.Errorf("%q: rest changed on failure", c.in)
			}
			continue
		}
		if string(rest) != c.rest || tm.Nanosecond() != c.ns || tm.Second() != 30 {
			t.Errorf("%q: rest=%q time=%v", c.in, rest, tm)
		}
	}
}
