package main

import (
	"flag"
	"time"
)

const runUsage = `Usage: loghorn run [flags] [--] <command> [args...]
       <command> | loghorn run [flags]

Run a command, passing its stdout and stderr through unchanged and exiting
with its status, and record its output; or record piped input, copying it to
stdout. Flags go before the command: everything from the command on is the
command's.
`

// runFlags registers run's flags on fs into o.
func runFlags(fs *flag.FlagSet, o *opts) {
	fs.BoolVar(&o.filter, "filter", false, "print only important records to stdout instead of everything")
	fs.BoolVar(&o.notify, "notify", false, "fire a desktop notification when fresh errors occur")
	fs.DurationVar(&o.notifyMaxAge, "notify-max-age", time.Second, "skip notifications for errors older than this (by their log timestamp)")
	fs.DurationVar(&o.notifyReset, "notify-reset", 15*time.Second, "end an error burst after this quiet gap, so the next error notifies again")
	fs.BoolVar(&o.notifySound, "notify-sound", true, "play the system alert sound with notifications (with -notify)")
	fs.DurationVar(&o.grace, "shutdown-grace", 5*time.Second, "after forwarding SIGTERM to the command, SIGKILL it this much later")
}

func cmdRun(args []string, env cliEnv) int {
	fs := newFlagSet("run", runUsage, env)
	var o opts
	runFlags(fs, &o)
	pos, code, ok := parseCmd(fs, args, false)
	if !ok {
		return code
	}
	o.args = pos
	cfg, _, cwd, code, ok := loadRepoConfig(env)
	if !ok {
		return code
	}
	switch {
	case len(pos) > 0:
		return runCommand(o, cfg, cwd)
	case stdinHasInput(env.stdin):
		return runPipe(o, cfg, cwd)
	}
	return usageError(env, "run needs a command (loghorn run -- <command>) or piped input")
}
