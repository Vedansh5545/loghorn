package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jimbeveridge/loghorn/internal/export"
	"github.com/jimbeveridge/loghorn/internal/instances"
	"github.com/jimbeveridge/loghorn/internal/query"
)

// queryEnv is what query mode reads and writes, injectable for tests. stdin is
// read only for -url @-.
type queryEnv struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	socketDir      func() (string, error)
}

// maxURLArg bounds what -url @- and -url @file read.
const maxURLArg = 1 << 20

// isQueryURL reports whether a positional argument is a loghorn page URL
// rather than a command: it starts with http://127.0.0.1 or http://localhost.
// The host is checked exactly later, so http://localhost.example.com is a
// refused query, never a command to launch.
func isQueryURL(s string) bool {
	return strings.HasPrefix(s, "http://127.0.0.1") || strings.HasPrefix(s, "http://localhost")
}

// queryRequested reports whether the run is a query. main asks before
// detectInput, so a query never treats stdin as log input and never launches
// a command. -o, -format and -limit count even without a URL: runQuery then
// refuses them, instead of a wrapped command silently ignoring them.
func queryRequested(o opts) bool {
	return o.url != "" || o.toSQL || o.instances || o.out != "" || o.format != "" || o.limitSet ||
		(len(o.args) > 0 && isQueryURL(o.args[0]))
}

// readURLArg resolves -url's value: @- reads stdin, @path reads the file, both
// trimmed of surrounding whitespace; anything else is the URL itself.
func readURLArg(v string, stdin io.Reader) (string, error) {
	var b []byte
	var err error
	switch {
	case v == "@-":
		b, err = io.ReadAll(io.LimitReader(stdin, maxURLArg))
	case strings.HasPrefix(v, "@"):
		var f *os.File
		if f, err = os.Open(v[1:]); err == nil {
			b, err = io.ReadAll(io.LimitReader(f, maxURLArg))
			f.Close()
		}
	default:
		return v, nil
	}
	return strings.TrimSpace(string(b)), err
}

// queryConflict names what a query can't be combined with, or "".
func queryConflict(o opts) string {
	for _, c := range []struct {
		set  bool
		name string
	}{{o.web, "-web"}, {o.otel, "-otel"}, {o.historical, "-historical"}, {o.ghJob != "", "-gh-job"}, {o.ghRun != "", "-gh-run"}} {
		if c.set {
			return "a query can't be combined with " + c.name
		}
	}
	if o.instances {
		if o.url != "" || len(o.args) > 0 || o.out != "" || o.format != "" || o.limitSet || o.toSQL {
			return "-instances takes no URL, command or other query flags"
		}
		return ""
	}
	positional := len(o.args) > 0 && isQueryURL(o.args[0])
	switch {
	case o.url != "" && len(o.args) > 0, positional && len(o.args) > 1:
		return "a query can't be combined with a command"
	case o.url == "" && !positional:
		if len(o.args) > 0 {
			return "-o, -format, -limit and -to-sql query a URL; they can't be combined with a command"
		}
		return "-o, -format, -limit and -to-sql need a URL: -url URL or loghorn http://127.0.0.1:PORT/?q=…"
	}
	return ""
}

