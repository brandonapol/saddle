package app

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/claims"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/tmux"
	"github.com/brandonapol/saddle/internal/usage"
)

// adapterCmd is the command and extra arguments an adapter launches with.
// Empty means the adapter's default binary. Claude's comes from [claude],
// the others' from [adapters.<name>] (#142).
func (a *App) adapterCmd(name string) (string, []string) {
	if name == usage.Claude {
		return a.Cfg.Claude.Cmd, nil
	}
	c := a.Cfg.Adapters[name]
	return c.Cmd, c.Args
}

// Adapters lists which adapters can run on this machine (#182).
// Each carries its provider, whether it runs hooks, and when it is out of
// quota, its reset (#321).
func (a *App) Adapters() []agent.Status {
	ss := a.adapterStatus()
	ex := a.ExhaustedAdapters(time.Now())
	for i := range ss {
		s := &ss[i]
		if s.Provider == "" {
			s.Provider = agent.ProviderOf(s.Name)
		}
		if ad, err := agent.ByName(s.Name); err == nil {
			s.Hooks = ad.Hooks()
		}
		if e, out := ex[s.Name]; out {
			s.OutOfQuotaUntil = e.when()
		}
	}
	return ss
}

// adapterStatus lists which adapters can run on this machine.
func (a *App) adapterStatus() []agent.Status {
	if a.AdapterStatus != nil {
		return a.AdapterStatus()
	}
	cmds := map[string]string{usage.Claude: a.Cfg.Claude.Cmd, usage.Grok: a.Cfg.Grok.Cmd}
	for n, c := range a.Cfg.Adapters {
		if c.Cmd != "" {
			cmds[n] = c.Cmd
		}
	}
	return agent.Availability(cmds)
}

// checkAdapter refuses an adapter that can't run here, naming the reason and
// the ones that can (#182). It never substitutes another adapter.
func (a *App) checkAdapter(name string) error {
	ss := a.adapterStatus()
	for _, s := range ss {
		if s.Name == name && !s.OK {
			return agent.Unavailable(name, errors.New(s.Reason), ss)
		}
	}
	return nil
}

// taskAdapter is the adapter a task was launched with; the session's
// harness when its run dir recorded none (#321).
func (a *App) taskAdapter(t store.Task) agent.Adapter {
	name, ok := agent.RecordedName(a.stateDir("run", t.ID))
	if !ok {
		name = a.Cfg.Harness
	}
	ad, err := agent.ByName(name)
	if err != nil {
		return agent.Claude{}
	}
	return ad
}

// injectNotices types every pending notice into a hookless agent's window:
// it has no hook to collect them and never reports itself idle. Notices are
// marked delivered only once the pane changed after the submit (#183); until
// then a second call leaves them alone rather than typing them twice. force
// types over what looks like a draft.
func (a *App) injectNotices(t store.Task, ad agent.Adapter, force bool) error {
	if !a.ownWindow(t) {
		return nil
	}
	w := a.wakeState()
	w.mu.Lock()
	if w.injecting[t.ID] != 0 {
		w.mu.Unlock()
		return nil
	}
	ns, err := a.Store.PeekNotices(t.ID, false)
	if err != nil || len(ns) == 0 {
		w.mu.Unlock()
		return err
	}
	w.gen++
	gen := w.gen
	w.injecting[t.ID] = gen
	w.mu.Unlock()
	release := func() {
		w.mu.Lock()
		if w.injecting[t.ID] == gen {
			delete(w.injecting, t.ID)
		}
		w.mu.Unlock()
	}
	// Let go if the text never goes in or the pane never confirms it; the
	// notices stay pending for the next notice or the idle wake.
	time.AfterFunc(tmux.RetryFor+tmux.ConfirmFor, release)
	tmux.Deliver(a.Tmux, t.Window, ad.Inject(store.FormatNotices(ns)), tmux.Delivery{
		Force: force,
		Sent: func() {
			_ = a.Store.MarkDelivered(ns)
			release()
			if n, err := a.Store.PendingNotices(t.ID); err == nil && n > 0 {
				_ = a.injectNotices(t, ad, false)
			}
		},
	})
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
