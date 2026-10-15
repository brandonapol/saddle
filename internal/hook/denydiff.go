package hook

import (
	"bytes"
	stdctx "context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/claims"
)

// Bounds on the owner's diff quoted in a claim denial (#328): enough to show
// the new shape, small enough to cost the denied agent next to nothing.
const (
	maxDiffLines   = 40
	maxDiffBytes   = 3 << 10
	maxHunkHeaders = 10
)

// diffTimeout bounds all the git calls behind one quote. The hook fails open:
// past it, the denial goes out without the diff.
var diffTimeout = 200 * time.Millisecond

// ownerDiff is the section a claim denial of the file at abs appends: the
// claim owner's current change to it against the integration branch,
// committed and dirty, or a note that the owner hasn't touched it yet. It is
// "" when no other task claims the file or the diff can't be had in time.
func ownerDiff(a *app.App, task, abs string) string {
	return denyDiff(a, task, abs, "")
}

// denyDiff is ownerDiff for the task a deny names: owner when set (a symbol
// deny names the task whose edit it overlaps, #591, which need not hold the
// path claim), else the file's path-claim owner.
func denyDiff(a *app.App, task, abs, owner string) string {
	t, err := a.Store.Task(task)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(filepath.Clean(t.Worktree), filepath.Clean(abs))
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if owner == "" || owner == task {
		all, err := a.Store.Claims()
		if err != nil {
			return ""
		}
		owner, _ = claims.Owner(all, task, rel)
	}
	if owner == "" {
		return ""
	}
	ot, err := a.Store.Task(owner)
	if err != nil || ot.Worktree == "" {
		return ""
	}
	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), diffTimeout)
	defer cancel()
	diff, ok := worktreeDiff(ctx, ot.Worktree, a.Cfg.Integration, rel)
	if !ok {
		return ""
	}
	if diff == "" {
		return fmt.Sprintf("\n\n%s claims this but has not changed it yet, so sequencing is cheap: wait for it to land, or ask the orchestrator to order the work.", owner)
	}
	return fmt.Sprintf("\n\n%s's current change to %s (against %s):\n```diff\n%s```\nDepend on the new shape shown above, stub it, or finish and mention the change you need in your done summary.",
		owner, rel, a.Cfg.Integration, quoteDiff(diff))
}

// worktreeDiff diffs rel in the worktree wt against its merge-base with base,
// covering commits and dirty edits, and a new untracked file as all added.
// ok is false when git failed or ran out of time.
func worktreeDiff(ctx stdctx.Context, wt, base, rel string) (diff string, ok bool) {
	mb, err := gitCtx(ctx, wt, "merge-base", base, "HEAD")
	if err != nil {
		return "", false
	}
	mb = strings.TrimSpace(mb)
	diff, err = gitCtx(ctx, wt, "diff", "--no-color", "--no-ext-diff", mb, "--", rel)
	if err != nil {
		return "", false
	}
	if diff != "" {
		return diff, true
	}
	untracked, err := gitCtx(ctx, wt, "ls-files", "--others", "--exclude-standard", "--", rel)
	if err != nil {
		return "", false
	}
	if strings.TrimSpace(untracked) == "" {
		return "", true
	}
	// --no-index exits 1 when the files differ, which they always do here.
	diff, err = gitCtx(ctx, wt, "diff", "--no-color", "--no-ext-diff", "--no-index", "--", "/dev/null", rel)
	var ee *exec.ExitError
	if err != nil && (!errors.As(err, &ee) || ee.ExitCode() != 1) {
		return "", false
	}
	return diff, true
}

// gitCtx runs git in dir under ctx and returns its raw stdout.
func gitCtx(ctx stdctx.Context, dir string, args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Stdout = &out
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return out.String(), err
}

// quoteDiff caps diff at maxDiffLines lines and maxDiffBytes bytes. A longer
// diff keeps its head, then lists the later hunks' headers so the reader
// still sees where else the file changes.
func quoteDiff(diff string) string {
	lines := strings.SplitAfter(diff, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var b strings.Builder
	i := 0
	for ; i < len(lines) && i < maxDiffLines && b.Len()+len(lines[i]) <= maxDiffBytes; i++ {
		b.WriteString(lines[i])
	}
	if i == len(lines) {
		return b.String()
	}
	if !strings.HasSuffix(b.String(), "\n") {
		b.WriteString("\n")
	}
	headers, more := 0, 0
	for _, l := range lines[i:] {
		if !strings.HasPrefix(l, "@@") {
			continue
		}
		if headers == maxHunkHeaders {
			more++
			continue
		}
		headers++
		b.WriteString(strings.TrimRight(l, "\n") + "\n")
	}
	note := fmt.Sprintf("... truncated: %d more lines", len(lines)-i)
	if headers > 0 {
		note += "; later hunk headers listed above"
	}
	if more > 0 {
		note += fmt.Sprintf(", %d more hunks not listed", more)
	}
	b.WriteString(note + "\n")
	return b.String()
}
