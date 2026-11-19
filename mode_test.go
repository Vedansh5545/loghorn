package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jimbeveridge/loghorn/internal/ghjob"
)

func TestRemovedFlags(t *testing.T) {
	const theme = "-theme was removed along with the terminal UI"
	const scrollback = "-scrollback was removed along with the terminal UI"
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-theme", "dark"}, theme},
		{[]string{"--scrollback=9"}, scrollback},
		{[]string{"-web", "--theme=light"}, theme},
		{[]string{"-scrollback", "9"}, scrollback},
		{[]string{"--theme", "dark"}, theme},
		{[]string{"-scrollback=9", "--", "x"}, scrollback},
		{[]string{"--", "prog", "-theme"}, ""},
		{[]string{"-web"}, ""},
		{[]string{"-themes"}, ""},
		{nil, ""},
	} {
		err := removedFlagError(tc.args)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("removedFlagError(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestModeCheck(t *testing.T) {
	for _, tc := range []struct {
		m    mode
		want string
	}{
		{mode{kind: kindNone}, "nothing to read: give a command after --, pipe input into loghorn, or pass -web to browse the stored logs"},
		{mode{kind: kindFile}, "reading a file (loghorn < file) needs -web or -filter"},
		{mode{kind: kindHistorical}, "-historical needs -web or -filter"},
		{mode{kind: kindGHJob}, "-gh-job needs -web or -filter"},
		{mode{kind: kindGHRun}, "-gh-run needs -web or -filter"},
		{mode{kind: kindNone, filter: true}, "nothing to read: give a command after --, pipe input into loghorn, or pass -web to browse the stored logs"},

		{mode{kind: kindCommand}, ""},
		{mode{kind: kindPipe}, ""},
		{mode{kind: kindFile, filter: true}, ""},
		{mode{kind: kindFile, web: true}, ""},
		{mode{kind: kindHistorical, web: true, filter: true}, ""},
		{mode{kind: kindGHJob, filter: true}, ""},
		{mode{kind: kindNone, web: true}, ""},
		{mode{kind: kindNone, web: true, filter: true}, ""},
	} {
		err := tc.m.check()
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%+v.check() = %q, want %q", tc.m, got, tc.want)
		}
	}
}

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
