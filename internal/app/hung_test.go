package app

import (
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/tmux"
)

// hungWorker spawns a claude worker that has started its task and then
// stopped moving: running, its screen frozen on a spinner, reading no
// notices. No process runs in its worktree.
func hungWorker(t *testing.T) (*App, *fakeTmux, store.Task) {
	t.Helper()
	return hungWorkerWith(t, "build the meter")
}

func hungWorkerWith(t *testing.T, prompt string) (*App, *fakeTmux, store.Task) {
	t.Helper()
	a, ft, _ := rotationApp(t)
	ft.timing = &tmux.Timing{RetryEvery: time.Millisecond, RetryFor: 20 * time.Millisecond, ConfirmEvery: time.Millisecond, ConfirmFor: 20 * time.Millisecond}
	a.Cfg.AdapterOrder = []string{"claude", "codex", "grok"}
	a.Cfg.AdapterPolicy.Pick = config.PickOrder
	procs := hungProcs
	hungProcs = func() ([]Proc, error) { return nil, nil }
	t.Cleanup(func() { hungProcs = procs })
	tk, err := a.Spawn(SpawnReq{Title: "meter", Prompt: prompt, Claims: []string{"billing/**"}, Adapter: "claude"})
	must(t, err)
	must(t, a.Store.SetStatus(tk.ID, store.Running))
	ft.setScreen(tk.Window, "✻ Compacting conversation… (12s)\n╭──────╮\n│ >    │\n╰──────╯")
	_, _ = a.Store.TakeNotices(tk.ID, false)
	tk, err = a.Store.Task(tk.ID)
	must(t, err)
	return a, ft, tk
}

func hungState(t *testing.T, a *App, task string) HungWorker {
	t.Helper()
	hs, err := a.HungWorkers()
	must(t, err)
	for _, h := range hs {
		if h.Task == task {
			return h
		}
	}
	return HungWorker{}
}

// #625 acceptance: a worker that stops reading its notices, with a screen
// that never moves, is flagged hung, nudged, pressed Enter at, and then
// replaced: a new session on a hooked adapter in the same worktree and
// branch, with its claims, told it is taking over. Nothing it committed or
// left uncommitted is lost.
func TestHungWorkerIsNudgedThenTakenOver(t *testing.T) {
	a, ft, tk := hungWorker(t)
	write(t, tk.Worktree, "billing/meter.go", "package billing\n\nfunc Meter() int { return 2 }\n")
	commitAll(t, tk.Worktree, "meter: return 2")
	write(t, tk.Worktree, "billing/wip.go", "package billing\n")
	window := a.StallConfig().HungAfter
	if window != DefaultHungAfter {
		t.Fatalf("hung_after default = %s", window)
	}
	now := time.Now().UTC()
	tick := func(at time.Time) []string {
		t.Helper()
		acted, err := a.HungTick(at)
		must(t, err)
		return acted
	}

	tick(now)
	for i := range 3 { // the orchestrator keeps writing; nobody reads
		must(t, a.Notify(tk.ID, store.NoticeAction, fmt.Sprintf("message %d", i)))
	}
	if acted := tick(now.Add(window / 2)); len(acted) != 0 {
		t.Fatalf("acted before the window: %v", acted)
	}
	if acted := tick(now.Add(window)); len(acted) != 1 {
		t.Fatalf("not flagged after %s still: %v", window, acted)
	}
	h := hungState(t, a, tk.ID)
	if h.Step != HungNudged || len(openCardsOf(t, a, DecisionWorkerHung)) != 1 {
		t.Fatalf("after flagging: %+v, cards %v", h, openCardsOf(t, a, DecisionWorkerHung))
	}
	if sent := ft.sentTo(tk.Window); len(sent) == 0 {
		t.Fatal("the nudge typed nothing")
	}
	if !hasEvent(t, a, EventWorkerHung, "notices sit unread") {
		t.Fatal("the hung event doesn't name the unread notices")
	}

	if acted := tick(now.Add(2 * window)); len(acted) != 1 || hungState(t, a, tk.ID).Step != HungEntered {
		t.Fatalf("Enter step: acted %v, %+v", acted, hungState(t, a, tk.ID))
	}

	old := tk.Window
	if acted := tick(now.Add(3 * window)); len(acted) != 1 {
		t.Fatalf("takeover: acted %v", acted)
	}
	got, err := a.Store.Task(tk.ID)
	must(t, err)
	if got.Window == "" || got.Window == old || ft.Alive(old) {
		t.Fatalf("not relaunched: window %q (old %q, alive %v)", got.Window, old, ft.Alive(old))
	}
	if got.Worktree != tk.Worktree || got.Branch != tk.Branch || got.Status != store.Running {
		t.Fatalf("takeover moved the task: %+v", got)
	}
	if ad, err := agent.ByName(a.AdapterOf(got)); err != nil || !ad.Hooks() || ad.Name() == "claude" {
		t.Fatalf("taken over on %q, want another hooked adapter", a.AdapterOf(got))
	}
	if !runDirHas(t, a, tk.ID, "You are taking over this task") {
		t.Fatal("the new agent isn't told it is taking over")
	}
	cl, err := a.Store.Claims()
	must(t, err)
	if len(cl[tk.ID]) == 0 {
		t.Fatal("the takeover lost its claims")
	}
	if out := git(t, tk.Worktree, "log", "-1", "--format=%s"); strings.TrimSpace(out) != "meter: return 2" {
		t.Fatalf("branch head %q", out)
	}
	if out := git(t, tk.Worktree, "status", "--porcelain"); !strings.Contains(out, "wip.go") {
		t.Fatalf("uncommitted work gone: %q", out)
	}
	if len(openCardsOf(t, a, DecisionWorkerHung)) != 0 {
		t.Fatal("the card stayed open after the takeover")
	}
	if !hasEvent(t, a, EventHungTakeover, "claude -> ") {
		t.Fatal("no takeover event")
	}
}

