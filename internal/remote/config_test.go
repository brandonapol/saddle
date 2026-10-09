package remote

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestConfigOffByDefault(t *testing.T) {
	c, err := LoadConfig(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Enabled {
		t.Fatal("remote control is on with no config")
	}
	if c.Listen != DefaultListen {
		t.Fatalf("Listen = %q, want %q", c.Listen, DefaultListen)
	}
}

func TestConfigOptIn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, p, "concurrency = 3\n[remote]\nenabled = true\nlisten = \"127.0.0.1:9999\"\n")
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Enabled || c.Listen != "127.0.0.1:9999" {
		t.Fatalf("config = %+v", c)
	}
}

// TestConfigRepoCannotEnable: only the user's own config turns remote
// control on. A repo's .saddle/config.toml, which may come from a clone,
// must not open a listener.
func TestConfigRepoCannotEnable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, ".saddle", "config.toml"), "[remote]\nenabled = true\n")
	c, err := LoadConfig(UserConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if c.Enabled {
		t.Fatal("a repo config enabled remote control")
	}
}

func TestCheckListen(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:7431", "localhost:7431", "[::1]:7431", "127.0.0.2:80"} {
		if err := CheckListen(ok); err != nil {
			t.Errorf("CheckListen(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{"0.0.0.0:7431", ":7431", "192.168.1.5:7431", "example.com:7431", "[::]:7431", "127.0.0.1"} {
		if err := CheckListen(bad); err == nil {
			t.Errorf("CheckListen(%q) = nil, want refused", bad)
		}
	}
}
