package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/automerge"
)

// automerger is what the merge view's keys do: the App calls behind `saddle
// automerge on|off|hold|release` and `saddle stack rebase`. Some read GitHub
// or rebase, so they only ever run in a tea.Cmd.
type automerger interface {
	SetAutomerge(on bool) error
	Hold(ref string) error
	Release(ref string) error
	RebaseStack(ref string) (string, error)
}

// appAutomerger is the automerger the TUI runs with.
type appAutomerger struct{ a *app.App }

func (x appAutomerger) SetAutomerge(on bool) error { return x.a.NewAutomerge(nil).SetEnabled(on) }

func (x appAutomerger) Hold(ref string) error {
	_, err := x.a.AutomergeHold(ref)
	return err
}

func (x appAutomerger) Release(ref string) error {
	_, err := x.a.AutomergeRelease(ref)
	return err
}

func (x appAutomerger) RebaseStack(ref string) (string, error) {
	res, err := x.a.RebaseStack(ref)
	if err != nil {
		return "", err
	}
	if len(res.Moves) == 0 {
		return fmt.Sprintf("stack %s is already on %s", res.Stack, x.a.Cfg.Base), nil
	}
	return fmt.Sprintf("rebased stack %s: %d branches moved", res.Stack, len(res.Moves)), nil
}

func (m *model) automerger() automerger {
	if m.amer != nil {
		return m.amer
	}
	return appAutomerger{m.app}
}

// amDoneMsg is an auto-merge action landing: what to flash, and on success
// the change it made to the auto-merge state, applied before the next
// refresh reads it so a second key press acts on what the screen shows
// (#207).
type amDoneMsg struct {
	text  string
	apply func(*automerge.Status)
}

// released is st once stack id's own hold is lifted. A hold on one of its
// tasks or PRs still holds it.
func released(st *automerge.Status, id string) {
	st.Holds = slices.DeleteFunc(st.Holds, func(h string) bool { return h == id })
	for i, s := range st.Stacks {
		if s.ID == id && !slices.ContainsFunc(st.Holds, func(h string) bool { return names(s, h) }) {
			st.Stacks[i].Held = false
		}
	}
}

// applyAm applies an auto-merge action's change to a copy of m.am, and
// makes refreshes that read the state before it stale.
func (m *model) applyAm(apply func(*automerge.Status)) {
	if apply == nil || m.am == nil {
		return
	}
	st := *m.am
	st.Holds = slices.Clone(st.Holds)
	st.Stacks = slices.Clone(st.Stacks)
	apply(&st)
	m.am = &st
	m.amGen++
}

// stackHeld reports whether s is held: the last check said so, or a hold
// added since names it or one of its PRs.
func stackHeld(st *automerge.Status, s automerge.Stack) bool {
	return s.Held || slices.ContainsFunc(st.Holds, func(h string) bool { return names(s, h) })
}

// names reports whether ref is s, one of its tasks or one of its PRs.
func names(s automerge.Stack, ref string) bool {
	return ref == s.ID || slices.ContainsFunc(s.Nodes, func(n automerge.Node) bool { return ref == n.Task || ref == n.PR })
}

// heldCount counts the held stacks; a hold on a stack the last check didn't
// see counts as one.
func heldCount(st *automerge.Status) int {
	n := 0
	for _, s := range st.Stacks {
		if stackHeld(st, s) {
			n++
		}
	}
	for _, h := range st.Holds {
		if !slices.ContainsFunc(st.Stacks, func(s automerge.Stack) bool { return names(s, h) }) {
			n++
		}
	}
	return n
}

// amHeader is the header's auto-merge part, long and short; "" when the
// state is unknown.
func (m *model) amHeader() (long, short string, c lipgloss.Color) {
	st := m.am
	if st == nil {
		return "", "", cDim
	}
	switch {
	case st.Stopped != "":
		return "auto-merge stopped: " + st.Stopped, "merge stopped", cAlert
	case !st.Enabled:
		return "auto-merge off", "merge off", cDim
	}
	long, short, c = "auto-merge on", "merge on", cDone
	if n := heldCount(st); n > 0 {
		long += fmt.Sprintf(" · %d held", n)
		short += fmt.Sprintf(" · %d held", n)
		c = cAccent
	}
	return long, short, c
}

// selectedStack is the stack the merge view's keys act on.
func (m *model) selectedStack() (automerge.Stack, bool) {
	if m.am == nil || len(m.am.Stacks) == 0 {
		return automerge.Stack{}, false
	}
	i := slices.IndexFunc(m.am.Stacks, func(s automerge.Stack) bool { return s.ID == m.stackSel })
	return m.am.Stacks[max(i, 0)], true
}

func (m *model) moveStack(dir int) {
	if m.am == nil || len(m.am.Stacks) == 0 {
		return
	}
	i := slices.IndexFunc(m.am.Stacks, func(s automerge.Stack) bool { return s.ID == m.stackSel })
	i = min(max(max(i, 0)+dir, 0), len(m.am.Stacks)-1)
	m.stackSel = m.am.Stacks[i].ID
}

