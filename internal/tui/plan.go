package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/planner"
)

// planState is the plan review view (#35): the newest plan under
// .saddle/plans, the checker's result for it, and the hooks its keys call.
// Every key does what `saddle plan approve|reopen|edit|replan` does, through
// the same planner functions.
type planState struct {
	paths   []string // plan files, newest first
	cur     string   // the plan shown
	doc     *planner.Doc
	check   planner.Plan
	chkErr  error // Check's error for doc
	loadErr error // the plan doesn't parse or validate
	sel     int   // selected card, in wave order
	busy    string
	loading bool
	noting  bool // the replan note prompt is open
	note    textinput.Model

	// Hooks; nil means the real thing.
	dir    string                           // plans directory
	base   func() (string, error)           // commit approve freezes at
	editor func(path string) tea.Cmd        // runs $EDITOR, then sends planEditedMsg
	replan func(path, note string) error    // asks the planner model to revise
	goPlan func(path string, d planner.Doc) // starts an approved plan
}

type (
	planLoadedMsg struct {
		paths   []string
		cur     string
		doc     *planner.Doc
		check   planner.Plan
		chkErr  error
		loadErr error
	}
	// planDoneMsg is a plan action landing: what to flash.
	planDoneMsg string
	// planEditedMsg is $EDITOR exiting on path.
	planEditedMsg struct {
		path string
		err  error
	}
)

// planModels is what + and - cycle a task's model through; "" is the
// configured default.
var planModels = []string{"", "haiku", "sonnet", "opus"}

func (m *model) planDir() string {
	if m.pl.dir != "" {
		return m.pl.dir
	}
	return filepath.Join(m.app.Root, ".saddle", "plans")
}

// loadPlan reads the plans directory and the current plan off the UI
// goroutine.
func (m *model) loadPlan() tea.Cmd {
	if m.pl.loading {
		return nil
	}
	m.pl.loading = true
	dir, cur := m.planDir(), m.pl.cur
	serial, limit := m.app.Cfg.Serial, m.app.Cfg.Concurrency
	return func() tea.Msg {
		msg := planLoadedMsg{paths: planFiles(dir)}
		if !slices.Contains(msg.paths, cur) {
			cur = ""
		}
		if cur == "" && len(msg.paths) > 0 {
			cur = msg.paths[0]
		}
		msg.cur = cur
		if cur == "" {
			return msg
		}
		d, err := planner.LoadDoc(cur)
		if err != nil {
			msg.loadErr = err
			return msg
		}
		msg.doc = &d
		msg.check, msg.chkErr = planner.Check(d.Tasks, serial, limit)
		return msg
	}
}

// planFiles lists the plan files in dir, newest first.
func planFiles(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type f struct {
		path string
		mod  time.Time
	}
	var fs []f
	for _, e := range ents {
		if e.IsDir() || filepath.Ext(e.Name()) != ".toml" {
			continue
		}
		if info, err := e.Info(); err == nil {
			fs = append(fs, f{filepath.Join(dir, e.Name()), info.ModTime()})
		}
	}
	slices.SortStableFunc(fs, func(a, b f) int { return b.mod.Compare(a.mod) })
	out := make([]string, len(fs))
	for i, x := range fs {
		out[i] = x.path
	}
	return out
}

func (m *model) planLoaded(msg planLoadedMsg) {
	m.pl.loading = false
	m.pl.paths, m.pl.cur, m.pl.doc = msg.paths, msg.cur, msg.doc
	m.pl.check, m.pl.chkErr, m.pl.loadErr = msg.check, msg.chkErr, msg.loadErr
	m.pl.sel = min(m.pl.sel, max(len(m.planOrder())-1, 0))
}

// planWaves is the plan's waves, or every task in one unchecked column when
// the checker failed.
func (m *model) planWaves() [][]string {
	d := m.pl.doc
	if d == nil {
		return nil
	}
	if m.pl.chkErr == nil && len(m.pl.check.Waves) > 0 {
		return m.pl.check.Waves
	}
	ids := make([]string, len(d.Tasks))
	for i, t := range d.Tasks {
		ids[i] = t.ID
	}
	return [][]string{ids}
}

// planOrder is every task ID in wave order, the order j/k move through.
func (m *model) planOrder() []string {
	var ids []string
	for _, w := range m.planWaves() {
		ids = append(ids, w...)
	}
	return ids
}

func (m *model) planSelected() (string, bool) {
	ids := m.planOrder()
	if m.pl.sel < len(ids) {
		return ids[m.pl.sel], true
	}
	return "", false
}