// #625: a slow worker that keeps producing output, or keeps committing
// under a screen that only shows a timer, is never flagged.
func TestSlowProgressingWorkerIsNotHung(t *testing.T) {
	a, ft, tk := hungWorker(t)
	window := a.StallConfig().HungAfter
	now := time.Now().UTC()
	pkgs := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet", "kilo", "lima"}
	for i, pkg := range pkgs {
		ft.setScreen(tk.Window, fmt.Sprintf("● Bash(go test ./...)\n  ok  %s\n✻ Working… (%dm)", pkg, i))
		if acted, err := a.HungTick(now.Add(time.Duration(i) * window / 2)); err != nil || len(acted) != 0 {
			t.Fatalf("tick %d: acted %v, %v", i, acted, err)
		}
	}
	now = now.Add(6 * window)
	for i := range 12 {
		ft.setScreen(tk.Window, fmt.Sprintf("✻ Working… (%dm %ds)", 40+i, i))
		write(t, tk.Worktree, fmt.Sprintf("billing/f%d.go", i), "package billing\n")
		if i%2 == 1 {
			commitAll(t, tk.Worktree, fmt.Sprintf("step %d", i))
		}
		if acted, err := a.HungTick(now.Add(time.Duration(i) * window / 2)); err != nil || len(acted) != 0 {
			t.Fatalf("committing tick %d: acted %v, %v", i, acted, err)
		}
	}
	if fl, _ := a.FlaggedHung(); len(fl) != 0 {
		t.Fatalf("flagged a progressing worker: %+v", fl)
	}
}

// #625 (t318, t320): grok's "Waiting for response" with a ticking timer is
// not progress.
func TestHungTimerOnlyScreenIsStill(t *testing.T) {
	if normScreen("⠋ Waiting for response 38m 12s · ↓ 1.2k") != normScreen("⠙ Waiting for response 38m 13s · ↓ 1.3k") {
		t.Fatal("a timer tick counted as a screen change")
	}
	if normScreen("ok pkg/a") == normScreen("ok pkg/b") {
		t.Fatal("new output didn't count")
	}
	a, ft, tk := hungWorker(t)
	window := a.StallConfig().HungAfter
	now := time.Now().UTC()
	for i := range 5 {
		ft.setScreen(tk.Window, fmt.Sprintf("⠋ Waiting for response %dm %ds", 20+i, i*7))
		_, err := a.HungTick(now.Add(time.Duration(i) * window / 4))
		must(t, err)
	}
	if h := hungState(t, a, tk.ID); h.Step != HungNudged {
		t.Fatalf("a worker waiting on a model call for %s wasn't flagged: %+v", window, h)
	}
}

// #625: a worker the owner asked saddle not to kill is nudged but never
// replaced.
func TestHungKeepNeverKills(t *testing.T) {
	for _, how := range []string{"keep", "card", "prompt"} {
		t.Run(how, func(t *testing.T) {
			prompt := "build the meter"
			if how == "prompt" {
				prompt = "long migration; do not kill this task"
			}
			a, ft, tk := hungWorkerWith(t, prompt)
			window := a.StallConfig().HungAfter
			now := time.Now().UTC()
			if how == "keep" {
				must(t, a.KeepWorker(tk.ID, "the owner said so"))
			}
			for i := range 3 {
				_, err := a.HungTick(now.Add(time.Duration(i) * window))
				must(t, err)
				if how == "card" && i == 1 {
					cs := openCardsOf(t, a, DecisionWorkerHung)
					if len(cs) != 1 {
						t.Fatalf("cards %v", cs)
					}
					_, err := a.AnswerDecisionID(cs[0].ID, 2, now)
					must(t, err)
				}
			}
			_, err := a.HungTick(now.Add(3 * window))
			must(t, err)
			got, err := a.Store.Task(tk.ID)
			must(t, err)
			if got.Window != tk.Window || !ft.Alive(tk.Window) || hasEvent(t, a, EventHungTakeover, "") {
				t.Fatalf("killed a kept worker: %+v", got)
			}
			if h := hungState(t, a, tk.ID); h.Step != HungKept {
				t.Fatalf("step %q, want kept", h.Step)
			}
		})
	}
}

