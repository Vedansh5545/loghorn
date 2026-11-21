package main

import (
	"bufio"
	"os"
	"path/filepath"

	"github.com/jimbeveridge/loghorn/internal/schema"
)

// repoConfigNotice tells, once per repo, that the repo's .config/loghorn
// schemas and favorites are no longer read. It looks for the repo config from
// start (schema.FindConfigDir); when that holds schemas/ or queries.toml and
// is not yet a line of <base>/notified-repos, it appends it there and returns
// the notice. Otherwise, and on any error, it returns "". It never touches
// the repo's files.
func repoConfigNotice(start, base string) string {
	dir, found, err := schema.FindConfigDir(start)
	if err != nil || !found {
		return ""
	}
	if !exists(filepath.Join(dir, "schemas")) && !exists(filepath.Join(dir, "queries.toml")) {
		return ""
	}
	list := filepath.Join(base, "notified-repos")
	if f, err := os.Open(list); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if sc.Text() == dir {
				f.Close()
				return ""
			}
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			return ""
		}
	} else if !os.IsNotExist(err) {
		return ""
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return ""
	}
	f, err := os.OpenFile(list, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return ""
	}
	_, werr := f.WriteString(dir + "\n")
	if cerr := f.Close(); werr != nil || cerr != nil {
		return ""
	}
	return "loghorn: " + dir + " is no longer read; pins, favorites and field settings now live in " +
		filepath.Join(base, "services") + "/<service>/ (see README)"
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
