// Package tui is saddle's terminal UI: tasks and a live peek on the left, a
// chat with the orchestrator on the right. Agents run hidden in tmux; the
// orchestrator is a headless Claude Code process this UI talks to.
package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/orch"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/termpane"
	"github.com/brandonapol/saddle/internal/triage"
	"github.com/brandonapol/saddle/internal/usage"
)

// Palette from the design canvas.
var (
	cAccent  = lipgloss.Color("#E0A458")
	cRun     = lipgloss.Color("#56C2B8")
	cDone    = lipgloss.Color("#8FBF6A")
	cAlert   = lipgloss.Color("#F2735A")
	cDim     = lipgloss.Color("#8A8F98")
	cFaint   = lipgloss.Color("#4A505A")
	cText    = lipgloss.Color("#D9D6CF")
	cBright  = lipgloss.Color("#F3EFE6")
	cBorder  = lipgloss.Color("#2A2F36")
	cFocus   = lipgloss.Color("#5A626E")
	cSelBg   = lipgloss.Color("#1E2329")
	cOpus    = lipgloss.Color("#E0A458")
	cSonnet  = lipgloss.Color("#2F9F94")
	cHaiku   = lipgloss.Color("#CFD6DE")
	sDim     = lipgloss.NewStyle().Foreground(cDim)
	sFaint   = lipgloss.NewStyle().Foreground(cFaint)
	sText    = lipgloss.NewStyle().Foreground(cText)
	sBright  = lipgloss.NewStyle().Foreground(cBright).Bold(true)
	sKey     = lipgloss.NewStyle().Foreground(cAccent)
	sLogo    = lipgloss.NewStyle().Foreground(lipgloss.Color("#0E1013")).Background(cAccent).Bold(true).Padding(0, 1)
	sSection = lipgloss.NewStyle().Foreground(cDim)
)

const (
	focusChat = iota
	focusTasks
	focusTerm
)

// Attention levels for orchestrator messages, set by Jev triage.
const (
	attnNormal = iota
	attnQuiet  // a status update; rendered dim
	attnUrgent // asks the user to act; highlighted, rings the bell
)

type chatLine struct {
	role string
	text string
	attn int
}

// attention is a worker event that may need the orchestrator or the user.
type attention struct {
	task, title, text, screen string
}

type model struct {
	app     *app.App
	launch  agent.Launch
	proc    *orch.Proc
	resumed bool // the current process was started with --resume
	gotInit bool

	width, height int
	focus         int

	tasks []mcpserver.TaskView
	sel   int
	peek  string
	prev  map[string]string // last seen status per task, for attention events

	chat      []chatLine
	streaming strings.Builder
	follow    bool
	vp        viewport.Model
	input     textarea.Model
	target    string   // task whose pane gets the next /command, if not the orchestrator
	skills    []string // skill names for completion, loaded on first use
	keys      keyMap
	prefix    string // the user's tmux prefix, for help text

	screens map[string]*screenState // recent screen per live worker, to spot stuck prompts

	// One refresh runs at a time. A slow one must not let ticks stack up
	// behind it and land out of order; a request made meanwhile sets stale,
	// which runs one more refresh when the current one lands.
	refreshing, stale bool

	pending   []string // events waiting for the orchestrator to be idle
	held      []string // info notices that ride along with the next message
	jev       *triage.Client
	eventTurn bool // the current orchestrator turn answers saddle events
	cost      float64
	limits    *usage.LimitEstimate // plan-limit estimate from the last refresh
	narr      narrSink             // narrator lines; nil when the narrator is off
	flash     string
	flashAt   time.Time

	quitArmedAt time.Time // first ctrl+c of a pending quit; zero when disarmed

	term      *termpane.Term // the shell in the bottom pane; nil until opened or after it exits
	termOpen  bool           // the pane is shown
	termShell string         // overrides $SHELL, for tests
}

// quitWindow is how long a first ctrl+c keeps quitting armed.
const quitWindow = 2 * time.Second

func (m *model) quitArmed() bool {
	return !m.quitArmedAt.IsZero() && time.Since(m.quitArmedAt) < quitWindow
}

// screenState tracks how long a worker's screen has been unchanged.
type screenState struct {
	text  string
	since time.Time
	acted bool
}

type (
	tickMsg    time.Time
	evMsg      struct{ e orch.Event }
	closedMsg  struct{}
	refreshMsg struct {
		err     error
		tasks   []mcpserver.TaskView
		peek    string
		screens map[string]string
		limits  *usage.LimitEstimate
	}
	flashMsg   string
	quitExpiry time.Time // the arming a timer was set for

	triagedMsg struct {
		att attention
		d   triage.Decision
		err error
	}
	salienceMsg struct {
		idx   int
		needs bool
	}
)

