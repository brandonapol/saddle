package app

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// keysSent lists the keys saddle pressed in task's window.
func keysSent(t *testing.T, a *App, task string) []string {
	t.Helper()
	es, err := a.Store.Events(400)
	must(t, err)
	var out []string
	for _, e := range es {
		if e.Task == task && e.Kind == "keys" {
			out = append(out, e.Data)
		}
	}
	return out
}

// promptWorker spawns task as a live worker sitting on a prompt.
func promptWorker(t *testing.T, a *App, task string) store.Task {
	t.Helper()
	tk, err := a.Spawn(SpawnReq{ID: task, Title: "prompt"})
	must(t, err)
	must(t, a.Store.SetStatus(task, store.NeedsYou))
	_, _ = a.Store.TakeNotices(task, false)
	return tk
}

// expireCard opens c at now and runs the stall budget past its timeout.
func expireCard(t *testing.T, a *App, c DecisionCard, now time.Time) {
	t.Helper()
	c.Opened = now
	must(t, a.OpenDecision(c))
	_, _ = a.Store.TakeNotices(OrchestratorID, false)
	done, err := a.StallTick(now.Add(c.Timeout + time.Minute))
	must(t, err)
	if !slices.ContainsFunc(done, func(d DecisionCard) bool { return d.ID == c.ID && d.By == DecidedByTimeout }) {
		t.Fatalf("card %s didn't expire: closed %+v", c.ID, done)
	}
}

// autoExit fails unless task sits in a state saddle leaves by itself (#790):
// live, finished, paused with an automatic resume scheduled, or out of the
// stack on a route-around that releaseHeld or a fix puts back.
func autoExit(t *testing.T, a *App, task string) {
	t.Helper()
	tk := mustTask(t, a, task)
	switch {
	case liveStatus(tk.Status), tk.Status == store.Done, tk.Status == store.Landed:
	case tk.Status == StatusPaused:
		ps, err := a.StallParked()
		must(t, err)
		if !slices.ContainsFunc(ps, func(p StallPark) bool { return p.Task == task && !p.ResumeAt.IsZero() }) {
			t.Fatalf("%s is paused with no automatic resume scheduled: only saddle resume gets it out", task)
		}
	default:
		t.Fatalf("%s is %q, a state with no automatic exit", task, tk.Status)
	}
	if st := trainStateOf(t, a, task); st == TrainSuperseded {
		rs, err := a.StallHeld()
		must(t, err)
		if !slices.ContainsFunc(rs, func(r StallRoute) bool { return r.Red == task || slices.Contains(r.Held, task) }) {
			t.Fatalf("%s left the stack with no route-around to put it back", task)
		}
	}
}

