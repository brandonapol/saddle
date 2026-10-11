package app

import (
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// #404 acceptance: each needs-you producer emits a card with every field
// filled, including a default and a timeout.
func TestDecisionProducersFillEveryField(t *testing.T) {
	t.Parallel()
	red := CIRedLayer{Task: "t150", PR: "https://github.com/o/r/pull/366", Head: "493fa9f4624a00", Checks: []string{"GitGuardian Security Checks"}, Held: []string{"t151"}}
	cards := map[string]DecisionCard{
		"stuck ci-red":   stuckCard(StuckAlarm{Stack: "t149", Layer: "t150", Reason: StuckCIRed, Detail: "CI is red on its PR", For: 31 * time.Minute}, `t150 ("Remote control")`, []string{"t151"}),
		"stuck at-risk":  stuckCard(StuckAlarm{Stack: "t1", Layer: "t2", Reason: StuckAtRisk, Detail: "conflict in a.go", For: time.Hour}, "t2", nil),
		"ci repair cap":  ciRepairCapCard(`t150 ("Remote control")`, red, 2),
		"ask_owner":      askOwnerCard("t5", `t5 ("cache")`, OrchestratorID, "go.mod", "serial go.mod", "may I bump\nx/tools?"),
		"risky prompt":   RiskyPromptCard("t7", `t7 ("cleanup")`, "Do you want to run rm -rf build?"),
		"risky no words": RiskyPromptCard("t7", "t7", ""),
	}
	for name, c := range cards {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if c.Timeout <= 0 || c.Default == 0 || c.To == "" || c.Task == "" {
			t.Errorf("%s: missing default/timeout/to/task: %+v", name, c)
		}
		if strings.Contains(c.DefaultOption().Action, "kill") || strings.Contains(c.DefaultOption().Action, "sentinel ack") {
			t.Errorf("%s: default %q is not a safe route-around", name, c.DefaultOption().Action)
		}
		if out := c.Render(); !strings.Contains(out, "If you ignore it:") || !strings.Contains(out, "(recommended") {
			t.Errorf("%s: render lacks the default or a recommendation:\n%s", name, out)
		}
	}
	if b := cards["ci repair cap"].Blocking; !strings.Contains(b, "t151") {
		t.Errorf("repair-cap blocking = %q, want the held layer", b)
	}
	if h := cards["ci repair cap"].Headline; !strings.Contains(h, "Remote control") || !strings.Contains(h, "after 2 repair attempts") {
		t.Errorf("repair-cap headline = %q", h)
	}
}

// The stuck-stack alarm sends a decision card, opened once per stack and
// reason, that names the layers it holds.
func TestCheckStuckOpensDecisionCard(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.Train.Output = "single"
	_, _ = originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	landTask(t, a, "t3", "three", map[string]string{"three.txt": "three\n"})
	_, _ = a.Store.TakeNotices(OrchestratorID, false)
	now := time.Now().UTC()
	must(t, a.SetCIRed(CIRedState{Red: []CIRedLayer{{Task: "t2", PR: "https://github.com/o/r/pull/2", Head: "abc", Checks: []string{"CI / test"}, Since: now.Add(-time.Hour)}}}))
	al, err := a.CheckStuck(now)
	must(t, err)
	if len(al) != 1 {
		t.Fatalf("alarms = %+v", al)
	}
	open, err := a.OpenDecisions()
	must(t, err)
	if len(open) != 1 || open[0].Kind != DecisionStuckStack || open[0].Task != "t2" || !strings.Contains(open[0].Blocking, "t3") {
		t.Fatalf("open cards = %+v", open)
	}
	ns := stuckNotices(t, a)
	if len(ns) != 1 || !strings.HasPrefix(ns[0].Text, "‼ Stuck stack:") || !strings.Contains(ns[0].Text, `t2 ("two")`) {
		t.Fatalf("notices = %+v", ns)
	}
}

// ask_owner sends its question as a decision card to the owner.
func TestAskOwnerSendsDecisionCard(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	a.Cfg.Serial = []string{"go.mod"}
	must(t, a.Store.CreateTask(store.Task{ID: "t5", Title: "cache layer", Role: store.RoleWorker, Status: store.Running}))
	owner, err := a.AskOwner("t5", "go.mod", "May I bump golang.org/x/tools?")
	must(t, err)
	if owner != OrchestratorID {
		t.Fatalf("owner = %q", owner)
	}
	ns, err := a.Store.TakeNotices(OrchestratorID, false)
	must(t, err)
	if len(ns) != 1 || !strings.Contains(ns[0].Text, `‼ Question from t5 ("cache layer") about go.mod`) ||
		!strings.Contains(ns[0].Text, "What:     May I bump golang.org/x/tools?") || !strings.Contains(ns[0].Text, "saddle message t5") {
		t.Fatalf("notices = %+v", ns)
	}
	if open, _ := a.OpenDecisions(); len(open) != 1 || open[0].Kind != DecisionAskOwner {
		t.Fatalf("open = %+v", open)
	}
}