// Run starts the orchestrator and the TUI. first, if set, is sent as the
// user's opening message.
func Run(a *app.App, first string) error {
	l, resume, err := a.Orchestrator()
	if err != nil {
		return err
	}
	m := &model{app: a, launch: l, prev: map[string]string{}, screens: map[string]*screenState{}, follow: true, keys: newKeyMap(), prefix: tmuxPrefix()}
	if !a.Cfg.Triage.Disabled {
		m.jev = triage.FromEnv()
	}
	if err := m.startProc(resume); err != nil {
		return err
	}
	defer func() { m.proc.Close() }()

	// saddle up holds the repo's TUI lock, so this is the one process that
	// meters usage and narrates.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.NewUsageMeter().Run(ctx, nil)
	sink := make(narrSink, 256)
	if n := a.NarratorFromEnv(sink); n != nil {
		m.narr = sink
		go a.RunNarrator(ctx, n)
	}
	defer func() {
		if m.term != nil {
			m.term.Close()
		}
	}()

	hist, _ := a.Store.Chat(300)
	for _, c := range hist {
		m.chat = append(m.chat, chatLine{role: c.Role, text: c.Text})
	}
	if len(m.chat) == 0 {
		m.chat = append(m.chat, chatLine{role: store.ChatEvent, text: "Tell me what to work on, e.g. \"work #46 and #47 in parallel\", or paste an epic. I'll plan it, run the agents out of sight, and tell you when one needs you."})
	}

	m.input = textarea.New()
	m.input.Placeholder = "Message the orchestrator…"
	m.input.ShowLineNumbers = false
	m.input.Prompt = "› "
	m.input.SetHeight(3)
	m.input.CharLimit = 0
	m.input.KeyMap.InsertNewline = m.keys.Newline
	m.input.FocusedStyle.CursorLine = lipgloss.NewStyle()
	m.input.FocusedStyle.Prompt = lipgloss.NewStyle().Foreground(cAccent)
	m.input.Focus()
	m.vp = viewport.New(40, 10)

	if first != "" {
		m.sendUser(first)
	}
	_, err = tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	return err
}

func (m *model) startProc(resume string) error {
	cmd, err := m.launch.Headless(resume)
	if err != nil {
		return err
	}
	p, err := orch.Start(cmd)
	if err != nil {
		return fmt.Errorf("start orchestrator: %w", err)
	}
	m.proc, m.resumed, m.gotInit = p, resume != "", false
	return nil
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.waitEvent(), m.waitNarr(), m.refresh(), tick(), textarea.Blink)
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) waitEvent() tea.Cmd {
	ch := m.proc.Events()
	return func() tea.Msg {
		e, ok := <-ch
		if !ok {
			return closedMsg{}
		}
		return evMsg{e}
	}
}

// refresh reads task state and the selected task's screen off the UI goroutine.
func (m *model) refresh() tea.Cmd {
	if m.refreshing {
		m.stale = true
		return nil
	}
	m.refreshing = true
	sel := ""
	if m.sel < len(m.tasks) {
		sel = m.tasks[m.sel].ID
	}
	a := m.app
	return func() tea.Msg {
		all, err := mcpserver.Tasks(a)
		if err != nil {
			return refreshMsg{err: err}
		}
		var ts []mcpserver.TaskView
		for _, t := range all {
			if t.ID != app.OrchestratorID {
				ts = append(ts, t)
			}
		}
		sort.SliceStable(ts, func(i, j int) bool { return rank(ts[i].Status) < rank(ts[j].Status) })
		if sel == "" && len(ts) > 0 {
			sel = ts[0].ID
		}
		peek := ""
		if sel != "" {
			peek, _ = a.Peek(sel, 200)
		}
		screens := map[string]string{}
		for _, t := range ts {
			if t.Window != "" && (t.Status == store.Running || t.Status == store.Idle) {
				if t.ID == sel && peek != "" {
					screens[t.ID] = tailLines(peek, 25)
				} else if s, err := a.Peek(t.ID, 25); err == nil {
					screens[t.ID] = s
				}
			}
		}
		msg := refreshMsg{tasks: ts, peek: peek, screens: screens}
		if e, err := a.Limits(time.Now()); err == nil {
			msg.limits = &e
		}
		return msg
	}
}

