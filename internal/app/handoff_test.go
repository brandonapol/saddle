package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// runDirHas reports whether any file in task's run dir contains s.
func runDirHas(t *testing.T, a *App, task, s string) bool {
	t.Helper()
	found := false
	_ = filepath.Walk(a.stateDir("run", task), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), s) {
				found = true
			}
		}
		return nil
	})
	return found
}

// #481: a claude worker whose adapter runs out mid-task is relaunched on
// the next adapter with quota, a hooked one since it claims paths, in the
// same worktree and branch, told it is taking over. The old window is
// gone, the orchestrator hears who took over, and the task runs on.
func TestLimitHitHandsWorkerToNextAdapter(t *testing.T) {
	a, ft, _ := rotationApp(t)
	a.Cfg.AdapterOrder = []string{"claude", "codex", "grok"}
	a.Cfg.AdapterPolicy.Pick = config.PickOrder
	tk, err := a.Spawn(SpawnReq{Title: "meter", Prompt: "build the meter", Claims: []string{"meter/**"}})
	must(t, err)
	must(t, a.Store.SetStatus(tk.ID, StatusPaused))
	old := tk.Window

	to, err := a.LimitHit(tk.ID, usage.LimitBanner{Resets: "in 3 hours"})
	must(t, err)
	if to != usage.Grok {
		t.Fatalf("handed to %q, want grok (hooked, for a task with claims)", to)
	}
	if tk.Adapter != usage.Claude {
		t.Fatalf("spawned task records adapter %q, want claude", tk.Adapter)
	}
	got, err := a.Store.Task(tk.ID)
	must(t, err)
	if got.Adapter != usage.Grok || a.AdapterOf(got) != usage.Grok {
		t.Fatalf("handed-off task adapter %q, want grok", got.Adapter)
	}
	if got.Status != store.Running || got.Window == "" || got.Window == old || got.Worktree != tk.Worktree || got.Branch != tk.Branch {
		t.Fatalf("after handoff: %+v (old window %s)", got, old)
	}
	if ft.Alive(old) {
		t.Fatal("the out-of-quota agent's window still runs")
	}
	if r := agent.Recorded(a.stateDir("run", tk.ID)); r != usage.Grok {
		t.Fatalf("recorded adapter %s", r)
	}
	if got.Model != a.Cfg.Grok.Model {
		t.Fatalf("model %q kept from claude", got.Model)
	}
	if !runDirHas(t, a, tk.ID, "You are taking over this task") || !runDirHas(t, a, tk.ID, "build the meter") {
		t.Fatal("the finisher's prompt lacks the task or the handoff note")
	}
	evs, err := a.Store.Events(200)
	must(t, err)
	var text string
	for _, e := range evs {
		if e.Task == OrchestratorID || e.Kind == EventAdapterHandoff {
			text += e.Kind + ": " + e.Data + "\n"
		}
	}
	for _, want := range []string{EventAdapterHandoff + ": claude -> grok", "handed it to grok", "are handed to the next adapter with quota"} {
		if !strings.Contains(text, want) {
			t.Errorf("events lack %q:\n%s", want, text)
		}
	}
}

// #481: a worker parks instead when failover = "park", when its reset is
// within handoff_wait, or when no other adapter has quota.
func TestHandOffParks(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(a *App)
		until time.Duration
	}{
		{"park policy", func(a *App) { a.Cfg.AdapterPolicy.Failover = config.FailoverPark }, 3 * time.Hour},
		{"reset soon", func(a *App) {}, 10 * time.Minute},
		{"nobody has quota", func(a *App) {
			a.Cfg.AdapterOrder = []string{"claude", "grok"}
			must(t, a.MarkExhausted("grok", time.Now().Add(time.Hour), ""))
		}, 3 * time.Hour},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, ft, _ := rotationApp(t)
			c.setup(a)
			tk, err := a.Spawn(SpawnReq{Title: "meter"})
			must(t, err)
			must(t, a.MarkExhausted("claude", time.Now().Add(c.until), ""))
			to, err := a.HandOff(tk.ID, time.Now())
			must(t, err)
			if to != "" || !ft.Alive(tk.Window) || agent.Recorded(a.stateDir("run", tk.ID)) != usage.Claude {
				t.Fatalf("handed to %q; window alive %v", to, ft.Alive(tk.Window))
			}
		})
	}
}
