package tui

import (
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/orch"
	"github.com/brandonapol/saddle/internal/store"
)

// procLog records the stand-in orchestrators a test started.
type procLog struct {
	resumes []string
	pids    []int
}

// fakeStarts makes m start stand-in orchestrators: script is the shell each
// runs, chosen by the resume id it was given.
func fakeStarts(t *testing.T, m *model, script func(resume string) string) *procLog {
	t.Helper()
	log := &procLog{}
	m.newProc = func(resume string) (orchProc, error) {
		cmd := exec.Command("sh", "-c", script(resume))
		p, err := orch.Start(cmd)
		if err != nil {
			return nil, err
		}
		t.Cleanup(p.Close)
		log.resumes = append(log.resumes, resume)
		log.pids = append(log.pids, cmd.Process.Pid)
		return p, nil
	}
	return log
}

// runMsg runs a blocking tea.Cmd, failing if it doesn't return in time.
func runMsg(t *testing.T, c tea.Cmd) tea.Msg {
	t.Helper()
	ch := make(chan tea.Msg, 1)
	go func() { ch <- c() }()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("command didn't return")
		return nil
	}
}

// gone waits for pid to be reaped.
func gone(pid int) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
	}
	return false
}

func hasChat(m *model, sub string) bool {
	for _, c := range m.chat {
		if strings.Contains(c.text, sub) {
			return true
		}
	}
	return false
}

func ctrlR() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyCtrlR} }

// #259: ctrl+r closes the running orchestrator and resumes the saved
// session. The old process's Exit must not read as a failed resume: no
// "Couldn't resume", no third process, the old one reaped.
func TestRestartIgnoresTheOldProcessExit(t *testing.T) {
	m := slashModel(t)
	m.savedSession = func() string { return "s1" }
	log := fakeStarts(t, m, func(string) string { return "cat >/dev/null" })
	if err := m.startProc("s1"); err != nil {
		t.Fatal(err)
	}
	old := m.proc
	m.focus = focusChat
	next, _ := m.key(ctrlR())
	if next == nil {
		t.Fatal("ctrl+r returned no wait on the new process")
	}
	// The old process's Exit can still reach Update: its wait was already
	// running when ctrl+r replaced it.
	m.Update(evMsg{e: orch.Event{Kind: orch.Exit}, p: old})
	m.Update(closedMsg{p: old})
	if hasChat(m, "Couldn't resume") {
		t.Errorf("the old process's exit read as a failed resume: %+v", m.chat)
	}
	if len(log.resumes) != 2 || log.resumes[1] != "s1" {
		t.Fatalf("processes started with resume ids %q, want [s1 s1]", log.resumes)
	}
	if !gone(log.pids[0]) {
		t.Error("the replaced orchestrator was never reaped")
	}
	if !m.resumed {
		t.Error("the restarted process should still count as resumed")
	}
}

// #259: a stale process's closed stream mustn't unregister the live one.
func TestStaleClosedMsgIsIgnored(t *testing.T) {
	m := slashModel(t)
	fakeStarts(t, m, func(string) string { return "cat >/dev/null" })
	if err := m.startProc(""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.app.SetCompactTarget(nil) })
	stale := m.proc
	if err := m.startProc(""); err != nil {
		t.Fatal(err)
	}
	m.Update(closedMsg{p: stale})
	if m.app.CompactTarget() == nil {
		t.Fatal("a closed stale process unregistered the live orchestrator")
	}
}

// #259: a resume that really fails starts one fresh session and doesn't
// leave the failed process behind.
func TestFailedResumeStartsOneFreshSession(t *testing.T) {
	m := slashModel(t)
	log := fakeStarts(t, m, func(resume string) string {
		if resume != "" {
			return "echo 'No conversation found' >&2; exit 1"
		}
		return "cat >/dev/null"
	})
	if err := m.startProc("gone"); err != nil {
		t.Fatal(err)
	}
	c := m.waitEvent()
	for range 10 {
		msg := runMsg(t, c)
		_, next := m.Update(msg)
		if hasChat(m, "Couldn't resume") || next == nil {
			break
		}
		c = next
	}
	if !hasChat(m, "Couldn't resume") {
		t.Fatalf("a failed resume wasn't reported: %+v", m.chat)
	}
	if len(log.resumes) != 2 || log.resumes[1] != "" {
		t.Fatalf("processes started with resume ids %q, want [gone \"\"]", log.resumes)
	}
	if !gone(log.pids[0]) {
		t.Error("the failed process was never reaped")
	}
}

