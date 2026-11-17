// Package ghjob fetches the log of one finished GitHub Actions job through
// the GitHub CLI (gh), so loghorn can replay it under -gh-job.
//
// A job ID alone is enough: GitHub serves a job's log by job ID, and the job's
// own record names the run it belongs to, so the run number is looked up
// rather than asked for. gh supplies the authentication, and with no
// repository given it picks the one the current directory's git remotes
// point at, exactly as `gh run view` does.
package ghjob

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// Ref names one Actions job.
type Ref struct {
	// Host is the GitHub host, empty for gh's default (github.com).
	Host string
	// Repo is "owner/name", empty for the repository of the current directory.
	Repo string
	// Job is the numeric job ID.
	Job string
}

// RunRef names one Actions workflow run.
type RunRef struct {
	Host, Repo string // as in Ref
	Run        string // the numeric run ID
}

// JobRef names one of the run's jobs.
func (r RunRef) JobRef(id int64) Ref {
	return Ref{Host: r.Host, Repo: r.Repo, Job: strconv.FormatInt(id, 10)}
}

// Job is what GitHub records about a job, as much as loghorn shows.
type Job struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Workflow   string `json:"workflow_name"`
	RunID      int64  `json:"run_id"`
	Branch     string `json:"head_branch"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
}

// Repo is the job's "owner/name", read from its page URL, or "" if that
// URL has an unexpected shape.
func (j Job) Repo() string {
	u, err := url.Parse(j.HTMLURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

// Describe is a one-line summary: repo, job, run, workflow and outcome.
func (j Job) Describe(id string) string {
	outcome := j.Outcome()
	s := fmt.Sprintf("job %s %q of run %d", id, j.Name, j.RunID)
	if repo := j.Repo(); repo != "" {
		s = repo + " " + s
	}
	if j.Workflow != "" {
		s += " (" + j.Workflow
		if outcome != "" {
			s += ", " + outcome
		}
		s += ")"
	} else if outcome != "" {
		s += " (" + outcome + ")"
	}
	return s
}

// Outcome is the job's conclusion, or its status while it has none.
func (j Job) Outcome() string {
	if j.Conclusion != "" {
		return j.Conclusion
	}
	return j.Status
}

// Failed reports whether the job ended in failure: a failed step, a timeout,
// or a run that never started. A cancelled or skipped job is not a failure.
func (j Job) Failed() bool {
	switch j.Conclusion {
	case "failure", "timed_out", "startup_failure":
		return true
	}
	return false
}

// FirstFailed is the index of the first failed job, or 0 when none failed,
// so a picker can start on the job most likely wanted.
func FirstFailed(jobs []Job) int {
	for i, j := range jobs {
		if j.Failed() {
			return i
		}
	}
	return 0
}

// Parse reads -gh-job's value: a bare job ID, or a job's page URL such as
// https://github.com/owner/repo/actions/runs/123/job/456, which also names
// the repository (and host). The run ID in a URL is not needed and not used.
func Parse(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	if isID(s) {
		return Ref{Job: s}, nil
	}
	u, err := url.Parse(s)
	if err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
		// owner/repo/actions/runs/<run>/job/<job> — "jobs" is accepted too, as
		// the API spells it.
		p := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(p) == 7 && p[2] == "actions" && p[3] == "runs" && (p[5] == "job" || p[5] == "jobs") && isID(p[6]) {
			host := u.Host
			if host == "github.com" || host == "www.github.com" {
				host = ""
			}
			return Ref{Host: host, Repo: p[0] + "/" + p[1], Job: p[6]}, nil
		}
	}
	return Ref{}, fmt.Errorf("-gh-job wants a job ID or a job URL (…/actions/runs/<run>/job/<job>), not %q", s)
}

// ParseRun reads -gh-run's value: a bare run ID, or a run's page URL such as
// https://github.com/owner/repo/actions/runs/123 — a job URL within the run
// (…/runs/123/job/456) is accepted too, and names the same run.
func ParseRun(s string) (RunRef, error) {
	s = strings.TrimSpace(s)
	if isID(s) {
		return RunRef{Run: s}, nil
	}
	u, err := url.Parse(s)
	if err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
		p := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(p) >= 5 && p[2] == "actions" && p[3] == "runs" && isID(p[4]) {
			host := u.Host
			if host == "github.com" || host == "www.github.com" {
				host = ""
			}
			return RunRef{Host: host, Repo: p[0] + "/" + p[1], Run: p[4]}, nil
		}
	}
	return RunRef{}, fmt.Errorf("-gh-run wants a run ID or a run URL (…/actions/runs/<run>), not %q", s)
}

func isID(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	_, err := strconv.ParseUint(s, 10, 64)
	return err == nil
}

// runGH runs gh with args and returns its stdout. A variable so tests can
// stand in for gh.
var runGH = func(args ...string) ([]byte, error) {
	out, err := exec.Command("gh", args...).Output()
	if errors.Is(err, exec.ErrNotFound) {
		return nil, errors.New("-gh-job and -gh-run need the GitHub CLI (gh) on PATH: https://cli.github.com")
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		msg := strings.TrimSpace(string(exit.Stderr))
		if msg == "" {
			msg = exit.Error()
		}
		return nil, fmt.Errorf("gh %s: %s", strings.Join(args, " "), msg)
	}
	return out, err
}

// bom is the UTF-8 byte order mark GitHub starts every job log with.
var bom = []byte("\xef\xbb\xbf")

// api runs `gh api` against host (empty for gh's default).
func api(host, path string) ([]byte, error) {
	args := []string{"api"}
	if host != "" {
		args = append(args, "--hostname", host)
	}
	return runGH(append(args, path)...)
}

// repoPath is "repos/<owner>/<name>", or gh's placeholders for the current
// directory's repository when repo is empty.
func repoPath(repo string) string {
	if repo == "" {
		return "repos/{owner}/{repo}"
	}
	return "repos/" + repo
}

// Jobs lists the jobs of a run's latest attempt, sorted by name so matrix
// legs like "integration (1)…(4)" sit together in order.
func Jobs(r RunRef) ([]Job, error) {
	// 100 is the API's page maximum; a run with more jobs than that is rare
	// enough that only the first page is offered.
	body, err := api(r.Host, repoPath(r.Repo)+"/actions/runs/"+r.Run+"/jobs?per_page=100")
	if err != nil {
		return nil, err
	}
	var page struct {
		Jobs []Job `json:"jobs"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("reading the jobs of run %s: %w", r.Run, err)
	}
	if len(page.Jobs) == 0 {
		return nil, fmt.Errorf("run %s has no jobs", r.Run)
	}
	sort.SliceStable(page.Jobs, func(i, k int) bool { return page.Jobs[i].Name < page.Jobs[k].Name })
	return page.Jobs, nil
}

// Fetch looks the job up and downloads its whole log. Both calls go through
// gh, so a missing gh, a failed login or an unknown job is an error here,
// before anything is shown.
func Fetch(r Ref) (Job, []byte, error) {
	base := repoPath(r.Repo) + "/actions/jobs/" + r.Job
	meta, err := api(r.Host, base)
	if err != nil {
		return Job{}, nil, err
	}
	var job Job
	if err := json.Unmarshal(meta, &job); err != nil {
		return Job{}, nil, fmt.Errorf("reading job %s: %w", r.Job, err)
	}
	logs, err := api(r.Host, base+"/logs")
	if err != nil {
		return job, nil, err
	}
	return job, bytes.TrimPrefix(logs, bom), nil
}
