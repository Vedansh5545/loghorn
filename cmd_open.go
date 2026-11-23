package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

const openUsage = `Usage: loghorn open [flags] <file | - | GitHub Actions URL>
       loghorn open [flags] -gh-job ID
       loghorn open [flags] -gh-run ID

Open a finished log in a temporary web view (its URL goes to stderr; Ctrl-C
ends it), or with -filter print its important records. Nothing is recorded.
A GitHub Actions run or job is fetched with the gh CLI; a run shows its first
failed job (or its first job) and lists the others.
`

// openSrc is the one log open reads.
type openSrc struct {
	kind  string // "file", "stdin", "gh-job" or "gh-run"
	value string // the path, URL or ID
}

// openSource decides open's source from its positional argument and its
// GitHub flags. A GitHub URL with /job/ in its path names a job, any other
// one a run.
func openSource(pos, ghJob, ghRun string) (openSrc, error) {
	var srcs []openSrc
	switch {
	case pos == "":
	case pos == "-":
		srcs = append(srcs, openSrc{"stdin", ""})
	case strings.HasPrefix(pos, "https://github.com/") || strings.HasPrefix(pos, "http://github.com/"):
		if strings.Contains(pos, "/job/") {
			srcs = append(srcs, openSrc{"gh-job", pos})
		} else {
			srcs = append(srcs, openSrc{"gh-run", pos})
		}
	default:
		srcs = append(srcs, openSrc{"file", pos})
	}
	if ghJob != "" {
		srcs = append(srcs, openSrc{"gh-job", ghJob})
	}
	if ghRun != "" {
		srcs = append(srcs, openSrc{"gh-run", ghRun})
	}
	if len(srcs) != 1 {
		return openSrc{}, errors.New("open needs one log: a file, -, a GitHub Actions URL, -gh-job ID or -gh-run ID")
	}
	return srcs[0], nil
}

func cmdOpen(args []string, env cliEnv) int {
	fs := newFlagSet("open", openUsage, env)
	var o opts
	var view bool
	fs.BoolVar(&o.filter, "filter", false, "print only the important records to stdout instead of serving the view")
	fs.BoolVar(&view, "view", false, "with -filter, also serve the view")
	fs.IntVar(&o.webPort, "web-port", 0, "port for the view (0 picks a free one)")
	fs.StringVar(&o.ghJob, "gh-job", "", "a GitHub Actions job's log, by job ID or job URL (needs gh)")
	fs.StringVar(&o.ghRun, "gh-run", "", "the first failed job of a GitHub Actions run, by run ID or run URL (needs gh)")
	pos, code, ok := parseCmd(fs, args, true)
	if !ok {
		return code
	}
	if len(pos) > 1 {
		return usageError(env, "open takes one log; got %q", pos)
	}
	first := ""
	if len(pos) == 1 {
		first = pos[0]
	}
	src, err := openSource(first, o.ghJob, o.ghRun)
	if err != nil {
		return usageError(env, "%v", err)
	}
	cfg, _, cwd, code, ok := loadRepoConfig(env)
	if !ok {
		return code
	}
	m := mode{filter: o.filter, web: !o.filter || view}
	o.ghJob, o.ghRun = "", ""
	switch src.kind {
	case "file":
		f, err := os.Open(src.value)
		if err != nil {
			fmt.Fprintln(env.stderr, "loghorn:", err)
			return 1
		}
		defer f.Close()
		os.Stdin = f
		m.kind = kindFile
	case "stdin":
		m.kind = kindFile
	case "gh-job":
		o.ghJob, m.kind = src.value, kindGHJob
	case "gh-run":
		o.ghRun, m.kind = src.value, kindGHRun
	}
	return runReplay(m, o, cfg, cwd)
}
