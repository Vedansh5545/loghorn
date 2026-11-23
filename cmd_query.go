package main

import (
	"flag"

	"github.com/jimbeveridge/loghorn/internal/web"
)

const queryUsage = `Usage: loghorn query [flags] <url | @file | @->

Print the data of the web view a page URL names, as TSV (default), CSV,
Markdown, JSON, JSON lines, YAML or TOON, or its DuckDB SQL with -to-sql. The
URL is a loghorn page URL (http://127.0.0.1:PORT/?q=…), a host-less /?q=…
for the running serve, or @file / @- to read it from a file or stdin.
`

func cmdQuery(args []string, env cliEnv) int {
	fs := newFlagSet("query", queryUsage, env)
	var o opts
	fs.StringVar(&o.out, "o", "", "write the output to this file; its extension picks the format")
	fs.StringVar(&o.format, "format", "", "output format: csv, tsv, md, json, jsonl, yaml, toon or sql (default tsv, or -o's extension)")
	fs.IntVar(&o.limit, "limit", 0, "rows: overrides the URL's limit (samples) or top (table, graph)")
	fs.BoolVar(&o.toSQL, "to-sql", false, "print the DuckDB SQL of the URL's query instead of its data")
	pos, code, ok := parseCmd(fs, args, true)
	if !ok {
		return code
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "limit" {
			o.limitSet = true
		}
	})
	if len(pos) != 1 {
		return usageError(env, "query takes one URL (or @file, @-); run 'loghorn help query'")
	}
	o.url = pos[0]
	return runQuery(o, queryEnv{stdin: env.stdin, stdout: env.stdout, stderr: env.stderr, socketDir: web.SocketDir})
}