// tailLines returns the last n lines of s, newlines kept.
func tailLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// rank orders the task list: what needs attention first, finished work last.
func rank(status string) int {
	switch status {
	case store.NeedsYou:
		return 0
	case store.Conflict:
		return 1
	case store.Idle:
		return 2
	case store.Running:
		return 3
	case store.Done:
		return 4
	case store.Landed:
		return 5
	}
	return 6
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()

	case tickMsg:
		cmds = append(cmds, m.refresh(), tick())

	case refreshMsg:
		m.refreshing = false
		if m.stale {
			m.stale = false
			cmds = append(cmds, m.refresh())
		}
		if msg.err != nil {
			m.flash, m.flashAt = "status: "+msg.err.Error(), time.Now()
			break
		}
		if msg.limits != nil {
			before := m.limits != nil && hasUsage(*m.limits)
			m.limits = msg.limits
			if hasUsage(*m.limits) != before && m.width > 0 {
				m.layout() // the strip changes the footer's height
			}
		}
		m.watchScreens(msg.tasks, msg.screens)
		cmds = append(cmds, m.noticeTransitions(msg.tasks)...)
		selID := ""
		if m.sel < len(m.tasks) {
			selID = m.tasks[m.sel].ID
		}
		m.tasks, m.peek = msg.tasks, msg.peek
		m.sel = 0
		for i, t := range m.tasks {
			if t.ID == selID {
				m.sel = i
			}
		}
		m.deliver()

	case evMsg:
		cmds = append(cmds, m.handleEvent(msg.e), m.waitEvent())

	case narrMsg:
		m.addChat(store.ChatNarrator, msg.line.String())
		cmds = append(cmds, m.waitNarr())

	case triagedMsg:
		m.applyTriage(msg)

	case salienceMsg:
		if msg.idx < len(m.chat) {
			if msg.needs {
				m.chat[msg.idx].attn = attnUrgent
				bell()
			} else {
				m.chat[msg.idx].attn = attnQuiet
			}
		}

	case termOutMsg, termExitMsg:
		cmds = append(cmds, m.termEvent(msg))

	case closedMsg:
		// The process is gone; ctrl+r restarts it.

	case quitExpiry:
		// Only the latest arming may clear the hint.
		if time.Time(msg).Equal(m.quitArmedAt) {
			m.quitArmedAt = time.Time{}
		}

	case flashMsg:
		m.flash, m.flashAt = string(msg), time.Now()

	case tea.MouseMsg:
		if m.termMouse(msg) {
			break
		}
		var c tea.Cmd
		m.vp, c = m.vp.Update(msg)
		m.follow = m.vp.AtBottom()
		cmds = append(cmds, c)

	case tea.KeyMsg:
		if c, handled := m.key(msg); handled {
			return m, c
		}
		if m.focus == focusChat {
			var c tea.Cmd
			m.input, c = m.input.Update(msg)
			cmds = append(cmds, c)
		}
	}
	m.renderChat()
	return m, tea.Batch(cmds...)
}

func (m *model) key(k tea.KeyMsg) (tea.Cmd, bool) {
	if m.focus == focusTerm && m.term != nil {
		return m.termKey(k), true
	}
	keys := m.keys
	if !key.Matches(k, keys.Quit) {
		m.quitArmedAt = time.Time{}
	}
	switch {
	case key.Matches(k, keys.Quit):
		if m.quitArmed() {
			return tea.Quit, true
		}
		m.quitArmedAt = time.Now()
		at := m.quitArmedAt
		return tea.Tick(quitWindow, func(time.Time) tea.Msg { return quitExpiry(at) }), true
	case key.Matches(k, keys.Terminal):
		return m.toggleTerm(), true
	case key.Matches(k, keys.Focus):
		if m.focus == focusChat && key.Matches(k, keys.Complete) {
			if c, ok := m.completeInput(); ok {
				return c, true
			}
		}
		if m.focus == focusChat {
			m.focus = focusTasks
			m.input.Blur()
		} else {
			m.focus = focusChat
			m.input.Focus()
		}
		return nil, true
	case key.Matches(k, keys.PageUp):
		m.vp.HalfPageUp()
		m.follow = false
		return nil, true
	case key.Matches(k, keys.PageDown):
		m.vp.HalfPageDown()
		m.follow = m.vp.AtBottom()
		return nil, true
	case key.Matches(k, keys.NextAgent, keys.PrevAgent):
		dir := 1
		if key.Matches(k, keys.PrevAgent) {
			dir = -1
		}
		if !m.cycleAgent(dir) {
			return func() tea.Msg { return flashMsg("no other running agent") }, true
		}
		return m.refresh(), true
	case key.Matches(k, keys.Restart):
		if m.proc != nil {
			m.proc.Close()
		}
		_, resume, _ := m.app.Orchestrator()
		if err := m.startProc(resume); err != nil {
			m.addChat(store.ChatEvent, "Restart failed: "+err.Error())
			return nil, true
		}
		m.addChat(store.ChatEvent, "Orchestrator restarted.")
		return m.waitEvent(), true
	}
	if m.focus == focusChat {
		if key.Matches(k, keys.Untarget) && m.target != "" {
			m.input.Reset()
			m.unaim()
			return nil, true
		}
		if key.Matches(k, keys.Send) {
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return nil, true
			}
			if m.target != "" {
				return m.submitTargeted(text), true
			}
			m.input.Reset()
			m.sendUser(text)
			return nil, true
		}
		return nil, false
	}
	// Task list keys.
	switch {
	case key.Matches(k, keys.Back):
		m.focus = focusChat
		m.input.Focus()
	case key.Matches(k, keys.Down):
		if m.sel < len(m.tasks)-1 {
			m.sel++
		}
		return m.refresh(), true
	case key.Matches(k, keys.Up):
		if m.sel > 0 {
			m.sel--
		}
		return m.refresh(), true
	case key.Matches(k, keys.Open):
		return m.attach(), true
	case key.Matches(k, keys.Skill):
		return m.aimAtAgent(), true
	case key.Matches(k, keys.Kill):
		if t, ok := m.selected(); ok {
			id := t.ID
			a := m.app
			return func() tea.Msg {
				if err := a.Kill(id, false); err != nil {
					return flashMsg(err.Error())
				}
				return flashMsg("killed " + id)
			}, true
		}
	case key.Matches(k, keys.Land):
		a := m.app
		return func() tea.Msg {
			rs, err := a.Land()
			if err != nil {
				return flashMsg("land: " + err.Error())
			}
			var parts []string
			for _, r := range rs {
				parts = append(parts, r.Task+" "+r.State)
			}
			if len(parts) == 0 {
				return flashMsg("train is empty")
			}
			return flashMsg("land: " + strings.Join(parts, ", "))
		}, true
	}
	return nil, true
}

