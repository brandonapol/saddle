package contract

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/e2e/fakegh"
)

// Fake is a fakegh world for one scenario: a bare origin holding the fake
// GitHub's state and a clone of it where gh runs.
type Fake struct {
	t     testing.TB
	Bare  string
	Clone string
	GH    fakegh.Repo
}

// NewFake makes a repo e2e/demo with main holding one commit.
func NewFake(t testing.TB) *Fake {
	t.Helper()
	d := t.TempDir()
	f := &Fake{t: t, Bare: filepath.Join(d, "origin.git"), Clone: filepath.Join(d, "work")}
	f.git(d, "init", "-q", "--bare", "-b", "main", f.Bare)
	f.git(d, "clone", "-q", f.Bare, f.Clone)
	f.write("README", "hello\n")
	f.git(f.Clone, "add", "-A")
	f.git(f.Clone, "commit", "-q", "-m", "init")
	f.git(f.Clone, "push", "-q", "origin", "HEAD:main")
	var err error
	if f.GH, err = fakegh.Init(f.Bare, "e2e", "demo"); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *Fake) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *Fake) write(name, body string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.Clone, name), []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// Run is a forge.Runner over the fake gh, run in the clone.
func (f *Fake) Run(_ context.Context, args ...string) (string, error) {
	var out, errb bytes.Buffer
	code := fakegh.Main(f.Clone, args, &out, &errb)
	return cliResult(args, out.String(), errb.String(), code)
}

// Must runs gh and fails the test unless it exits 0.
func (f *Fake) Must(args ...string) string {
	f.t.Helper()
	out, err := f.Run(context.Background(), args...)
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

// PR commits a file on branch head, cut from base, pushes it and opens a
// PR of head into base. It returns the PR's number.
func (f *Fake) PR(head, base string) string {
	f.t.Helper()
	f.git(f.Clone, "fetch", "-q", "origin")
	f.git(f.Clone, "checkout", "-q", "-B", head, "origin/"+base)
	f.write(head+".txt", head+"\n")
	f.git(f.Clone, "add", "-A")
	f.git(f.Clone, "commit", "-q", "-m", head)
	f.git(f.Clone, "push", "-q", "-f", "origin", head)
	url := f.Must("pr", "create", "--base", base, "--head", head, "--title", head, "--body", "")
	return url[strings.LastIndex(url, "/")+1:]
}

// Stack opens a PR per branch, each on the one before and the first on
// main, and links them into a native stack. It returns the PR numbers and
// the stack's.
func (f *Fake) Stack(branches ...string) (prs []string, stack string) {
	f.t.Helper()
	base := "main"
	for _, b := range branches {
		prs = append(prs, f.PR(b, base))
		base = b
	}
	f.Must(append([]string{"stack", "link", "--base", "main"}, prs...)...)
	s, err := f.GH.Load()
	if err != nil {
		f.t.Fatal(err)
	}
	return prs, strconv.Itoa(s.Stacks[len(s.Stacks)-1].Number)
}

// Update changes the fake's state.
func (f *Fake) Update(fn func(*fakegh.State)) {
	f.t.Helper()
	if err := f.GH.Update(func(s *fakegh.State) error { fn(s); return nil }); err != nil {
		f.t.Fatal(err)
	}
}

// Num is PR (or stack) n's state number.
func Num(n string) int {
	v, _ := strconv.Atoi(n)
	return v
}