// planKey handles the plan view's keys; ok is false for any other key.
func (m *model) planKey(k tea.KeyMsg) (tea.Cmd, bool) {
	keys := m.keys
	if m.pl.noting {
		switch k.Type {
		case tea.KeyEnter:
			note := strings.TrimSpace(m.pl.note.Value())
			m.pl.noting = false
			m.pl.note.Blur()
			if note == "" {
				return flashCmd("replan needs a note saying what to change"), true
			}
			return m.planReplan(note), true
		case tea.KeyEsc:
			m.pl.noting = false
			m.pl.note.Blur()
			return nil, true
		}
		var c tea.Cmd
		m.pl.note, c = m.pl.note.Update(k)
		return c, true
	}
	switch {
	case key.Matches(k, keys.Down):
		m.pl.sel = min(m.pl.sel+1, max(len(m.planOrder())-1, 0))
		return nil, true
	case key.Matches(k, keys.Up):
		m.pl.sel = max(m.pl.sel-1, 0)
		return nil, true
	case key.Matches(k, keys.PlanNext, keys.PlanPrev):
		if len(m.pl.paths) < 2 {
			return flashCmd("no other plan"), true
		}
		dir := 1
		if key.Matches(k, keys.PlanPrev) {
			dir = -1
		}
		i := slices.Index(m.pl.paths, m.pl.cur)
		m.pl.cur = m.pl.paths[((i+dir)%len(m.pl.paths)+len(m.pl.paths))%len(m.pl.paths)]
		m.pl.sel = 0
		return m.loadPlan(), true
	case key.Matches(k, keys.Approve):
		return m.planApprove(), true
	case key.Matches(k, keys.EditPlan):
		return m.planEdit(), true
	case key.Matches(k, keys.Replan):
		if m.pl.cur == "" {
			return flashCmd("no plan to replan"), true
		}
		if m.pl.doc != nil && m.pl.doc.Approved {
			return flashCmd("plan is approved; a reopens it before replanning"), true
		}
		m.pl.note = textinput.New()
		m.pl.note.Placeholder = "what should the planner change?"
		m.pl.noting = true
		return m.pl.note.Focus(), true
	case key.Matches(k, keys.ModelUp):
		return m.planModel(1), true
	case key.Matches(k, keys.ModelDown):
		return m.planModel(-1), true
	case key.Matches(k, keys.Go):
		return m.planGo(), true
	}
	return nil, false
}

// planRun runs one plan action off the UI goroutine, one at a time; the plan
// reloads when it lands.
func (m *model) planRun(doing string, do func() (string, error)) tea.Cmd {
	if m.pl.busy != "" {
		return flashCmd("still " + m.pl.busy)
	}
	m.pl.busy = doing
	m.flash, m.flashAt = doing+"…", time.Now()
	return func() tea.Msg {
		out, err := do()
		if err != nil {
			return planDoneMsg(doing + ": " + err.Error())
		}
		return planDoneMsg(out)
	}
}

func (m *model) planApprove() tea.Cmd {
	path, d := m.pl.cur, m.pl.doc
	if path == "" {
		return flashCmd("no plan to approve")
	}
	serial, limit := m.app.Cfg.Serial, m.app.Cfg.Concurrency
	name := filepath.Base(path)
	if d != nil && d.Approved {
		return m.planRun("reopening "+name, func() (string, error) {
			_, err := planner.Reopen(path, serial, limit)
			return "reopened " + name, err
		})
	}
	base := m.pl.base
	if base == nil {
		a := m.app
		base = func() (string, error) { return gitx.RevParse(a.Root, a.Cfg.Base) }
	}
	return m.planRun("approving "+name, func() (string, error) {
		at, err := base()
		if err != nil {
			return "", err
		}
		d, err := planner.Approve(path, at, serial, limit)
		return fmt.Sprintf("approved %s: %d tasks frozen at %s", name, len(d.Tasks), short(at)), err
	})
}

func short(sha string) string { return sha[:min(len(sha), 12)] }

// planEdit hands the terminal to $EDITOR on the plan; planEdited re-checks it
// when the editor exits.
func (m *model) planEdit() tea.Cmd {
	path := m.pl.cur
	if path == "" {
		return flashCmd("no plan to edit")
	}
	if m.pl.doc != nil && m.pl.doc.Approved {
		return flashCmd(planner.ErrApproved.Error() + ": press a")
	}
	if m.pl.editor != nil {
		return m.pl.editor(path)
	}
	return tea.ExecProcess(editorCmd(path), func(err error) tea.Msg { return planEditedMsg{path: path, err: err} })
}