func (m *model) selected() (mcpserver.TaskView, bool) {
	if m.sel < len(m.tasks) {
		return m.tasks[m.sel], true
	}
	return mcpserver.TaskView{}, false
}

// attach hands the terminal to the selected agent's tmux window. Detaching
// (prefix d) comes back here.
func (m *model) attach() tea.Cmd {
	t, ok := m.selected()
	if !ok || t.Window == "" {
		return func() tea.Msg { return flashMsg("no live window for that task") }
	}
	session := m.app.Cfg.Session
	_ = exec.Command("tmux", "select-window", "-t", t.Window).Run()
	var c *exec.Cmd
	if os.Getenv("TMUX") != "" {
		c = exec.Command("tmux", "display-popup", "-E", "-w", "95%", "-h", "95%",
			"env -u TMUX tmux attach-session -t ="+session)
	} else {
		c = exec.Command("tmux", "attach-session", "-t", "="+session)
	}
	return tea.ExecProcess(c, func(err error) tea.Msg {
		if err != nil {
			return flashMsg("attach: " + err.Error())
		}
		return flashMsg("back from " + t.ID)
	})
}

func (m *model) sendUser(text string) {
	m.eventTurn = false
	m.addChat(store.ChatUser, text)
	m.follow = true
	if err := m.proc.Send(text); err != nil {
		m.addChat(store.ChatEvent, "Could not reach the orchestrator ("+err.Error()+"). Press ctrl+r to restart it.")
	}
}

func (m *model) addChat(role, text string) {
	m.chat = append(m.chat, chatLine{role: role, text: text})
	_ = m.app.Store.AddChat(role, text)
}

func (m *model) handleEvent(e orch.Event) tea.Cmd {
	switch e.Kind {
	case orch.Init:
		m.gotInit = true
		if e.SessionID != "" {
			_ = m.app.Store.SetField(app.OrchestratorID, "session_id", e.SessionID)
		}
	case orch.Delta:
		m.streaming.WriteString(e.Text)
	case orch.Text:
		m.streaming.Reset()
		m.addChat(store.ChatAssistant, strings.TrimSpace(e.Text))
		if _, ok := urgentMark(e.Text); ok {
			// The orchestrator flagged it itself; no need to ask Jev.
			m.chat[len(m.chat)-1].attn = attnUrgent
			bell()
			return nil
		}
		if m.eventTurn && m.jev != nil {
			// Was this reply to a saddle event worth interrupting the user for?
			idx, text, c := len(m.chat)-1, e.Text, m.jev
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				needs, _ := triage.NeedsUser(ctx, c, text)
				return salienceMsg{idx: idx, needs: needs}
			}
		}
	case orch.Tool:
		m.streaming.Reset()
		m.addChat(store.ChatTool, e.Text)
	case orch.Result:
		m.streaming.Reset()
		m.cost = e.CostUSD
		if e.SessionID != "" {
			_ = m.app.Store.SetField(app.OrchestratorID, "session_id", e.SessionID)
		}
		m.deliver()
	case orch.Error:
		m.streaming.Reset()
		m.addChat(store.ChatEvent, "Orchestrator error: "+e.Text)
	case orch.Exit:
		m.streaming.Reset()
		if m.resumed && !m.gotInit {
			// The saved session couldn't be resumed; start a fresh one.
			_ = m.app.Store.SetField(app.OrchestratorID, "session_id", "")
			if err := m.startProc(""); err == nil {
				m.addChat(store.ChatEvent, "Couldn't resume the last conversation; started a new one.")
				return m.waitEvent()
			}
		}
		msg := "The orchestrator stopped."
		if e.Text != "" {
			msg += " " + lastLines(e.Text, 3)
		}
		m.addChat(store.ChatEvent, msg+" Press ctrl+r to restart it.")
	}
	return nil
}

