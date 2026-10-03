package tui

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
)

// trainer is what the train keys do: the App calls behind `saddle queue
// move|hold|release`.
type trainer interface {
	MoveInQueue(task string, pos int) error
	Hold(task, reason string) error
	Unhold(task string) error
}

// trainState is the merge view's train panel (#36): every train entry with
// its state, the files landed work moved, and what is left of each returned
// entry's conflict.
type trainState struct {
	focus   bool   // the merge view's keys act on the train, not the stacks
	sel     string // selected entry's task
	entries []store.TrainEntry
	loaded  bool
	renames []renameRow
	hunks   map[string]string // task → first conflict still marked in its worktree
	cache   map[string][]renameRow
	busy    string
	loading bool

	// Hooks; nil means the real thing.
	ops    trainer
	load   func() trainLoadedMsg
	attach func(mcpserver.TaskView) tea.Cmd
}

type (
	trainLoadedMsg struct {
		entries []store.TrainEntry
		renames []renameRow
		hunks   map[string]string
		cache   map[string][]renameRow
		err     error
	}
	// trainDoneMsg is a queue action landing: what to flash.
	trainDoneMsg string
)

// renameRow is one file a landed task moved.
type renameRow struct{ task, old, new string }

// trainLanded is how many landed entries the panel lists and reads renames
// for, newest first.
const trainLanded = 5

// loadTrain reads the train, the renames and the conflict hunks off the UI
// goroutine.
func (m *model) loadTrain() tea.Cmd {
	if m.tr.loading {
		return nil
	}
	m.tr.loading = true
	if m.tr.load != nil {
		load := m.tr.load
		return func() tea.Msg { return load() }
	}
	a := m.app
	cache := make(map[string][]renameRow, len(m.tr.cache))
	for k, v := range m.tr.cache {
		cache[k] = v
	}
	worktrees := map[string]string{}
	for _, t := range m.tasks {
		worktrees[t.ID] = t.Worktree
	}
	return func() tea.Msg {
		es, err := a.Store.Train()
		if err != nil {
			return trainLoadedMsg{err: err}
		}
		msg := trainLoadedMsg{entries: es, cache: cache, hunks: map[string]string{}}
		msg.renames = landedRenames(a.Root, es, cache)
		for _, e := range es {
			if returned(e.State) && worktrees[e.Task] != "" {
				if h := conflictHunk(worktrees[e.Task], conflictFiles(worktrees[e.Task], e.Note)); h != "" {
					msg.hunks[e.Task] = h
				}
			}
		}
		return msg
	}
}

func (m *model) trainLoaded(msg trainLoadedMsg) {
	m.tr.loading = false
	if msg.err != nil {
		m.flash, m.flashAt = "train: "+msg.err.Error(), time.Now()
		return
	}
	m.tr.entries, m.tr.renames, m.tr.hunks, m.tr.loaded = msg.entries, msg.renames, msg.hunks, true
	if msg.cache != nil {
		m.tr.cache = msg.cache
	}
}

// landedRenames lists the files the newest landed entries moved, read from
// each entry's landing range (the train notes it as old..head). A range
// never changes, so cache keeps each one's answer.
func landedRenames(root string, es []store.TrainEntry, cache map[string][]renameRow) []renameRow {
	var out []renameRow
	n := 0
	for i := len(es) - 1; i >= 0 && n < trainLanded; i-- {
		e := es[i]
		from, to, ok := strings.Cut(e.Note, "..")
		if e.State != store.TrainOK || !ok {
			continue
		}
		n++
		rows, hit := cache[e.Note]
		if !hit {
			rs, err := gitx.Renames(root, from, to)
			if err != nil {
				continue
			}
			rows = []renameRow{}
			for _, r := range rs {
				rows = append(rows, renameRow{task: e.Task, old: r.Old, new: r.New})
			}
			cache[e.Note] = rows
		}
		out = append(out, rows...)
	}
	return out
}

