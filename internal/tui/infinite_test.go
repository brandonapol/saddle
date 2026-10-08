package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/autopilot"
	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/usage"
)

// fakeInfinite records what the infinite-mode key asked for.
type fakeInfinite struct {
	calls []bool
	st    autopilot.State
}

func (f *fakeInfinite) SetInfinite(on bool) (autopilot.State, error) {
	f.calls = append(f.calls, on)
	f.st.On, f.st.Infinite = on, on
	return f.st, nil
}

// #285: alt+i flips infinite mode from any view and focus, turns auto-merge
// on with it so the pipeline merges too, and the header shows "∞ on".
func TestInfiniteKeyTogglesAutopilot(t *testing.T) {
	m := newViewModel(140, 40)
	f := &fakeInfinite{}
	am := &fakeAutomerger{}
	m.aper, m.amer = f, am
	m.am = &automerge.Status{}
	m.ap = &autopilot.State{}

	if strings.Contains(m.viewHeader(), "∞") {
		t.Fatalf("header shows infinite mode while off:\n%s", m.viewHeader())
	}
	runKey(m, altKey('i')) // chat focused: alt+i must not type an "i"
	if !slices.Equal(f.calls, []bool{true}) {
		t.Fatalf("calls = %v, want on", f.calls)
	}
	if strings.Contains(m.input.Value(), "i") {
		t.Errorf("alt+i typed into the chat: %q", m.input.Value())
	}
	if !slices.Equal(am.calls, []string{"on"}) {
		t.Errorf("auto-merge calls = %q, want on", am.calls)
	}
	if !strings.Contains(m.flash, "infinite mode on") {
		t.Errorf("flash = %q", m.flash)
	}
	if h := m.viewHeader(); !strings.Contains(h, "∞ on") {
		t.Fatalf("header lacks the indicator:\n%s", h)
	}

	// From the merge view, it turns off; auto-merge is left as it is.
	m.Update(altKey('3'))
	m.am.Enabled = true
	runKey(m, altKey('i'))
	if !slices.Equal(f.calls, []bool{true, false}) {
		t.Fatalf("calls = %v, want on then off", f.calls)
	}
	if len(am.calls) != 1 {
		t.Errorf("turning infinite off touched auto-merge: %q", am.calls)
	}
	if h := m.viewHeader(); strings.Contains(h, "∞") {
		t.Errorf("header still shows infinite mode:\n%s", h)
	}
}

// The indicator carries the weekly budget left, and says when the run is
// parked at the limit; narrow headers keep at least "∞".
func TestInfiniteHeaderShowsBudgetAndParking(t *testing.T) {
	m := newViewModel(160, 40)
	m.ap = &autopilot.State{On: true, Infinite: true}
	m.limits = &usage.LimitEstimate{Weekly: usage.WindowEstimate{Name: "weekly", Percent: 0.38, Cap: usage.Cap{Tokens: 100}}}
	if h := m.viewHeader(); !strings.Contains(h, "∞ on") || !strings.Contains(h, "62% wk left") {
		t.Fatalf("header lacks the weekly budget:\n%s", h)
	}
	reset := time.Date(2026, 10, 9, 17, 20, 0, 0, time.Local)
	m.ap.SleepUntil, m.ap.Parked = reset, []string{"t1", "t2"}
	if h := m.viewHeader(); !strings.Contains(h, "∞ parked") || !strings.Contains(h, "17:20") {
		t.Fatalf("header lacks the parking:\n%s", h)
	}
	for _, w := range []int{40, 60, 80} {
		m.Update(tea.WindowSizeMsg{Width: w, Height: 30})
		h := m.viewHeader()
		if !strings.Contains(h, "∞") || lipgloss.Width(h) > w {
			t.Errorf("width %d: header %q (width %d)", w, h, lipgloss.Width(h))
		}
	}
}

// SADDLE_KEY_INFINITE rebinds the toggle; the help overlay lists it.
func TestInfiniteKeyRebinds(t *testing.T) {
	t.Setenv("SADDLE_KEY_INFINITE", "alt+0, ctrl+g")
	km := newKeyMap()
	if !slices.Equal(km.Infinite.Keys(), []string{"alt+0", "ctrl+g"}) || km.Infinite.Help().Key != "alt+0" {
		t.Fatalf("rebound keys = %q, help %q", km.Infinite.Keys(), km.Infinite.Help().Key)
	}
	t.Setenv("SADDLE_KEY_INFINITE", "")
	if km := newKeyMap(); !key.Matches(altKey('i'), km.Infinite) {
		t.Errorf("default key = %q, want alt+i", km.Infinite.Keys())
	}
}