// noticeTransitions turns worker status changes into events for the
// orchestrator, with the agent's screen when it is blocked.
func (m *model) noticeTransitions(ts []mcpserver.TaskView) []tea.Cmd {
	var cmds []tea.Cmd
	first := len(m.prev) == 0 && len(m.tasks) == 0
	for _, t := range ts {
		was, seen := m.prev[t.ID]
		m.prev[t.ID] = t.Status
		if first || !seen || was == t.Status {
			continue
		}
		var ev string
		switch t.Status {
		case store.NeedsYou:
			ev = fmt.Sprintf("%s (%s) is waiting on a permission prompt or a question.", t.ID, t.Title)
		case store.Idle:
			if strings.HasPrefix(t.Train, store.Queued) {
				continue
			}
			ev = fmt.Sprintf("%s (%s) stopped without calling done. It may be asking something, stuck, or finished without saying so.", t.ID, t.Title)
		default:
			continue
		}
		screen, _ := m.app.Peek(t.ID, 30)
		if c := m.raise(attention{task: t.ID, title: t.Title, text: ev, screen: screen}); c != nil {
			cmds = append(cmds, c)
		}
	}
	return cmds
}

// raise routes a worker event. With Jev, the screen is triaged first so that
// working agents and routine prompts never wake the orchestrator.
func (m *model) raise(att attention) tea.Cmd {
	if m.jev == nil || att.screen == "" {
		m.escalate(att, false)
		return nil
	}
	c := m.jev
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		d, err := triage.Screen(ctx, c, att.title, att.screen)
		return triagedMsg{att: att, d: d, err: err}
	}
}

func (m *model) applyTriage(msg triagedMsg) {
	att, d := msg.att, msg.d
	tag := fmt.Sprintf("jev: %s %.2f", d.Verdict, d.Confidence)
	if msg.err != nil {
		tag = "jev unavailable"
	}
	switch {
	case d.Route == triage.Drop:
		_ = m.app.Store.SetStatus(att.task, store.Running)
		m.app.Store.Event(att.task, "triage_drop", tag)
		return
	case d.Route == triage.AutoApprove && !m.app.Cfg.Triage.NoAutoApprove:
		if err := m.app.SendKeys(att.task, "", d.Keys); err == nil {
			_ = m.app.Store.SetStatus(att.task, store.Running)
			m.addChat(store.ChatEvent, fmt.Sprintf("✓ Approved a routine prompt for %s (%s): %s", att.task, tag, promptLine(att.screen)))
			return
		}
	case d.Route == triage.Human:
		att.text += " Saddle's triage (" + tag + ") flagged this for the user: tell them now, plainly, with your suggestion."
		m.escalate(att, true)
		return
	}
	m.escalate(att, false)
}

// escalate hands an event to the orchestrator; urgent ones also ring the bell.
func (m *model) escalate(att attention, urgent bool) {
	ev := att.text
	if att.screen != "" {
		ev += "\nIts screen:\n```\n" + att.screen + "\n```"
	}
	m.pending = append(m.pending, ev)
	if urgent {
		m.addChat(store.ChatEvent, "▲ "+att.task+" needs you: "+firstLine(att.text))
		bell()
	} else {
		m.addChat(store.ChatEvent, "▲ "+firstLine(att.text))
	}
}

// promptLine picks the question out of a prompt screen for the chat log.
func promptLine(screen string) string {
	for _, l := range strings.Split(screen, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "Do you want") || strings.HasSuffix(l, "?") {
			return l
		}
	}
	return "permission prompt"
}

func bell() { fmt.Fprint(os.Stderr, "\a") }