// conflictFiles is the files a returned entry conflicts in: the unmerged
// paths of a rebase the agent has under way, else the files the train noted.
func conflictFiles(worktree, note string) []string {
	if out, err := gitx.Run(worktree, "diff", "--name-only", "--diff-filter=U"); err == nil && out != "" {
		return strings.Split(out, "\n")
	}
	var files []string
	for _, f := range strings.Split(note, ",") {
		if f = strings.TrimSpace(f); f != "" && !strings.Contains(f, " ") {
			files = append(files, f)
		}
	}
	return files
}

// maxHunk caps the lines of a conflict hunk shown.
const maxHunk = 14

// conflictHunk is the first conflict still marked in files under worktree,
// headed by its file name; "" when none is left.
func conflictHunk(worktree string, files []string) string {
	for _, f := range files {
		fh, err := os.Open(filepath.Join(worktree, f))
		if err != nil {
			continue
		}
		var lines []string
		in := false
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			l := sc.Text()
			if strings.HasPrefix(l, "<<<<<<< ") {
				in = true
			}
			if !in {
				continue
			}
			if len(lines) < maxHunk {
				lines = append(lines, l)
			}
			if strings.HasPrefix(l, ">>>>>>> ") {
				break
			}
		}
		fh.Close()
		if len(lines) > 0 {
			if last := lines[len(lines)-1]; !strings.HasPrefix(last, ">>>>>>> ") {
				lines = append(lines, "…")
			}
			return f + "\n" + strings.Join(lines, "\n")
		}
	}
	return ""
}

func waiting(state string) bool { return state == store.Queued || state == store.OnHold }

func returned(state string) bool {
	return state == store.TrainError || state == store.TestFailed || state == app.TrainEscalated
}

// trainEntries is the train as loaded, or as the task list knows it until
// the first load lands.
func (m *model) trainEntries() []store.TrainEntry {
	if m.tr.loaded {
		return m.tr.entries
	}
	var es []store.TrainEntry
	for _, t := range m.tasks {
		if t.Train == "" {
			continue
		}
		state, note, _ := strings.Cut(t.Train, ": ")
		es = append(es, store.TrainEntry{Task: t.ID, State: state, Note: note})
	}
	return es
}

// trainSections splits the train into the waiting queue in landing order,
// the returned entries and the newest landed ones.
func (m *model) trainSections() (queue, back, landed []store.TrainEntry) {
	for _, e := range m.trainEntries() {
		switch {
		case waiting(e.State):
			queue = append(queue, e)
		case e.State == store.TrainOK:
			landed = append(landed, e)
		default:
			back = append(back, e)
		}
	}
	slices.Reverse(landed)
	return queue, back, landed[:min(len(landed), trainLanded)]
}

// trainOrder is the entries in the order the panel lists and j/k move.
func (m *model) trainOrder() []store.TrainEntry {
	q, b, l := m.trainSections()
	return slices.Concat(q, b, l)
}

// trainSelected is the train entry the keys act on: the selected one, else
// the first listed.
func (m *model) trainSelected() store.TrainEntry {
	order := m.trainOrder()
	if i := slices.IndexFunc(order, func(e store.TrainEntry) bool { return e.Task == m.tr.sel }); i >= 0 {
		return order[i]
	}
	if len(order) > 0 {
		return order[0]
	}
	return store.TrainEntry{}
}

func (m *model) moveTrainSel(dir int) {
	order := m.trainOrder()
	if len(order) == 0 {
		return
	}
	i := slices.IndexFunc(order, func(e store.TrainEntry) bool { return e.Task == m.trainSelected().Task })
	m.tr.sel = order[min(max(i+dir, 0), len(order)-1)].Task
}

func (m *model) trainer() trainer {
	if m.tr.ops != nil {
		return m.tr.ops
	}
	return m.app
}

