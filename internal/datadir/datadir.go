// Package datadir locates the per-user folder where loghorn serve keeps its
// store, its lock and the runs recorded while it was down.
package datadir

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// EnvVar overrides the folder for every command.
const EnvVar = "LOGHORN_DATA_DIR"

// Dir returns $LOGHORN_DATA_DIR when set, else the platform's per-user folder:
// macOS ~/Library/Application Support/loghorn, Linux $XDG_STATE_HOME/loghorn
// (default ~/.local/state/loghorn), Windows %LOCALAPPDATA%\loghorn. It does not
// create the folder.
func Dir() (string, error) {
	if d := os.Getenv(EnvVar); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil && runtime.GOOS != "windows" {
		return "", err
	}
	d := forOS(runtime.GOOS, os.Getenv, home)
	if d == "" {
		return "", errors.New("can't find a folder for loghorn's data: set " + EnvVar)
	}
	return d, nil
}

func forOS(goos string, getenv func(string) string, home string) string {
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "loghorn")
	case "windows":
		if l := getenv("LOCALAPPDATA"); l != "" {
			return filepath.Join(l, "loghorn")
		}
		if home == "" {
			return ""
		}
		return filepath.Join(home, "AppData", "Local", "loghorn")
	}
	if s := getenv("XDG_STATE_HOME"); s != "" {
		return filepath.Join(s, "loghorn")
	}
	return filepath.Join(home, ".local", "state", "loghorn")
}
