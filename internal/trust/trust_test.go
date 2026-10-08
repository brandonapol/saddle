package trust

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// repo is a git repo with origin set, and a private trust store.
func repo(t *testing.T) (string, *Store) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv(EnvFile, "")
	t.Setenv(EnvTrust, "")
	t.Setenv("SADDLE_TASK", "")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	root := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-q", "-b", "main")
	gitRun(t, root, "commit", "-q", "--allow-empty", "-m", "one")
	gitRun(t, root, "remote", "add", "origin", "git@github.com:o/demo.git")
	s, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cfg, "saddle", "trust.json"); s.Path() != want {
		t.Fatalf("store path %s, want %s (XDG_CONFIG_HOME)", s.Path(), want)
	}
	return root, s
}

// SADDLE_TRUST_FILE wins over XDG_CONFIG_HOME, so a test run never reads
// the user's real decisions.
func TestDefaultHonorsTrustFile(t *testing.T) {
	root, _ := repo(t)
	f := filepath.Join(t.TempDir(), "trust.json")
	t.Setenv(EnvFile, f)
	s, err := Default()
	if err != nil || s.Path() != f {
		t.Fatalf("store %v, err %v; want %s", s, err, f)
	}
	if err := Decide(root, Options{Yes: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f); err != nil {
		t.Fatalf("decision not written to %s: %v", f, err)
	}
}

func TestPromptListsWhatSaddleDoes(t *testing.T) {
	p := Prompt("/src/demo")
	for _, want := range []string{
		"/src/demo",
		".saddle/",
		"git hooks", "reference-transaction", "pre-push", "chained",
		".claude/settings.local.json", "MCP server",
		"edit files", "run commands",
		"push", "merge", "auto-merge",
		"1. Yes, trust this folder and continue",
		"2. No, exit (nothing written)",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
}

func TestRememberTrustedForget(t *testing.T) {
	root, s := repo(t)
	r, err := Identify(root)
	if err != nil {
		t.Fatal(err)
	}
	if r.Origin != "git@github.com:o/demo.git" {
		t.Fatalf("origin %q", r.Origin)
	}
	if ok, err := s.Trusted(r); err != nil || ok {
		t.Fatalf("fresh store trusted=%v err=%v", ok, err)
	}
	if err := s.Remember(r); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Trusted(r); !ok {
		t.Fatal("remembered repo isn't trusted")
	}
	fi, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("trust.json mode %v, want 0600", fi.Mode().Perm())
	}
	// Remembering twice keeps one entry.
	if err := s.Remember(r); err != nil {
		t.Fatal(err)
	}
	if es, _ := s.Entries(); len(es) != 1 {
		t.Fatalf("entries %+v, want one", es)
	}
	if err := s.Forget(r); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Trusted(r); ok {
		t.Fatal("forgotten repo still trusted")
	}
}

func TestChangedOriginOrMovedPathReasks(t *testing.T) {
	root, s := repo(t)
	r, _ := Identify(root)
	if err := s.Remember(r); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "remote", "set-url", "origin", "git@github.com:evil/demo.git")
	r2, _ := Identify(root)
	if ok, _ := s.Trusted(r2); ok {
		t.Fatal("changed origin is still trusted")
	}
	gitRun(t, root, "remote", "set-url", "origin", "git@github.com:o/demo.git")
	moved := filepath.Join(filepath.Dir(root), "moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	r3, _ := Identify(moved)
	if ok, _ := s.Trusted(r3); ok {
		t.Fatal("moved repo is still trusted")
	}
	var out bytes.Buffer
	err := Gate(moved, Options{Store: s, Out: &out})
	if !errors.Is(err, ErrUntrusted) || !strings.Contains(err.Error(), "saddle trust") {
		t.Fatalf("non-interactive gate on a moved repo: %v", err)
	}
}

func TestSymlinkedPathIsTheSameRepo(t *testing.T) {
	root, s := repo(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skip(err)
	}
	r, _ := Identify(root)
	if err := s.Remember(r); err != nil {
		t.Fatal(err)
	}
	r2, _ := Identify(link)
	if ok, _ := s.Trusted(r2); !ok {
		t.Fatalf("symlinked path %s isn't trusted as %s", r2.Path, r.Path)
	}
}

func TestGateInteractive(t *testing.T) {
	root, s := repo(t)
	var out bytes.Buffer
	err := Gate(root, Options{Store: s, Interactive: true, In: strings.NewReader("2\n"), Out: &out})
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("declined: %v", err)
	}
	if !strings.Contains(out.String(), "Yes, trust this folder") {
		t.Fatalf("prompt not shown:\n%s", out.String())
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Fatalf("declining wrote trust.json: %v", err)
	}
	out.Reset()
	if err := Gate(root, Options{Store: s, Interactive: true, In: strings.NewReader("1\n"), Out: &out}); err != nil {
		t.Fatal(err)
	}
	// Remembered: no prompt, no input needed.
	out.Reset()
	if err := Gate(root, Options{Store: s, Interactive: true, In: strings.NewReader(""), Out: &out}); err != nil || out.Len() != 0 {
		t.Fatalf("remembered decision re-asked: err %v out %q", err, out.String())
	}
}