// trainKey handles the train panel's keys; ok is false for any other key.
func (m *model) trainKey(k tea.KeyMsg) (tea.Cmd, bool) {
	keys := m.keys
	e := m.trainSelected()
	switch {
	case key.Matches(k, keys.Down):
		m.moveTrainSel(1)
		return nil, true
	case key.Matches(k, keys.Up):
		m.moveTrainSel(-1)
		return nil, true
	case key.Matches(k, keys.QueueDown, keys.QueueUp):
		if !waiting(e.State) {
			return flashCmd(e.Task + " isn't waiting in the queue"), true
		}
		q, _, _ := m.trainSections()
		i := slices.IndexFunc(q, func(x store.TrainEntry) bool { return x.Task == e.Task })
		pos := i + 2
		if key.Matches(k, keys.QueueUp) {
			pos = i
		}
		if pos < 1 || pos > len(q) {
			return nil, true
		}
		m.tr.sel = e.Task
		return m.trainRun(fmt.Sprintf("moving %s to %d", e.Task, pos), func(x trainer) (string, error) {
			return fmt.Sprintf("moved %s to %d", e.Task, pos), x.MoveInQueue(e.Task, pos)
		}), true
	case key.Matches(k, keys.Hold):
		switch e.State {
		case store.OnHold:
			return m.trainRun("releasing "+e.Task, func(x trainer) (string, error) {
				return e.Task + " is back in line", x.Unhold(e.Task)
			}), true
		case store.Queued:
			return m.trainRun("holding "+e.Task, func(x trainer) (string, error) {
				return e.Task + " is on hold; h releases it", x.Hold(e.Task, "held from the TUI")
			}), true
		}
		return flashCmd(cmpOr(e.Task, "nothing") + " isn't waiting in the queue"), true
	case key.Matches(k, keys.TakeOver):
		i := slices.IndexFunc(m.tasks, func(t mcpserver.TaskView) bool { return t.ID == e.Task })
		if i < 0 {
			return flashCmd("no task to take over"), true
		}
		t := m.tasks[i]
		if !live(t.Status, t.Window) {
			return flashCmd(t.ID + " has no live agent window; its worktree is " + cmpOr(t.Worktree, "gone")), true
		}
		if m.tr.attach != nil {
			return m.tr.attach(t), true
		}
		return m.attachTask(t), true
	}
	return nil, false
}

// trainRun runs one queue action off the UI goroutine, one at a time.
func (m *model) trainRun(doing string, do func(trainer) (string, error)) tea.Cmd {
	if m.tr.busy != "" {
		return flashCmd("still " + m.tr.busy)
	}
	m.tr.busy = doing
	m.flash, m.flashAt = doing+"…", time.Now()
	x := m.trainer()
	return func() tea.Msg {
		out, err := do(x)
		if err != nil {
			return trainDoneMsg(doing + ": " + err.Error())
		}
		return trainDoneMsg(out)
	}
}

