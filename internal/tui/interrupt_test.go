package tui

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/orch"
	"github.com/brandonapol/saddle/internal/store"
)

// sentInterrupts counts the interrupt control requests the fake received.
func sentInterrupts(t *testing.T, path string, n int) int {
	t.Helper()
	got := 0
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		b, _ := os.ReadFile(path)
		got = strings.Count(string(b), `"subtype":"interrupt"`)
		if got >= n {
			break
		}
	}
	return got
}

// Esc in the chat interrupts a running turn; the chat says so and the
// orchestrator is back to idle.
func TestEscInterruptsBusyOrchestrator(t *testing.T) {
	m := slashModel(t)
	out := fakeProc(t, m)
	m.sendUser("do a long thing")
	m.key(escKey())
	if got := sentInterrupts(t, out, 1); got != 1 {
		t.Fatalf("interrupt requests sent = %d, want 1", got)
	}
	m.renderChat()
	if !strings.Contains(m.vp.View(), "interrupting") {
		t.Errorf("chat doesn't show the interrupt in progress:\n%s", m.vp.View())
	}
	m.handleEvent(orch.Event{Kind: orch.Delta, Text: "half an ans"})
	m.handleEvent(orch.Event{Kind: orch.Interrupted})
	last := m.chat[len(m.chat)-1]
	if last.role != store.ChatEvent || !strings.Contains(last.text, "Interrupted") {
		t.Fatalf("last chat line = %+v, want an Interrupted note", last)
	}
	if prev := m.chat[len(m.chat)-2]; !strings.Contains(prev.text, "half an ans") {
		t.Errorf("the partial reply was dropped: %+v", prev)
	}
}

// Esc while idle doesn't send anything; Esc while aimed at an agent still
// just un-aims.
func TestEscIdleOrAimedDoesNotInterrupt(t *testing.T) {
	m := slashModel(t)
	out := fakeProc(t, m)
	m.key(escKey())
	m.sendUser("busy now")
	m.target = "t1"
	m.key(escKey())
	if m.target != "" {
		t.Fatal("esc didn't un-aim")
	}
	time.Sleep(100 * time.Millisecond)
	if got := sentInterrupts(t, out, 0); got != 0 {
		t.Fatalf("interrupt requests sent = %d, want 0", got)
	}
}

// One ctrl+c while a turn runs interrupts it and arms quitting; a second
// still quits (#111).
func TestCtrlCInterruptsThenQuits(t *testing.T) {
	m := slashModel(t)
	out := fakeProc(t, m)
	m.sendUser("do a long thing")
	c, _ := m.key(ctrlC())
	if isQuit(c) {
		t.Fatal("the first ctrl+c quit")
	}
	if got := sentInterrupts(t, out, 1); got != 1 {
		t.Fatalf("interrupt requests sent = %d, want 1", got)
	}
	if f := m.viewFooter(); !strings.Contains(f, "Ctrl+C again to quit") {
		t.Errorf("footer = %q", f)
	}
	if c, _ := m.key(ctrlC()); !isQuit(c) {
		t.Fatal("the second ctrl+c didn't quit")
	}
}
