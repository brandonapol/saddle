package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/automerge"
)

// fakeAutomerger records what the merge view's keys asked for.
type fakeAutomerger struct {
	calls []string
	err   error
}

func (f *fakeAutomerger) SetAutomerge(on bool) error {
	if on {
		f.calls = append(f.calls, "on")
	} else {
		f.calls = append(f.calls, "off")
	}
	return f.err
}

func (f *fakeAutomerger) Hold(ref string) error {
	f.calls = append(f.calls, "hold "+ref)
	return f.err
}

func (f *fakeAutomerger) Release(ref string) error {
	f.calls = append(f.calls, "release "+ref)
	return f.err
}

func (f *fakeAutomerger) RebaseStack(ref string) (string, error) {
	f.calls = append(f.calls, "rebase "+ref)
	return "rebased " + ref, f.err
}

// twoStacks is auto-merge on with stack t5 held (by its top task, which the
// last check hasn't seen yet), behind main and waiting, and stack t8 ready.
func twoStacks() *automerge.Status {
	return &automerge.Status{
		Enabled: true,
		Source:  automerge.SourceRuntime,
		Holds:   []string{"t6"},
		Stacks: []automerge.Stack{
			{
				ID: "t5",
				Nodes: []automerge.Node{
					{Task: "t5", PR: "https://github.com/o/r/pull/12", Base: "main", Checks: "pending", Mergeable: "MERGEABLE", MergeState: "BLOCKED"},
					{Task: "t6", PR: "https://github.com/o/r/pull/13", Base: "saddle/t5", Draft: true, AtRisk: "from t6 up: tests failed"},
				},
				Behind: 3,
				Next:   "https://github.com/o/r/pull/12",
				Why:    "checks pending",
			},
			{
				ID:    "t8",
				Nodes: []automerge.Node{{Task: "t8", PR: "https://github.com/o/r/pull/20", Base: "main", Checks: "pass", Mergeable: "MERGEABLE", MergeState: "CLEAN"}},
				Next:  "https://github.com/o/r/pull/20",
				Ready: true,
			},
		},
	}
}

// The header says whether auto-merge is off, on, stopped and why, and how
// many stacks are held, in one line at any width.
func TestHeaderShowsAutomergeState(t *testing.T) {
	cases := []struct {
		name      string
		st        automerge.Status
		wide      []string
		narrowAny []string
	}{
		{"off", automerge.Status{}, []string{" off"}, nil}, // off trails, so it may be short
		{"on", automerge.Status{Enabled: true}, []string{"auto-merge on"}, []string{"auto-merge on", "merge on"}},
		{"stopped", automerge.Status{Enabled: true, Stopped: "merge of #12 failed"}, []string{"auto-merge stopped: merge of #12 failed"}, []string{"stopped"}},
		{"held", *twoStacks(), []string{"auto-merge on · 1 held"}, []string{"1 held"}},
	}
	for _, c := range cases {
		for _, w := range []int{36, 160} {
			m := newViewModel(w, 30)
			st := c.st
			m.am = &st
			h := m.viewHeader()
			if lipgloss.Height(h) != 1 || lipgloss.Width(h) > w {
				t.Errorf("%s at %d: header %q is %dx%d", c.name, w, h, lipgloss.Width(h), lipgloss.Height(h))
			}
			want := c.wide
			if w == 36 {
				want = nil
				if len(c.narrowAny) > 0 && !slices.ContainsFunc(c.narrowAny, func(s string) bool { return strings.Contains(h, s) }) {
					t.Errorf("%s at %d: header lacks any of %q: %q", c.name, w, c.narrowAny, h)
				}
			}
			for _, s := range want {
				if !strings.Contains(h, s) {
					t.Errorf("%s at %d: header lacks %q: %q", c.name, w, s, h)
				}
			}
		}
	}
	// Unknown state says nothing rather than guessing off.
	m := newViewModel(120, 30)
	if h := m.viewHeader(); strings.Contains(h, "auto-merge") {
		t.Errorf("no state read yet, but the header claims one: %q", h)
	}
}

// The merge view draws each stack bottom-up with each PR's state, the held
// and behind-main markers and what the next PR waits on.
func TestMergeViewShowsStacks(t *testing.T) {
	for _, w := range []int{36, 120} {
		m := newViewModel(w, 40)
		m.view = viewMerge
		m.am = twoStacks()
		out := m.View()
		checkScreen(t, "merge", out, w, 40)
		for _, want := range []string{"auto-merge on", "t5", "held", "3 behind main", "#12", "#13", "draft", "at risk", "waiting on: checks pending", "t8", "#20", "ready to merge", "MERGE TRAIN", "t2"} {
			if !strings.Contains(out, want) {
				t.Errorf("width %d: merge view lacks %q:\n%s", w, want, out)
			}
		}
		if w == 120 {
			for _, want := range []string{"checks pending", "mergeable/blocked", "→ main"} {
				if !strings.Contains(out, want) {
					t.Errorf("width %d: merge view lacks %q:\n%s", w, want, out)
				}
			}
		}
	}

	m := newViewModel(120, 30)
	m.view = viewMerge
	m.am = &automerge.Status{Stopped: "merge of #12 failed: not mergeable"}
	out := m.View()
	for _, want := range []string{"stopped: merge of #12 failed: not mergeable", "no open PR stacks"} {
		if !strings.Contains(out, want) {
			t.Errorf("stopped merge view lacks %q:\n%s", want, out)
		}
	}
}

