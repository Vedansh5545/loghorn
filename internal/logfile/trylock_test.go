package logfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTryLock(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, time.Now, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	_, err = TryLock(dir)
	var locked *LockedError
	if !errors.As(err, &locked) || locked.PID != os.Getpid() {
		t.Fatalf("TryLock while held = %v; want *LockedError with this pid", err)
	}
	w.Close()

	release, err := TryLock(dir)
	if err != nil {
		t.Fatalf("TryLock after Close: %v", err)
	}
	if _, err := TryLock(dir); !errors.As(err, &locked) {
		t.Fatalf("second TryLock = %v; want *LockedError", err)
	}
	release()
	release2, err := TryLock(dir)
	if err != nil {
		t.Fatalf("TryLock after release: %v", err)
	}
	release2()
}

func TestTryLockDoesNotCreateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	if _, err := TryLock(dir); err == nil {
		t.Fatal("TryLock on a missing dir succeeded")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("TryLock created %s", dir)
	}
}
