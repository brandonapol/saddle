package tui

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/orch"
)

type fakeOrch struct {
	busy bool
	sent []string
}

func (f *fakeOrch) Busy() bool { return f.busy }
func (f *fakeOrch) Send(s string) error {
	f.sent = append(f.sent, s)
	return nil
}

// #179: while the orchestrator process runs, the compact watcher sees its
// busy state, whether the owner is typing, and can send to it; once it
// exits the target is gone.
func TestCompactTargetFollowsOrchestrator(t *testing.T) {
	m := newViewModel(120, 40)
	p := &fakeOrch{}
	t.Cleanup(func() { m.app.SetCompactTarget(nil) })
	m.registerCompact(p)
	tg := m.app.CompactTarget()
	if tg == nil {
		t.Fatal("no compact target registered for the running orchestrator")
	}
	if tg.Busy() || tg.Drafting() {
		t.Fatal("idle orchestrator with an empty input reads busy or drafting")
	}
	p.busy = true
	if !tg.Busy() {
		t.Fatal("Busy doesn't follow the process")
	}
	m.Update(runeKey('h'))
	if !tg.Drafting() {
		t.Fatal("text in the input box must read as drafting")
	}
	if err := tg.Send("/compact"); err != nil || len(p.sent) != 1 {
		t.Fatalf("send: %v %q", err, p.sent)
	}
	m.Update(closedMsg{}) // the process's event stream ended
	if m.app.CompactTarget() != nil {
		t.Fatal("target still registered after the orchestrator exited")
	}
}

// #260: a turn that ends in an error result leaves the orchestrator idle:
// no "thinking…" under the chat, and events held during the turn go out
// as the next turn.
func TestFailedTurnGoesIdleAndDelivers(t *testing.T) {
	m := slashModel(t)
	out := filepath.Join(t.TempDir(), "stdin")
	p, err := orch.Start(exec.Command("sh", "-c",
		`read l; read l; echo '{"type":"result","subtype":"error","is_error":true,"result":"API down"}'; cat > `+out))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	m.proc = p
	m.sendUser("hello")
	m.pending = append(m.pending, "t4 stopped without calling done")
	msg := runMsg(t, m.waitEvent())
	ev, ok := msg.(evMsg)
	if !ok || ev.e.Kind != orch.Error {
		t.Fatalf("event = %+v, want an error", msg)
	}
	if p.Busy() {
		t.Fatal("the failed turn left the orchestrator busy")
	}
	m.renderChat()
	if v := m.vp.View(); strings.Contains(v, "thinking…") {
		t.Errorf("still thinking after the error:\n%s", v)
	}
	m.Update(ev)
	if !hasChat(m, "Orchestrator error: API down") {
		t.Errorf("error not shown: %+v", m.chat)
	}
	got := sentUser(t, out, 1)
	if len(got) != 1 || !strings.Contains(got[0], "t4 stopped without calling done") {
		t.Fatalf("held event not delivered after the error: %q", got)
	}
}
