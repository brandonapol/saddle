package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/orch"
	"github.com/brandonapol/saddle/internal/store"
)

// deadProc is an orchestrator that has exited: every Send fails, and, as
// orch.Proc does since #792, a failed Send leaves it idle.
type deadProc struct{ stubProc }

func (p *deadProc) Send(string) error { return errors.New("write |1: file already closed") }

// #792: when the orchestrator exits, notices a delivery tried to send stay
// pending in the store, the process doesn't read as busy, and a new
// orchestrator starts without anyone pressing ctrl+r.
func TestOrchestratorExitRestartsAndKeepsNotices(t *testing.T) {
	m := slashModel(t)
	m.restartBase = time.Millisecond
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(m.app.Store.Notify(app.OrchestratorID, store.NoticeAction, "t3 needs you: which database?"))
	dead := &deadProc{}
	m.proc = dead
	cmd := m.handleEvent(orch.Event{Kind: orch.Exit, Text: "signal: killed"})
	m.deliver()
	if n, err := m.app.Store.PendingActionNotices(app.OrchestratorID); err != nil || n != 1 {
		t.Fatalf("pending action notices = %d (%v), want 1: a failed send lost them", n, err)
	}
	if dead.Busy() {
		t.Fatal("the dead orchestrator reads as busy")
	}
	var started []string
	fresh := &stubProc{}
	m.newProc = func(resume string) (orchProc, error) {
		started = append(started, resume)
		return fresh, nil
	}
	if cmd == nil {
		t.Fatal("no restart was scheduled")
	}
	m.Update(runMsg(t, cmd))
	if len(started) != 1 || m.proc != fresh {
		t.Fatalf("restarts = %q, proc = %T; want one restart without a key press", started, m.proc)
	}
	m.deliver()
	if len(fresh.sent) != 1 || !strings.Contains(fresh.sent[0], "which database?") {
		t.Fatalf("the new orchestrator got %q", fresh.sent)
	}
	if n, _ := m.app.Store.PendingActionNotices(app.OrchestratorID); n != 0 {
		t.Errorf("%d notices still pending after a successful send", n)
	}
}

// #792: restarts back off, so a crash loop doesn't spin.
func TestRestartBacksOff(t *testing.T) {
	m := &model{restartBase: time.Second}
	var last time.Duration
	for i := range 12 {
		d := m.restartDelay()
		if i > 0 && d < last {
			t.Fatalf("delay %d = %s, shorter than %s", i, d, last)
		}
		last = d
	}
	if last != restartMax {
		t.Fatalf("delay settles at %s, want the cap %s", last, restartMax)
	}
}

// #793: every wake prompt lists the open decision cards waiting on the
// owner, so one whose notice went out before a compaction isn't forgotten.
func TestWakePromptListsOpenCards(t *testing.T) {
	m := slashModel(t)
	m.app.Root = t.TempDir()
	c, err := m.app.FileDecision(app.DecisionReq{
		By: "t5", Task: "t5", Class: "release-tag", Subject: "v1.4.0", Question: "Push the release tag v1.4.0?", Irreversible: true,
		Options: []app.DecisionOption{{Label: "Push it", Action: "git push origin v1.4.0", Recommended: true}, {Label: "Wait", Action: "nothing"}},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.app.Store.TakeNotices(app.OrchestratorID, false); err != nil {
		t.Fatal(err)
	}
	p := &stubProc{}
	m.proc = p
	m.pending = []string{"t3 stopped without calling done."}
	m.deliver()
	if len(p.sent) != 1 || !strings.Contains(p.sent[0], c.ID) {
		t.Fatalf("wake prompt lacks card %s: %q", c.ID, p.sent)
	}
}
