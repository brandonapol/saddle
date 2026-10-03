package tui

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/planner"
)

// planTasks has an overlap (t2 after t1), a barrier (t3) and a serial write
// (t4).
func planTasks() []planner.Task {
	return []planner.Task{
		{ID: "t1", Title: "store schema", Plan: "add the table", Claims: []string{"internal/store/**"}, DoneWhen: []string{"go test ./internal/store"}},
		{ID: "t2", Title: "store queries", Plan: "query it", Claims: []string{"internal/store/query.go"}, DoneWhen: []string{"tests pass"}, Model: "sonnet"},
		{ID: "t3", Title: "split the cli package", Plan: "restructure internal/cli", Claims: []string{"internal/cli/**"}, DoneWhen: []string{"builds"}, Barrier: true},
		{ID: "t4", Title: "bump deps", Plan: "update modules", Claims: []string{"go.mod"}, DoneWhen: []string{"go build ./..."}},
	}
}

// newPlanModel is a view model reviewing one plan file in a temp plans dir.
func newPlanModel(t *testing.T, w, h int) (*model, string) {
	t.Helper()
	m := newViewModel(w, h)
	m.app.Cfg.Serial = []string{"go.mod", "go.sum"}
	m.app.Cfg.Concurrency = 4
	dir := t.TempDir()
	path := filepath.Join(dir, "store-epic.toml")
	doc := planner.Doc{Epic: "Store epic", Source: "epic.md", Text: "# Store epic\n\nAdd a table and query it.", Tasks: planTasks()}
	if err := planner.WriteDoc(path, doc, m.app.Cfg.Serial, m.app.Cfg.Concurrency); err != nil {
		t.Fatal(err)
	}
	m.pl.dir = dir
	m.pl.base = func() (string, error) { return "abc123", nil }
	return m, path
}

// drain runs c and every command its messages lead to, as the program
// would, so an action and the reload it triggers both land.
func drain(m *model, c tea.Cmd) {
	for queue := []tea.Cmd{c}; len(queue) > 0; queue = queue[1:] {
		if queue[0] == nil {
			continue
		}
		switch msg := queue[0]().(type) {
		case nil:
		case tea.BatchMsg:
			queue = append(queue, msg...)
		default:
			_, next := m.Update(msg)
			queue = append(queue, next)
		}
	}
}

// press presses k and drains the commands it starts.
func press(m *model, k tea.KeyMsg) {
	_, c := m.Update(k)
	drain(m, c)
}

// openPlan switches to the plan view and runs the load it starts.
func openPlan(t *testing.T, m *model) {
	t.Helper()
	_, c := m.Update(altKey('2'))
	if c == nil {
		t.Fatal("opening the plan view should load the plan in a command")
	}
	drain(m, c)
	if m.pl.doc == nil {
		t.Fatalf("plan not loaded: %v", m.pl.loadErr)
	}
}

// The plan view shows the epic, waves as columns of cards with the barrier
// outlined, the checker's overlaps, serial routes and barriers, and an
// estimate line, at a narrow split and a wide terminal.
func TestPlanViewShowsWavesChecksEstimate(t *testing.T) {
	for _, w := range []int{36, 120} {
		m, _ := newPlanModel(t, w, 50)
		openPlan(t, m)
		out := m.View()
		checkScreen(t, "plan", out, w, 50)
		for _, want := range []string{"PLAN", "Store epic", "wave 1", "wave 2", "t1", "t2", "t3", "t4", "overlap", "serial", "barrier", "4 tasks"} {
			if !strings.Contains(out, want) {
				t.Errorf("width %d: plan view lacks %q:\n%s", w, want, out)
			}
		}
		if w == 120 {
			for _, want := range []string{"store queries", "Add a table and query it", "draft", "sonnet", "via train"} {
				if !strings.Contains(out, want) {
					t.Errorf("width %d: plan view lacks %q:\n%s", w, want, out)
				}
			}
		}
	}
}