// mergeKey handles the merge view's keys; ok is false for any other key.
func (m *model) mergeKey(k tea.KeyMsg) (tea.Cmd, bool) {
	keys := m.keys
	if key.Matches(k, keys.Focus) {
		m.tr.focus = !m.tr.focus
		return nil, true
	}
	if m.tr.focus {
		if c, ok := m.trainKey(k); ok || !key.Matches(k, keys.AutoMerge) {
			return c, ok
		}
	}
	switch {
	case key.Matches(k, keys.Down):
		m.moveStack(1)
		return nil, true
	case key.Matches(k, keys.Up):
		m.moveStack(-1)
		return nil, true
	case key.Matches(k, keys.AutoMerge):
		if m.am == nil {
			return flashCmd("auto-merge state not read yet"), true
		}
		on := !m.am.Enabled || m.am.Stopped != ""
		word := "off"
		if on {
			word = "on"
		}
		return m.amRun("turning auto-merge "+word, func(x automerger) (string, error) {
			return "auto-merge " + word, x.SetAutomerge(on)
		}, func(st *automerge.Status) { st.Enabled, st.Stopped = on, "" }), true
	case key.Matches(k, keys.Hold):
		s, ok := m.selectedStack()
		if !ok {
			return flashCmd("no PR stack to hold"), true
		}
		if stackHeld(m.am, s) {
			return m.amRun("releasing "+s.ID, func(x automerger) (string, error) {
				return "released stack " + s.ID, x.Release(s.ID)
			}, func(st *automerge.Status) { released(st, s.ID) }), true
		}
		return m.amRun("holding "+s.ID, func(x automerger) (string, error) {
			return "held stack " + s.ID + "; it won't auto-merge until released", x.Hold(s.ID)
		}, func(st *automerge.Status) { st.Holds = append(st.Holds, s.ID) }), true
	case key.Matches(k, keys.Rebase):
		s, ok := m.selectedStack()
		if !ok {
			return flashCmd("no PR stack to rebase"), true
		}
		return m.amRun("rebasing "+s.ID, func(x automerger) (string, error) {
			return x.RebaseStack(s.ID)
		}, nil), true
	}
	return nil, false
}

func flashCmd(s string) tea.Cmd { return func() tea.Msg { return flashMsg(s) } }

// amRun runs one auto-merge action off the UI goroutine, one at a time; on
// success apply, if not nil, is its change to the auto-merge state.
func (m *model) amRun(doing string, do func(automerger) (string, error), apply func(*automerge.Status)) tea.Cmd {
	if m.amBusy != "" {
		return flashCmd("still " + m.amBusy)
	}
	m.amBusy = doing
	m.flash, m.flashAt = doing+"…", time.Now()
	x := m.automerger()
	return func() tea.Msg {
		out, err := do(x)
		if err != nil {
			return amDoneMsg{text: doing + ": " + err.Error()}
		}
		return amDoneMsg{text: out, apply: apply}
	}
}

// viewStacks renders the auto-merge state and the PR stacks for the merge
// view, w columns wide.
func (m *model) viewStacks(w int) []string {
	st := m.am
	if st == nil {
		return []string{sDim.Render("auto-merge: not read yet")}
	}
	wrap := lipgloss.NewStyle().Width(w)
	color := func(c lipgloss.Color, s string) string { return lipgloss.NewStyle().Foreground(c).Render(s) }

	state := color(cDim, "auto-merge off")
	if st.Enabled {
		state = color(cDone, "auto-merge on")
	}
	if st.Source != "" {
		state += sDim.Render(" (" + st.Source + ")")
	}
	if n := heldCount(st); n > 0 {
		state += color(cAccent, fmt.Sprintf(" · %d held", n))
	}
	rows := []string{wrap.Render(state)}
	if st.Stopped != "" {
		rows = append(rows, wrap.Render(color(cAlert, "stopped: "+st.Stopped)+sDim.Render(" · ")+sKey.Render(m.keys.AutoMerge.Help().Key)+sDim.Render(" resumes")))
	}
	if len(st.Stacks) == 0 {
		return append(rows, sDim.Render("no open PR stacks"))
	}
	sel, _ := m.selectedStack()
	base := m.app.Cfg.Base
	for _, s := range st.Stacks {
		mark := "  "
		if s.ID == sel.ID && !m.tr.focus {
			mark = sKey.Render("▸ ")
		}
		head := mark + sBright.Render("stack "+s.ID)
		if stackHeld(st, s) {
			head += sDim.Render(" · ") + color(cAccent, "held")
		}
		if s.Behind > 0 {
			head += sDim.Render(" · ") + color(cAlert, fmt.Sprintf("%d behind %s", s.Behind, base))
		}
		rows = append(rows, "", head)
		for i, n := range s.Nodes {
			line := fmt.Sprintf("   %d %s %s", i+1, n.Task, prNumber(n.PR))
			if n.PR == s.Next {
				line = sText.Render(line) + color(cAccent, " next")
			} else {
				line = sText.Render(line)
			}
			rows = append(rows, wrap.Render(line+sDim.Render(" → "+n.Base+"  "+nodeState(n))))
			if n.AtRisk != "" {
				rows = append(rows, wrap.Render(color(cAlert, "     at risk: "+n.AtRisk)))
			}
		}
		if s.Ready {
			rows = append(rows, color(cDone, "   ready to merge"))
		} else if s.Why != "" {
			rows = append(rows, wrap.Render(sDim.Render("   waiting on: ")+sText.Render(s.Why)))
		}
	}
	return rows
}

// nodeState is a PR's state as the last check read it.
func nodeState(n automerge.Node) string {
	if n.Error != "" {
		return "can't read: " + n.Error
	}
	var parts []string
	if n.Checks != "" {
		parts = append(parts, "checks "+n.Checks)
	}
	if n.Mergeable != "" || n.MergeState != "" {
		parts = append(parts, strings.ToLower(n.Mergeable)+"/"+strings.ToLower(n.MergeState))
	}
	if n.Draft {
		parts = append(parts, "draft")
	}
	if len(n.Labels) > 0 {
		parts = append(parts, "labels "+strings.Join(n.Labels, ","))
	}
	return strings.Join(parts, ", ")
}

// prNumber shortens a PR URL to #N.
func prNumber(url string) string {
	if i := strings.LastIndex(url, "/pull/"); i >= 0 {
		return "#" + url[i+len("/pull/"):]
	}
	return url
}