// #261: /compact and /clear say what they did; /clear hides the old
// transcript, now and after a restart.
func TestCompactAndClearShowInChat(t *testing.T) {
	m := slashModel(t)
	m.addChat(store.ChatUser, "old question")
	m.addChat(store.ChatAssistant, "old answer")
	m.handleEvent(orch.Event{Kind: orch.Compacted, Text: "manual"})
	if !hasChat(m, "Conversation compacted") {
		t.Fatalf("no compact note: %+v", m.chat)
	}
	m.cost = 1.5
	m.handleEvent(orch.Event{Kind: orch.Cleared})
	if hasChat(m, "old answer") {
		t.Errorf("the old transcript is still shown after /clear: %+v", m.chat)
	}
	if !hasChat(m, "Conversation cleared") {
		t.Fatalf("no clear note: %+v", m.chat)
	}
	hist, err := m.app.Store.Chat(300)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range visibleHistory(hist) {
		if strings.Contains(c.Text, "old answer") {
			t.Errorf("a restart would show the cleared transcript again: %+v", c)
		}
	}
}

// #262: retries show in place of the bare "thinking…", and go once the
// model answers.
func TestRetryReplacesThinking(t *testing.T) {
	m := slashModel(t)
	fakeProc(t, m)
	m.sendUser("hello")
	m.handleEvent(orch.Event{Kind: orch.Retry, Text: "API error (unknown); retrying, attempt 3/10, next in 2s"})
	m.renderChat()
	if v := m.vp.View(); !strings.Contains(v, "attempt 3/10") {
		t.Fatalf("retry not shown:\n%s", v)
	}
	m.handleEvent(orch.Event{Kind: orch.Delta, Text: "hi"})
	m.handleEvent(orch.Event{Kind: orch.Result})
	m.renderChat()
	if v := m.vp.View(); strings.Contains(v, "attempt 3/10") {
		t.Fatalf("retry still shown after the turn:\n%s", v)
	}
}

// #264: x and L ask before killing an agent or running the train; any key
// but y keeps things as they are.
func TestKillAndLandAskFirst(t *testing.T) {
	m := newViewModel(120, 40)
	var killed []string
	landed := 0
	m.killer = func(id string) error { killed = append(killed, id); return nil }
	m.lander = func() ([]app.LandResult, error) { landed++; return nil, nil }
	m.focus = focusTasks
	m.sel = 0

	if c, _ := m.key(runeKey('x')); c != nil {
		runMsg(t, c)
	}
	if len(killed) != 0 {
		t.Fatal("x killed without asking")
	}
	if f := m.viewFooter(); !strings.Contains(f, "Kill t1") {
		t.Fatalf("footer doesn't ask: %q", f)
	}
	m.key(runeKey('n'))
	if len(killed) != 0 || m.confirm != nil {
		t.Fatalf("n should keep the agent: killed=%v", killed)
	}
	m.key(runeKey('x'))
	c, _ := m.key(runeKey('y'))
	if c == nil {
		t.Fatal("y returned no kill")
	}
	if msg := runMsg(t, c); msg != flashMsg("killed t1") || len(killed) != 1 || killed[0] != "t1" {
		t.Fatalf("y: msg %v, killed %v", msg, killed)
	}

	if c, _ := m.key(runeKey('L')); c != nil {
		runMsg(t, c)
	}
	if landed != 0 {
		t.Fatal("L landed without asking")
	}
	if f := m.viewFooter(); !strings.Contains(f, "Land") {
		t.Fatalf("footer doesn't ask: %q", f)
	}
	c, _ = m.key(runeKey('y'))
	runMsg(t, c)
	if landed != 1 {
		t.Fatalf("landed %d times, want 1", landed)
	}
}

// #263: grok's model shows in the header and pane title even when config
// leaves it unset, and an error with no text still says something.
func TestGrokModelAndBlankError(t *testing.T) {
	m := slashModel(t)
	m.launch = agent.Launch{Kind: agent.KindGrok}
	m.tasks = nil // leave the header room for the orchestrator segment
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 40})
	if !strings.Contains(m.chatTitle(), "grok") {
		t.Errorf("title before init = %q, want it to name grok", m.chatTitle())
	}
	m.handleEvent(orch.Event{Kind: orch.Init, Model: "grok-4-fast"})
	if !strings.Contains(m.chatTitle(), "grok-4-fast") {
		t.Errorf("title = %q", m.chatTitle())
	}
	if h := m.viewHeader(); !strings.Contains(h, "grok-4-fast") {
		t.Errorf("header = %q", h)
	}
	m.handleEvent(orch.Event{Kind: orch.Init, Model: "unknown"})
	if strings.Contains(m.chatTitle(), "unknown") {
		t.Errorf("title = %q", m.chatTitle())
	}
	m.handleEvent(orch.Event{Kind: orch.Error})
	last := m.chat[len(m.chat)-1]
	if strings.TrimSpace(strings.TrimPrefix(last.text, "Orchestrator error:")) == "" {
		t.Fatalf("blank error line: %q", last.text)
	}
}
