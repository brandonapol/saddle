package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brandonapol/saddle/internal/app"
)

// confirmRig is an agent list whose kill and land are counted, not run.
type confirmRig struct {
	m         *model
	killed    []string
	landed    int
	landedMsg string
}

func newConfirmRig() *confirmRig {
	r := &confirmRig{landedMsg: "land: t2 landed"}
	r.m = newViewModel(120, 40)
	r.m.focus = focusTasks
	r.m.sel = 2 // t3, the row a stray key would destroy
	r.m.killer = func(id string) error {
		r.killed = append(r.killed, id)
		return nil
	}
	r.m.lander = func() ([]app.LandResult, error) {
		r.landed++
		return []app.LandResult{{Task: "t2", State: "landed"}}, nil
	}
	return r
}

func (r *confirmRig) press(t *testing.T, k tea.KeyMsg) tea.Msg {
	t.Helper()
	c, handled := r.m.key(k)
	if !handled {
		t.Fatalf("key %+v was not handled", k)
	}
	if c == nil {
		return nil
	}
	return runMsg(t, c)
}

// #264: x in the agent list asks before it kills. The key alone does not
// kill; y kills the selected agent; any other key cancels and leaves the
// list where it was, including a second x or L.
func TestAgentListKillAsksFirst(t *testing.T) {
	r := newConfirmRig()
	if msg := r.press(t, runeKey('x')); msg != nil || len(r.killed) != 0 || r.m.confirm == nil {
		t.Fatalf("x alone: msg %v, killed %v, confirm %v", msg, r.killed, r.m.confirm != nil)
	}
	foot := flat(r.m.viewFooter())
	if !strings.Contains(foot, "Kill t3?") || !strings.Contains(foot, "y kills it") {
		t.Fatalf("footer doesn't ask to kill t3: %q", foot)
	}
	if r.m.sel != 2 || r.m.focus != focusTasks {
		t.Fatalf("asking moved the list: sel %d focus %d", r.m.sel, r.m.focus)
	}

	// Decline, then ask again so the next key is the one under test.
	if msg := r.press(t, runeKey('n')); msg != flashMsg("kept t3") || r.m.confirm != nil || len(r.killed) != 0 {
		t.Fatalf("n: msg %v, confirm %v, killed %v", msg, r.m.confirm != nil, r.killed)
	}

	cancels := []tea.KeyMsg{
		runeKey('n'),
		runeKey('Y'), // only lowercase y confirms
		runeKey('j'),
		runeKey('x'),
		runeKey('L'),
		{Type: tea.KeyEnter},
		{Type: tea.KeyEsc},
		{Type: tea.KeyTab},
	}
	for _, k := range cancels {
		r.press(t, runeKey('x'))
		if len(r.killed) != 0 {
			t.Fatalf("after x, before %v: killed %v", k, r.killed)
		}
		msg := r.press(t, k)
		if msg != flashMsg("kept t3") || r.m.confirm != nil || len(r.killed) != 0 || r.landed != 0 {
			t.Fatalf("%v should cancel: msg %v, confirm %v, killed %v, landed %d", k, msg, r.m.confirm != nil, r.killed, r.landed)
		}
		if r.m.sel != 2 || r.m.focus != focusTasks {
			t.Fatalf("%v moved the list: sel %d focus %d", k, r.m.sel, r.m.focus)
		}
	}

	r.press(t, runeKey('x'))
	msg := r.press(t, runeKey('y'))
	if msg != flashMsg("killed t3") || len(r.killed) != 1 || r.killed[0] != "t3" {
		t.Fatalf("y: msg %v, killed %v", msg, r.killed)
	}
	if r.m.confirm != nil {
		t.Fatal("confirm still set after y")
	}
}