// runQuery is query mode: it sends the view a page URL names to that loghorn
// -web instance's /api/query in an export format and writes the answer to -o
// or stdout. It never launches anything and reads stdin only for -url @-.
// Exit codes: 2 for usage errors, 1 for a failed query, 0 otherwise.
func runQuery(o opts, env queryEnv) int {
	usage := func(format string, a ...any) int {
		fmt.Fprintf(env.stderr, "loghorn: "+format+"\n", a...)
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(env.stderr, "loghorn: "+format+"\n", a...)
		return 1
	}
	if msg := queryConflict(o); msg != "" {
		return usage("%s", msg)
	}
	if o.instances {
		return listInstances(env)
	}

	raw := ""
	if o.url != "" {
		v, err := readURLArg(o.url, env.stdin)
		if err != nil {
			return usage("-url %s: %v", o.url, err)
		}
		if v == "" {
			return usage("-url %s: no URL in it", o.url)
		}
		raw = v
	} else {
		raw = o.args[0]
	}
	u, err := url.Parse(raw)
	if err != nil {
		return usage("bad URL: %v", err)
	}
	viaSocket := u.Scheme == "" && u.Host == ""
	if !viaSocket {
		if u.Scheme != "http" {
			return usage("loghorn serves only http://127.0.0.1 and http://localhost; refusing %s", u.Scheme+"://"+u.Host)
		}
		if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" {
			return usage("loghorn serves only 127.0.0.1 and localhost; refusing host %q", h)
		}
	}

	params := u.Query()
	if params.Get("trace") != "" || params.Get("span") != "" {
		fmt.Fprintln(env.stderr, "loghorn: the URL's trace view is not exported; exporting the main view")
	}
	if params.Get("agg") != "" {
		fmt.Fprintln(env.stderr, "loghorn: the URL's aggregate view is not exported; exporting the main view")
	}
	q, err := decodeQ(params.Get("q"))
	if err != nil {
		return usage("%v", err)
	}

	f, err := queryFormat(o)
	if err != nil {
		return usage("%v", err)
	}
	q.Format = string(f)
	if o.limitSet {
		if o.limit < 1 {
			return usage("-limit must be at least 1")
		}
		switch q.View {
		case "heatmap":
			return usage("-limit does not apply to the heatmap, which has no row limit")
		case "table", "graph":
			q.Top = o.limit
		default:
			q.Limit = o.limit
		}
	}
	body, err := json.Marshal(q)
	if err != nil {
		return fail("%v", err)
	}

	client, endpoint, where := (*http.Client)(nil), "", ""
	if viaSocket {
		dir, err := env.socketDir()
		if err != nil {
			return fail("socket dir: %v", err)
		}
		list, err := instances.List(dir)
		if err != nil {
			return fail("socket dir: %v", err)
		}
		switch len(list) {
		case 0:
			return fail("no loghorn -web instance is running (no live socket in %s)", dir)
		case 1:
		default:
			fmt.Fprintln(env.stderr, "loghorn: several loghorn -web instances are running; pass the full URL (http://127.0.0.1:PORT/?q=…) of one:")
			writeInstances(env.stderr, list)
			return 1
		}
		client, endpoint, where = instances.Client(list[0].Socket), "http://loghorn/api/query", fmt.Sprintf("pid %d", list[0].PID)
	} else {
		client = &http.Client{Transport: &http.Transport{
			Proxy:             nil,
			DialContext:       (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			DisableKeepAlives: true,
		}}
		endpoint, where = "http://"+u.Host+"/api/query", u.Host
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fail("%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fail("can't reach loghorn at %s: %v\nloghorn: is it still running? loghorn -instances lists the running ones", where, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return fail("%s", e.Error)
		}
		return fail("%s answered %s: %s", where, resp.Status, strings.TrimSpace(string(b)))
	}

	if o.out != "" {
		// Created only now, so a failed query leaves an existing file alone.
		out, err := os.Create(o.out)
		if err != nil {
			return fail("%v", err)
		}
		_, err = io.Copy(out, resp.Body)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fail("%s: %v", o.out, err)
		}
	} else if _, err := io.Copy(env.stdout, resp.Body); err != nil {
		return fail("%v", err)
	}
	if note := resp.Header.Get("X-Loghorn-Note"); note != "" {
		fmt.Fprintln(env.stderr, "loghorn:", note)
	}
	return 0
}

// decodeQ decodes the URL's q parameter as the page does: JSON, unknown
// fields ignored, a missing range end defaulting as in the page, and no
// batch offset.
func decodeQ(s string) (query.Query, error) {
	var q query.Query
	if s == "" {
		return q, errors.New("the URL has no ?q= query; copy it from the page's address bar")
	}
	if err := json.Unmarshal([]byte(s), &q); err != nil {
		return q, fmt.Errorf("the URL's q is not a loghorn query: %v", err)
	}
	if q.Range.From == "" {
		q.Range.From = "this-run"
	}
	if q.Range.To == "" {
		q.Range.To = "now"
	}
	q.Offset = 0
	return q, nil
}

// queryFormat picks the output format: -format, else -to-sql's sql, else -o's
// extension, else tsv.
func queryFormat(o opts) (export.Format, error) {
	switch {
	case o.format != "":
		f, err := export.ParseFormat(o.format)
		if err != nil {
			return "", err
		}
		if o.toSQL && f != export.SQL {
			return "", fmt.Errorf("-to-sql prints SQL; it can't be combined with -format %s", f)
		}
		return f, nil
	case o.toSQL:
		return export.SQL, nil
	case o.out != "":
		if f, ok := export.FormatFromPath(o.out); ok {
			return f, nil
		}
	}
	return export.TSV, nil
}

// listInstances is -instances: a row per live instance on stdout.
func listInstances(env queryEnv) int {
	dir, err := env.socketDir()
	if err != nil {
		fmt.Fprintln(env.stderr, "loghorn: socket dir:", err)
		return 1
	}
	list, err := instances.List(dir)
	if err != nil {
		fmt.Fprintln(env.stderr, "loghorn: socket dir:", err)
		return 1
	}
	if len(list) == 0 {
		fmt.Fprintf(env.stderr, "loghorn: no loghorn -web instance is running (no live socket in %s)\n", dir)
		return 0
	}
	writeInstances(env.stdout, list)
	return 0
}

// writeInstances writes the instance table: pid, port, start time, cwd.
func writeInstances(w io.Writer, list []instances.Instance) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PID\tPORT\tSTARTED\tCWD")
	for _, in := range list {
		fmt.Fprintf(tw, "%d\t%d\t%s\t%s\n", in.PID, in.Port, in.Started.Local().Format("2006-01-02 15:04:05"), in.Cwd)
	}
	_ = tw.Flush()
}