// watchScreens catches prompts no hook reports (like Claude Code's folder-trust
// dialog): a prompt that sits unchanged for a few seconds marks the worker as
// needing attention, which the transition check then reports with its screen.
func (m *model) watchScreens(ts []mcpserver.TaskView, screens map[string]string) {
	status := map[string]string{}
	for _, t := range ts {
		status[t.ID] = t.Status
	}
	for id, s := range screens {
		st := m.screens[id]
		if st == nil || st.text != s {
			m.screens[id] = &screenState{text: s, since: time.Now()}
			continue
		}
		if st.acted || time.Since(st.since) < 4*time.Second {
			continue
		}
		kind := app.DetectPrompt(s)
		if kind == app.PromptNone {
			continue
		}
		st.acted = true
		if kind == app.PromptTrust && m.app.RootTrusted() {
			if err := m.app.SendKeys(id, "", []string{"Down", "Enter"}); err == nil {
				m.addChat(store.ChatEvent, "Accepted the folder-trust prompt for "+id+"'s worktree (you already trust this repo).")
				continue
			}
		}
		switch status[id] {
		case store.Running, store.Idle:
			// The status change reaches the orchestrator via noticeTransitions.
			_ = m.app.Store.SetStatus(id, store.NeedsYou)
		case store.NeedsYou:
			// Already reported, but the screen changed and it is still a prompt:
			// an answer didn't take (e.g. a selection moved without Enter).
			m.escalate(attention{task: id, text: id + " is still waiting on a prompt after the last answer.", screen: s}, false)
		}
	}
}

// deliver sends queued events and saddle notices to the orchestrator once it is idle.
func (m *model) deliver() {
	if m.proc == nil || m.proc.Busy() {
		return
	}
	ns, _ := m.app.Store.TakeNotices(app.OrchestratorID, false)
	var actions []string
	for _, n := range ns {
		m.addChat(store.ChatEvent, firstLine(n.Text))
		if n.Kind == store.NoticeAction {
			actions = append(actions, n.Text)
		} else {
			// Landed/spawned updates don't need a turn of their own.
			m.held = append(m.held, n.Text)
		}
	}
	if len(m.pending) == 0 && len(actions) == 0 {
		return
	}
	msgs := append(append(append([]string{}, m.held...), actions...), m.pending...)
	m.pending, m.held = nil, nil
	m.eventTurn = true
	var b strings.Builder
	for i, s := range msgs {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("[saddle] " + s)
	}
	b.WriteString("\n\nDecide what to do. Tell the user only what they need to know, briefly.")
	if err := m.proc.Send(b.String()); err != nil {
		m.pending = msgs
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func lastLines(s string, n int) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, " ")
}

// Layout.

func (m *model) chatWidth() int {
	w := m.width * 42 / 100
	if w < 44 {
		w = 44
	}
	if w > 90 {
		w = 90
	}
	if w > m.width-30 {
		w = m.width - 30
	}
	return w
}

func (m *model) layout() {
	cw := m.chatWidth()
	m.input.SetWidth(cw - 4)
	m.vp.Width = cw - 4
	h := m.bodyHeight()
	m.setChatHeight(h - m.termHeight(h))
	m.resizeTerm()
	m.renderChat()
}

// bodyHeight is the screen between the header and the footer.
func (m *model) bodyHeight() int {
	return m.height - lipgloss.Height(m.viewHeader()) - lipgloss.Height(m.viewFooter())
}

func (m *model) renderChat() {
	w := m.vp.Width
	if w <= 0 {
		return
	}
	var b strings.Builder
	wrap := lipgloss.NewStyle().Width(w - 2)
	for _, c := range m.chat {
		b.WriteString(renderLine(c, w, wrap))
		b.WriteString("\n")
	}
	if m.streaming.Len() > 0 {
		b.WriteString(renderLine(chatLine{role: store.ChatAssistant, text: m.streaming.String()}, w, wrap))
		b.WriteString("\n")
	} else if m.proc != nil && m.proc.Busy() {
		b.WriteString(sDim.Render("  thinking…") + "\n")
	}
	m.vp.SetContent(b.String())
	if m.follow {
		m.vp.GotoBottom()
	}
}