// #625: after two takeovers the next hang goes to the orchestrator instead
// of a third relaunch.
func TestHungGivesUpAfterTwoTakeovers(t *testing.T) {
	a, ft, tk := hungWorker(t)
	a.Store.Event(tk.ID, EventHungTakeover, "claude -> grok window @9")
	a.Store.Event(tk.ID, EventHungTakeover, "grok -> claude window @10")
	window := a.StallConfig().HungAfter
	now := time.Now().UTC()
	for i := range 4 {
		_, err := a.HungTick(now.Add(time.Duration(i) * window))
		must(t, err)
	}
	if h := hungState(t, a, tk.ID); h.Step != HungGaveUp || !ft.Alive(tk.Window) {
		t.Fatalf("step %q, window alive %v", h.Step, ft.Alive(tk.Window))
	}
}

// #625 (t309, and #432): a gate in a worker's worktree past twice its own
// timeout is stopped, SIGTERM then SIGKILL; one within it is progress.
func TestHungGateIsKilledFirst(t *testing.T) {
	a, _, tk := hungWorker(t)
	elapsed := 15 * time.Minute
	hungProcs = func() ([]Proc, error) {
		return []Proc{{PID: 4242, Elapsed: elapsed, Args: "go test -timeout 5m ./...", Cwd: tk.Worktree}}, nil
	}
	var sent []syscall.Signal
	sig := signalProc
	signalProc = func(pid int, s syscall.Signal) error {
		if pid != 4242 {
			t.Errorf("signalled pid %d", pid)
		}
		sent = append(sent, s)
		return nil
	}
	t.Cleanup(func() { signalProc = sig })
	now := time.Now().UTC()
	for i := range 2 {
		acted, err := a.HungTick(now.Add(time.Duration(i) * time.Minute))
		must(t, err)
		if len(acted) != 1 {
			t.Fatalf("tick %d acted %v", i, acted)
		}
	}
	if len(sent) != 2 || sent[0] != syscall.SIGTERM || sent[1] != syscall.SIGKILL {
		t.Fatalf("signals %v", sent)
	}
	if !hasEvent(t, a, EventHungGateKilled, "past twice its 5m timeout") {
		t.Fatal("no hung-gate event")
	}

	// A gate still inside its timeout keeps the worker un-flagged however
	// long its screen sits still.
	sent, elapsed = nil, 4*time.Minute
	window := a.StallConfig().HungAfter
	for i := range 5 {
		_, err := a.HungTick(now.Add(time.Duration(i) * window))
		must(t, err)
	}
	if len(sent) != 0 || hungState(t, a, tk.ID).Flagged() {
		t.Fatalf("a live gate: signals %v, %+v", sent, hungState(t, a, tk.ID))
	}
}

// #625: a done-orphaned worker is the #467 watch's, and an idle worker
// with nothing unread is the engine's; the hung watch leaves both alone.
func TestHungSkipsDoneOrphansAndQuietIdle(t *testing.T) {
	a, _, tk := hungWorker(t)
	must(t, a.writeOrphans(map[string]DoneOrphan{tk.ID: {Task: tk.ID, Since: time.Now()}}))
	window := a.StallConfig().HungAfter
	now := time.Now().UTC()
	for i := range 4 {
		_, err := a.HungTick(now.Add(time.Duration(i) * window))
		must(t, err)
	}
	if hs, _ := a.HungWorkers(); len(hs) != 0 {
		t.Fatalf("watched a done-orphan: %+v", hs)
	}

	must(t, a.writeOrphans(nil))
	must(t, a.Store.SetStatus(tk.ID, store.Idle))
	for i := range 4 {
		_, err := a.HungTick(now.Add(time.Duration(4+i) * window))
		must(t, err)
	}
	if hs, _ := a.HungWorkers(); len(hs) != 0 {
		t.Fatalf("watched a quiet idle worker: %+v", hs)
	}
	// Idle with notices piling up unread is watched.
	must(t, a.Notify(tk.ID, store.NoticeAction, "rebase onto t12"))
	for i := range 3 {
		_, err := a.HungTick(now.Add(time.Duration(8+i) * window))
		must(t, err)
	}
	if !hungState(t, a, tk.ID).Flagged() {
		t.Fatal("an idle worker ignoring its notices wasn't flagged")
	}
}

// #625: hung_after < 0 turns the watch off.
func TestHungWatchOff(t *testing.T) {
	a, _, tk := hungWorker(t)
	write(t, a.Root, ".saddle/config.toml", "[stall]\nhung_after = \"-1s\"\n")
	now := time.Now().UTC()
	for i := range 4 {
		acted, err := a.HungTick(now.Add(time.Duration(i) * DefaultHungAfter))
		must(t, err)
		if len(acted) != 0 {
			t.Fatalf("acted with the watch off: %v", acted)
		}
	}
	if hs, _ := a.HungWorkers(); len(hs) != 0 {
		t.Fatalf("watched %s with the watch off: %+v", tk.ID, hs)
	}
}
