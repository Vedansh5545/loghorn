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

func TestValidateRejectsNegativeWidth(t *testing.T) {
	if _, err := MergeFields(evalDir(t), "log", []Field{{Path: "a", Type: TInt, Visualizer: VNumber, Width: -1}}); err == nil {
		t.Fatal("negative width accepted")
	}
}

func TestPlainAndLinkVisualizersValid(t *testing.T) {
	for _, v := range []string{VPlain, VLink} {
		if _, err := MergeFields(evalDir(t), "log", []Field{{Path: "a", Type: TString, Visualizer: v}}); err != nil {
			t.Errorf("%s rejected: %v", v, err)
		}
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMergeFieldsKeepsOthers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := evalDir(t)
	if _, err := MergeFields(dir, "otel", []Field{{Path: "status", Type: TInt, Visualizer: VPlain}}); err != nil {
		t.Fatal(err)
	}
	path, err := MergeFields(dir, "otel", []Field{{Path: "latency", Type: TDuration, Unit: UnitMs, Visualizer: VDuration, Width: 120}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadFields(dir, "otel")
	if err != nil || len(got) != 2 || got[0].Path != "latency" || got[1].Path != "status" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if !got[0].Saved || got[0].Width != 120 {
		t.Errorf("latency = %+v", got[0])
	}
	if _, err := MergeFields(dir, "otel", []Field{{Path: "status", Type: TInt, Visualizer: VPlain, Label: "code"}}); err != nil {
		t.Fatal(err)
	}
	got, _ = LoadFields(dir, "otel")
	if len(got) != 2 || got[1].Label != "code" || got[0].Label != "" || got[0].Width != 120 {
		t.Fatalf("after re-merge: %+v", got)
	}
	if filepath.Base(path) != "fields-otel.toml" {
		t.Errorf("path = %s", path)
	}
	if _, err := os.Stat(filepath.Join(dir, "fields-log.toml")); !os.IsNotExist(err) {
		t.Errorf("log file touched: %v", err)
	}
	if strings.Contains(readFile(t, path), "width = 0") {
		t.Error("zero width written")
	}
	if got, err := LoadFields(dir, "log"); err != nil || got != nil {
		t.Errorf("log fields = %v, %v", got, err)
	}
}

func TestMergeFieldsBadFileNotOverwritten(t *testing.T) {
	dir := evalDir(t)
	p := filepath.Join(dir, "fields-log.toml")
	if err := os.WriteFile(p, []byte("[[field\ngarbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MergeFields(dir, "log", []Field{{Path: "a", Type: TInt, Visualizer: VNumber}}); err == nil {
		t.Fatal("expected error")
	}
	if readFile(t, p) != "[[field\ngarbage" {
		t.Error("bad file was overwritten")
	}
}

func TestMergeFieldsRejectsInvalid(t *testing.T) {
	dir := evalDir(t)
	_, err := MergeFields(dir, "log", []Field{{Path: "a", Type: TInt, Visualizer: "nope"}})
	if err == nil {
		t.Fatal("expected error")
	}
	if _, err := os.Stat(filepath.Join(dir, "fields-log.toml")); !os.IsNotExist(err) {
		t.Errorf("file written: %v", err)
	}
	if _, err := MergeFields(dir, "bogus", nil); !errors.Is(err, ErrBadSource) {
		t.Errorf("err = %v", err)
	}
}

func TestLoadFieldsDefaultsMissingVisualizer(t *testing.T) {
	dir := evalDir(t)
	if err := os.WriteFile(filepath.Join(dir, "fields-log.toml"), []byte("[[field]]\npath = \"x\"\ntype = \"int\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFields(dir, "log")
	if err != nil || len(got) != 1 || got[0].Visualizer == "" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestEffectiveSourceWide(t *testing.T) {
	inferred := Schema{
		Columns: []string{"a", "status", "secret"},
		Fields: []Field{
			{Path: "a", Type: TString, Visualizer: VRaw, Count: 3},
			{Path: "secret", Type: TString, Visualizer: VRaw, Count: 3},
			{Path: "status", Type: TString, Visualizer: VRaw, Count: 3},
		},
	}
	saved := []Field{
		{Path: "status", Type: TInt, Visualizer: VNumber},
		{Path: "other", Type: TInt, Visualizer: VNumber},
		{Path: "secret", Type: TString, Visualizer: VRaw, Hidden: true},
	}
	eff := Effective(inferred, saved)
	if f, ok := eff.Field("status"); !ok || f.Type != TInt || !f.Saved || f.Count != 3 {
		t.Errorf("status = %+v", f)
	}
	if _, ok := eff.Field("other"); ok {
		t.Error("absent saved path leaked in")
	}
	if !reflect.DeepEqual(eff.Columns, []string{"a", "status"}) {
		t.Errorf("columns = %v", eff.Columns)
	}
	if f, _ := eff.Field("a"); f.Saved {
		t.Error("untouched field marked saved")
	}
}

// A width-only save in one kind must not retype the same path in another.
func TestWidthOnlySaveKeepsInferredTypePerKind(t *testing.T) {
	dir := evalDir(t)
	if _, err := MergeFields(dir, "otel", []Field{{Path: "status", Width: 90}}); err != nil {
		t.Fatal(err)
	}
	saved, err := LoadFields(dir, "otel")
	if err != nil || len(saved) != 1 || saved[0].Type != "" || saved[0].Visualizer != "" {
		t.Fatalf("saved = %+v, %v", saved, err)
	}
	kindA := Schema{Stream: "a", Fields: []Field{{Path: "status", Type: TInt, Visualizer: VNumber}}}
	kindB := Schema{Stream: "b", Fields: []Field{{Path: "status", Type: TString, Visualizer: VPlain}}}
	for _, k := range []Schema{kindA, kindB} {
		f, _ := Effective(k, saved).Field("status")
		if f.Type != k.Fields[0].Type || f.Visualizer != k.Fields[0].Visualizer || f.Width != 90 || !f.Saved {
			t.Errorf("kind %s: %+v", k.Stream, f)
		}
	}
	// An explicit type edit is kept by a later width-only edit.
	if _, err := MergeFields(dir, "otel", []Field{{Path: "status", Type: TInt, Visualizer: VNumber, Width: 90}}); err != nil {
		t.Fatal(err)
	}
	if _, err := MergeFields(dir, "otel", []Field{{Path: "status", Width: 100}}); err != nil {
		t.Fatal(err)
	}
	saved, _ = LoadFields(dir, "otel")
	if saved[0].Type != TInt || saved[0].Visualizer != VNumber || saved[0].Width != 100 {
		t.Errorf("after merge: %+v", saved[0])
	}
}
