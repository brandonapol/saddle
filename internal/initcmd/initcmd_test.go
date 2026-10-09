package initcmd

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	// Keep the user's own ~/.config/saddle/config.toml out of it.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	return dir
}

// fakeInit writes what saddle init writes, except the hooks (a test binary
// can't be one).
func fakeInit(root string, calls *int) func() error {
	return func() error {
		*calls++
		if err := os.MkdirAll(filepath.Join(root, ".saddle"), 0o755); err != nil {
			return err
		}
		cfg := filepath.Join(root, ".saddle", "config.toml")
		if _, err := os.Stat(cfg); err != nil {
			if err := os.WriteFile(cfg, []byte("[test]\ncmd = \"go test ./...\"\n"), 0o644); err != nil {
				return err
			}
		}
		f, err := os.OpenFile(filepath.Join(root, ".git", "info", "exclude"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteString("/.saddle/\n")
		return err
	}
}

func TestEnsureReportsWhatItSetUp(t *testing.T) {
	root := gitRepo(t)
	calls := 0
	steps, err := Ensure(root, fakeInit(root, &calls))
	if err != nil || calls != 1 {
		t.Fatalf("Ensure: %v, init ran %d times", err, calls)
	}
	got := strings.Join(steps, "; ")
	for _, want := range []string{"wrote .saddle/config.toml", "test.cmd go test ./...", "/.saddle/ to .git/info/exclude"} {
		if !strings.Contains(got, want) {
			t.Errorf("steps %q lack %q", got, want)
		}
	}
	if strings.Contains(got, "hooks") {
		t.Errorf("reported hooks init didn't install: %q", got)
	}

	var out bytes.Buffer
	Report(&out, steps)
	if !strings.HasPrefix(out.String(), "saddle set up this repo: ") || !strings.Contains(out.String(), "wrote .saddle/config.toml") {
		t.Fatalf("report %q", out.String())
	}
}

// A second run repairs nothing and says nothing; config.toml is kept.
func TestEnsureIsIdempotentAndKeepsConfig(t *testing.T) {
	root := gitRepo(t)
	calls := 0
	if _, err := Ensure(root, fakeInit(root, &calls)); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, ".saddle", "config.toml")
	if err := os.WriteFile(cfg, []byte("# mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	steps, err := Ensure(root, fakeInit(root, &calls))
	if err != nil || len(steps) != 0 {
		t.Fatalf("second Ensure: %v %q", err, steps)
	}
	if b, _ := os.ReadFile(cfg); string(b) != "# mine\n" {
		t.Fatalf("config.toml overwritten: %q", b)
	}
	var out bytes.Buffer
	Report(&out, nil)
	if out.Len() != 0 {
		t.Fatalf("report of nothing: %q", out.String())
	}
}

// The half-initialized repo from #163: state.db exists, nothing else.
func TestInspectHalfInit(t *testing.T) {
	root := gitRepo(t)
	if err := os.MkdirAll(filepath.Join(root, ".saddle"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".saddle", "state.db"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := Inspect(root)
	if s.Config || s.Hooks || s.Ignored || s.Complete() {
		t.Fatalf("half-initialized repo inspected as %+v", s)
	}
}

func TestEnsureReturnsInitError(t *testing.T) {
	root := gitRepo(t)
	if _, err := Ensure(root, func() error { return errors.New("boom") }); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}