func TestGateEOFDeclines(t *testing.T) {
	root, s := repo(t)
	var out bytes.Buffer
	if err := Gate(root, Options{Store: s, Interactive: true, In: strings.NewReader(""), Out: &out}); !errors.Is(err, ErrDeclined) {
		t.Fatalf("EOF: %v", err)
	}
}

func TestGateNonInteractiveFlagAndEnv(t *testing.T) {
	root, s := repo(t)
	var out bytes.Buffer
	if err := Gate(root, Options{Store: s, Out: &out}); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("non-interactive without --trust: %v", err)
	}
	if !strings.Contains(out.String(), ".saddle/") {
		t.Fatalf("refusal doesn't show what saddle would do:\n%s", out.String())
	}
	t.Setenv(EnvTrust, "1")
	if err := Gate(root, Options{Store: s, Out: &out}); err != nil {
		t.Fatalf("SADDLE_TRUST=1: %v", err)
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Fatal("SADDLE_TRUST=1 recorded a decision; it should apply to this run only")
	}
	t.Setenv(EnvTrust, "")
	if err := Gate(root, Options{Store: s, Out: &out, Yes: true}); err != nil {
		t.Fatalf("--trust: %v", err)
	}
	r, _ := Identify(root)
	if ok, _ := s.Trusted(r); !ok {
		t.Fatal("--trust didn't remember the decision")
	}
}

func TestWorktreeAgentsUnaffected(t *testing.T) {
	root, s := repo(t)
	wt := filepath.Join(root, ".saddle", "worktrees", "t1")
	gitRun(t, root, "worktree", "add", "-q", "-b", "saddle/t1", wt)
	r, _ := Identify(root)
	if err := s.Remember(r); err != nil {
		t.Fatal(err)
	}
	// A worktree of a trusted repo is trusted: it is the same repo.
	if err := Gate(wt, Options{Store: s}); err != nil {
		t.Fatalf("worktree of a trusted repo: %v", err)
	}
	// An agent (SADDLE_TASK set) runs under a trusted parent and is never asked.
	if err := s.Forget(r); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SADDLE_TASK", "t1")
	if err := Gate(wt, Options{Store: s}); err != nil {
		t.Fatalf("agent blocked: %v", err)
	}
}

func TestCorruptStoreIsAnError(t *testing.T) {
	root, s := repo(t)
	if err := os.MkdirAll(filepath.Dir(s.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path(), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, _ := Identify(root)
	if _, err := s.Trusted(r); err == nil {
		t.Fatal("corrupt trust.json read as untrusted without an error")
	}
	// Remember repairs it rather than leaving the user stuck (#119).
	if err := s.Remember(r); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Trusted(r); err != nil || !ok {
		t.Fatalf("after Remember over a corrupt file: ok %v err %v", ok, err)
	}
}

func TestDevNullIsNotATerminal(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if IsTerminal(f) {
		t.Fatal("/dev/null counted as a terminal: the prompt would wait on it")
	}
}