// #264: L asks before it runs the merge train. Same rule as kill: y runs
// it, and any other key, including x, cancels.
func TestAgentListLandAsksFirst(t *testing.T) {
	r := newConfirmRig()
	if msg := r.press(t, runeKey('L')); msg != nil || r.landed != 0 || r.m.confirm == nil {
		t.Fatalf("L alone: msg %v, landed %d, confirm %v", msg, r.landed, r.m.confirm != nil)
	}
	foot := flat(r.m.viewFooter())
	if !strings.Contains(foot, "Land the queued branches now?") || !strings.Contains(foot, "any other key cancels") {
		t.Fatalf("footer doesn't ask to land: %q", foot)
	}

	cancels := []tea.KeyMsg{
		runeKey('n'),
		runeKey('Y'),
		runeKey('x'),
		runeKey('j'),
		{Type: tea.KeyEsc},
		{Type: tea.KeyEnter},
	}
	for _, k := range cancels {
		if r.m.confirm == nil {
			r.press(t, runeKey('L'))
		}
		msg := r.press(t, k)
		if msg != flashMsg("land cancelled") || r.m.confirm != nil || r.landed != 0 || len(r.killed) != 0 {
			t.Fatalf("%v should cancel land: msg %v, confirm %v, landed %d, killed %v", k, msg, r.m.confirm != nil, r.landed, r.killed)
		}
	}

	r.press(t, runeKey('L'))
	msg := r.press(t, runeKey('y'))
	if msg != flashMsg(r.landedMsg) || r.landed != 1 {
		t.Fatalf("y: msg %v, landed %d", msg, r.landed)
	}
}

// A kill or land that fails tells the footer and does not claim success.
func TestAgentListConfirmReportsErrors(t *testing.T) {
	r := newConfirmRig()
	r.m.killer = func(string) error { return errors.New("tmux gone") }
	r.press(t, runeKey('x'))
	if msg := r.press(t, runeKey('y')); msg != flashMsg("tmux gone") {
		t.Fatalf("kill error: %v", msg)
	}

	r.m.lander = func() ([]app.LandResult, error) { return nil, errors.New("no test cmd") }
	r.press(t, runeKey('L'))
	if msg := r.press(t, runeKey('y')); msg != flashMsg("land: no test cmd") || r.landed != 0 {
		t.Fatalf("land error: msg %v, landed %d", msg, r.landed)
	}

	r.m.lander = func() ([]app.LandResult, error) { r.landed++; return nil, nil }
	r.press(t, runeKey('L'))
	if msg := r.press(t, runeKey('y')); msg != flashMsg("train is empty") || r.landed != 1 {
		t.Fatalf("empty train: msg %v, landed %d", msg, r.landed)
	}
}

// x with no row selected, and x typed in the chat, do not ask or kill.
func TestAgentListKillNeedsASelectedRow(t *testing.T) {
	r := newConfirmRig()
	r.m.sel = len(r.m.tasks)
	r.press(t, runeKey('x'))
	if r.m.confirm != nil || len(r.killed) != 0 {
		t.Fatalf("x with no row: confirm %v, killed %v", r.m.confirm != nil, r.killed)
	}

	r.m.sel = 0
	r.m.focus = focusChat
	if _, handled := r.m.key(runeKey('x')); handled || r.m.confirm != nil || len(r.killed) != 0 {
		t.Fatal("x in the chat should be typed, not a kill")
	}
}

// The agent-list help names both keys as asking first, so the footer and
// the ? overlay don't describe an immediate kill or land.
func TestAgentListKillAndLandHelpAsksFirst(t *testing.T) {
	m := newViewModel(200, 40)
	m.focus = focusTasks
	var kill, land bool
	for _, b := range m.help() {
		h := b.Help()
		switch h.Key {
		case "x":
			kill = strings.Contains(h.Desc, "asks first")
		case "L":
			land = strings.Contains(h.Desc, "asks first")
		}
	}
	if !kill || !land {
		t.Fatalf("help kill=%v land=%v", kill, land)
	}
	if s := m.viewKeys(400); !strings.Contains(s, "asks first") {
		t.Fatalf("wide footer lacks the confirm hint: %q", s)
	}
}