// runKey presses k and runs the command it returns, feeding the result back.
func runKey(m *model, k tea.KeyMsg) {
	_, c := m.Update(k)
	if c == nil {
		return
	}
	if msg := c(); msg != nil {
		m.Update(msg)
	}
}

// In the merge view, M toggles auto-merge, h holds or releases the selected
// stack and r rebases it, through the same App calls as the CLI.
func TestMergeKeysDriveAutomerge(t *testing.T) {
	m := newViewModel(120, 40)
	f := &fakeAutomerger{}
	m.amer = f
	m.am = twoStacks()

	// Outside the merge view the keys do nothing to auto-merge.
	m.focus = focusTasks
	m.input.Blur()
	for _, r := range "Mhr" {
		runKey(m, runeKey(r))
	}
	if len(f.calls) > 0 {
		t.Fatalf("control view keys reached auto-merge: %q", f.calls)
	}

	m.Update(altKey('3'))
	runKey(m, runeKey('M')) // on → off
	runKey(m, runeKey('h')) // t5 is held by its top task: release
	runKey(m, runeKey('r')) // rebase t5
	runKey(m, runeKey('j')) // select t8
	runKey(m, runeKey('j')) // stays on the last stack
	runKey(m, runeKey('h')) // hold t8
	runKey(m, runeKey('k')) // back to t5
	runKey(m, runeKey('r')) // rebase t5
	want := []string{"off", "release t5", "rebase t5", "hold t8", "rebase t5"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
	if !strings.Contains(m.flash, "rebased t5") {
		t.Errorf("the result should be flashed, got %q", m.flash)
	}

	// Off or stopped, M turns it on.
	f.calls = nil
	m.am = &automerge.Status{Enabled: true, Stopped: "boom"}
	runKey(m, runeKey('M'))
	m.am = &automerge.Status{}
	runKey(m, runeKey('M'))
	if !slices.Equal(f.calls, []string{"on", "on"}) {
		t.Errorf("stopped then off: calls = %q, want on, on", f.calls)
	}

	// Errors are flashed; no stacks means nothing to hold.
	f.calls, f.err = nil, errors.New("gh: not logged in")
	m.am = twoStacks()
	runKey(m, runeKey('r'))
	if !strings.Contains(m.flash, "gh: not logged in") {
		t.Errorf("error not flashed: %q", m.flash)
	}
	f.calls = nil
	m.am = &automerge.Status{Enabled: true}
	runKey(m, runeKey('h'))
	runKey(m, runeKey('r'))
	if len(f.calls) > 0 {
		t.Errorf("no stacks, yet keys called %q", f.calls)
	}
}

// An action runs off the UI goroutine: the key returns a command and calls
// nothing until it runs, and a second press while one runs is refused.
func TestMergeActionsRunInCommands(t *testing.T) {
	m := newViewModel(120, 40)
	f := &fakeAutomerger{}
	m.amer = f
	m.am = twoStacks()
	m.view = viewMerge
	_, c := m.Update(runeKey('r'))
	if c == nil || len(f.calls) > 0 {
		t.Fatalf("r should return a command and call nothing yet; calls %q", f.calls)
	}
	if _, c2 := m.Update(runeKey('r')); c2 != nil {
		if msg := c2(); msg != nil {
			m.Update(msg)
		}
	}
	msg := c()
	if len(f.calls) != 1 {
		t.Errorf("a second press while busy ran again: %q", f.calls)
	}
	m.Update(msg)
	if m.amBusy != "" {
		t.Errorf("busy should clear when the action lands: %q", m.amBusy)
	}
}

// A refresh carries the auto-merge state into the model.
func TestRefreshCarriesAutomergeState(t *testing.T) {
	m := newViewModel(120, 40)
	m.prev = map[string]string{}
	m.Update(refreshMsg{tasks: m.tasks, am: twoStacks()})
	if m.am == nil || len(m.am.Stacks) != 2 {
		t.Fatalf("am = %+v", m.am)
	}
}

// M pressed again right after the first toggle lands, before a refresh
// reads the new state, flips what the screen shows (#207); so does h. A
// refresh read before the toggle and landing after it does not undo it.
func TestMergeKeysToggleAgainstFreshState(t *testing.T) {
	m := newViewModel(120, 40)
	f := &fakeAutomerger{}
	m.amer = f
	m.am = twoStacks()
	m.Update(altKey('3'))

	stale := refreshMsg{am: twoStacks()} // read while auto-merge was on
	runKey(m, runeKey('M'))              // on → off
	if long, _, _ := m.amHeader(); long != "auto-merge off" {
		t.Errorf("header after M: %q", long)
	}
	m.refreshing = false
	m.Update(stale)
	runKey(m, runeKey('M')) // off → on
	runKey(m, runeKey('j')) // t8, not held
	runKey(m, runeKey('h')) // hold t8
	runKey(m, runeKey('h')) // release t8
	want := []string{"off", "on", "hold t8", "release t8"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}

	// A failed toggle leaves the state as it was.
	f.calls, f.err = nil, errors.New("boom")
	runKey(m, runeKey('M'))
	f.err = nil
	runKey(m, runeKey('M'))
	if want := []string{"off", "off"}; !slices.Equal(f.calls, want) {
		t.Errorf("after a failure calls = %q, want %q", f.calls, want)
	}
}
