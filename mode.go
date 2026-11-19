package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jimbeveridge/loghorn/internal/ghjob"
)

// inputKind is where a run's records come from.
type inputKind int

const (
	kindNone       inputKind = iota // nothing: no command, stdin a terminal or /dev/null
	kindCommand                     // a command after --
	kindPipe                        // stdin a pipe or socket
	kindFile                        // stdin redirected from a regular file
	kindHistorical                  // -historical
	kindGHJob                       // -gh-job
	kindGHRun                       // -gh-run
)

func (k inputKind) String() string {
	switch k {
	case kindNone:
		return "none"
	case kindCommand:
		return "command"
	case kindPipe:
		return "pipe"
	case kindFile:
		return "file"
	case kindHistorical:
		return "historical"
	case kindGHJob:
		return "gh-job"
	case kindGHRun:
		return "gh-run"
	}
	return fmt.Sprintf("inputKind(%d)", int(k))
}

// describe names the input in an error about a flag that can't go with it.
func (k inputKind) describe() string {
	switch k {
	case kindPipe:
		return "piped input"
	case kindFile:
		return "a file on stdin"
	case kindHistorical:
		return "-historical"
	case kindGHJob:
		return "-gh-job"
	case kindGHRun:
		return "-gh-run"
	}
	return k.String()
}

// mode is the input plus the output flags that decide what a run does.
type mode struct {
	kind              inputKind
	web, filter, otel bool
}

// check rejects a combination with nothing to show. A replay never writes the
// log file, so without -web or -filter it would read its input for nothing.
func (m mode) check() error {
	shown := m.web || m.filter
	if m.otel {
		switch m.kind {
		case kindCommand:
		case kindNone:
			return errors.New("-otel needs a command after --")
		default:
			return fmt.Errorf("-otel needs a command after --; it can't be used with %s", m.kind.describe())
		}
	}
	switch m.kind {
	case kindNone:
		if !m.web {
			return errors.New("nothing to read: give a command after --, pipe input into loghorn, or pass -web to browse the stored logs")
		}
	case kindFile:
		if !shown {
			return errors.New("reading a file (loghorn < file) needs -web or -filter")
		}
	case kindHistorical:
		if !shown {
			return errors.New("-historical needs -web or -filter")
		}
	case kindGHJob:
		if !shown {
			return errors.New("-gh-job needs -web or -filter")
		}
	case kindGHRun:
		if !shown {
			return errors.New("-gh-run needs -web or -filter")
		}
	}
	return nil
}

// detectInput decides the input. A replay flag wins (refuseOtherInput later
// rejects it alongside a command or stdin), then a command, then stdin: a
// regular file is a replay, anything else that is not a character device (a
// pipe, a socket) is live input. A stdin that can't be identified counts as
// none.
func detectInput(args []string, stdin *os.File, historical bool, ghJob, ghRun string) inputKind {
	switch {
	case historical:
		return kindHistorical
	case ghJob != "":
		return kindGHJob
	case ghRun != "":
		return kindGHRun
	case len(args) > 0:
		return kindCommand
	}
	fi, err := stdin.Stat()
	switch {
	case err != nil:
		return kindNone
	case fi.Mode().IsRegular():
		return kindFile
	case fi.Mode()&os.ModeCharDevice == 0:
		return kindPipe
	}
	return kindNone
}

// removedFlags are flags of the removed terminal UI. flag.Parse would only
// call them unknown; this names why.
var removedFlags = []string{"scrollback", "theme"}

// removedFlagError reports a removed flag among args, which are scanned up to
// the first "--" so a command's own flags are left alone.
func removedFlagError(args []string) error {
	for _, a := range args {
		if a == "--" {
			return nil
		}
		name, ok := strings.CutPrefix(a, "--")
		if !ok {
			if name, ok = strings.CutPrefix(a, "-"); !ok {
				continue
			}
		}
		name, _, _ = strings.Cut(name, "=")
		for _, r := range removedFlags {
			if name == r {
				return fmt.Errorf("-%s was removed along with the terminal UI", r)
			}
		}
	}
	return nil
}

// describeJobs tells which of a run's jobs -gh-run is showing, and lists the
// others so another can be named with -gh-job.
func describeJobs(w io.Writer, run string, jobs []ghjob.Job, chosen int) {
	which := "the first one"
	if jobs[chosen].Failed() {
		which = "the first failed one"
	}
	fmt.Fprintf(w, "loghorn: run %s has %d jobs; showing %s (pick another with -gh-job <id>):\n", run, len(jobs), which)
	items := jobItems(jobs)
	for i, j := range jobs {
		mark := "  "
		if i == chosen {
			mark = "* "
		}
		fmt.Fprintf(w, "  %s%d  %s\n", mark, j.ID, items[i])
	}
}
