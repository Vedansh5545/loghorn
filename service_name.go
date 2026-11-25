package main

import (
	"os"
	"path/filepath"
)

// runService names the service a run's own output is recorded under: the
// -service flag, else the command's OTEL_SERVICE_NAME (the last entry wins,
// as in a process environment), else the name of the repository (or, outside
// one, the current directory) plus the command's base name, e.g. "shop-npm".
// Piped input has no command, so it is just the directory's name.
func runService(flagValue string, environ []string, cwd string, args []string) string {
	if flagValue != "" {
		return flagValue
	}
	if svc := childService(environ); svc != "" {
		return svc
	}
	name := filepath.Base(repoRoot(cwd))
	if len(args) == 0 {
		return name
	}
	return name + "-" + filepath.Base(args[0])
}

// repoRoot is the nearest ancestor of dir (or dir itself) holding a .git
// entry, a directory or (in a worktree) a file; dir when there is none.
func repoRoot(dir string) string {
	for d := dir; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}
