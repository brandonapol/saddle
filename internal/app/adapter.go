package app

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
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
	if name == usage.Codex {
		if sa := a.codexSSOArgs(); sa != nil {
			// The user's own args come last, so they can still override.
			return c.Cmd, append(append([]string(nil), sa...), c.Args...)
		}
	}
	return c.Cmd, c.Args
}

// Adapters lists which adapters can run on this machine (#182).
// Each carries its provider, whether it runs hooks, when it is out of
// quota, its reset (#321), and its rank for the next pick (#481).
func (a *App) Adapters() []agent.Status {
	ss := a.adapterStatus()
	now := time.Now()
	ex := a.ExhaustedAdapters(now)
	exm := a.ExhaustedModels(now)
	picks := map[string]AdapterChoice{}
	for _, c := range a.rankAdapters(pickReq{def: a.Cfg.Harness}, ex, now) {
		picks[c.Name] = c
	}
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
		s.ModelsOutOfQuota = modelsOut(exm, s.Name)
		if c, ok := picks[s.Name]; ok {
			s.PickRank, s.Pick = c.Rank, c.Why
		}
		s.ExcludedModels = a.Cfg.ExcludedModels(s.Name)
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
	return a.applySSOStatus(agent.Availability(cmds))
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

// TaskAdapterName is the adapter task was launched with, "" when it is unknown.
func (a *App) TaskAdapterName(task string) string {
	t, err := a.Store.Task(task)
	if err != nil {
		return ""
	}
	return a.AdapterOf(t)
}

// AdapterOf is the adapter t was launched with, for status (#480).
func (a *App) AdapterOf(t store.Task) string {
	if t.Adapter != "" {
		return t.Adapter
	}
	return a.taskAdapter(t).Name()
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
	tm := tmux.TimingOf(a.Tmux)
	time.AfterFunc(tm.RetryFor+tm.ConfirmFor, release)
	tmux.Deliver(a.Tmux, t.Window, ad.Inject(store.FormatNotices(ns)), tmux.Delivery{
		Force:   force,
		Blocked: promptShown,
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
// claims. Done let them through only under AllowClaimedEnv (#489), so the
// orchestrator is told.
func (a *App) advisoryClaims(t store.Task) string {
	lines := a.claimedByOthers(t)
	if len(lines) == 0 {
		return ""
	}
	return fmt.Sprintf("\n%s's %s agent runs no saddle hooks, and it finished with files other tasks claim (%s=1): %s. Check with their owners before landing.",
		t.ID, a.taskAdapter(t).Name(), agent.AllowClaimedEnv, strings.Join(lines, "; "))
}