// editorCmd runs $VISUAL or $EDITOR (vi if neither is set) on path through
// sh, as `saddle plan edit` does.
func editorCmd(path string) *exec.Cmd {
	ed := os.Getenv("VISUAL")
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}
	if ed == "" {
		ed = "vi"
	}
	return exec.Command("sh", "-c", ed+` "$1"`, "sh", path)
}

// planEdited re-runs the checker on an edited plan through planner.Edit, the
// edit itself having already happened in the terminal.
func (m *model) planEdited(msg planEditedMsg) tea.Cmd {
	if msg.err != nil {
		return flashCmd("editor: " + msg.err.Error())
	}
	serial, limit := m.app.Cfg.Serial, m.app.Cfg.Concurrency
	name := filepath.Base(msg.path)
	return m.planRun("checking "+name, func() (string, error) {
		_, _, err := planner.Edit(msg.path, func(string) error { return nil }, serial, limit)
		if err != nil {
			return "", fmt.Errorf("%w; your edit is kept, e to fix it", err)
		}
		return "checked " + name, nil
	})
}

// planModel moves the selected task's model dir steps through planModels and
// saves the plan.
func (m *model) planModel(dir int) tea.Cmd {
	d, path := m.pl.doc, m.pl.cur
	id, ok := m.planSelected()
	if d == nil || !ok {
		return flashCmd("no task selected")
	}
	if d.Approved {
		return flashCmd(planner.ErrApproved.Error() + ": press a")
	}
	serial, limit := m.app.Cfg.Serial, m.app.Cfg.Concurrency
	doc := *d
	doc.Tasks = slices.Clone(d.Tasks)
	i := slices.IndexFunc(doc.Tasks, func(t planner.Task) bool { return t.ID == id })
	if i < 0 {
		return flashCmd("no task selected")
	}
	j := max(slices.Index(planModels, doc.Tasks[i].Model), 0)
	doc.Tasks[i].Model = planModels[(j+dir+len(planModels))%len(planModels)]
	label := cmpOr(doc.Tasks[i].Model, "the default model")
	return m.planRun("setting "+id+" to "+label, func() (string, error) {
		return id + " runs on " + label, planner.WriteDoc(path, doc, serial, limit)
	})
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (m *model) planReplan(note string) tea.Cmd {
	path := m.pl.cur
	replan := m.pl.replan
	if replan == nil {
		replan = appReplan(m.app)
	}
	return m.planRun("replanning "+filepath.Base(path), func() (string, error) {
		return "replanned " + filepath.Base(path), replan(path, note)
	})
}

// appReplan is `saddle plan replan`: snapshot the repo, ask the planner model
// to revise the plan as note says, and save it.
func appReplan(a *app.App) func(path, note string) error {
	return func(path, note string) error {
		d, err := planner.LoadDoc(path)
		if err != nil {
			return err
		}
		if d.Approved {
			return planner.ErrApproved
		}
		ctx := context.Background()
		snap, err := planner.TakeSnapshot(ctx, a.Root, 3)
		if err != nil {
			return err
		}
		snap.Serial = a.Cfg.Serial
		dr, err := planner.Generate(ctx, plannerModel(a), planner.Request{Epic: d.Text, Snapshot: snap, Previous: d.Tasks, Note: note}, a.Cfg.Concurrency)
		if err != nil {
			return err
		}
		d.Tasks = dr.Tasks
		return planner.WriteDoc(path, d, a.Cfg.Serial, a.Cfg.Concurrency)
	}
}

// plannerModel picks the planner model as `saddle plan` does: the Messages
// API with ANTHROPIC_API_KEY, else the claude CLI on the subscription.
func plannerModel(a *app.App) planner.Model {
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return &planner.Anthropic{APIKey: key}
	}
	return &planner.ClaudeCLI{Cmd: a.Cfg.Claude.Cmd, Model: a.Cfg.Claude.Model}
}

