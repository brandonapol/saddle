package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
)

// beatMod writes the mod's heartbeat, as the mod does on each tick.
func beatMod(t *testing.T, a *app.App) {
	t.Helper()
	if err := os.WriteFile(a.ModHeartbeatPath(), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPluginEventsTakesNoticesOnlyWhileEngineRuns(t *testing.T) {
	a := pluginRepo(t)
	_ = a.Store.Notify(app.OrchestratorID, store.NoticeInfo, "t1 landed")
	_ = a.Store.Notify(app.OrchestratorID, store.NoticeAction, "t2 conflicted")
	var out bytes.Buffer
	if err := writeEvents(&out, a); err != nil {
		t.Fatal(err)
	}
	var got eventsOut
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got.Events) != 0 {
		t.Fatalf("no engine: %q err %v", out.String(), err)
	}

	release, err := a.AcquireLock(app.LockEngine)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	out.Reset()
	if err := writeEvents(&out, a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []event{{Kind: store.NoticeInfo, Text: "t1 landed"}, {Kind: store.NoticeAction, Text: "t2 conflicted"}}
	if len(got.Events) != 2 || got.Events[0] != want[0] || got.Events[1] != want[1] {
		t.Fatalf("events %+v, want %+v", got.Events, want)
	}
	if n, _ := a.Store.PendingNotices(app.OrchestratorID); n != 0 {
		t.Fatalf("%d notices left pending: the settings hook would deliver them again", n)
	}
}

func TestPluginHookStepsAsideWhileModIsActive(t *testing.T) {
	a := pluginRepo(t)
	release, err := a.AcquireLock(app.LockEngine)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_ = a.Notify(app.OrchestratorID, store.NoticeAction, "t1 needs you")
	beatMod(t, a)
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "PostToolUse", "Stop"} {
		var out bytes.Buffer
		if err := runPluginHook(hookIn(t, a.Root, ev), &out); err != nil || out.Len() != 0 {
			t.Fatalf("%s delivered while the mod is active: %q err %v", ev, out.String(), err)
		}
	}
	if n, _ := a.Store.PendingNotices(app.OrchestratorID); n != 1 {
		t.Fatalf("hook took the notice the mod should get: %d pending", n)
	}
}

func TestWaitNoticesExitsWhenModIsActive(t *testing.T) {
	a := pluginRepo(t)
	_ = a.Notify(app.OrchestratorID, store.NoticeAction, "t1 needs you")
	beatMod(t, a)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := waitNotices(ctx, a, 5*time.Millisecond, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "saddle mod") || strings.Contains(out.String(), "t1 needs you") {
		t.Fatalf("out %q", out.String())
	}
	if n, _ := a.Store.PendingNotices(app.OrchestratorID); n != 1 {
		t.Fatalf("wait took the notice the mod should get: %d pending", n)
	}
}

func TestPluginBriefSaysWhetherTheModWakes(t *testing.T) {
	a := pluginRepo(t)
	var out bytes.Buffer
	if err := writeBrief(&out, a); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Mod: not active") {
		t.Fatalf("brief without the mod:\n%s", out.String())
	}
	beatMod(t, a)
	out.Reset()
	if err := writeBrief(&out, a); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Mod: active") {
		t.Fatalf("brief with the mod:\n%s", out.String())
	}
}

func TestPluginEventsCommandRegistered(t *testing.T) {
	cmd, _, err := Root().Find([]string{"plugin", "events"})
	if err != nil || cmd.Name() != "events" {
		t.Fatalf("saddle plugin events: %v", err)
	}
}

// The mod asks for events only when status counts some for t0, so status
// must keep reporting the orchestrator's pending notices.
func TestStatusCountsOrchestratorEventsForTheMod(t *testing.T) {
	a := pluginRepo(t)
	_ = a.Store.Notify(app.OrchestratorID, store.NoticeAction, "t1 needs you")
	st, err := mcpserver.Status(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range st.Tasks {
		if task.ID == app.OrchestratorID {
			if task.Notices != 1 {
				t.Fatalf("t0 pending_notices %d, want 1", task.Notices)
			}
			return
		}
	}
	t.Fatal("status lists no t0")
}
