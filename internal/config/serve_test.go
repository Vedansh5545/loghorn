package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeUserConfig writes body as home's user-level config file.
func writeUserConfig(t *testing.T, home, body string) string {
	t.Helper()
	p := filepath.Join(home, ".config", "loghorn", "config.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestServeAndRunSections(t *testing.T) {
	home := t.TempDir()
	writeUserConfig(t, home, "[serve]\nweb-port = 5000\n\n[run]\nserve-down = \"fail\"\n")
	cfg, _, err := Loader{Home: home}.LoadUser()
	if err != nil {
		t.Fatal(err)
	}
	web, otlp := cfg.Serve.Resolved()
	if web != 5000 || otlp != 4318 {
		t.Fatalf("Resolved = %d, %d; want 5000, 4318", web, otlp)
	}
	if cfg.Run.Mode() != ServeDownFail {
		t.Fatalf("Run.Mode = %q; want fail", cfg.Run.Mode())
	}
}

func TestServeDefaults(t *testing.T) {
	web, otlp := Default().Serve.Resolved()
	if web != 4380 || otlp != 4318 || DefaultWebPort != 4380 || DefaultOTLPHTTPPort != 4318 {
		t.Fatalf("Resolved defaults = %d, %d", web, otlp)
	}
	if Default().Run.Mode() != ServeDownRecord {
		t.Fatalf("default serve-down = %q; want record", Default().Run.Mode())
	}
}

func TestServeDownRejectsUnknown(t *testing.T) {
	home := t.TempDir()
	writeUserConfig(t, home, "[run]\nserve-down = \"maybe\"\n")
	_, _, err := Loader{Home: home}.LoadUser()
	if err == nil || !strings.Contains(err.Error(), "record, skip, fail") {
		t.Fatalf("err = %v; want the allowed values named", err)
	}
}

func TestServePortRange(t *testing.T) {
	home := t.TempDir()
	writeUserConfig(t, home, "[serve]\nweb-port = 70000\n")
	_, _, err := Loader{Home: home}.LoadUser()
	if err == nil || !strings.Contains(err.Error(), "serve.web-port") {
		t.Fatalf("err = %v; want a serve.web-port range error", err)
	}
}

func TestLoadUserIgnoresRepo(t *testing.T) {
	home := t.TempDir()
	userPath := writeUserConfig(t, home, "[input]\nformat = \"json\"\n")
	repo := t.TempDir()
	repoCfg := filepath.Join(repo, RelPath)
	if err := os.MkdirAll(filepath.Dir(repoCfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repoCfg, []byte("[input]\nformat = \"yaml\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, path, err := Loader{Dir: repo, Home: home}.LoadUser()
	if err != nil {
		t.Fatal(err)
	}
	if path != userPath || cfg.Input.Format != FormatJSON {
		t.Fatalf("LoadUser = %q, %q; want the user file", path, cfg.Input.Format)
	}
}

func TestLoadUserNoFile(t *testing.T) {
	cfg, path, err := Loader{Home: t.TempDir()}.LoadUser()
	if err != nil || path != "" || cfg.Input.Format != FormatAuto {
		t.Fatalf("LoadUser = %+v, %q, %v; want defaults", cfg, path, err)
	}
}
