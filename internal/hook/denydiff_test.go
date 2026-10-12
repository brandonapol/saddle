package hook

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
)

// The hook's 200ms diff budget keeps a denial fast in a live session, failing
// open past it. Tests run beside other agents' gates at load average 50+,
// where git alone can take longer, so they lift it and check the diff, not
// the clock (#425). TestDenyFailsOpenWhenDiffIsLate keeps the bound covered.
func init() { diffTimeout = time.Minute }

// denyReason runs a PreToolUse Edit of rel in task's worktree and returns the
// deny reason, failing the test if the write was allowed.
func denyReason(t *testing.T, a *app.App, task, wt, rel string) string {
	t.Helper()
	out := run(t, a, task, map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": filepath.Join(wt, rel)},
	})
	hs, _ := out["hookSpecificOutput"].(map[string]any)
	if hs["permissionDecision"] != "deny" {
		t.Fatalf("not denied: %v", out)
	}
	return hs["permissionDecisionReason"].(string)
}

// #328: the denied agent sees what the owner is changing in the file.
func TestDenyQuotesOwnersDirtyHunk(t *testing.T) {
	a := setup(t)
	t1, err := a.Spawn(app.SpawnReq{Title: "one", Claims: []string{"a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	t2, _ := a.Spawn(app.SpawnReq{Title: "two"})
	if err := os.WriteFile(filepath.Join(t1.Worktree, "a.go"), []byte("package a\n\nfunc NewShape() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := denyReason(t, a, t2.ID, t2.Worktree, "a.go")
	for _, want := range []string{t1.ID, "@@", "+func NewShape() int { return 1 }", "Depend on the new shape"} {
		if !strings.Contains(r, want) {
			t.Fatalf("reason lacks %q:\n%s", want, r)
		}
	}
}

func TestDenyQuotesOwnersNewUntrackedFile(t *testing.T) {
	a := setup(t)
	t1, err := a.Spawn(app.SpawnReq{Title: "one", Claims: []string{"b.go"}})
	if err != nil {
		t.Fatal(err)
	}
	t2, _ := a.Spawn(app.SpawnReq{Title: "two"})
	if err := os.WriteFile(filepath.Join(t1.Worktree, "b.go"), []byte("package a\n\nvar Brand = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := denyReason(t, a, t2.ID, t2.Worktree, "b.go"); !strings.Contains(r, "+var Brand = 1") {
		t.Fatalf("reason lacks the new file:\n%s", r)
	}
}

func TestDenySaysOwnerHasNotChangedFileYet(t *testing.T) {
	a := setup(t)
	t1, err := a.Spawn(app.SpawnReq{Title: "one", Claims: []string{"a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	t2, _ := a.Spawn(app.SpawnReq{Title: "two"})
	r := denyReason(t, a, t2.ID, t2.Worktree, "a.go")
	if !strings.Contains(r, t1.ID+" claims this but has not changed it yet") || strings.Contains(r, "```") {
		t.Fatalf("reason = %s", r)
	}
}

func TestDenyTruncatesHugeOwnerDiff(t *testing.T) {
	a := setup(t)
	t1, err := a.Spawn(app.SpawnReq{Title: "one", Claims: []string{"a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	t2, _ := a.Spawn(app.SpawnReq{Title: "two"})
	var b strings.Builder
	b.WriteString("package a\n")
	for i := range 2000 {
		fmt.Fprintf(&b, "var v%d = %d\n", i, i)
	}
	if err := os.WriteFile(filepath.Join(t1.Worktree, "a.go"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	r := denyReason(t, a, t2.ID, t2.Worktree, "a.go")
	if !strings.Contains(r, "truncated") || len(r) > 5000 {
		t.Fatalf("not truncated (%d bytes):\n%s", len(r), r)
	}
	if !strings.Contains(r, "+var v0 = 0") {
		t.Fatalf("lost the start of the diff:\n%s", r)
	}
}

func TestQuoteDiffKeepsLaterHunkHeaders(t *testing.T) {
	var b strings.Builder
	b.WriteString("diff --git a/x b/x\n--- a/x\n+++ b/x\n")
	for h := range 5 {
		fmt.Fprintf(&b, "@@ -%d,30 +%d,30 @@ func f%d()\n", h*100, h*100, h)
		for i := range 30 {
			fmt.Fprintf(&b, "+line %d %d\n", h, i)
		}
	}
	q := quoteDiff(b.String())
	if n := strings.Count(q, "\n"); n > maxDiffLines+maxHunkHeaders+5 {
		t.Fatalf("quote has %d lines", n)
	}
	if len(q) > maxDiffBytes+1024 {
		t.Fatalf("quote has %d bytes", len(q))
	}
	if !strings.Contains(q, "@@ -400,30 +400,30 @@ func f4()") || !strings.Contains(q, "truncated") {
		t.Fatalf("quote = %s", q)
	}
	if small := "@@ -1 +1 @@\n-a\n+b\n"; quoteDiff(small) != small {
		t.Fatalf("short diff changed: %q", quoteDiff(small))
	}
}

// The write guard's other denials (serial files) get no diff section.
func TestDenyWithoutOwnerAddsNoDiff(t *testing.T) {
	a := setup(t)
	a.Cfg.Serial = []string{"a.go"}
	t2, _ := a.Spawn(app.SpawnReq{Title: "two"})
	if r := denyReason(t, a, t2.ID, t2.Worktree, "a.go"); strings.Contains(r, "has not changed") || strings.Contains(r, "```") {
		t.Fatalf("reason = %s", r)
	}
}

// slowGit puts a git first on PATH that sleeps before running the real one,
// as git does on a machine at load average 50+ beside other agents' gates.
func slowGit(t *testing.T, delay string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	shim := fmt.Sprintf("#!/bin/sh\nsleep %s\nexec %q \"$@\"\n", delay, real)
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// #425: a loaded gate made git slower than the hook's diff budget, so the
// TestDeny* tests lost the owner's diff and failed on load alone.
func TestDenyQuotesOwnersDiffWhenGitIsSlow(t *testing.T) {
	a := setup(t)
	t1, err := a.Spawn(app.SpawnReq{Title: "one", Claims: []string{"b.go"}})
	if err != nil {
		t.Fatal(err)
	}
	t2, _ := a.Spawn(app.SpawnReq{Title: "two"})
	if err := os.WriteFile(filepath.Join(t1.Worktree, "b.go"), []byte("package a\n\nvar Brand = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	slowGit(t, "0.3")
	if r := denyReason(t, a, t2.ID, t2.Worktree, "b.go"); !strings.Contains(r, "+var Brand = 1") {
		t.Fatalf("reason lacks the new file:\n%s", r)
	}
}

// Past diffTimeout the denial still goes out, without the diff.
func TestDenyFailsOpenWhenDiffIsLate(t *testing.T) {
	a := setup(t)
	t1, err := a.Spawn(app.SpawnReq{Title: "one", Claims: []string{"b.go"}})
	if err != nil {
		t.Fatal(err)
	}
	t2, _ := a.Spawn(app.SpawnReq{Title: "two"})
	if err := os.WriteFile(filepath.Join(t1.Worktree, "b.go"), []byte("package a\n\nvar Brand = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	slowGit(t, "0.3")
	defer func(d time.Duration) { diffTimeout = d }(diffTimeout)
	diffTimeout = 100 * time.Millisecond
	r := denyReason(t, a, t2.ID, t2.Worktree, "b.go")
	if !strings.Contains(r, "b.go is claimed by "+t1.ID) || strings.Contains(r, "```") {
		t.Fatalf("reason = %s", r)
	}
}
