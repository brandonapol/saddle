package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #588: [claims] granularity picks path-only or symbol-aware edit checks.
// It defaults to file, and anything but file or symbol fails to load.
func TestClaimsGranularity(t *testing.T) {
	for _, tc := range []struct {
		name, body, want, wantErr string
	}{
		{"missing key", "", GranularityFile, ""},
		{"empty table", "[claims]\n", GranularityFile, ""},
		{"file", "[claims]\ngranularity = \"file\"\n", GranularityFile, ""},
		{"symbol", "[claims]\ngranularity = \"symbol\"\n", GranularitySymbol, ""},
		{"line", "[claims]\ngranularity = \"line\"\n", "", `claims.granularity "line": want one of file, symbol`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, ".saddle"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, ".saddle", "config.toml"), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(root)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Claims.Granularity != tc.want {
				t.Fatalf("granularity = %q, want %q", cfg.Claims.Granularity, tc.want)
			}
		})
	}
}
