//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type planTask struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Goal     string   `json:"goal"`
	Claims   []string `json:"claims"`
	Deps     []string `json:"deps"`
	Barrier  bool     `json:"barrier"`
	DoneWhen []string `json:"done_when"`
}

func task(id, title, claim string) planTask {
	return planTask{ID: id, Title: title, Goal: title + ".", Claims: []string{claim}, Deps: []string{}, DoneWhen: []string{"tests pass"}}
}

// fakePlanner makes the World's claude answer the planner's calls (claude -p
// --output-format json, the prompt on stdin) with canned plans: first, or
// replanned once the prompt carries replanNote. Every other call still goes
// to the fake agent.
func (w *World) fakePlanner(replanNote string, first, replanned []planTask) {
	w.T.Helper()
	for name, ts := range map[string][]planTask{"plan-first.json": first, "plan-replanned.json": replanned} {
		plan, err := json.Marshal(map[string]any{"tasks": ts})
		must(w.T, err)
		out, err := json.Marshal(map[string]any{"type": "result", "is_error": false, "result": string(plan)})
		must(w.T, err)
		must(w.T, os.WriteFile(filepath.Join(w.Scripts, name), out, 0o644))
	}
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = -p ] && [ "$2" = --output-format ] && [ "$3" = json ]; then
	if grep -q %s; then cat %s; else cat %s; fi
	exit 0
fi
exec %s --script-dir %s "$@"
`, shq(replanNote), shq(filepath.Join(w.Scripts, "plan-replanned.json")), shq(filepath.Join(w.Scripts, "plan-first.json")),
		shq(w.Bins.Agent), shq(w.Scripts))
	must(w.T, os.WriteFile(filepath.Join(w.Bin, "claude"), []byte(script), 0o755))
}

// setTUIEnv sets an environment variable for the TUI session started next.
func (w *World) setTUIEnv(k, v string) {
	w.T.Helper()
	// set-environment needs a running server; a parked session keeps it up.
	if !w.tmuxUp() {
		must(w.T, w.Tmux.NewSession("e2e-park", 40, 10, w.Repo, "exec cat"))
	}
	_, err := w.Tmux.Run("set-environment", "-g", k, v)
	must(w.T, err)
}

func (w *World) tmuxUp() bool {
	_, err := w.Tmux.Run("list-sessions")
	return err == nil
}

// TestJourneyPlanViewKeys: saddle plan writes a draft from the planner; the
// TUI's plan view (alt+2) shows it, e edits it in $EDITOR and re-checks it,
// r replans it with a note, a approves and freezes it (after which e and r
// refuse), g hands it to the orchestrator, and a on an approved plan reopens
// it.
func TestJourneyPlanViewKeys(t *testing.T) {
	w := world(t, Options{})
	w.fakePlanner("add a cli task",
		[]planTask{task("api", "Build the API", "api/**"), task("docs", "Write the docs", "docs/**")},
		[]planTask{task("api", "Build the API", "api/**"), task("docs", "Write the docs", "docs/**"), task("cli", "Add the CLI", "cli/**")})
	w.WriteFile("epic.md", "# Widget epic\n\nBuild widgets: an API, docs for it and a CLI.\n")
	r := w.MustSaddle("plan", "epic.md")
	path := filepath.Join(w.Repo, ".saddle", "plans", "widget-epic.toml")
	if !strings.Contains(r.Stdout, path) || !strings.Contains(r.Stdout, "Build the API") {
		t.Fatalf("saddle plan:\n%s", r)
	}

	editor := filepath.Join(w.Bin, "edit-plan")
	must(t, os.WriteFile(editor, []byte("#!/bin/sh\nsed -i 's/Write the docs/Write the user guide/' \"$1\"\n"), 0o755))
	w.setTUIEnv("EDITOR", editor)
	w.setTUIEnv("VISUAL", "")
	u := w.StartTUI(140, 45)

	u.Keys("M-2")
	u.WaitScreen("Widget epic", "draft", "Build the API", "Write the docs")
	if err := u.Fits(140); err != nil {
		t.Fatal(err)
	}

	u.Keys("e")
	u.WaitScreen("checked widget-epic.toml")
	u.WaitScreen("Write the user guide")
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "Write the user guide") {
		t.Fatalf("the edit isn't in the plan file:\n%s", b)
	}

	u.Keys("r")
	u.WaitScreen("replan:")
	u.Type("add a cli task")
	u.Keys("Enter")
	u.WaitScreen("replanned widget-epic.toml")
	u.WaitScreen("Add the CLI")

	u.Keys("a")
	u.WaitScreen("approved widget-epic.toml: 3 tasks frozen")
	head := w.Git(w.Repo, "rev-parse", "main")
	u.WaitScreen("approved at " + head[:12])
	if r := w.MustSaddle("plan", "show", path); !strings.Contains(r.Stdout, "Add the CLI") {
		t.Fatalf("plan show after approve:\n%s", r.Stdout)
	}
	u.Keys("e")
	u.WaitScreen("press a")
	u.Keys("r")
	u.WaitScreen("plan is approved; a reopens it before replanning")

	u.Keys("g")
	u.WaitScreen("plan handed to the orchestrator")
	u.WaitScreen("fake orchestrator ack: Run the approved plan")
	if s := u.Screen(); !strings.Contains(s, "ORCHESTRATOR") {
		t.Fatalf("g didn't return to the control view:\n%s", s)
	}

	u.Keys("M-2")
	u.WaitScreen("approved at")
	u.Keys("a")
	u.WaitScreen("reopened widget-epic.toml")
	u.WaitScreen("draft")
	if r := w.Exec(w.Repo, "env", "EDITOR=true", w.Bins.Saddle, "plan", "edit", path); r.Code != 0 {
		t.Fatalf("plan edit after reopening from the TUI: %s", r)
	}
	u.Quit()
}
