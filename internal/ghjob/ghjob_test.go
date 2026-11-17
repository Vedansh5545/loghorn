package ghjob

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Ref
	}{
		{"133607019104", Ref{Job: "133607019104"}},
		{" 42 ", Ref{Job: "42"}},
		{"https://github.com/o/r/actions/runs/43954926722/job/133607019104", Ref{Repo: "o/r", Job: "133607019104"}},
		{"https://github.com/o/r/actions/runs/1/job/2?pr=7#step:4:12", Ref{Repo: "o/r", Job: "2"}},
		{"https://github.com/o/r/actions/runs/1/jobs/2/", Ref{Repo: "o/r", Job: "2"}},
		{"https://ghe.example.com/o/r/actions/runs/1/job/2", Ref{Host: "ghe.example.com", Repo: "o/r", Job: "2"}},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil || got != c.want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "abc", "-5", "https://github.com/o/r/actions/runs/1", "https://github.com/o/r/pull/3", "ftp://github.com/o/r/actions/runs/1/job/2"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func stubGH(t *testing.T, fn func(args ...string) ([]byte, error)) *[][]string {
	t.Helper()
	var calls [][]string
	old := runGH
	runGH = func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		return fn(args...)
	}
	t.Cleanup(func() { runGH = old })
	return &calls
}

// With no repo, gh's {owner}/{repo} placeholders stand in for it; the run ID
// comes from the job record, and the log's byte order mark is dropped.
func TestFetchInfersRunAndStripsBOM(t *testing.T) {
	calls := stubGH(t, func(args ...string) ([]byte, error) {
		if strings.HasSuffix(args[len(args)-1], "/logs") {
			return []byte("\xef\xbb\xbf2026-11-16T20:56:51Z hello\n"), nil
		}
		return []byte(`{"name":"integration (2)","workflow_name":"ci","run_id":43954926722,"conclusion":"success","html_url":"https://github.com/o/r/actions/runs/43954926722/job/7"}`), nil
	})
	job, logs, err := Fetch(Ref{Job: "7"})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"api", "repos/{owner}/{repo}/actions/jobs/7"},
		{"api", "repos/{owner}/{repo}/actions/jobs/7/logs"},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("gh calls = %v, want %v", *calls, want)
	}
	if job.RunID != 43954926722 {
		t.Fatalf("RunID = %d", job.RunID)
	}
	if string(logs) != "2026-11-16T20:56:51Z hello\n" {
		t.Fatalf("logs = %q", logs)
	}
	if got := job.Describe("7"); got != `o/r job 7 "integration (2)" of run 43954926722 (ci, success)` {
		t.Fatalf("Describe = %s", got)
	}
}

func TestFetchPassesRepoAndHost(t *testing.T) {
	calls := stubGH(t, func(args ...string) ([]byte, error) { return []byte(`{}`), nil })
	if _, _, err := Fetch(Ref{Host: "ghe.example.com", Repo: "o/r", Job: "9"}); err != nil {
		t.Fatal(err)
	}
	if got := (*calls)[0]; !reflect.DeepEqual(got, []string{"api", "--hostname", "ghe.example.com", "repos/o/r/actions/jobs/9"}) {
		t.Fatalf("gh call = %v", got)
	}
}

func TestFetchStopsAtTheFirstGHError(t *testing.T) {
	calls := stubGH(t, func(args ...string) ([]byte, error) { return nil, errors.New("HTTP 404") })
	if _, _, err := Fetch(Ref{Job: "9"}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("gh ran %d times after a failed lookup", len(*calls))
	}
}

