package app

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/claims"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// Claims for hookless agents (#489). Codex and gemini run no saddle hooks,
// so nothing stops their writes. Saddle checks their claims where it can
// without the agent's help: the pre-commit hook (`saddle refguard
// pre-commit`) runs CheckCommit, and done refuses a branch that changes
// files other tasks claim. Both take AllowClaimedEnv=1 as the escape hatch
// (#119), recorded as EventClaimsOverridden. Claude and grok workers are
// left alone: their hooks already enforce claims.

// EventClaimsOverridden is a hookless task committing or finishing with
// files other tasks claim under AllowClaimedEnv; data: what and the files.
const EventClaimsOverridden = "claims_overridden"

// allowClaimed reports whether the caller set AllowClaimedEnv=1.
func allowClaimed() bool { return os.Getenv(agent.AllowClaimedEnv) == "1" }

// hookless reports whether t runs no saddle hooks, so claims are checked at
// commit and done instead.
func (a *App) hookless(t store.Task) bool {
	return t.Role == store.RoleWorker && !a.taskAdapter(t).Hooks()
}

// CheckCommit is the pre-commit check for a hookless task: each staged file
// (relative to the task's worktree) goes through CheckWrite, as a hooked
// agent's write would, so a file nobody claims becomes the task's and one
// another task claims is refused, naming its owner. allow commits anyway and
// records the override. Any other task, or one saddle can't find, passes.
func (a *App) CheckCommit(task string, files []string, allow bool) error {
	t, err := a.Store.Task(task)
	if err != nil || !a.hookless(t) {
		return nil
	}
	var refused, reasons []string
	for _, f := range files {
		if d := a.CheckWrite(task, filepath.Join(t.Worktree, f)); !d.Allow {
			refused = append(refused, f)
			reasons = append(reasons, "- "+d.Reason)
		}
	}
	if len(refused) == 0 {
		return nil
	}
	if allow {
		a.Store.Event(task, EventClaimsOverridden, "commit: "+strings.Join(refused, ", "))
		return nil
	}
	return fmt.Errorf("[saddle] commit refused: %s runs no saddle hooks, so saddle checks claims here, and this commit touches files it may not take:\n%s\n"+
		"Unstage and restore them (git restore --staged --worktree -- <file>), then commit again. "+
		"If the change truly must land with this task, commit with %s=1; saddle records the override and tells the orchestrator",
		task, strings.Join(reasons, "\n"), agent.AllowClaimedEnv)
}

// claimedByOthers lists the files t's branch changes from integration that
// another task claims, as "file (claimed by owner, glob)", sorted.
func (a *App) claimedByOthers(t store.Task) []string {
	out, err := gitx.Run(t.Worktree, "diff", "--name-only", "--no-renames", a.Cfg.Integration+"...HEAD")
	if err != nil || out == "" {
		return nil
	}
	all, err := a.Store.Claims()
	if err != nil {
		return nil
	}
	var lines []string
	for _, f := range strings.Split(out, "\n") {
		if owner, glob := claims.Owner(all, t.ID, f); owner != "" {
			lines = append(lines, fmt.Sprintf("%s (claimed by %s, %s)", f, owner, glob))
		}
	}
	sort.Strings(lines)
	return lines
}

// doneClaims refuses done for a hookless task whose branch changes files
// other tasks claim, unless AllowClaimedEnv=1, which is recorded. A commit
// made with --no-verify, or before the hook was installed, is caught here.
func (a *App) doneClaims(t store.Task) error {
	if !a.hookless(t) {
		return nil
	}
	lines := a.claimedByOthers(t)
	if len(lines) == 0 {
		return nil
	}
	if allowClaimed() {
		a.Store.Event(t.ID, EventClaimsOverridden, "done: "+strings.Join(lines, "; "))
		return nil
	}
	return fmt.Errorf("[saddle] done refused: %s runs no saddle hooks, and its branch changes files other tasks claim: %s. "+
		"Restore them from %s (git checkout %s -- <file>) and commit, or ask each owner (message tool) or the orchestrator to release the file (`saddle release --task <owner> <file>`), then call done again. "+
		"If the change truly must land with this task, run done with %s=1; saddle records the override and tells the orchestrator",
		t.ID, strings.Join(lines, "; "), a.Cfg.Integration, a.Cfg.Integration, agent.AllowClaimedEnv)
}
