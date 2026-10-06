package tui

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
)

// fakeTrainer records what the train keys asked for.
type fakeTrainer struct {
	calls []string
	err   error
}

func (f *fakeTrainer) MoveInQueue(task string, pos int) error {
	f.calls = append(f.calls, "move "+task+" "+string(rune('0'+pos)))
	return f.err
}

func (f *fakeTrainer) Hold(task, reason string) error {
	f.calls = append(f.calls, "hold "+task)
	return f.err
}

func (f *fakeTrainer) Unhold(task string) error {
	f.calls = append(f.calls, "release "+task)
	return f.err
}

// trainSnap is a train with every state: two queued, one held, one returned
// on a conflict with a hunk left, one failing tests, one escalated, one
// landed that moved a file.
func trainSnap() trainLoadedMsg {
	return trainLoadedMsg{
		entries: []store.TrainEntry{
			{Task: "t4", State: store.TrainOK, Note: "aaa..bbb"},
			{Task: "t2", State: store.Queued},
			{Task: "t5", State: store.OnHold, Note: "waiting on review"},
			{Task: "t6", State: store.Queued},
			{Task: "t3", State: store.TrainError, Note: "go.mod", Attempts: 1},
			{Task: "t7", State: store.TestFailed, Note: "tests failed", Attempts: 2},
			{Task: "t8", State: "escalated", Note: "go.mod (conflict)", Attempts: 3},
		},
		renames: []renameRow{{task: "t4", old: "internal/old/a.go", new: "internal/new/a.go"}},
		hunks:   map[string]string{"t3": "go.mod\n<<<<<<< HEAD\ngo 1.24\n=======\ngo 1.25\n>>>>>>> t3"},
	}
}

func newTrainModel(w, h int) (*model, *fakeTrainer) {
	m := newViewModel(w, h)
	m.am = twoStacks()
	f := &fakeTrainer{}
	m.tr.ops = f
	m.tr.load = func() trainLoadedMsg { return trainSnap() }
	m.view = viewMerge
	m.Update(trainSnap())
	return m, f
}

// flat is a screen's text with borders dropped and wrapped lines joined, so
// a phrase wrapped at a narrow width still matches.
func flat(screen string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(screen, "│", " ")), " ")
}

// The merge view shows the queue with every state, counters, the rename map
// and the selected returned entry's resolution steps and remaining hunk,
// below the stacks it already had, at 36 and 120 columns.
func TestMergeViewShowsTrainStatesRenamesConflict(t *testing.T) {
	for _, w := range []int{36, 120} {
		m, _ := newTrainModel(w, 70)
		m.tr.focus = true
		m.tr.sel = "t3"
		out := m.View()
		checkScreen(t, "merge", out, w, 70)
		text := flat(out)
		for _, want := range []string{"stack t5", "2 queued", "1 held", "2 returned", "1 escalated", "1 landed",
			"1.", "t2", "on hold", "waiting on review", "t3", "conflict", "test failed", "escalated",
			"Renames", "internal/new/a.go", "saddle sync", "rebase --continue", "go 1.25", ">>>>>>>"} {
			if !strings.Contains(text, want) {
				t.Errorf("width %d: merge view lacks %q:\n%s", w, want, out)
			}
		}
	}
	// An escalated entry says what the owner can do.
	m, _ := newTrainModel(120, 70)
	m.tr.focus, m.tr.sel = true, "t8"
	if out := m.View(); !strings.Contains(out, "take over") || !strings.Contains(out, "3 failed attempts") {
		t.Errorf("escalated detail:\n%s", out)
	}
}

