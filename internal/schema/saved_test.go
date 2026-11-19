package schema

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func evalDir(t *testing.T) string {
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestFindConfigDirInRepo(t *testing.T) {
	root := evalDir(t)
	repo := filepath.Join(root, "repo")
	mkdirs(t, filepath.Join(repo, ".git"), filepath.Join(repo, ".config", "loghorn"), filepath.Join(repo, "backend", "src"))
	dir, found, err := FindConfigDir(filepath.Join(repo, "backend", "src"))
	if err != nil || !found || dir != filepath.Join(repo, ".config", "loghorn") {
		t.Fatalf("got %q %v %v", dir, found, err)
	}
}

func TestFindConfigDirWorktreeGitFile(t *testing.T) {
	root := evalDir(t)
	wt := filepath.Join(root, "wt")
	mkdirs(t, filepath.Join(wt, "sub"), filepath.Join(root, ".config", "loghorn"))
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, found, err := FindConfigDir(filepath.Join(wt, "sub"))
	if err != nil || found || dir != filepath.Join(wt, ".config", "loghorn") {
		t.Fatalf("got %q %v %v", dir, found, err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("FindConfigDir created the directory")
	}
}

func TestFindConfigDirStopsAtGitRoot(t *testing.T) {
	root := evalDir(t)
	mkdirs(t, filepath.Join(root, ".config", "loghorn"), filepath.Join(root, "repo", ".git"), filepath.Join(root, "repo", "a"))
	dir, found, err := FindConfigDir(filepath.Join(root, "repo", "a"))
	if err != nil || found || dir != filepath.Join(root, "repo", ".config", "loghorn") {
		t.Fatalf("got %q %v %v", dir, found, err)
	}
}

func TestFindConfigDirIgnoresHome(t *testing.T) {
	root := evalDir(t)
	home, xdg := filepath.Join(root, "home"), filepath.Join(root, "xdg")
	mkdirs(t, filepath.Join(home, "loghorn"), filepath.Join(xdg, "loghorn"), filepath.Join(home, ".config", "loghorn"), filepath.Join(root, "repo", ".git"))
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir, found, err := FindConfigDir(filepath.Join(root, "repo"))
	if err != nil || found || dir != filepath.Join(root, "repo", ".config", "loghorn") {
		t.Fatalf("got %q %v %v", dir, found, err)
	}
}

func sampleSchema() Schema {
	return Schema{
		Stream:  "pino",
		Columns: []string{"time", "msg"},
		Fields: []Field{
			{Path: "msg", Type: TString, Visualizer: VRaw, Label: "Message"},
			{Path: "time", Type: TTimestamp, Unit: UnitNs, Visualizer: VLocalTime},
			{Path: "took", Type: TDuration, Unit: UnitMs, Visualizer: VDuration, Hidden: true},
		},
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := filepath.Join(evalDir(t), ".config", "loghorn")
	s := sampleSchema()
	path, err := SaveSchema(dir, s)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "schemas", "pino.toml") {
		t.Fatalf("path %q", path)
	}
	got, err := LoadSaved(dir, "pino")
	if err != nil || got == nil {
		t.Fatalf("load: %v %v", got, err)
	}
	for i := range got.Fields {
		if !got.Fields[i].Saved {
			t.Errorf("field %s not marked saved", got.Fields[i].Path)
		}
		got.Fields[i].Saved = false
	}
	if !reflect.DeepEqual(*got, s) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", *got, s)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "schemas"))
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
	if got, err := LoadSaved(dir, "absent"); got != nil || err != nil {
		t.Fatalf("absent: %v %v", got, err)
	}
}

func TestSaveRefusesBadStream(t *testing.T) {
	dir := evalDir(t)
	for _, name := range []string{"../evil", "a/b", ".", "UPPER", ""} {
		s := sampleSchema()
		s.Stream = name
		if _, err := SaveSchema(dir, s); !errors.Is(err, ErrUnsavableStream) {
			t.Errorf("%q: err %v", name, err)
		}
		if got, err := LoadSaved(dir, name); got != nil || err != nil {
			t.Errorf("load %q: %v %v", name, got, err)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("temp dir not empty: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.toml")); err == nil {
		t.Fatal("traversal wrote a file")
	}
}

func TestSaveRejectsUnknownVisualizer(t *testing.T) {
	dir := evalDir(t)
	s := sampleSchema()
	s.Fields[0].Visualizer = "sparkle"
	_, err := SaveSchema(dir, s)
	if err == nil || !contains(err.Error(), `"msg"`) {
		t.Fatalf("err %v", err)
	}
	s = sampleSchema()
	s.Fields[0].Type = "blob"
	if _, err = SaveSchema(dir, s); err == nil {
		t.Fatal("unknown type accepted")
	}
	s = sampleSchema()
	s.Fields[0].Unit = "hours"
	if _, err = SaveSchema(dir, s); err == nil {
		t.Fatal("bad unit accepted")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("files written: %v", entries)
	}
}

func TestLoadSavedRejectsUnknownVisualizer(t *testing.T) {
	dir := evalDir(t)
	mkdirs(t, filepath.Join(dir, "schemas"))
	body := "stream = \"x\"\n[[field]]\npath = \"a\"\ntype = \"string\"\nvisualizer = \"sparkle\"\n"
	if err := os.WriteFile(filepath.Join(dir, "schemas", "x.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadSaved(dir, "x")
	if err == nil || !contains(err.Error(), "x.toml") || !contains(err.Error(), `"a"`) {
		t.Fatalf("err %v", err)
	}
}

func TestLoadSavedDefaultsMissingVisualizer(t *testing.T) {
	dir := evalDir(t)
	mkdirs(t, filepath.Join(dir, "schemas"))
	body := "stream = \"x\"\n[[field]]\npath = \"ts\"\ntype = \"timestamp\"\nunit = \"ms\"\n"
	if err := os.WriteFile(filepath.Join(dir, "schemas", "x.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSaved(dir, "x")
	if err != nil {
		t.Fatal(err)
	}
	if want := DefaultVisualizer("ts", TTimestamp); got.Fields[0].Visualizer != want || want == "" {
		t.Fatalf("visualizer = %q, want %q", got.Fields[0].Visualizer, want)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestEffectiveMerge(t *testing.T) {
	inferred := Schema{
		Stream:  "pino",
		Columns: []string{"a", "b", "c"},
		Fields: []Field{
			{Path: "a", Type: TString, Inferred: TString, Visualizer: VRaw, Count: 5},
			{Path: "b", Type: TInt, Inferred: TInt, Visualizer: VNumber, Count: 5},
			{Path: "c", Type: TString, Inferred: TString, Visualizer: VRaw, Count: 5},
			{Path: "fresh", Type: TString, Inferred: TString, Visualizer: VRaw, Count: 2},
		},
	}
	saved := &Schema{
		Stream: "pino",
		Fields: []Field{
			{Path: "a", Type: TJSON, Visualizer: VJSON, Label: "A!"},
			{Path: "b", Type: TInt, Visualizer: VNumber, Hidden: true},
			{Path: "c", Type: TString, Visualizer: VRaw},
			{Path: "gone", Type: TInt, Visualizer: VNumber},
		},
	}
	eff := Effective(inferred, saved)
	a, _ := eff.Field("a")
	if a.Type != TJSON || a.Visualizer != VJSON || a.Label != "A!" || !a.Saved || a.Count != 5 || a.Inferred != TString {
		t.Errorf("override: %+v", a)
	}
	if f, _ := eff.Field("fresh"); !f.New || f.Saved {
		t.Errorf("fresh: %+v", f)
	}
	if f, ok := eff.Field("gone"); !ok || f.Count != 0 || !f.Saved || f.New {
		t.Errorf("gone: %+v ok=%v", f, ok)
	}
	if f, ok := eff.Field("b"); !ok || !f.Hidden {
		t.Errorf("hidden field must stay in Fields: %+v", f)
	}
	if want := []string{"a", "c"}; !reflect.DeepEqual(eff.Columns, want) {
		t.Errorf("columns %v want %v", eff.Columns, want)
	}
	if len(eff.Fields) != 5 {
		t.Errorf("fields %d", len(eff.Fields))
	}

	// No saved schema: nothing is New.
	plain := Effective(inferred, nil)
	for _, f := range plain.Fields {
		if f.New || f.Saved {
			t.Errorf("nil saved flagged %+v", f)
		}
	}
	// Saved columns win.
	saved.Columns = []string{"c", "a"}
	if got := Effective(inferred, saved).Columns; !reflect.DeepEqual(got, []string{"c", "a"}) {
		t.Errorf("saved columns %v", got)
	}
}

func TestFindConfigDirOutsideGitIgnoresAncestors(t *testing.T) {
	root := evalDir(t)
	start := filepath.Join(root, "scratch", "deep")
	mkdirs(t, start, filepath.Join(root, ".config", "loghorn"))
	t.Setenv("HOME", root)
	dir, found, err := FindConfigDir(start)
	if err != nil || found || dir != filepath.Join(start, ".config", "loghorn") {
		t.Fatalf("got %q %v %v", dir, found, err)
	}
}

func TestSavedWidthRoundTrip(t *testing.T) {
	dir := filepath.Join(evalDir(t), ".config", "loghorn")
	s := sampleSchema()
	s.Fields[0].Width = 140
	if _, err := SaveSchema(dir, s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSaved(dir, "pino")
	if err != nil || got == nil || got.Fields[0].Width != 140 {
		t.Fatalf("got %+v %v", got, err)
	}
	eff := Effective(s, got)
	if eff.Fields[0].Width != 140 {
		t.Fatalf("effective width %d", eff.Fields[0].Width)
	}
}

// A zero (automatic) width is not written, so files without widths keep
// their old form.
func TestSaveOmitsZeroWidth(t *testing.T) {
	dir := filepath.Join(evalDir(t), ".config", "loghorn")
	path, err := SaveSchema(dir, sampleSchema())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "width") {
		t.Fatalf("zero width written:\n%s", data)
	}
}

func TestLoadSavedWithoutWidth(t *testing.T) {
	dir := evalDir(t)
	mkdirs(t, filepath.Join(dir, "schemas"))
	body := "stream = \"x\"\n[[field]]\npath = \"a\"\ntype = \"string\"\nvisualizer = \"raw\"\n"
	if err := os.WriteFile(filepath.Join(dir, "schemas", "x.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSaved(dir, "x")
	if err != nil || got.Fields[0].Width != 0 {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestValidateRejectsNegativeWidth(t *testing.T) {
	s := sampleSchema()
	s.Fields[0].Width = -1
	if _, err := SaveSchema(evalDir(t), s); err == nil {
		t.Fatal("negative width accepted")
	}
}

func TestPlainAndLinkVisualizersValid(t *testing.T) {
	for _, v := range []string{VPlain, VLink} {
		s := sampleSchema()
		s.Fields[0].Visualizer = v
		if _, err := SaveSchema(evalDir(t), s); err != nil {
			t.Errorf("%s rejected: %v", v, err)
		}
	}
}

// A column set to wrap keeps that setting through save, load and merge;
// crop (the default) is not written.
func TestSavedWrapRoundTrip(t *testing.T) {
	dir := filepath.Join(evalDir(t), ".config", "loghorn")
	s := sampleSchema()
	s.Fields[0].Wrap = true
	path, err := SaveSchema(dir, s)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "wrap") != 1 {
		t.Fatalf("want one wrap line:\n%s", data)
	}
	got, err := LoadSaved(dir, "pino")
	if err != nil || got == nil || !got.Fields[0].Wrap {
		t.Fatalf("got %+v %v", got, err)
	}
	if eff := Effective(sampleSchema(), got); !eff.Fields[0].Wrap {
		t.Fatal("effective schema lost wrap")
	}
}

func TestValidStreamAllowsDots(t *testing.T) {
	for _, s := range []string{"auction.db", "a..b", "0x", "a-b_c"} {
		if !ValidStream(s) {
			t.Errorf("ValidStream(%q) = false, want true", s)
		}
	}
	for _, s := range []string{".hidden", "..", ".", "a/b", "UPPER", "", "-a"} {
		if ValidStream(s) {
			t.Errorf("ValidStream(%q) = true, want false", s)
		}
	}
	dir := t.TempDir()
	if _, err := SaveSchema(dir, Schema{Stream: "auction.db", Columns: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSaved(dir, "auction.db")
	if err != nil || got == nil || got.Stream != "auction.db" {
		t.Fatalf("LoadSaved = %+v, %v", got, err)
	}
}
