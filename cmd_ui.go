package main

import (
	"errors"
	"fmt"

	"github.com/jimbeveridge/loghorn/internal/browser"
	"github.com/jimbeveridge/loghorn/internal/sock"
)

const uiUsage = `Usage: loghorn ui [-print]

Open the running serve's web explorer in your default browser. -print only
prints its URL. Exits 1 when serve isn't running; it never starts serve.
`

// openBrowser is browser.Open, replaceable in tests.
var openBrowser = browser.Open

func cmdUI(args []string, env cliEnv) int {
	fs := newFlagSet("ui", uiUsage, env)
	printOnly := fs.Bool("print", false, "print the URL instead of opening it")
	pos, code, ok := parseCmd(fs, args, true)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		return usageError(env, "ui takes no arguments")
	}
	path := sock.DefaultPath()
	sess, err := sock.Probe(path)
	if err != nil {
		if errors.Is(err, sock.ErrNotRunning) {
			fmt.Fprintln(env.stderr, "loghorn: "+notRunningMessage(path))
		} else {
			fmt.Fprintf(env.stderr, "loghorn: serve on %s: %v\n", path, err)
		}
		return 1
	}
	u := sess.WebURL()
	if *printOnly {
		fmt.Fprintln(env.stdout, u)
		return 0
	}
	fmt.Fprintln(env.stderr, "loghorn: opening", u)
	if err := openBrowser(u); err != nil {
		fmt.Fprintf(env.stderr, "loghorn: can't open a browser (%v); open %s yourself\n", err, u)
		return 1
	}
	return 0
}