func TestParseRun(t *testing.T) {
	cases := []struct {
		in   string
		want RunRef
	}{
		{"43954926722", RunRef{Run: "43954926722"}},
		{"https://github.com/o/r/actions/runs/43954926722", RunRef{Repo: "o/r", Run: "43954926722"}},
		{"https://github.com/o/r/actions/runs/1/", RunRef{Repo: "o/r", Run: "1"}},
		{"https://github.com/o/r/actions/runs/1/job/2", RunRef{Repo: "o/r", Run: "1"}},
		{"https://github.com/o/r/actions/runs/1/attempts/2", RunRef{Repo: "o/r", Run: "1"}},
		{"https://ghe.example.com/o/r/actions/runs/1", RunRef{Host: "ghe.example.com", Repo: "o/r", Run: "1"}},
	}
	for _, c := range cases {
		got, err := ParseRun(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseRun(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "x1", "https://github.com/o/r/actions", "https://github.com/o/r/actions/runs/abc"} {
		if _, err := ParseRun(bad); err == nil {
			t.Errorf("ParseRun(%q) should fail", bad)
		}
	}
}

func TestJobsListsTheRunSortedByName(t *testing.T) {
	calls := stubGH(t, func(args ...string) ([]byte, error) {
		return []byte(`{"total_count":3,"jobs":[
			{"id":3,"name":"web","conclusion":"success"},
			{"id":2,"name":"integration (2)","status":"in_progress"},
			{"id":1,"name":"integration (1)","conclusion":"failure"}]}`), nil
	})
	jobs, err := Jobs(RunRef{Repo: "o/r", Run: "5"})
	if err != nil {
		t.Fatal(err)
	}
	if got := (*calls)[0]; !reflect.DeepEqual(got, []string{"api", "repos/o/r/actions/runs/5/jobs?per_page=100"}) {
		t.Fatalf("gh call = %v", got)
	}
	var names []string
	for _, j := range jobs {
		names = append(names, j.Name+"="+j.Outcome())
	}
	if want := []string{"integration (1)=failure", "integration (2)=in_progress", "web=success"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("jobs = %v, want %v", names, want)
	}
	if ref := (RunRef{Repo: "o/r", Run: "5"}).JobRef(jobs[0].ID); ref != (Ref{Repo: "o/r", Job: "1"}) {
		t.Fatalf("JobRef = %+v", ref)
	}
}

func TestJobsRejectsAnEmptyRun(t *testing.T) {
	stubGH(t, func(args ...string) ([]byte, error) { return []byte(`{"jobs":[]}`), nil })
	if _, err := Jobs(RunRef{Run: "5"}); err == nil {
		t.Fatal("an empty run should be an error")
	}
}

func TestFirstFailed(t *testing.T) {
	jobs := []Job{{Conclusion: "success"}, {Conclusion: "cancelled"}, {Conclusion: "skipped"}, {Conclusion: "timed_out"}, {Conclusion: "failure"}}
	if got := FirstFailed(jobs); got != 3 {
		t.Fatalf("FirstFailed = %d, want 3", got)
	}
	if got := FirstFailed(jobs[:3]); got != 0 {
		t.Fatalf("FirstFailed with no failures = %d, want 0", got)
	}
	if got := FirstFailed([]Job{{Status: "in_progress"}, {Conclusion: "startup_failure"}}); got != 1 {
		t.Fatalf("FirstFailed = %d, want 1", got)
	}
}

// Names come from whoever wrote the workflow, so escape sequences in them
// must not reach the terminal, from either Jobs or Fetch.
func TestJobTextLosesControlCharacters(t *testing.T) {
	const evil = `"name":"x\u001b]0;pwned\u0007y\u009bz","workflow_name":"w\u001b[2Jv","conclusion":"fail\nure"`
	stubGH(t, func(args ...string) ([]byte, error) {
		if strings.HasSuffix(args[1], "/logs") {
			return []byte("log"), nil
		}
		if strings.Contains(args[1], "/runs/") {
			return []byte(`{"jobs":[{"id":1,` + evil + `}]}`), nil
		}
		return []byte(`{"id":1,` + evil + `}`), nil
	})
	jobs, err := Jobs(RunRef{Repo: "o/r", Run: "5"})
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := Fetch(Ref{Repo: "o/r", Job: "1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range []Job{jobs[0], job} {
		if j.Name != "x]0;pwnedyz" || j.Workflow != "w[2Jv" || j.Conclusion != "failure" {
			t.Fatalf("control characters survived: %+v", j)
		}
	}
}
