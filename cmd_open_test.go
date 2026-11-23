package main

import "testing"

func TestOpenSource(t *testing.T) {
	const run = "https://github.com/o/r/actions/runs/43954926722"
	const job = run + "/job/133607019104"
	for _, c := range []struct {
		pos, job, run string
		want          openSrc
		err           bool
	}{
		{pos: "log.txt", want: openSrc{"file", "log.txt"}},
		{pos: "-", want: openSrc{"stdin", ""}},
		{pos: job, want: openSrc{"gh-job", job}},
		{pos: run, want: openSrc{"gh-run", run}},
		{job: "5", want: openSrc{"gh-job", "5"}},
		{run: "6", want: openSrc{"gh-run", "6"}},
		{pos: "a.log", job: "5", err: true},
		{job: "5", run: "6", err: true},
		{err: true},
	} {
		got, err := openSource(c.pos, c.job, c.run)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("openSource(%q, %q, %q) = %+v, %v", c.pos, c.job, c.run, got, err)
		}
	}
}
