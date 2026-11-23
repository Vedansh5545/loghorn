package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// cliEnv is what a command reads and writes, injectable for tests.
type cliEnv struct {
	stdin          *os.File
	stdout, stderr io.Writer
}

func stdEnv() cliEnv { return cliEnv{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr} }

// topHelp is bare loghorn's and `loghorn help`'s text. README.md carries a
// copy between <!-- loghorn help --> markers; a test keeps them equal.
const topHelp = `loghorn — capture, store and query the logs and traces of local apps

Usage: loghorn <command> [flags] [args]

Store
  serve            run the store: receive OTLP, serve the web explorer

Capture
  run -- <cmd>     run a command, passing its output through, and record it
  open <log>       open a finished log: a file, or a GitHub Actions run or job

Read
  ui               open the web explorer in your browser
  query <url>      print a web view's data as TSV, CSV, Markdown, JSON, ...
  status           show serve's state and the OTEL environment for apps
  config           show the config file in effect

Shortcuts: 'loghorn -- <cmd>' is 'loghorn run'; 'loghorn <url>' is 'loghorn query'.
Run 'loghorn help <command>' for its flags.
`

// pipeHint follows the help when bare loghorn has input waiting on stdin,
// which earlier versions recorded.
const pipeHint = "to record piped input use 'loghorn run'; to view a file use 'loghorn open'\n"

// commands maps each command to its entry point. Each parses its own flags
// and returns the process's exit status.
func commands() map[string]func(args []string, env cliEnv) int {
	return map[string]func([]string, cliEnv) int{
		"serve":  cmdServe,
		"run":    cmdRun,
		"open":   cmdOpen,
		"query":  cmdQuery,
		"status": cmdStatus,
		"ui":     cmdUI,
		"config": cmdConfig,
		"help":   cmdHelp,
	}
}

// dispatch routes the arguments after the program name to a command.
func dispatch(args []string, env cliEnv) int {
	if len(args) == 0 {
		fmt.Fprint(env.stderr, topHelp)
		if stdinHasInput(env.stdin) {
			fmt.Fprint(env.stderr, "\n"+pipeHint)
		}
		return 2
	}
	first := args[0]
	switch {
	case first == "--":
		return cmdRun(args, env)
	case isQueryURL(first):
		return cmdQuery(args, env)
	case first == "-h" || first == "-help" || first == "--help":
		fmt.Fprint(env.stdout, topHelp)
		return 0
	}
	if run, ok := commands()[first]; ok {
		return run(args[1:], env)
	}
	if strings.HasPrefix(first, "-") {
		name := strings.TrimLeft(first, "-")
		name, _, _ = strings.Cut(name, "=")
		if msg, ok := migrationMessage(name); ok {
			fmt.Fprintln(env.stderr, "loghorn: "+msg)
			return 2
		}
		fmt.Fprintf(env.stderr, "loghorn: unknown flag %s; run 'loghorn help' for the commands\n", first)
		return 2
	}
	fmt.Fprintf(env.stderr, "loghorn: unknown command %q; to run a command use 'loghorn run -- %s …'\n", first, first)
	return 2
}

// stdinHasInput reports whether stdin is a pipe, socket or regular file:
// input someone might have expected bare loghorn to record.
func stdinHasInput(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice == 0 && (fi.Mode().IsRegular() || fi.Mode()&(os.ModeNamedPipe|os.ModeSocket) != 0)
}

func cmdHelp(args []string, env cliEnv) int {
	if len(args) == 0 {
		fmt.Fprint(env.stdout, topHelp)
		return 0
	}
	run, ok := commands()[args[0]]
	if !ok || args[0] == "help" {
		fmt.Fprintf(env.stderr, "loghorn: unknown command %q; the commands are %s\n", args[0], commandNames())
		return 2
	}
	run([]string{"-h"}, cliEnv{stdin: env.stdin, stdout: env.stdout, stderr: env.stdout})
	return 0
}

func commandNames() string {
	var names []string
	for n := range commands() {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// migrations names where each removed top-level flag went.
var migrations = map[string]string{
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

func migrationMessage(name string) (string, bool) {
	m, ok := migrations[name]
	return m, ok
}

// newFlagSet returns a command's flag set. usage is the synopsis line(s) and
// a sentence on what the command does; the flags follow it in -h.
func newFlagSet(name, usage string, env cliEnv) *flag.FlagSet {
	fs := flag.NewFlagSet("loghorn "+name, flag.ContinueOnError)
	fs.SetOutput(env.stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usage)
		if !strings.HasSuffix(usage, "\n") {
			fmt.Fprintln(fs.Output())
		}
		fmt.Fprintln(fs.Output(), "\nFlags:")
		fs.PrintDefaults()
	}
	return fs
}

// parseCmd parses a command's flags: interspersed with its arguments, or (for
// run, whose arguments are a command line) only before the first argument.
// ok is false when the command must return code: 0 after -h, 2 after a bad
// flag (the flag package has already printed why).
func parseCmd(fs *flag.FlagSet, args []string, interspersed bool) (pos []string, code int, ok bool) {
	var err error
	if interspersed {
		pos, err = parseInterspersed(fs, args)
	} else {
		err = fs.Parse(args)
		pos = fs.Args()
	}
	switch {
	case errors.Is(err, flag.ErrHelp):
		return nil, 0, false
	case err != nil:
		return nil, 2, false
	}
	return pos, 0, true
}

// parseInterspersed parses flags wherever they appear among the arguments
// and returns the arguments that aren't flags, in order. A "--" ends the
// flags: everything after it is an argument.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		consumed := len(args) - len(rest)
		if consumed > 0 && args[consumed-1] == "--" {
			return append(pos, rest...), nil
		}
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// usageError prints a command's usage error and returns 2.
func usageError(env cliEnv, format string, a ...any) int {
	fmt.Fprintf(env.stderr, "loghorn: "+format+"\n", a...)
	return 2
}
