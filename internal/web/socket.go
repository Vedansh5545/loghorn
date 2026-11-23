package web

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// maxSocketPath is the longest unix socket path that fits sun_path on macOS
// (104 bytes including the terminating NUL); Linux allows 108.
const maxSocketPath = 103

// SocketDir returns the per-user directory that holds one <pid>.sock per
// running -web instance: $TMPDIR/loghorn, else $XDG_RUNTIME_DIR/loghorn, else
// /tmp/loghorn-<uid>. It creates the directory 0700 and refuses one that is not
// a real directory owned by the user with no group or other permission bits,
// so a socket there can only be reached (or planted) by this user.
func SocketDir() (string, error) {
	var dir string
	switch {
	case os.Getenv("TMPDIR") != "":
		dir = filepath.Join(os.Getenv("TMPDIR"), "loghorn")
	case os.Getenv("XDG_RUNTIME_DIR") != "":
		dir = filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "loghorn")
	default:
		dir = "/tmp/loghorn-" + strconv.Itoa(os.Getuid())
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	// Lstat, not Stat: a symlink in our place must not pass for a directory.
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("cannot check the owner of %s", dir)
	}
	if int(st.Uid) != os.Getuid() {
		return "", fmt.Errorf("%s is not owned by you", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s has mode %04o; it must not be accessible to group or others", dir, fi.Mode().Perm())
	}
	return dir, nil
}

// ListenSocket listens on <dir>/<pid>.sock, first removing a leftover entry of
// exactly that name (a killed instance's socket), and returns the listener and
// the path. dir should come from SocketDir.
func ListenSocket(dir string, pid int) (net.Listener, string, error) {
	path := filepath.Join(dir, strconv.Itoa(pid)+".sock")
	if len(path) > maxSocketPath {
		return nil, "", fmt.Errorf("socket path too long (%d bytes, max %d): %s", len(path), maxSocketPath, path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, "", err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, "", err
	}
	return ln, path, nil
}
