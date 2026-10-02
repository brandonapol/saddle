package app

import (
	"fmt"
	"sort"
	"strings"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/claims"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/tmux"
	"github.com/brandonapol/saddle/internal/usage"
)

// adapterCmd is the command and extra arguments an adapter launches with.
// Empty means the adapter's default binary. Codex and Grok have no config
// keys yet (#142).
func (a *App) adapterCmd(name string) (string, []string) {
	if name == usage.Claude {
		return a.Cfg.Claude.Cmd, nil
	}
	return "", nil
}

// taskAdapter is the adapter a task was launched with.
func (a *App) taskAdapter(t store.Task) agent.Adapter {
	ad, err := agent.ByName(agent.Recorded(a.stateDir("run", t.ID)))
	if err != nil {
		return agent.Claude{}
	}
	return ad
}

// injectNotices types every pending notice into a hookless agent's window:
// it has no hook to collect them and never reports itself idle.
func (a *App) injectNotices(t store.Task, ad agent.Adapter) error {
	if !a.ownWindow(t) {
		return nil
	}
	ns, err := a.Store.TakeNotices(t.ID, false)
	if err != nil || len(ns) == 0 {
		return err
	}
	tmux.SendWhenIdle(a.Tmux, t.Window, ad.Inject(store.FormatNotices(ns)), nil)
	return nil
}

// advisoryClaims lists files a hookless task changed that another task
// claims. Nothing stopped those writes, so the orchestrator is told.
func (a *App) advisoryClaims(t store.Task) string {
	out, err := gitx.Run(t.Worktree, "diff", "--name-only", a.Cfg.Integration+"...HEAD")
	if err != nil || out == "" {
		return ""
	}
	all, err := a.Store.Claims()
	if err != nil {
		return ""
	}
	var lines []string
	for _, f := range strings.Split(out, "\n") {
		if owner, glob := claims.Owner(all, t.ID, f); owner != "" {
			lines = append(lines, fmt.Sprintf("%s (claimed by %s, %s)", f, owner, glob))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	sort.Strings(lines)
	return fmt.Sprintf("\nClaims are advisory for %s's %s agent, and it changed files other tasks claim: %s. Check with their owners before landing.",
		t.ID, a.taskAdapter(t).Name(), strings.Join(lines, "; "))
}
