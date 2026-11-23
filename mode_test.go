package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jimbeveridge/loghorn/internal/ghjob"
)

func TestDetectInput(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()

	p := filepath.Join(t.TempDir(), "in.log")
	if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	for _, tc := range []struct {
		name       string
		args       []string
		stdin      *os.File
		historical bool
		ghJob      string
		ghRun      string
		want       inputKind
	}{
		{"command", []string{"true"}, devNull, false, "", "", kindCommand},
		{"command with piped stdin", []string{"true"}, r, false, "", "", kindCommand},
		{"regular file", nil, file, false, "", "", kindFile},
		{"pipe", nil, r, false, "", "", kindPipe},
		{"char device", nil, devNull, false, "", "", kindNone},
		{"historical", nil, devNull, true, "", "", kindHistorical},
		{"gh-job", nil, devNull, false, "1", "", kindGHJob},
		{"gh-run", nil, devNull, false, "", "1", kindGHRun},
	} {
		if got := detectInput(tc.args, tc.stdin, tc.historical, tc.ghJob, tc.ghRun); got != tc.want {
			t.Errorf("%s: detectInput = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDescribeJobs(t *testing.T) {
	jobs := []ghjob.Job{
		{ID: 11, Name: "lint", Conclusion: "success"},
		{ID: 22, Name: "test-linux", Conclusion: "failure"},
		{ID: 33, Name: "build", Status: "in_progress"},
	}
	var b strings.Builder
	describeJobs(&b, "900", jobs, 1)
	want := "loghorn: run 900 has 3 jobs; showing the first failed one (pick another with -gh-job <id>):\n" +
		"    11  lint        success\n" +
		"  * 22  test-linux  failure\n" +
		"    33  build       in_progress\n"
	if b.String() != want {
		t.Errorf("describeJobs (failed) =\n%s\nwant\n%s", b.String(), want)
	}

	ok := []ghjob.Job{
		{ID: 11, Name: "lint", Conclusion: "success"},
		{ID: 22, Name: "test", Conclusion: "success"},
	}
	b.Reset()
	describeJobs(&b, "901", ok, 0)
	want = "loghorn: run 901 has 2 jobs; showing the first one (pick another with -gh-job <id>):\n" +
		"  * 11  lint  success\n" +
		"    22  test  success\n"
	if b.String() != want {
		t.Errorf("describeJobs (none failed) =\n%s\nwant\n%s", b.String(), want)
	}
}