// planGo hands an approved plan to the orchestrator to run.
func (m *model) planGo() tea.Cmd {
	d, path := m.pl.doc, m.pl.cur
	if d == nil {
		return flashCmd("no plan to run")
	}
	if !d.Approved {
		return flashCmd("approve the plan first: a")
	}
	if m.pl.goPlan != nil {
		m.pl.goPlan(path, *d)
		return nil
	}
	if m.proc == nil {
		return flashCmd("the orchestrator isn't running; ctrl+r restarts it")
	}
	var waves []string
	for i, w := range m.pl.check.Waves {
		waves = append(waves, fmt.Sprintf("wave %d: %s", i+1, strings.Join(w, ", ")))
	}
	m.sendUser(fmt.Sprintf("Run the approved plan %s (frozen at %s). Spawn its tasks with the claims, models and done_when checks the plan lists, "+
		"one wave at a time, starting each wave when the one before has landed. %s.", path, short(d.Base), strings.Join(waves, "; ")))
	m.setView(viewControl)
	return flashCmd("plan handed to the orchestrator")
}

// viewPlan renders the plan under review: the epic on the left on a wide
// screen, then the waves as columns of cards, the checker's findings and
// the estimate.
func (m *model) viewPlan(w, h int) string {
	inner := w - 2
	wrap := lipgloss.NewStyle().Width(inner)
	if m.pl.cur == "" {
		body := sDim.Render("No plan under review. Plan an epic with ") + sKey.Render("saddle plan <epic.md|gh:#N>") +
			sDim.Render(" or ask the orchestrator to; plans live in .saddle/plans.")
		return box("PLAN", w, h, false, wrap.Render(body))
	}
	title := "PLAN · " + filepath.Base(m.pl.cur)
	if len(m.pl.paths) > 1 {
		title += fmt.Sprintf(" (%d/%d)", slices.Index(m.pl.paths, m.pl.cur)+1, len(m.pl.paths))
	}
	var rows []string
	if m.pl.noting {
		rows = append(rows, sKey.Render("replan: ")+m.pl.note.View(), "")
	}
	d := m.pl.doc
	if d == nil {
		msg := "loading…"
		if m.pl.loadErr != nil {
			msg = m.pl.loadErr.Error()
		}
		rows = append(rows, wrap.Render(lipgloss.NewStyle().Foreground(cAlert).Render(msg)), "",
			wrap.Render(sKey.Render(m.keys.EditPlan.Help().Key)+sDim.Render(" edit to fix it")))
		return box(title, w, h, false, clip(strings.Join(rows, "\n"), inner))
	}

	state := lipgloss.NewStyle().Foreground(cAccent).Render("draft")
	if d.Approved {
		state = lipgloss.NewStyle().Foreground(cDone).Render("approved at " + short(d.Base))
	}
	rows = append(rows, wrap.Render(sBright.Render(d.Epic)+sDim.Render(" · ")+state), wrap.Render(m.planEstimate()), "")

	right := inner
	var left string
	if inner >= 100 {
		srcW := min(inner/3, 44)
		right = inner - srcW - 1
		left = lipgloss.NewStyle().Width(srcW).Render(sSection.Render("EPIC") + "\n" + renderMarkdown(d.Text, srcW, sText))
	}
	main := append(m.planColumns(right), "")
	main = append(main, m.planChecks(right)...)
	body := strings.Join(main, "\n")
	if left != "" {
		body = lipgloss.JoinHorizontal(lipgloss.Top, left, " ", lipgloss.NewStyle().Width(right).Render(body))
	}
	rows = append(rows, body)
	return box(title, w, h, false, clip(strings.Join(rows, "\n"), inner))
}

// planEstimate is the one-line size of the plan: tasks, waves, how wide the
// widest wave runs against the limit, train routes, barriers and models.
func (m *model) planEstimate() string {
	d, p := m.pl.doc, m.pl.check
	parts := []string{plural(len(d.Tasks), "task")}
	if m.pl.chkErr == nil {
		widest := 0
		for _, w := range p.Waves {
			widest = max(widest, len(w))
		}
		parts = append(parts, plural(len(p.Waves), "wave"), fmt.Sprintf("≤%d at once of %d", widest, m.app.Cfg.Concurrency))
		if n := len(p.Train); n > 0 {
			parts = append(parts, fmt.Sprintf("%d via train", n))
		}
		if n := len(p.Barriers); n > 0 {
			parts = append(parts, plural(n, "barrier"))
		}
	}
	counts := map[string]int{}
	for _, t := range d.Tasks {
		counts[cmpOr(t.Model, "default")]++
	}
	var models, rest []string
	for _, name := range append(slices.Clone(planModels[1:]), "default") {
		if counts[name] > 0 {
			models = append(models, fmt.Sprintf("%s %d", name, counts[name]))
			delete(counts, name)
		}
	}
	for name, n := range counts {
		rest = append(rest, fmt.Sprintf("%s %d", name, n))
	}
	slices.Sort(rest)
	models = append(models, rest...)
	return sDim.Render(strings.Join(parts, " · ") + " · " + strings.Join(models, ", "))
}