// tab moves the keys from the stacks to the train; there j/k select an
// entry, J/K reorder the queue, h holds or releases, through the same App
// calls as `saddle queue move|hold|release`, each in a command.
func TestTrainKeysReorderHold(t *testing.T) {
	m, f := newTrainModel(120, 60)
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if !m.tr.focus {
		t.Fatal("tab should focus the train")
	}
	if m.trainSelected().Task != "t2" {
		t.Fatalf("first selection = %q, want t2", m.trainSelected().Task)
	}
	_, c := m.Update(runeKey('J'))
	if c == nil || len(f.calls) > 0 {
		t.Fatalf("J should return a command and call nothing yet: %q", f.calls)
	}
	drain(m, c)
	press(m, runeKey('j')) // t5, on hold
	press(m, runeKey('h')) // release
	press(m, runeKey('j')) // t6
	press(m, runeKey('K')) // up to 2
	press(m, runeKey('h')) // hold
	press(m, runeKey('j')) // t3, returned: can't move or hold
	press(m, runeKey('K'))
	press(m, runeKey('h'))
	want := []string{"move t2 2", "release t5", "move t6 2", "hold t6"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
	if !strings.Contains(m.flash, "waiting") {
		t.Errorf("a returned entry should say it isn't waiting, flash %q", m.flash)
	}
	// The top entry doesn't move up.
	f.calls = nil
	m.tr.sel = "t2"
	press(m, runeKey('K'))
	if len(f.calls) > 0 {
		t.Errorf("K on the first entry called %q", f.calls)
	}

	f.err = errors.New("t6 isn't waiting in the merge train")
	m.tr.sel = "t6"
	press(m, runeKey('h'))
	if !strings.Contains(m.flash, "isn't waiting") {
		t.Errorf("error not flashed: %q", m.flash)
	}

	// Back on the stacks, h holds the stack again.
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.tr.focus {
		t.Fatal("tab again should focus the stacks")
	}
	am := &fakeAutomerger{}
	m.amer = am
	runKey(m, runeKey('h'))
	if len(am.calls) != 1 {
		t.Errorf("stack hold: %q", am.calls)
	}
}

// t takes over the selected entry: it opens the task's agent window, or
// says where its worktree is when the agent is gone.
func TestTrainTakeOver(t *testing.T) {
	m, _ := newTrainModel(120, 60)
	var opened string
	m.tr.attach = func(tv mcpserver.TaskView) tea.Cmd { opened = tv.ID; return nil }
	m.tasks = append(m.tasks, mcpserver.TaskView{ID: "t8", Title: "escalated work", Status: store.NeedsYou, Worktree: "/wt/t8"})
	m.tr.focus = true
	m.tr.sel = "t3"
	press(m, runeKey('t'))
	if opened != "t3" {
		t.Errorf("take over t3 opened %q", opened)
	}
	opened = ""
	m.tr.sel = "t8"
	press(m, runeKey('t'))
	if opened != "" || !strings.Contains(m.flash, "/wt/t8") {
		t.Errorf("no window: opened %q, flash %q", opened, m.flash)
	}
}

// The train loads off the UI goroutine when the merge view opens.
func TestMergeViewLoadsTrainInCommand(t *testing.T) {
	m := newViewModel(120, 40)
	m.am = twoStacks()
	calls := 0
	m.tr.load = func() trainLoadedMsg { calls++; return trainSnap() }
	_, c := m.Update(altKey('3'))
	if c == nil || calls != 0 {
		t.Fatalf("alt+3 should load the train in a command (calls %d)", calls)
	}
	drain(m, c)
	if calls != 1 || len(m.tr.entries) != 7 {
		t.Errorf("calls %d, entries %d", calls, len(m.tr.entries))
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// landedRenames reads the files each landed entry moved from its landing
// range, and caches them: a range never changes.
func TestLandedRenames(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "t@t")
	git(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-qm", "one")
	old := git(t, dir, "rev-parse", "HEAD")
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "mv", "a.go", "pkg/a.go")
	git(t, dir, "commit", "-qm", "two")
	head := git(t, dir, "rev-parse", "HEAD")

	es := []store.TrainEntry{{Task: "t4", State: store.TrainOK, Note: old + ".." + head}, {Task: "t2", State: store.Queued}}
	cache := map[string][]renameRow{}
	rows := landedRenames(dir, es, cache)
	if len(rows) != 1 || rows[0].task != "t4" || rows[0].old != "a.go" || rows[0].new != "pkg/a.go" {
		t.Fatalf("rows = %+v", rows)
	}
	if _, ok := cache[old+".."+head]; !ok {
		t.Error("range not cached")
	}
	if rows := landedRenames(t.TempDir(), es, cache); len(rows) != 1 {
		t.Errorf("a cached range should not hit git again: %+v", rows)
	}
}

// conflictHunk finds the first conflict still marked in the named files of a
// worktree; resolved files have none.
func TestConflictHunk(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module x\n\n<<<<<<< HEAD\ngo 1.24\n=======\ngo 1.25\n>>>>>>> t3 (bump)\n\nrequire y v1\n")
	write("clean.go", "package x\n")
	got := conflictHunk(dir, []string{"clean.go", "missing.go", "go.mod"})
	for _, want := range []string{"go.mod", "<<<<<<< HEAD", "go 1.24", "=======", "go 1.25", ">>>>>>> t3"} {
		if !strings.Contains(got, want) {
			t.Errorf("hunk lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "require") {
		t.Errorf("hunk runs past its end:\n%s", got)
	}
	if got := conflictHunk(dir, []string{"clean.go"}); got != "" {
		t.Errorf("resolved file has a hunk: %q", got)
	}
}

// A held entry is waiting, not stuck: the header counts it as queued, not as
// a conflict.
func TestHeaderCountsHeldAsQueued(t *testing.T) {
	m := newViewModel(160, 30)
	m.tasks = []mcpserver.TaskView{
		{ID: "t2", Title: "a", Status: store.Done, Train: "queued"},
		{ID: "t5", Title: "b", Status: store.Done, Train: "on_hold: waiting on review"},
	}
	h := m.viewHeader()
	if !strings.Contains(h, "train 2 queued") || strings.Contains(h, "conflict") {
		t.Errorf("header: %q", h)
	}
}

// A merged or superseded entry has left the stack for good (#206): the
// header counts neither as a conflict, and the merge view lists merged as
// landed and superseded with the settled entries, never as returned.
func TestMergedAndSupersededAreNotReturned(t *testing.T) {
	m := newViewModel(160, 30)
	m.tasks = []mcpserver.TaskView{
		{ID: "t1", Title: "a", Status: store.Landed, Train: "merged: aaa..bbb"},
		{ID: "t2", Title: "b", Status: store.Landed, Train: "superseded: ccc..ddd"},
		{ID: "t3", Title: "c", Status: store.Done, Train: "queued"},
	}
	if h := m.viewHeader(); !strings.Contains(h, "train 1 queued") || strings.Contains(h, "conflict") {
		t.Errorf("header: %q", h)
	}
	m.view = viewMerge
	m.Update(trainLoadedMsg{entries: []store.TrainEntry{
		{Task: "t1", State: app.TrainMerged, Note: "aaa..bbb"},
		{Task: "t2", State: app.TrainSuperseded, Note: "ccc..ddd"},
		{Task: "t3", State: store.Queued},
	}})
	q, back, landed := m.trainSections()
	if len(q) != 1 || len(back) != 0 || len(landed) != 2 {
		t.Errorf("sections: queue %v back %v landed %v", q, back, landed)
	}
	text := flat(strings.Join(m.viewTrain(120), "\n"))
	if strings.Contains(text, "returned") || !strings.Contains(text, "1 landed") {
		t.Errorf("train panel:\n%s", text)
	}
}