// viewTrain renders the train panel w columns wide: counters, the queue,
// returned and landed entries, the rename map and the selected returned
// entry's conflict detail.
func (m *model) viewTrain(w int) []string {
	wrap := lipgloss.NewStyle().Width(w)
	color := func(c lipgloss.Color, s string) string { return lipgloss.NewStyle().Foreground(c).Render(s) }
	queue, back, landed := m.trainSections()
	if len(queue)+len(back)+len(landed) == 0 {
		return []string{sBright.Render("Train"), sDim.Render("The train is empty. Tasks join it when they call done.")}
	}
	held, esc := 0, 0
	for _, e := range queue {
		if e.State == store.OnHold {
			held++
		}
	}
	for _, e := range back {
		if e.State == app.TrainEscalated {
			esc++
		}
	}
	nLanded := 0
	for _, e := range m.trainEntries() {
		if e.State == store.TrainOK {
			nLanded++
		}
	}
	counts := []string{color(cAccent, fmt.Sprintf("%d queued", len(queue)-held))}
	if held > 0 {
		counts = append(counts, color(cAccent, fmt.Sprintf("%d held", held)))
	}
	if n := len(back) - esc; n > 0 {
		counts = append(counts, color(cAlert, fmt.Sprintf("%d returned", n)))
	}
	if esc > 0 {
		counts = append(counts, color(cAlert, fmt.Sprintf("%d escalated", esc)))
	}
	counts = append(counts, color(cDone, fmt.Sprintf("%d landed", nLanded)))
	rows := []string{wrap.Render(sBright.Render("Train") + "  " + strings.Join(counts, sDim.Render(" · ")))}

	sel := m.trainSelected()
	titles := map[string]string{}
	for _, t := range m.tasks {
		titles[t.ID] = t.Title
	}
	row := func(lead string, e store.TrainEntry) string {
		mark := "  "
		if m.tr.focus && e.Task == sel.Task {
			mark = sKey.Render("▸ ")
		}
		state := strings.ReplaceAll(e.State, "_", " ")
		if e.Note != "" && e.State != store.TrainOK {
			state += ": " + e.Note
		}
		c := cDim
		switch {
		case e.State == store.OnHold:
			c = cAccent
		case returned(e.State):
			c = cAlert
		case e.State == store.TrainOK:
			c = cDone
			if _, to, ok := strings.Cut(e.Note, ".."); ok {
				state += " " + to[:min(len(to), 8)]
			}
		}
		line := mark + sDim.Render(lead) + sText.Render(e.Task)
		if t := titles[e.Task]; t != "" {
			line += " " + sText.Render(t)
		}
		return wrap.Render(line + "  " + color(c, state))
	}
	if len(queue) > 0 {
		rows = append(rows, sSection.Render("queue"))
		for i, e := range queue {
			rows = append(rows, row(fmt.Sprintf("%d. ", i+1), e))
		}
	}
	if len(back) > 0 {
		rows = append(rows, sSection.Render("returned"))
		for _, e := range back {
			rows = append(rows, row("", e))
		}
	}
	if len(landed) > 0 {
		rows = append(rows, sSection.Render("landed"))
		for _, e := range landed {
			rows = append(rows, row("", e))
		}
	}
	if len(m.tr.renames) > 0 {
		rows = append(rows, "", sBright.Render("Renames"))
		for _, r := range m.tr.renames {
			rows = append(rows, wrap.Render("  "+sText.Render(r.old)+sDim.Render(" → ")+sText.Render(r.new)+sDim.Render("  "+r.task)))
		}
	}
	// Conflict detail for the selected returned entry, else the first one.
	detail := sel
	if !returned(detail.State) && len(back) > 0 {
		detail = back[0]
	}
	if returned(detail.State) {
		rows = append(rows, "", wrap.Render(sBright.Render("Conflict "+detail.Task)+sDim.Render(" · "+plural(detail.Attempts, "failed attempt"))))
		for _, s := range m.resolution(detail) {
			rows = append(rows, wrap.Render(sText.Render("  "+s)))
		}
		if h := m.tr.hunks[detail.Task]; h != "" {
			file, hunk, _ := strings.Cut(h, "\n")
			rows = append(rows, wrap.Render(sDim.Render("  remaining in ")+sText.Render(file)+sDim.Render(":")))
			for _, l := range strings.Split(hunk, "\n") {
				c := cText
				switch {
				case strings.HasPrefix(l, "<<<<<<<"), strings.HasPrefix(l, "======="), strings.HasPrefix(l, ">>>>>>>"):
					c = cAccent
				}
				rows = append(rows, clip("    "+color(c, l), w))
			}
		}
	}
	return rows
}

// resolution is the steps that get a returned entry landing again, the same
// steps the train sent its agent.
func (m *model) resolution(e store.TrainEntry) []string {
	integ := m.app.Cfg.Integration
	switch e.State {
	case store.TrainError:
		if e.Note == "" || strings.Contains(e.Note, " ") {
			return []string{e.Note, "Fix it in the worktree, commit, and call done again."}
		}
		return []string{
			"Rebasing onto " + integ + " conflicts in " + e.Note + ".",
			"1. saddle sync starts the rebase and stops at the conflicts.",
			"2. Resolve them keeping both sides' intent, then git add and git rebase --continue.",
			"3. Run the tests, then call done again.",
		}
	case store.TestFailed:
		return []string{
			"Rebased cleanly onto " + integ + ", but the tests failed on the result.",
			"Fix it, commit, and call done again.",
		}
	}
	return []string{
		"The train stopped returning it to its agent: " + e.Note + ".",
		"Fix it yourself (" + m.keys.TakeOver.Help().Key + " take over), spawn a repair task, or tell the agent what to do; done queues it again.",
	}
}
