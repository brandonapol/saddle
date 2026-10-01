package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func TestUsageDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	u := cfg.Usage
	if u.Poll != 15*time.Second || u.CountCacheReads || len(u.Windows) != 2 ||
		u.Windows[0] != (Window{Name: "5h", Span: 5 * time.Hour}) ||
		u.Windows[1] != (Window{Name: "7d", Span: 168 * time.Hour}) {
		t.Fatalf("defaults: %+v", u)
	}
}

func TestUsageCaps(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	body := `
[usage]
poll = "1m"
count_cache_reads = true
[[usage.windows]]
name = "5h"
span = "5h"
cap = 2000000
[[usage.windows]]
name = "broken"
`
	if err := os.MkdirAll(filepath.Join(root, ".saddle"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".saddle", "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	u := cfg.Usage
	if u.Poll != time.Minute || !u.CountCacheReads || len(u.Windows) != 1 ||
		u.Windows[0] != (Window{Name: "5h", Span: 5 * time.Hour, Cap: 2_000_000}) {
		t.Fatalf("usage: %+v", u)
	}
}

// The commented template stays valid TOML once uncommented.
func TestTemplateParses(t *testing.T) {
	var cfg Config
	if _, err := toml.Decode(Template, &cfg); err != nil {
		t.Fatal(err)
	}
}