// plural is "1 task", "2 tasks".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// minCardW is the narrowest card worth drawing as a column.
const minCardW = 18

// planColumns lays the waves out side by side when each column gets at
// least minCardW, else one under another.
func (m *model) planColumns(w int) []string {
	d := m.pl.doc
	waves := m.planWaves()
	byID := map[string]planner.Task{}
	for _, t := range d.Tasks {
		byID[t.ID] = t
	}
	barrier := map[string]bool{}
	for _, f := range m.pl.check.Barriers {
		barrier[f.Task] = true
	}
	train := map[string]bool{}
	for _, f := range m.pl.check.Train {
		train[f.Task] = true
	}
	sel, _ := m.planSelected()
	head := func(i int) string {
		if m.pl.chkErr != nil {
			return lipgloss.NewStyle().Foreground(cAlert).Render("unchecked")
		}
		return sSection.Render(fmt.Sprintf("wave %d", i+1))
	}
	card := func(id string, cw int, compact bool) string {
		t := byID[id]
		bc := cBorder
		border := lipgloss.RoundedBorder()
		if barrier[id] {
			bc, border = cAlert, lipgloss.ThickBorder()
		}
		if id == sel {
			bc = cAccent
		}
		top := sBright.Render(id) + " " + lipgloss.NewStyle().Foreground(modelColor(t.Model)).Render(cmpOr(t.Model, "default"))
		if train[id] {
			top += sDim.Render(" ◆")
		}
		lines := []string{top}
		if compact {
			lines[0] += " " + sText.Render(t.Title)
		} else {
			lines = append(lines, sText.Render(t.Title), sDim.Render(plural(len(t.Claims), "claim")))
		}
		body := clip(lipgloss.NewStyle().Width(cw-2).Render(strings.Join(lines, "\n")), cw-2)
		if compact {
			body = clip(strings.Join(lines, "\n"), cw-2)
		}
		return lipgloss.NewStyle().Border(border).BorderForeground(bc).Width(cw - 2).Render(body)
	}
	if n := len(waves); n > 0 && w/n >= minCardW {
		cw := w / n
		cols := make([]string, n)
		for i, wave := range waves {
			cards := []string{head(i)}
			for _, id := range wave {
				cards = append(cards, card(id, cw-1, false))
			}
			cols[i] = lipgloss.NewStyle().Width(cw).Render(strings.Join(cards, "\n"))
		}
		return []string{lipgloss.JoinHorizontal(lipgloss.Top, cols...)}
	}
	var rows []string
	for i, wave := range waves {
		rows = append(rows, head(i))
		for _, id := range wave {
			rows = append(rows, card(id, w, true))
		}
	}
	return rows
}

// planChecks lists the checker's findings: claim overlaps it ordered,
// serial writes it routed through the train and barriers, or why it failed.
func (m *model) planChecks(w int) []string {
	wrap := lipgloss.NewStyle().Width(w)
	alert := lipgloss.NewStyle().Foreground(cAlert)
	rows := []string{sSection.Render("CHECKS")}
	if err := m.pl.chkErr; err != nil {
		return append(rows, wrap.Render(alert.Render("✗ "+err.Error())))
	}
	p := m.pl.check
	n := 0
	for _, e := range p.Edges {
		if strings.HasPrefix(e.Reason, "claims overlap") {
			rows = append(rows, wrap.Render(lipgloss.NewStyle().Foreground(cAccent).Render("overlap ")+sText.Render(strings.TrimPrefix(e.Reason, "claims overlap: "))))
			n++
		}
	}
	for _, f := range p.Train {
		rows = append(rows, wrap.Render(lipgloss.NewStyle().Foreground(cAccent).Render("serial ")+sText.Render(f.Task)+sDim.Render(" "+strings.TrimPrefix(f.Reason, "serial: "))))
		n++
	}
	for _, f := range p.Barriers {
		rows = append(rows, wrap.Render(alert.Render("barrier ")+sText.Render(f.Task)+sDim.Render(" "+strings.TrimPrefix(f.Reason, "barrier: "))))
		n++
	}
	if n == 0 {
		rows = append(rows, lipgloss.NewStyle().Foreground(cDone).Render("✓ no overlaps, serial writes or barriers"))
	}
	return rows
}
