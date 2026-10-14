package app

import (
	"os"
	"path/filepath"
	"testing"

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
