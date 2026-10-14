package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/store"
)

func openCodePane(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "agent", "testdata", "opencode", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// #520: the hookless idle watcher reads opencode's captured panes. Busy is
// the "esc interrupt" footer, idle is the cwd footer, approval is the
// permission panel, and quota is the usage-limit reply.
func TestOpenCodeIdleDetectorCapturedPanes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, want string }{
		{"busy.txt", screenBusy},
		{"idle.txt", screenIdle},
		{"worker-idle.txt", screenIdle},
		{"draft.txt", screenIdle},
		{"approval.txt", screenApproval},
		{"quota.txt", screenQuota},
	} {
		if got := classifyHooklessScreen(openCodePane(t, c.name)); got != c.want {
			t.Errorf("classify(%s) = %q, want %q", c.name, got, c.want)
		}
	}
}

// #520: a busy opencode stays running, an idle one becomes idle, and an
// approval or quota pane is left for the prompt and limit watchers.
func TestObserveHooklessOpenCodePanes(t *testing.T) {
	a, _ := setup(t)
	w, err := a.Spawn(SpawnReq{Title: "opencode work", Adapter: agent.OpenCodeName})
	must(t, err)
	if ad := a.taskAdapter(ocTask(t, a, w.ID)); ad.Hooks() || ad.Name() != agent.OpenCodeName {
		t.Fatalf("task adapter = %s hooks %v", ad.Name(), ad.Hooks())
	}
	still := HooklessIdleAfter
	a.ObserveHooklessScreen(w.ID, store.Running, still, openCodePane(t, "busy.txt"))
	if got := ocTask(t, a, w.ID).Status; got != store.Running {
		t.Fatalf("busy pane set status %q", got)
	}
	a.ObserveHooklessScreen(w.ID, store.Running, still, openCodePane(t, "idle.txt"))
	if got := ocTask(t, a, w.ID).Status; got != store.Idle {
		t.Fatalf("idle pane set status %q, want idle", got)
	}
	must(t, a.Store.SetStatus(w.ID, store.Running))
	a.ObserveHooklessScreen(w.ID, store.Running, still, openCodePane(t, "approval.txt"))
	if got := ocTask(t, a, w.ID).Status; got != store.Running {
		t.Fatalf("approval pane set status %q", got)
	}
	a.ObserveHooklessScreen(w.ID, store.Running, still, openCodePane(t, "quota.txt"))
	if got := ocTask(t, a, w.ID).Status; got != store.Running {
		t.Fatalf("quota pane set status %q", got)
	}
	must(t, a.Store.SetStatus(w.ID, store.Idle))
	a.ObserveHooklessScreen(w.ID, store.Idle, 0, openCodePane(t, "busy.txt"))
	if got := ocTask(t, a, w.ID).Status; got != store.Running {
		t.Fatalf("busy footer on an idle worker set status %q, want running", got)
	}
}

func ocTask(t *testing.T, a *App, id string) store.Task {
	t.Helper()
	tk, err := a.Store.Task(id)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// #533: an opencode worker's limit screen marks the worker's configured
// provider/model out (not the footer's display name), and spawns that would
// use it fail over while the worker's other models stay usable.
func TestOpenCodeLimitScreenMarksConfiguredModelAndFailsOver(t *testing.T) {
	a, _, _ := rotationApp(t)
	a.Cfg.Fallback = map[string][]string{"opencode:openai/gpt-5": {"claude"}}
	w, err := a.Spawn(SpawnReq{Title: "oc", Adapter: agent.OpenCodeName, Model: "openai/gpt-5"})
	must(t, err)
	a.ObserveHooklessScreen(w.ID, store.Running, HooklessIdleAfter, openCodePane(t, "quota.txt"))
	ex := a.ExhaustedModels(time.Now())
	e, ok := ex[exhaustKey(agent.OpenCodeName, "openai/gpt-5")]
	if !ok || len(ex) != 1 {
		t.Fatalf("exhausted models %+v", ex)
	}
	if d := time.Until(e.Until); d < 2*time.Hour || d > 2*time.Hour+6*time.Minute {
		t.Fatalf("until in %v, want about 2h5m", d)
	}
	if _, err := a.Spawn(SpawnReq{Title: "next", Adapter: agent.OpenCodeName, Model: "openai/gpt-5"}); err == nil || !strings.Contains(err.Error(), "out of quota") {
		t.Fatalf("spawn on the out model: %v, want an out-of-quota refusal", err)
	}
	o, err := a.Spawn(SpawnReq{Title: "other", Adapter: agent.OpenCodeName, Model: "anthropic/claude-sonnet-4-5"})
	must(t, err)
	if r := agent.Recorded(a.stateDir("run", o.ID)); r != agent.OpenCodeName {
		t.Fatalf("another model ran %s, want opencode", r)
	}
}