// With no plan file the view says how to make one; a broken plan shows why.
func TestPlanViewEmptyAndBroken(t *testing.T) {
	m := newViewModel(120, 30)
	m.pl.dir = t.TempDir()
	press(m, altKey('2'))
	if out := m.View(); !strings.Contains(out, "saddle plan") {
		t.Errorf("empty plan view should say how to plan:\n%s", out)
	}

	m, path := newPlanModel(t, 120, 30)
	if err := os.WriteFile(path, []byte("epic = \"x\"\nbogus = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	press(m, altKey('2'))
	if out := m.View(); !strings.Contains(out, "unknown keys") || !strings.Contains(out, "e edit") {
		t.Errorf("broken plan should show the error and the edit key:\n%s", out)
	}
}

// a approves and freezes the plan at the base commit (a again reopens it),
// + and - cycle the selected task's model, refused while approved.
func TestPlanKeysApproveAndModel(t *testing.T) {
	m, path := newPlanModel(t, 120, 50)
	openPlan(t, m)

	press(m, runeKey('+'))
	d, err := planner.LoadDoc(path)
	if err != nil {
		t.Fatal(err)
	}
	if d.Tasks[0].Model != "haiku" {
		t.Errorf("+ on t1: model = %q, want haiku", d.Tasks[0].Model)
	}
	press(m, runeKey('j')) // t2, sonnet
	press(m, runeKey('-'))
	d, _ = planner.LoadDoc(path)
	if d.Tasks[1].Model != "haiku" {
		t.Errorf("- on t2: model = %q, want haiku", d.Tasks[1].Model)
	}

	press(m, runeKey('a'))
	d, _ = planner.LoadDoc(path)
	if !d.Approved || d.Base != "abc123" {
		t.Fatalf("a should approve at the base: %+v", d)
	}
	if !strings.Contains(m.View(), "approved") {
		t.Errorf("view should say the plan is approved")
	}
	press(m, runeKey('+'))
	if !strings.Contains(m.flash, "reopen") {
		t.Errorf("editing an approved plan should say to reopen, flash %q", m.flash)
	}
	press(m, runeKey('a'))
	d, _ = planner.LoadDoc(path)
	if d.Approved {
		t.Error("a on an approved plan should reopen it")
	}

	// A plan the checker rejects isn't approved.
	m.pl.base = func() (string, error) { return "", errors.New("no base") }
	press(m, runeKey('a'))
	if !strings.Contains(m.flash, "no base") {
		t.Errorf("approve error should flash, got %q", m.flash)
	}
}

// e hands the terminal to $EDITOR; when it exits the view re-runs the checker
// through planner.Edit, so a good edit gets a fresh header and a bad one
// shows the error.
func TestPlanEditRechecks(t *testing.T) {
	m, path := newPlanModel(t, 120, 50)
	var edited string
	m.pl.editor = func(p string) tea.Cmd {
		edited = p
		return func() tea.Msg { return planEditedMsg{path: p} }
	}
	openPlan(t, m)

	// Simulate the editor dropping t4.
	d, _ := planner.LoadDoc(path)
	d.Tasks = d.Tasks[:3]
	b := planner.Render(d, nil, 0)
	b = []byte(strings.Replace(string(b), "# wave", "# stale wave", 1))
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	press(m, runeKey('e'))
	if edited != path {
		t.Fatalf("editor ran on %q, want %q", edited, path)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "stale wave") {
		t.Errorf("edit should re-render the check header:\n%s", raw)
	}
	if strings.Contains(m.View(), "bump deps") {
		t.Error("view should show the edited plan")
	}

	// A bad edit is kept and the error shown.
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), `id = "t2"`, `id = "t1"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	press(m, runeKey('e'))
	if out := m.View(); !strings.Contains(out, "t1") || !strings.Contains(m.flash+out, "duplicate") {
		t.Errorf("bad edit should show the error; flash %q:\n%s", m.flash, out)
	}

	// An approved plan doesn't open.
	m2, path2 := newPlanModel(t, 120, 50)
	m2.pl.editor = m.pl.editor
	edited = ""
	if _, err := planner.Approve(path2, "abc", m2.app.Cfg.Serial, m2.app.Cfg.Concurrency); err != nil {
		t.Fatal(err)
	}
	openPlan(t, m2)
	press(m2, runeKey('e'))
	if edited != "" || !strings.Contains(m2.flash, "reopen") {
		t.Errorf("approved plan opened in the editor (flash %q)", m2.flash)
	}
}

// r asks for a note and replans off the UI goroutine; g hands an approved
// plan to the orchestrator, and refuses a draft.
func TestPlanReplanAndGo(t *testing.T) {
	m, path := newPlanModel(t, 120, 50)
	var gotPath, gotNote string
	m.pl.replan = func(p, note string) error { gotPath, gotNote = p, note; return nil }
	var went string
	m.pl.goPlan = func(p string, _ planner.Doc) { went = p }
	openPlan(t, m)

	press(m, runeKey('g'))
	if went != "" || !strings.Contains(m.flash, "approve") {
		t.Errorf("g on a draft should ask to approve first; went %q flash %q", went, m.flash)
	}

	m.Update(runeKey('r'))
	if !m.pl.noting {
		t.Fatal("r should ask for a note")
	}
	if !strings.Contains(m.View(), "replan") {
		t.Error("the note prompt should show")
	}
	for _, r := range "merge t1 and t2" {
		m.Update(runeKey(r))
	}
	_, c := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if c == nil || gotNote != "" {
		t.Fatal("enter should replan in a command, not inline")
	}
	drain(m, c)
	if gotPath != path || gotNote != "merge t1 and t2" {
		t.Errorf("replan(%q, %q)", gotPath, gotNote)
	}
	if m.pl.noting {
		t.Error("the prompt should close")
	}

	// esc cancels a note.
	gotNote = ""
	m.Update(runeKey('r'))
	m.Update(runeKey('x'))
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.pl.noting || gotNote != "" {
		t.Error("esc should cancel the note")
	}

	if _, err := planner.Approve(path, "abc", m.app.Cfg.Serial, m.app.Cfg.Concurrency); err != nil {
		t.Fatal(err)
	}
	press(m, altKey('2')) // reload, as the tick would
	press(m, runeKey('g'))
	if went != path {
		t.Errorf("g on an approved plan: went %q", went)
	}
}

// #176: the plan view shows running bots against the limit, with or
// without a plan, at narrow and wide widths.
func TestPlanViewShowsBots(t *testing.T) {
	for _, w := range []int{36, 120} {
		m, _ := newPlanModel(t, w, 50)
		m.conc = &app.Concurrency{Limit: 4, Source: app.ConcurrencyRuntime, Config: 5}
		openPlan(t, m)
		out := m.View()
		checkScreen(t, "plan", out, w, 50)
		for _, want := range []string{"bots: 1/4", ">/<"} {
			if !strings.Contains(out, want) {
				t.Errorf("width %d: plan view lacks %q:\n%s", w, want, out)
			}
		}
		if w == 120 && !strings.Contains(out, "runtime") {
			t.Errorf("wide plan view should say the limit is a runtime override:\n%s", out)
		}

		e := newViewModel(w, 30)
		e.pl.dir = t.TempDir()
		press(e, altKey('2'))
		out = e.View()
		checkScreen(t, "empty plan", out, w, 30)
		if !strings.Contains(out, "bots: 1/5") { // config's 5 before a refresh reads the override
			t.Errorf("width %d: empty plan view lacks the bots line:\n%s", w, out)
		}
	}
}

// #176: > and < raise and lower the limit within [1, 16], off the UI
// goroutine; past the bounds they only say why.
func TestPlanKeysAdjustBots(t *testing.T) {
	m, _ := newPlanModel(t, 120, 40)
	var calls []int
	m.concSet = func(n int) (app.Concurrency, error) {
		calls = append(calls, n)
		return app.Concurrency{Limit: n, Running: 1, Source: app.ConcurrencyRuntime}, nil
	}
	m.conc = &app.Concurrency{Limit: 4, Running: 1}
	openPlan(t, m)
	press(m, runeKey('>'))
	press(m, runeKey('<'))
	press(m, runeKey('<'))
	if want := []int{5, 4, 3}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	if m.conc.Limit != 3 || !strings.Contains(m.flash, "3") {
		t.Fatalf("limit %d, flash %q", m.conc.Limit, m.flash)
	}

	calls = nil
	m.conc = &app.Concurrency{Limit: app.MaxConcurrency}
	press(m, runeKey('>'))
	m.conc = &app.Concurrency{Limit: app.MinConcurrency}
	press(m, runeKey('<'))
	if len(calls) > 0 {
		t.Fatalf("stepped past [1, 16]: %v", calls)
	}
	if !strings.Contains(m.flash, "at least 1") {
		t.Errorf("flash = %q, want it to name the bound", m.flash)
	}

	m.concSet = func(int) (app.Concurrency, error) { return app.Concurrency{}, errors.New("disk full") }
	m.conc = &app.Concurrency{Limit: 4}
	press(m, runeKey('>'))
	if !strings.Contains(m.flash, "disk full") || m.conc.Limit != 4 {
		t.Errorf("error: flash %q, limit %d", m.flash, m.conc.Limit)
	}
}

// A refresh carries the concurrency state into the model.
func TestRefreshCarriesConcurrency(t *testing.T) {
	m := newViewModel(120, 40)
	m.prev = map[string]string{}
	m.Update(refreshMsg{tasks: m.tasks, conc: &app.Concurrency{Limit: 7}})
	if m.conc == nil || m.conc.Limit != 7 {
		t.Fatalf("conc = %+v", m.conc)
	}
}
