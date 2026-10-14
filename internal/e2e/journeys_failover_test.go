//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

// fakeGrok installs a grok CLI and its login in w. As a worker (tmux) it
// records its arguments in grok-args, commits a file in its worktree and
// calls saddle done. Headless (grok-bridge's --prompt-file turns) it
// appends each prompt to grok-orch.log and answers in Claude's stream-json.
func fakeGrok(t *testing.T, w *World) {
	t.Helper()
	root := shq(w.Root)
	script := `#!/usr/bin/env bash
pf=""
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
  if [ "${args[i]}" = --prompt-file ]; then pf="${args[i+1]}"; fi
done
if [ -n "$pf" ]; then
  { echo "--- turn"; cat "$pf"; echo; } >> ` + root + `/grok-orch.log
  echo '{"type":"system","subtype":"init","session_id":"grok-orch-1"}'
  echo '{"type":"assistant","message":{"content":[{"type":"text","text":"grok orchestrator took over"}]},"session_id":"grok-orch-1"}'
  echo '{"type":"result","subtype":"success","result":"grok orchestrator took over","session_id":"grok-orch-1"}'
  exit 0
fi
printf '%s\n' "$@" > ` + root + `/grok-args
mkdir -p finished
echo "finished on grok" > finished/grok.txt
` + fa.IndexLockRetry(`git add -A && git commit -qm "finish on grok"`) + `
` + shq(w.Bins.Saddle) + ` done -s "finished on grok after claude ran out" > ` + root + `/grok-done.out 2>&1
exec sleep 600
`
	must(t, os.WriteFile(filepath.Join(w.Bin, "grok"), []byte(script), 0o755))
	must(t, os.MkdirAll(filepath.Join(w.Home, ".grok"), 0o755))
	must(t, os.WriteFile(filepath.Join(w.Home, ".grok", "auth.json"), []byte("{}"), 0o600))
}

// TestJourneyHandoffWhenWorkerRunsOutOfQuota (#481): a claude worker hits
// its usage limit mid-task with a reset hours away. Instead of parking it,
// the engine marks claude out and hands the task to grok: a new grok agent
// in the same worktree and branch, told it is taking over, finishes the
// work and calls done, and the task queues in the train. Status names grok
// as the task's adapter and ranks grok first for new spawns.
func TestJourneyHandoffWhenWorkerRunsOutOfQuota(t *testing.T) {
	if Fork(t) {
		return
	}
	w := world(t, Options{})
	fakeGrok(t, w)
	banner := "You've hit your session limit · resets in 3 hours\n\n● Usage limit reached · esc to cancel\n"
	w.Spawn("t1", "Limited work", []string{"limited/**"},
		fa.Write("limited/a.txt", "a\n"), fa.Commit("claude's half"),
		fa.Step{Run: "printf '%s' " + shq(banner) + " > /dev/tty; exec sleep 600", Unhooked: true})
	before := w.Status()
	var wt string
	for _, v := range before.Tasks {
		if v.ID == "t1" {
			wt = v.Worktree
		}
	}
	startEngine(w)

	args := waitFile(t, filepath.Join(w.Root, "grok-args"))
	for _, want := range []string{"You are taking over this task", "ran out of quota on claude", "Prompt for Limited work"} {
		if !strings.Contains(args, want) {
			t.Fatalf("grok's prompt lacks %q:\n%s", want, args)
		}
	}
	w.WaitTask("t1", "queued in the train", func(v mcpserver.TaskView) bool { return queued(v) })
	st := w.Status()
	for _, v := range st.Tasks {
		if v.ID == "t1" && (v.Adapter != "grok" || v.Worktree != wt) {
			t.Fatalf("t1 after the handoff: adapter %q worktree %q (was %q)", v.Adapter, v.Worktree, wt)
		}
	}
	log := w.Git(wt, "log", "--format=%s", "-3")
	if !strings.Contains(log, "finish on grok") || !strings.Contains(log, "claude's half") {
		t.Fatalf("the branch lacks one agent's commits:\n%s", log)
	}
	ranks := map[string]int{}
	for _, s := range st.Adapters {
		ranks[s.Name] = s.PickRank
		if s.Name == "claude" && s.OutOfQuotaUntil == "" {
			t.Fatalf("claude not out of quota in status: %+v", st.Adapters)
		}
	}
	if ranks["grok"] != 1 || ranks["claude"] != 0 || !strings.Contains(st.AdapterPolicy, "failover handoff") {
		t.Fatalf("status picks %v, policy %q", ranks, st.AdapterPolicy)
	}
}

// TestJourneyOrchestratorSurvivesItsAdapterRunningOut (#481): saddle up's
// orchestrator (Claude Code) fails a turn on its usage limit. The TUI marks
// claude out of quota and restarts the orchestrator on grok, whose first
// turn is a state summary naming the live task and the owner's last words;
// the chat says what happened and the header names grok. The owner's next
// message reaches grok.
func TestJourneyOrchestratorSurvivesItsAdapterRunningOut(t *testing.T) {
	if Fork(t) {
		return
	}
	w := world(t, Options{})
	fakeGrok(t, w)
	w.Spawn("t1", "Steady work", []string{"steady/**"}, fa.Wait("never-comes"))
	w.WaitStatus("t1", "idle")

	u := w.StartTUI(160, 45)
	u.Type("fail: You've hit your session limit · resets in 3 hours")
	u.Keys("Enter")
	u.WaitScreen("ran out of quota on claude", "grok orchestrator took over")
	u.WaitScreen("grok ·")

	orch := waitFile(t, filepath.Join(w.Root, "grok-orch.log"))
	for _, want := range []string{"taking over from the previous session on claude", "t1", "Steady work", "You've hit your session limit"} {
		if !strings.Contains(orch, want) {
			t.Fatalf("grok's first turn lacks %q:\n%s", want, orch)
		}
	}
	out := map[string]string{}
	for _, s := range w.Status().Adapters {
		out[s.Name] = s.OutOfQuotaUntil
	}
	if out["claude"] == "" || out["grok"] != "" {
		t.Fatalf("status adapters: %+v", w.Status().Adapters)
	}

	u.Type("how is t1 doing")
	u.Keys("Enter")
	Eventually(t, "grok to get the next message", func() error {
		b, _ := os.ReadFile(filepath.Join(w.Root, "grok-orch.log"))
		if !strings.Contains(string(b), "how is t1 doing") {
			return errorf("grok-orch.log:\n%s", b)
		}
		return nil
	})
	u.Quit()
}