func renderLine(c chatLine, w int, wrap lipgloss.Style) string {
	switch c.role {
	case store.ChatUser:
		return lipgloss.NewStyle().Foreground(cAccent).Bold(true).Render("you") + "\n" + sBright.UnsetBold().Render(wrap.Render(c.text)) + "\n"
	case store.ChatAssistant:
		attn := c.attn
		if text, ok := urgentMark(c.text); ok {
			c.text, attn = text, attnUrgent
		}
		switch attn {
		case attnQuiet:
			return sFaint.Render("saddle ·") + "\n" + renderMarkdown(c.text, w-2, sDim) + "\n"
		case attnUrgent:
			return sUrgent.Render("saddle ▲ needs you") + "\n" + renderUrgent(c.text, w-2) + "\n"
		}
		return lipgloss.NewStyle().Foreground(cRun).Bold(true).Render("saddle") + "\n" + renderMarkdown(c.text, w-2, sText) + "\n"
	case store.ChatTool:
		return sFaint.Render("  ⚙ " + truncate(c.text, w-6))
	case store.ChatNarrator:
		if narratorNeedsYou(c) {
			return renderUrgent(c.text, w-2)
		}
		return renderMarkdown("· "+c.text, w-2, sDim)
	default:
		return renderMarkdown("◇ "+c.text, w-2, lipgloss.NewStyle().Foreground(lipgloss.Color("#C9A26B")))
	}
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if n < 2 {
		n = 2
	}
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// View.

func (m *model) View() string {
	if m.width == 0 {
		return ""
	}
	header := m.viewHeader()
	footer := m.viewFooter()
	bodyH := m.height - lipgloss.Height(header) - lipgloss.Height(footer)
	termH := m.termHeight(bodyH)
	if m.term == nil {
		termH = 0
	}
	cw := m.chatWidth()
	left := m.viewLeft(m.width-cw, bodyH-termH)
	right := m.viewChat(cw, bodyH-termH)
	parts := []string{header, lipgloss.JoinHorizontal(lipgloss.Top, left, right)}
	if termH > 0 {
		parts = append(parts, m.viewTerm(termH))
	}
	return lipgloss.JoinVertical(lipgloss.Left, append(parts, footer)...)
}

func (m *model) viewHeader() string {
	counts := map[string]int{}
	for _, t := range m.tasks {
		counts[t.Status]++
	}
	parts := []string{sLogo.Render("SADDLE"), sBright.Render(m.app.Cfg.Session), sDim.Render("→ " + m.app.Cfg.Integration)}
	add := func(n int, s string, c lipgloss.Color) {
		if n > 0 {
			parts = append(parts, lipgloss.NewStyle().Foreground(c).Render(fmt.Sprintf(s, n)))
		}
	}
	add(counts[store.Running], "● %d running", cRun)
	add(counts[store.NeedsYou]+counts[store.Conflict]+counts[store.Idle], "▲ %d need attention", cAlert)
	add(counts[store.Done], "◆ %d queued", cAccent)
	add(counts[store.Landed], "✓ %d landed", cDone)
	left := strings.Join(parts, "  ")
	state := "idle"
	if m.proc != nil && m.proc.Busy() {
		state = "working"
	}
	jev := ""
	if m.jev != nil {
		jev = " · jev triage"
	}
	if m.narr != nil {
		jev += " · narrator"
	}
	right := sDim.Render(fmt.Sprintf("orchestrator %s · %s%s · $%.2f ", m.launch.Model, state, jev, m.cost))
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return lipgloss.NewStyle().Width(m.width).Render(left + strings.Repeat(" ", gap) + right)
}

// viewFooter is the bottom of the page: shortcuts on the last line. Other
// status lines (like usage) go above it.
func (m *model) viewFooter() string {
	line := m.viewKeys(m.width)
	if m.flash != "" && time.Since(m.flashAt) < 6*time.Second {
		line = " " + lipgloss.NewStyle().Foreground(cAccent).Render(m.flash)
	}
	if m.quitArmed() {
		line = " " + lipgloss.NewStyle().Foreground(cAccent).Render("Press Ctrl+C again to quit")
	}
	if m.limits != nil && hasUsage(*m.limits) {
		line = usageStrip(*m.limits, m.width) + "\n" + line
	}
	return lipgloss.NewStyle().Width(m.width).MaxWidth(m.width).Render(line)
}

func box(title string, w, h int, focused bool, body string) string {
	bc := cBorder
	if focused {
		bc = cFocus
	}
	inner := lipgloss.NewStyle().Width(w - 2).Height(h - 2).MaxHeight(h - 2).Render(body)
	b := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(bc).Render(inner)
	// Put the title into the top border.
	lines := strings.SplitN(b, "\n", 2)
	if len(lines) == 2 && title != "" {
		t := " " + truncate(title, w-6) + " "
		top := lipgloss.NewStyle().Foreground(bc).Render("╭─") + sSection.Render(t)
		rest := w - lipgloss.Width(top) - 1
		if rest < 0 {
			rest = 0
		}
		top += lipgloss.NewStyle().Foreground(bc).Render(strings.Repeat("─", rest) + "╮")
		b = top + "\n" + lines[1]
	}
	return b
}

func glyph(status string) (string, lipgloss.Color) {
	switch status {
	case store.Running:
		return "●", cRun
	case store.Idle:
		return "◐", cAccent
	case store.NeedsYou:
		return "▲", cAlert
	case store.Done:
		return "◆", cAccent
	case store.Conflict:
		return "✗", cAlert
	case store.Landed:
		return "✓", cDone
	}
	return "·", cFaint
}

func modelColor(model string) lipgloss.Color {
	switch {
	case strings.Contains(model, "opus"):
		return cOpus
	case strings.Contains(model, "sonnet"):
		return cSonnet
	case strings.Contains(model, "haiku"):
		return cHaiku
	}
	return cDim
}

func (m *model) viewLeft(w, h int) string {
	listH := len(m.tasks) + 2
	if listH < 5 {
		listH = 5
	}
	if listH > h/2 {
		listH = h / 2
	}
	var rows []string
	if len(m.tasks) == 0 {
		rows = append(rows, sDim.Render(" No agents yet. Ask the orchestrator to start some."))
	}
	for i, t := range m.tasks {
		g, gc := glyph(t.Status)
		train := ""
		if t.Train != "" {
			train = firstWord(t.Train)
		}
		meta := lipgloss.NewStyle().Foreground(modelColor(t.Model)).Render(fmt.Sprintf("%-6s", t.Model)) + " " + sDim.Render(fmt.Sprintf("%-9s", statusLabel(t.Status)))
		if train != "" && train != "landed" {
			meta += " " + sDim.Render(train)
		}
		titleW := w - 4 - 3 - 6 - lipgloss.Width(meta) - 2
		row := lipgloss.NewStyle().Foreground(gc).Render(g) + " " + sDim.Render(fmt.Sprintf("%-5s", t.ID)) + " " +
			sText.Render(fmt.Sprintf("%-*s", maxInt(titleW, 4), truncate(t.Title, maxInt(titleW, 4)))) + " " + meta
		if i == m.sel {
			marker := "›"
			row = lipgloss.NewStyle().Foreground(cAccent).Render(marker) + row
			if m.focus == focusTasks {
				row = lipgloss.NewStyle().Background(cSelBg).Width(w - 2).Render(row)
			}
		} else {
			row = " " + row
		}
		rows = append(rows, row)
	}
	list := box("AGENTS", w, listH, m.focus == focusTasks, strings.Join(rows, "\n"))

	peekH := h - listH
	title := "PEEK"
	body := sDim.Render(" Select an agent to see its terminal.")
	if t, ok := m.selected(); ok {
		title = "PEEK · " + t.ID + " " + t.Title
		if i, n := m.livePos(); n > 1 {
			// Make switching discoverable where the user is looking.
			pos := fmt.Sprintf("%d/%d", i, n)
			if i == 0 {
				pos = fmt.Sprintf("%d live", n)
			}
			title = fmt.Sprintf("PEEK %s %s · %s %s", pos, m.keys.NextAgent.Help().Key, t.ID, t.Title)
		}
		if m.peek != "" {
			lines := strings.Split(m.peek, "\n")
			if n := peekH - 2; len(lines) > n && n > 0 {
				lines = lines[len(lines)-n:]
			}
			for i, l := range lines {
				lines[i] = truncate(l, w-3)
			}
			body = sText.Render(strings.Join(lines, "\n"))
		} else if t.Window == "" || t.Status == store.Landed || t.Status == store.Killed {
			body = sDim.Render(" Window closed (" + statusLabel(t.Status) + ").")
			if t.PR != "" {
				body += "\n " + sDim.Render(t.PR)
			}
		}
	}
	return lipgloss.JoinVertical(lipgloss.Left, list, box(title, w, peekH, false, body))
}

// setChatHeight fits the chat viewport into a body of height h: the box
// border, the input and its top rule take the rest.
func (m *model) setChatHeight(h int) {
	m.vp.Height = max(h-2-m.input.Height()-1, 3)
	if m.follow {
		m.vp.GotoBottom()
	}
}

func (m *model) viewChat(w, h int) string {
	m.input.SetWidth(w - 4)
	m.setChatHeight(h)
	in := lipgloss.NewStyle().Border(lipgloss.NormalBorder(), true, false, false, false).BorderForeground(cBorder).Width(w - 2).Render(m.input.View())
	body := lipgloss.JoinVertical(lipgloss.Left, m.vp.View(), in)
	return box("ORCHESTRATOR · "+m.launch.Model, w, h, m.focus == focusChat, body)
}

func statusLabel(s string) string {
	switch s {
	case store.NeedsYou:
		return "needs you"
	case store.Done:
		return "queued"
	}
	return s
}

func firstWord(s string) string {
	if i := strings.IndexAny(s, ": "); i > 0 {
		return s[:i]
	}
	return s
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
