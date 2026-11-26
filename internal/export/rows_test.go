package export

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"
)

func TestWriteRows(t *testing.T) {
	cols := []string{"service", "n", "tags"}
	rows := [][]any{{"api", int64(2), json.RawMessage(`["a"]`)}, {nil, math.Inf(1), nil}, {"tab\there", int64(0), nil}}
	cases := map[Format]string{
		TSV:   "service\tn\ttags\napi\t2\t[\"a\"]\n\tInfinity\t\ntab\\there\t0\t\n",
		CSV:   "service,n,tags\napi,2,\"[\"\"a\"\"]\"\n,Infinity,\ntab\there,0,\n",
		MD:    "| service | n | tags |\n| --- | --- | --- |\n| api | 2 | [\"a\"] |\n|  | Infinity |  |\n| tab\there | 0 |  |\n",
		JSONL: "{\"service\":\"api\",\"n\":2,\"tags\":[\"a\"]}\n{\"service\":null,\"n\":\"Infinity\",\"tags\":null}\n{\"service\":\"tab\\there\",\"n\":0,\"tags\":null}\n",
		JSON:  "[\n{\"service\":\"api\",\"n\":2,\"tags\":[\"a\"]},\n{\"service\":null,\"n\":\"Infinity\",\"tags\":null},\n{\"service\":\"tab\\there\",\"n\":0,\"tags\":null}\n]\n",
	}
	for f, want := range cases {
		var b bytes.Buffer
		if err := WriteRows(&b, f, cols, rows); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if b.String() != want {
			t.Errorf("%s:\n got %q\nwant %q", f, b.String(), want)
		}
	}
	var b bytes.Buffer
	if err := WriteRows(&b, JSON, cols, nil); err != nil || b.String() != "[]\n" {
		t.Errorf("empty JSON = %q, %v", b.String(), err)
	}
	for _, f := range []Format{YAML, TOON, SQL} {
		if err := WriteRows(&b, f, cols, rows); err == nil {
			t.Errorf("%s: no error", f)
		}
	}
}
