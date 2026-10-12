package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
)

// saddle up --rc, or [orchestrator] remote_control, takes the engine lock as
// up-rc, which leaves t0's notices to the Remote Control session (#409).
func TestUpRCFlagAndConfig(t *testing.T) {
	if upCmd().Flags().Lookup("rc") == nil {
		t.Fatal("saddle up has no --rc")
	}
	cmd := &cobra.Command{}
	wants := addRCFlag(cmd)
	a := &app.App{}
	if wants(a) {
		t.Error("RC on with neither the flag nor config")
	}
	a.Cfg.Orchestrator.RemoteControl = true
	if !wants(a) {
		t.Error("[orchestrator] remote_control = true doesn't turn RC on")
	}
	a.Cfg.Orchestrator.RemoteControl = false
	if err := cmd.Flags().Set("rc", "true"); err != nil {
		t.Fatal(err)
	}
	if !wants(a) {
		t.Error("--rc doesn't turn RC on")
	}
	if upLock(false) != app.LockUp || upLock(true) != app.LockUpRC {
		t.Error("the lock owner doesn't follow --rc")
	}
	if !app.SessionWakes(upLock(true)) || app.SessionWakes(upLock(false)) {
		t.Error("only saddle up --rc leaves notices to the session")
	}
}

// The mod's feed runs inside the Remote Control orchestrator (SADDLE_TASK
// t0) and takes its notices while saddle up --rc holds the lock (#409).
// Workers stay out.
func TestPluginEventsFeedTheRCSession(t *testing.T) {
	a := pluginRepo(t)
	t.Setenv("SADDLE_TASK", app.OrchestratorID)
	if s, why := openSession(a.Root); s == nil {
		t.Fatalf("the t0 session can't open the repo: %s", why)
	} else {
		s.Close()
	}
	if s, _ := openPlugin(a.Root); s != nil {
		s.Close()
		t.Error("openPlugin admits t0: the plugin's MCP server would duplicate saddle's")
	}
	t.Setenv("SADDLE_TASK", "t3")
	if s, _ := openSession(a.Root); s != nil {
		s.Close()
		t.Error("a worker session opens the orchestrator's feed")
	}

	release, err := a.AcquireLock(app.LockUpRC)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_ = a.Store.Notify(app.OrchestratorID, store.NoticeAction, "t2 needs you")
	var out bytes.Buffer
	if err := writeEvents(&out, a); err != nil {
		t.Fatal(err)
	}
	var got eventsOut
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got.Events) != 1 || got.Events[0].Kind != store.NoticeAction {
		t.Fatalf("under up-rc the feed gave %q (%v)", out.String(), err)
	}
}
