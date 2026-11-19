package otlp

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestNewRunID(t *testing.T) {
	re := regexp.MustCompile(`^20261119T204012Z-[0-9a-f]{4}$`)
	if got := NewRunID(time.Date(2026, 11, 19, 20, 40, 12, 0, time.UTC)); !re.MatchString(got) {
		t.Errorf("NewRunID = %q", got)
	}
	zone := time.FixedZone("x", -5*3600)
	if got := NewRunID(time.Date(2026, 11, 19, 15, 40, 12, 0, zone)); !re.MatchString(got) {
		t.Errorf("non-UTC NewRunID = %q", got)
	}
}

func envMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := m[k]; dup {
			t.Errorf("key %s appears twice in %q", k, env)
		}
		m[k] = v
	}
	return m
}

func TestChildEnvFresh(t *testing.T) {
	got := ChildEnv([]string{"PATH=/bin"}, "http://127.0.0.1:4000", "R")
	want := []string{
		"PATH=/bin",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=http://127.0.0.1:4000/v1/logs",
		"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL=http/protobuf",
		"OTEL_LOGS_EXPORTER=otlp",
		"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4000",
		"OTEL_RESOURCE_ATTRIBUTES=loghorn.run.id=R",
		"LOGHORN_RUN_ID=R",
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestChildEnvKeepsResourceAttributes(t *testing.T) {
	m := envMap(t, ChildEnv([]string{"OTEL_RESOURCE_ATTRIBUTES=service.name=api,team=x"}, "http://127.0.0.1:1", "R"))
	if got, want := m["OTEL_RESOURCE_ATTRIBUTES"], "service.name=api,team=x,loghorn.run.id=R"; got != want {
		t.Errorf("OTEL_RESOURCE_ATTRIBUTES = %q, want %q", got, want)
	}
}

func TestChildEnvLeavesGenericEndpoint(t *testing.T) {
	m := envMap(t, ChildEnv([]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4318",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=http://x/v1/logs",
	}, "http://127.0.0.1:1", "R"))
	if got := m["OTEL_EXPORTER_OTLP_ENDPOINT"]; got != "http://collector:4318" {
		t.Errorf("generic endpoint = %q, want kept", got)
	}
	if got := m["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"]; got != "http://127.0.0.1:1/v1/logs" {
		t.Errorf("logs endpoint = %q, want replaced", got)
	}
	m = envMap(t, ChildEnv([]string{"OTEL_EXPORTER_OTLP_ENDPOINT="}, "http://127.0.0.1:1", "R"))
	if got := m["OTEL_EXPORTER_OTLP_ENDPOINT"]; got != "http://127.0.0.1:1" {
		t.Errorf("empty generic endpoint = %q, want replaced", got)
	}
}
