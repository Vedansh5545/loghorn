package query

import (
	"testing"

	"github.com/jimbeveridge/loghorn/internal/schema"
)

func TestValidateOpTypeMismatch(t *testing.T) {
	s := schema.Schema{Stream: "a", Fields: []schema.Field{
		{Path: "n", Type: schema.TInt}, {Path: "s", Type: schema.TString},
		{Path: "d", Type: schema.TDuration}, {Path: "t", Type: schema.TTimestamp},
	}}
	bad := []Filter{
		{Field: "n", Op: "contains", Values: []string{"1"}},
		{Field: "s", Op: "<", Values: []string{"x"}},
		{Field: "n", Op: "=", Values: []string{"abc"}},
		{Field: "d", Op: ">", Values: []string{"fast"}},
		{Field: "t", Op: ">", Values: []string{"yesterday"}},
		{Field: "n", Op: "=", Values: nil},
		{Field: "n", Op: "empty", Values: []string{"1"}},
	}
	for _, f := range bad {
		if err := (Query{Filters: []Filter{f}}).Validate(s); err == nil {
			t.Errorf("%+v accepted", f)
		}
	}
	good := []Filter{
		{Field: "n", Op: "<=", Values: []string{"5"}},
		{Field: "d", Op: ">", Values: []string{"50ms"}},
		{Field: "t", Op: ">", Values: []string{"2026-11-17 10:00:00.000"}},
		{Field: "t", Op: "<", Values: []string{"2026-11-17T10:00:00Z"}},
		{Field: "unknown", Op: "contains", Values: []string{"x"}}, // absent => string
		{Field: "s", Op: "in", Values: []string{"a", "b"}},
		{Field: "n", Op: "not-empty"},
	}
	for _, f := range good {
		if err := (Query{Filters: []Filter{f}}).Validate(s); err != nil {
			t.Errorf("%+v rejected: %v", f, err)
		}
	}
	if err := (Query{Range: Range{From: "bogus"}}).Validate(s); err == nil {
		t.Error("bad range accepted")
	}
}

func TestOpsFor(t *testing.T) {
	if got := OpsFor(schema.TBool); len(got) != 3 || got[0] != "=" {
		t.Errorf("bool ops = %v", got)
	}
}