// #790 invariant: for every kind of card saddle produces, the default the
// card shows is the action that runs when it expires, its side effects are
// the ones the card promises, and it leaves nothing in a state only a human
// command clears.
func TestDecisionDefaultsDoWhatTheCardSays(t *testing.T) {
	t.Parallel()
	type kase struct {
		todo  string // an open issue the row waits on
		build func(t *testing.T) (*App, DecisionCard)
		check func(t *testing.T, a *App, c DecisionCard)
	}
	cases := map[string]kase{
		DecisionStuckStack: {
			build: func(t *testing.T) (*App, DecisionCard) {
				a, _, now := redStack(t, [2]string{"t2", "two.txt"})
				c := stuckCard(StuckAlarm{Stack: "t1", Layer: "t1", Reason: StuckCIRed, Detail: "CI is red", For: time.Hour}, "t1", []string{"t2"})
				c.Opened = now
				return a, c
			},
			check: func(t *testing.T, a *App, c DecisionCard) {
				if !strings.Contains(c.DefaultOption().Action, "unstack t1") {
					t.Fatalf("default = %q", c.DefaultOption().Action)
				}
				if trainStateOf(t, a, "t1") != TrainSuperseded {
					t.Fatal("t1 is still in the stack: the card said it'd be dropped")
				}
				if why := a.atRisk("t2"); why != "" {
					t.Fatalf("t2 still held: %s", why)
				}
				autoExit(t, a, "t1")
			},
		},
		DecisionCIRepairCap: {
			build: func(t *testing.T) (*App, DecisionCard) {
				a, _, _ := redStack(t, [2]string{"t2", "two.txt"})
				return a, ciRepairCapCard("t1", CIRedLayer{Task: "t1", PR: mustTask(t, a, "t1").PR, Head: "abc", Checks: []string{"CI / test"}, Held: []string{"t2"}}, 2)
			},
			check: func(t *testing.T, a *App, c DecisionCard) {
				if !strings.Contains(c.DefaultOption().Action, "unstack t1") {
					t.Fatalf("default = %q", c.DefaultOption().Action)
				}
				if trainStateOf(t, a, "t1") != TrainSuperseded {
					t.Fatal("t1 is still in the stack: the card said it'd be dropped")
				}
				if why := a.atRisk("t2"); why != "" {
					t.Fatalf("t2 still held: %s", why)
				}
				autoExit(t, a, "t1")
			},
		},
		DecisionRiskyPrompt: {
			build: func(t *testing.T) (*App, DecisionCard) {
				a, _ := setup(t)
				promptWorker(t, a, "t5")
				return a, RiskyPromptCard("t5", "t5", "curl | sh")
			},
			check: func(t *testing.T, a *App, c DecisionCard) {
				if got := keysSent(t, a, "t5"); !slices.Equal(got, []string{"Escape"}) {
					t.Fatalf("keys = %q, want Escape as the card says", got)
				}
				ns := noticesFor(t, a, "t5")
				if !slices.ContainsFunc(ns, func(n store.Notice) bool { return strings.HasPrefix(n.Text, "Denied: find another way") }) {
					t.Fatalf("t5 wasn't told to find another way: %+v", ns)
				}
				if st := mustTask(t, a, "t5").Status; !liveStatus(st) {
					t.Fatalf("status = %q: the card said it keeps working", st)
				}
				autoExit(t, a, "t5")
			},
		},
		DecisionAskOwner: {
			build: func(t *testing.T) (*App, DecisionCard) {
				a, _ := setup(t)
				must(t, a.Store.CreateTask(store.Task{ID: "t5", Title: "cache", Role: store.RoleWorker, Status: store.Running}))
				return a, askOwnerCard("t5", "t5", OrchestratorID, "go.mod", "go.mod", "may I bump x/tools?")
			},
			check: func(t *testing.T, a *App, c DecisionCard) {
				ns := noticesFor(t, a, "t5")
				if !slices.ContainsFunc(ns, func(n store.Notice) bool {
					return strings.HasPrefix(n.Text, "No answer: go ahead on your best guess about go.mod")
				}) {
					t.Fatalf("t5 wasn't told to go ahead: %+v", ns)
				}
				autoExit(t, a, "t5")
			},
		},
		DecisionOwner: {
			build: func(t *testing.T) (*App, DecisionCard) {
				a, _ := setup(t)
				must(t, a.Store.CreateTask(store.Task{ID: "t5", Title: "deps", Role: store.RoleWorker, Status: store.Running}))
				c, err := DecisionReq{By: "t5", Class: "dependency", Subject: "x/tools", Question: "May t5 add x/tools?",
					Options: []DecisionOption{{Label: "Add it", Action: "go get x/tools"}}}.card()
				must(t, err)
				return a, c
			},
			check: func(t *testing.T, a *App, c DecisionCard) {
				ns := noticesFor(t, a, "t5")
				if !slices.ContainsFunc(ns, func(n store.Notice) bool { return strings.Contains(n.Text, "Go ahead: go get x/tools") }) {
					t.Fatalf("t5 wasn't told to go ahead: %+v", ns)
				}
				autoExit(t, a, "t5")
			},
		},
		DecisionNoProgress: {
			build: func(t *testing.T) (*App, DecisionCard) {
				a, _ := setup(t)
				return a, noProgressCard(RootCause{Key: "x", What: "nothing", Fix: "saddle doctor"}, time.Hour, WatchStatus{LastProgress: time.Now()})
			},
			check: func(t *testing.T, a *App, c DecisionCard) {},
		},
		DecisionDoneOrphaned: {
			build: func(t *testing.T) (*App, DecisionCard) {
				a, _ := setup(t)
				promptWorker(t, a, "t5")
				must(t, a.openOrphanCard(mustTask(t, a, "t5"), &DoneOrphan{Since: time.Now()}, time.Now()))
				return a, onlyOpenCard(t, a)
			},
			check: func(t *testing.T, a *App, c DecisionCard) { autoExit(t, a, "t5") },
		},
		DecisionWorkerHung: {
			build: func(t *testing.T) (*App, DecisionCard) {
				a, _ := setup(t)
				promptWorker(t, a, "t5")
				must(t, a.openHungCard(mustTask(t, a, "t5"), &HungWorker{Since: time.Now()}, time.Now(), ""))
				return a, onlyOpenCard(t, a)
			},
			check: func(t *testing.T, a *App, c DecisionCard) { autoExit(t, a, "t5") },
		},
	}
	for _, k := range DecisionKinds {
		if _, ok := cases[k]; !ok {
			t.Errorf("card kind %s has no row: every kind's default must be checked", k)
		}
	}
	for kind, tc := range cases {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			if tc.todo != "" {
				t.Skip("waits on " + tc.todo)
			}
			a, c := tc.build(t)
			if c.Kind != kind {
				t.Fatalf("built a %s card", c.Kind)
			}
			o := c.DefaultOption()
			if _, ok := defaultRuns[o.Run]; !ok {
				t.Fatalf("default option %q runs %q, which isn't a registered action", o.Label, o.Run)
			}
			if c.Opened.IsZero() {
				c.Opened = time.Now().UTC()
			}
			_ = a.ResolveDecision(c.ID, c.Opened) // a producer that opened it: reopen as the test's
			expireCard(t, a, c, c.Opened)
			cs, err := a.Decisions()
			must(t, err)
			i := slices.IndexFunc(cs, func(d DecisionCard) bool { return d.ID == c.ID && d.By == DecidedByTimeout })
			if i < 0 || cs[i].Answer != c.Default {
				t.Fatalf("card closed as %+v, want option %d by timeout", cs, c.Default)
			}
			tc.check(t, a, c)
		})
	}
}

