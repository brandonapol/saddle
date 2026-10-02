package tui

import (
	"os/exec"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/store"
)

// keyMap is every binding the TUI handles. Update matches against it and the
// footer renders its help, so the two can't drift apart.
type keyMap struct {
	Quit, Focus, PageUp, PageDown, Restart key.Binding
	NextAgent, PrevAgent                   key.Binding

	// Views and the help overlay.
	ViewControl, ViewPlan, ViewMerge, Help key.Binding

	// Terminal pane.
	Terminal, TermBack, TermScrollUp, TermScrollDown key.Binding

	// Chat.
	Send, Newline, Complete, Untarget key.Binding

	// Task list.
	Up, Down, Open, Skill, Spawn, Pause, Kill, Land, Back key.Binding
}

func newKeyMap() keyMap {
	b := func(help, desc string, keys ...string) key.Binding {
		return key.NewBinding(key.WithKeys(keys...), key.WithHelp(help, desc))
	}
	return keyMap{
		Quit:      b("ctrl+c", "quit (agents keep running)", "ctrl+c"),
		Focus:     b("tab", "switch pane", "tab", "shift+tab"),
		PageUp:    b("pgup", "scroll up", "pgup"),
		PageDown:  b("pgdn", "scroll down", "pgdown"),
		Restart:   b("ctrl+r", "restart orchestrator", "ctrl+r"),
		NextAgent: b("alt+n/p", "next/prev agent", "alt+n"),
		PrevAgent: b("alt+p", "prev agent", "alt+p"),

		ViewControl: b("alt+1/2/3", "views", "alt+1"),
		ViewPlan:    b("alt+2", "plan view", "alt+2"),
		ViewMerge:   b("alt+3", "merge view", "alt+3"),
		Help:        b("?", "keys", "?", "f1"),

		// Terminals send ctrl+` as NUL, which bubbletea names ctrl+@.
		Terminal:       b("ctrl+`", "terminal", "ctrl+@"),
		TermBack:       b("esc", "chat", "esc"),
		TermScrollUp:   b("ctrl+pgup/dn", "history", "ctrl+pgup"),
		TermScrollDown: b("ctrl+pgdn", "history", "ctrl+pgdown"),

		Send:     b("enter", "send", "enter"),
		Newline:  b("alt+enter", "newline", "alt+enter", "ctrl+j"),
		Complete: b("/skill tab", "complete", "tab"),
		Untarget: b("esc", "back to orchestrator", "esc"),

		Up:    b("k", "up", "k", "up"),
		Down:  b("j/k", "select", "j", "down"),
		Open:  b("enter", "open window", "enter", "a"),
		Skill: b("/", "skill in agent", "/"),
		Spawn: b("s", "spawn", "s"),
		Pause: b("p", "pause (esc)", "p"),
		Kill:  b("x", "kill", "x"),
		Land:  b("L", "land", "L"),
		Back:  b("esc/tab", "orchestrator", "esc"),
	}
}

// keyGroup is a titled section of the help overlay.
type keyGroup struct {
	title string
	keys  []key.Binding
}

// groups is every binding, once, for the help overlay. TestHelpGroupsCoverKeyMap
// fails if a binding is added to keyMap but not here.
func (k keyMap) groups() []keyGroup {
	return []keyGroup{
		{"Anywhere", []key.Binding{k.ViewControl, k.ViewPlan, k.ViewMerge, k.Help, k.NextAgent, k.PrevAgent, k.Focus, k.PageUp, k.PageDown, k.Restart, k.Quit}},
		{"Chat", []key.Binding{k.Send, k.Newline, k.Complete, k.Untarget}},
		{"Agents", []key.Binding{k.Down, k.Up, k.Open, k.Skill, k.Spawn, k.Pause, k.Kill, k.Land, k.Back}},
		{"Terminal", []key.Binding{k.Terminal, k.TermBack, k.TermScrollUp, k.TermScrollDown}},
	}
}

// help lists the bindings to show for the current focus, most useful first;
// the footer drops whatever doesn't fit from the end.
func (m *model) help() []key.Binding {
	k := m.keys
	if m.helpOpen {
		return []key.Binding{withHelp(k.Help, "esc/?", "close"), k.ViewControl, k.Quit}
	}
	if m.focus == focusTerm {
		return []key.Binding{withHelp(k.Terminal, "ctrl+`", "hide"), k.TermBack, k.TermScrollUp}
	}
	if m.view != viewControl {
		return []key.Binding{k.ViewControl, k.Help, k.Terminal, k.Quit}
	}
	if m.focus == focusChat {
		hs := []key.Binding{k.Send, k.Newline}
		if m.target != "" {
			hs = append(hs, k.Untarget)
		} else {
			hs = append(hs, k.Complete)
		}
		return append(hs, k.NextAgent, withHelp(k.Focus, "tab", "agents"), k.ViewControl, k.Terminal, withHelp(k.Help, "f1", "keys"), k.PageUp, k.Restart, k.Quit)
	}
	return []key.Binding{k.Down, k.NextAgent, k.Open, k.ViewControl, m.detachHelp(), k.Skill, k.Spawn, k.Pause, k.Back, k.Kill, k.Land, k.Help, k.Restart, k.Quit}
}

func withHelp(b key.Binding, h, desc string) key.Binding {
	b.SetHelp(h, desc)
	return b
}

// detachHelp describes the tmux keys that work inside an opened agent window,
// using the user's real prefix. It is display only: tmux handles these.
func (m *model) detachHelp() key.Binding {
	return key.NewBinding(key.WithHelp(m.prefix+" d / "+m.prefix+" n,p", "in a window: back / switch"))
}

// tmuxPrefix reads the user's tmux prefix key, like "C-b".
func tmuxPrefix() string {
	out, err := exec.Command("tmux", "show-options", "-gv", "prefix").Output()
	if p := strings.TrimSpace(string(out)); err == nil && p != "" && p != "None" {
		return p
	}
	return "C-b"
}

// viewKeys renders the shortcut line, fitting as many bindings as width allows.
func (m *model) viewKeys(width int) string {
	line := ""
	for _, b := range m.help() {
		h := b.Help()
		part := sKey.Render(h.Key) + " " + sDim.Render(h.Desc)
		if line != "" {
			part = "   " + part
		}
		if lipgloss.Width(line+part) > width-1 {
			break
		}
		line += part
	}
	return " " + line
}

// live reports whether a task has an agent window worth peeking at.
func live(status, window string) bool {
	if window == "" {
		return false
	}
	switch status {
	case store.Running, store.Idle, store.NeedsYou, store.Conflict:
		return true
	}
	return false
}

// cycleAgent moves the selection to the next (dir 1) or previous (dir -1)
// task with a live window, wrapping around. It reports whether it moved.
func (m *model) cycleAgent(dir int) bool {
	n := len(m.tasks)
	for step := 1; step < n; step++ {
		i := ((m.sel+dir*step)%n + n) % n
		if live(m.tasks[i].Status, m.tasks[i].Window) {
			m.sel = i
			return true
		}
	}
	return false
}

// livePos reports the selected task's 1-based place among tasks with live
// windows, and how many there are. pos is 0 if the selection isn't live.
func (m *model) livePos() (pos, n int) {
	for i, t := range m.tasks {
		if live(t.Status, t.Window) {
			n++
			if i == m.sel {
				pos = n
			}
		}
	}
	return pos, n
}
