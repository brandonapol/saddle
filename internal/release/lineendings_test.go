package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A checkout with core.autocrlf=true (Windows Git, then built from WSL) must
// still give LF line endings. Under CRLF the shebang reads "bash\r" and
// `make install` dies with "env: 'bash\r': No such file or directory".
func TestCheckoutKeepsLFUnderAutocrlf(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	attrs, err := os.ReadFile(filepath.Join(root, ".gitattributes"))
	if err != nil {
		t.Fatalf("read .gitattributes: %v", err)
	}

	// A throwaway repo that holds the real .gitattributes and the kinds of
	// files that break under CRLF, checked out the way Windows Git does.
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "core.autocrlf=true", "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	files := map[string]string{
		".gitattributes":          string(attrs),
		"scripts/install-path.sh": "#!/usr/bin/env bash\necho ok\n",
		"Makefile":                "all:\n\techo ok\n",
		"main.go":                 "package main\n",
	}
	git("init", "-q")
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("commit", "-q", "-m", "fixture")
	for name := range files {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	git("checkout", "--", ".")

	for name := range files {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "\r") {
			t.Errorf("%s checked out with CRLF under core.autocrlf=true; .gitattributes must force eol=lf", name)
		}
	}
}