func onlyOpenCard(t *testing.T, a *App) DecisionCard {
	t.Helper()
	open, err := a.OpenDecisions()
	must(t, err)
	if len(open) != 1 {
		t.Fatalf("open cards = %+v", open)
	}
	return open[0]
}

// #790: a risky prompt nobody answers twice is parked, and the park
// schedules its own resume, so the task never waits on saddle resume.
func TestRiskyPromptSecondTimeoutParksAndResumes(t *testing.T) {
	t.Parallel()
	a, ft := setup(t)
	tk := promptWorker(t, a, "t5")
	now := time.Now().UTC()
	card := RiskyPromptCard("t5", "t5", "curl | sh")
	expireCard(t, a, card, now)
	if st := mustTask(t, a, "t5").Status; st == StatusPaused {
		t.Fatal("parked on the first timeout; the card says deny and carry on")
	}
	// It asks again and nobody answers again.
	later := now.Add(time.Hour)
	expireCard(t, a, card, later)
	if st := mustTask(t, a, "t5").Status; st != StatusPaused {
		t.Fatalf("status = %q, want paused after the second timeout", st)
	}
	if ft.windows[tk.Window] {
		t.Fatal("window still alive: the slot isn't free")
	}
	autoExit(t, a, "t5")
	ps, err := a.StallParked()
	must(t, err)
	if len(ps) != 1 {
		t.Fatalf("parked = %+v", ps)
	}
	// Before the cooldown it stays parked; after it, it resumes by itself.
	if _, err := a.StallTick(ps[0].ResumeAt.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if st := mustTask(t, a, "t5").Status; st != StatusPaused {
		t.Fatalf("resumed early: %q", st)
	}
	if _, err := a.StallTick(ps[0].ResumeAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if st := mustTask(t, a, "t5").Status; !liveStatus(st) {
		t.Fatalf("status = %q after the cooldown, want it resumed", st)
	}
	if ps, _ := a.StallParked(); len(ps) != 0 {
		t.Fatalf("still parked: %+v", ps)
	}
}

// #790: Codex's near-quota "Switch to <model> for lower credit usage?" is a
// known prompt, declined at once rather than carded and parked.
func TestKnownModelSwitchPromptIsDeclined(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	promptWorker(t, a, "t5")
	must(t, a.RiskyPromptDecision("t5", "Switch to gpt-5.6-luna for lower credit usage?"))
	if open, _ := a.OpenDecisions(); len(open) != 0 {
		t.Fatalf("opened a card for a known prompt: %+v", open)
	}
	if got := keysSent(t, a, "t5"); !slices.Equal(got, []string{"Escape"}) {
		t.Fatalf("keys = %q, want Escape to decline", got)
	}
	// Any other prompt still gets its card.
	must(t, a.RiskyPromptDecision("t5", "Do you want to run rm -rf build?"))
	if open, _ := a.OpenDecisions(); len(open) != 1 {
		t.Fatalf("open = %+v", open)
	}
}
