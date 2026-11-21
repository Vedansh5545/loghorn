package svcconf

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var keys = []string{"shop-api", "a.b_c", "ns/svc", "..", ".", ".hidden", "%", "日本", "*", "../x", "a/../../b", strings.Repeat("k", 300), "a b\x00c"}

func TestBase(t *testing.T) {
	if got, err := Base("/x", "/h"); err != nil || got != "/x/loghorn" {
		t.Errorf("xdg: %q %v", got, err)
	}
	if got, err := Base("", "/h"); err != nil || got != "/h/.config/loghorn" {
		t.Errorf("home: %q %v", got, err)
	}
	if _, err := Base("", ""); err == nil {
		t.Error("expected error")
	}
}

func TestBaseFromEnv(t *testing.T) {
	t.Setenv("HOME", "/h")
	t.Setenv("XDG_CONFIG_HOME", "/x")
	if got, err := BaseFromEnv(); err != nil || got != "/x/loghorn" {
		t.Errorf("xdg: %q %v", got, err)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	if got, err := BaseFromEnv(); err != nil || got != "/h/.config/loghorn" {
		t.Errorf("home: %q %v", got, err)
	}
}

func TestEscapeKeyRoundTrip(t *testing.T) {
	for _, k := range keys {
		got, err := UnescapeKey(EscapeKey(k))
		if err != nil || got != k {
			t.Errorf("%q -> %q -> %q, %v", k, EscapeKey(k), got, err)
		}
	}
	if EscapeKey("") != "default" {
		t.Error("empty key")
	}
	if EscapeKey("ns/svc") != "ns%2Fsvc" {
		t.Errorf("ns/svc = %q", EscapeKey("ns/svc"))
	}
	if EscapeKey("..") != "%2E." {
		t.Errorf(".. = %q", EscapeKey(".."))
	}
	for _, bad := range []string{"%", "%2", "%zz", "%2e"} {
		if _, err := UnescapeKey(bad); err == nil {
			t.Errorf("UnescapeKey(%q) accepted", bad)
		}
	}
}

func TestServiceDirStaysUnderBase(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "services")
	for _, k := range append([]string{""}, keys...) {
		rel, err := filepath.Rel(root, Dir(base, k))
		if err != nil {
			t.Fatal(err)
		}
		if rel == "" || rel == "." || strings.ContainsRune(rel, filepath.Separator) {
			t.Errorf("key %q -> rel %q is not a single element", k, rel)
		}
		for _, el := range strings.Split(rel, string(filepath.Separator)) {
			if el == ".." || el == "." {
				t.Errorf("key %q -> rel %q has a dot element", k, rel)
			}
		}
		if strings.HasPrefix(rel, ".") {
			t.Errorf("key %q -> hidden element %q", k, rel)
		}
	}
}

func TestPinsRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "svc")
	got, err := LoadPins(dir)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("absent: %#v, %v", got, err)
	}
	in := []Pin{{"otel", "db"}, {"otel", "sweep orders"}, {"otel", "db"}}
	if err := SavePins(dir, in); err != nil {
		t.Fatal(err)
	}
	got, err = LoadPins(dir)
	want := []Pin{{"otel", "db"}, {"otel", "sweep orders"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, %v", got, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "pins.toml"))
	if !strings.Contains(string(b), "[[pin]]") {
		t.Errorf("file:\n%s", b)
	}
	if err := SavePins(dir, []Pin{{"bogus", "x"}}); !errors.Is(err, ErrBadPin) {
		t.Errorf("bogus source: %v", err)
	}
	if err := SavePins(dir, []Pin{{"log", ""}}); !errors.Is(err, ErrBadPin) {
		t.Errorf("empty kind: %v", err)
	}
	if got, _ := LoadPins(dir); !reflect.DeepEqual(got, want) {
		t.Errorf("file changed by a rejected save: %v", got)
	}
}
