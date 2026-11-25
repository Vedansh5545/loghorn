package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// devNull opens /dev/null for a test's stdin.
func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// dispatchIn runs dispatch with stdin and returns stdout, stderr and the code.
func dispatchIn(t *testing.T, stdin *os.File, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := dispatch(args, cliEnv{stdin: stdin, stdout: &out, stderr: &errb})
	return out.String(), errb.String(), code
}

// isolate points HOME and the data folder at empty temp dirs and returns them.
func isolate(t *testing.T) (home, data string) {
	t.Helper()
	home, data = t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LOGHORN_DATA_DIR", data)
	return home, data
}

func empty(t *testing.T, dir string) bool {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(ents) == 0
}

func TestNoArgsPrintsHelpExit2(t *testing.T) {
	const hint = "to record piped input use 'loghorn run'; to view a file use 'loghorn open'"
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	pw.WriteString("piped data\n")
	pw.Close()
	defer pr.Close()
	file := filepath.Join(t.TempDir(), "in.log")
	os.WriteFile(file, []byte("x\n"), 0o644)
	ff, _ := os.Open(file)
	defer ff.Close()

	for _, c := range []struct {
		name  string
		stdin *os.File
		hint  bool
	}{{"devnull", devNull(t), false}, {"pipe", pr, true}, {"file", ff, true}} {
		t.Run(c.name, func(t *testing.T) {
			home, data := isolate(t)
			_, stderr, code := dispatchIn(t, c.stdin)
			if code != 2 || !strings.HasPrefix(stderr, "loghorn — capture") {
				t.Fatalf("code %d stderr %q", code, stderr)
			}
			if got := strings.Contains(stderr, hint); got != c.hint {
				t.Fatalf("hint shown = %v; want %v\n%s", got, c.hint, stderr)
			}
			if !empty(t, home) || !empty(t, data) {
				t.Fatal("bare loghorn wrote files")
			}
		})
	}
}

func TestHelpExit0(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}, {"-help"}, {"help"}} {
		out, _, code := dispatchIn(t, devNull(t), args...)
		if code != 0 || out != topHelp {
			t.Fatalf("%q: code %d stdout %q", args, code, out)
		}
	}
	out, _, code := dispatchIn(t, devNull(t), "help", "run")
	if code != 0 || !strings.Contains(out, "-shutdown-grace") || !strings.Contains(out, "loghorn run") {
		t.Fatalf("help run: code %d stdout %q", code, out)
	}
	_, stderr, code := dispatchIn(t, devNull(t), "help", "nope")
	if code != 2 || !strings.Contains(stderr, `unknown command "nope"`) {
		t.Fatalf("help nope: code %d stderr %q", code, stderr)
	}
}

func TestUnknownCommand(t *testing.T) {
	_, stderr, code := dispatchIn(t, devNull(t), "npm", "test")
	want := `loghorn: unknown command "npm"; to run a command use 'loghorn run -- npm …'`
	if code != 2 || !strings.Contains(stderr, want) {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
}

func TestMigrationMessages(t *testing.T) {
	want := map[string]string{
		"web":            "-web was removed: run 'loghorn serve' for the web view; 'loghorn run' sends to it, and 'loghorn open <log>' shows a finished log",
		"web-port":       "-web-port is now a flag of 'loghorn serve' and 'loghorn open'",
		"otel":           "-otel was removed: 'loghorn run' always points the command's OpenTelemetry SDK at 'loghorn serve'",
		"filter":         "-filter is now a flag of 'loghorn run' and 'loghorn open'",
		"notify":         "-notify is now a flag of 'loghorn run'",
		"notify-max-age": "-notify-max-age is now a flag of 'loghorn run'",
		"notify-reset":   "-notify-reset is now a flag of 'loghorn run'",
		"notify-sound":   "-notify-sound is now a flag of 'loghorn run'",
		"shutdown-grace": "-shutdown-grace is now a flag of 'loghorn run'",
		"exclusive":      "-exclusive was removed: 'loghorn run' no longer writes a log file in the current directory",
		"historical":     "-historical was removed: 'loghorn serve' shows the stored days; 'loghorn open <file>' reads one",
		"gh-job":         "-gh-job is now a flag of 'loghorn open'",
		"gh-run":         "-gh-run is now a flag of 'loghorn open'",
		"config":         "-config is now a command: 'loghorn config'",
		"url":            "-url was removed: 'loghorn query <url>' takes the URL",
		"o":              "-o is now a flag of 'loghorn query'",
		"format":         "-format is now a flag of 'loghorn query'",
		"limit":          "-limit is now a flag of 'loghorn query'",
		"to-sql":         "-to-sql is now a flag of 'loghorn query'",
		"instances":      "-instances was removed: 'loghorn status' shows the running serve",
		"scrollback":     "-scrollback was removed along with the terminal UI",
		"theme":          "-theme was removed along with the terminal UI",
	}
	for name, msg := range want {
		for _, arg := range []string{"-" + name, "--" + name, "-" + name + "=x"} {
			_, stderr, code := dispatchIn(t, devNull(t), arg, "--", "true")
			if code != 2 || stderr != "loghorn: "+msg+"\n" {
				t.Fatalf("%s: code %d stderr %q; want %q", arg, code, stderr, msg)
			}
		}
	}
	_, stderr, code := dispatchIn(t, devNull(t), "-bogus")
	if code != 2 || !strings.Contains(stderr, "unknown flag -bogus") {
		t.Fatalf("-bogus: code %d stderr %q", code, stderr)
	}
}

func TestParseInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	o := fs.String("o", "", "")
	pos, err := parseInterspersed(fs, []string{"URL", "-o", "f.csv"})
	if err != nil || len(pos) != 1 || pos[0] != "URL" || *o != "f.csv" {
		t.Fatalf("pos %q o %q err %v", pos, *o, err)
	}
	fs = flag.NewFlagSet("t", flag.ContinueOnError)
	o = fs.String("o", "", "")
	pos, err = parseInterspersed(fs, []string{"-o", "f", "--", "-x", "y"})
	if err != nil || strings.Join(pos, " ") != "-x y" || *o != "f" {
		t.Fatalf("pos %q o %q err %v", pos, *o, err)
	}
}

func TestReadmeUsageMatchesHelp(t *testing.T) {
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?s)<!-- loghorn help -->\n```\n(.*?)```\n<!-- /loghorn help -->").FindSubmatch(b)
	if m == nil {
		t.Fatal("README has no <!-- loghorn help --> block")
	}
	if string(m[1]) != topHelp {
		t.Fatalf("README usage block differs from topHelp:\n%s\n---\n%s", m[1], topHelp)
	}
}
