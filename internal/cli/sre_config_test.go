package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runSRECheckConfig(t *testing.T, dir string) (string, error) {
	t.Helper()
	cmd := sreCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"check-config", "--root", dir})
	err := cmd.Execute()
	return out.String(), err
}

func TestSRECheckConfig(t *testing.T) {
	dir := t.TempDir()
	if _, err := runSRECheckConfig(t, dir); err == nil {
		t.Fatal("missing file: want error")
	}
	if err := os.MkdirAll(filepath.Join(dir, ".saddle"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".saddle", "sre.toml")
	if err := os.WriteFile(path, []byte("[[check]]\nname=\"a\"\ncommand=[\"true\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runSRECheckConfig(t, dir); err == nil {
		t.Fatal("invalid file: want error")
	}
	good := "[[check]]\nname=\"a\"\ncommand=[\"true\"]\nexpect=\"exit 0\"\n"
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runSRECheckConfig(t, dir)
	if err != nil || !strings.Contains(out, "1 check") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
}
