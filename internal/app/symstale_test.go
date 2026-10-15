package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
)

// staleBase is pkg/f.go at the task's base. Foo's body is line 4, Adj is
// line 6 (two lines below it, a different symbol), Bar is far below both.
const staleBase = "package pkg\n\nfunc Foo() int {\n\treturn 1\n}\nfunc Adj() int { return 2 }\n\n" +
	"func M1() {}\n\nfunc M2() {}\n\nfunc M3() {}\n\nfunc M4() {}\n\n" +
	"func Bar() int {\n\treturn 3\n}\n"

const staleSubject = "feat: Foo returns ten (#42)"

// staleSetup cuts a task on staleBase, then lands a change to Foo's body on
// integration, so the task's base is behind integration in pkg/f.go (and in
// README.md). The rule runs with a generous budget so a slow machine does
// not turn a deny into a fallback.
func staleSetup(t *testing.T) (*App, store.Task) {
	t.Helper()
	withStaleBudget(t, time.Minute)
	a, _ := setup(t)
	a.Cfg.Claims.Granularity = config.GranularitySymbol
	write(t, a.Root, "pkg/f.go", staleBase)
	commitAll(t, a.Root, "add pkg")
	must(t, a.Init())
	must(t, a.ensureIntegration())
	tk, err := a.Spawn(SpawnReq{Title: "b"})
	must(t, err)
	commitOnIntegration(t, a, "docs: readme", map[string]string{"README.md": "landed\n"})
	commitOnIntegration(t, a, staleSubject, map[string]string{
		"pkg/f.go": strings.Replace(staleBase, "return 1", "return 10", 1),
	})
	return a, tk
}

func withStaleBudget(t *testing.T, d time.Duration) {
	t.Helper()
	t.Cleanup(SetStaleBudget(d))
}

// staleEdit is task tk replacing old with new once in rel, as the hook
// would compute it from the file on disk.
func staleEdit(t *testing.T, tk store.Task, rel, old, new string) Edit {
	t.Helper()
	before := readFile(t, filepath.Join(tk.Worktree, rel))
	if !strings.Contains(before, old) {
		t.Fatalf("%s has no %q", rel, old)
	}
	return Edit{Task: tk.ID, Abs: filepath.Join(tk.Worktree, rel), Before: []byte(before),
		After: []byte(strings.Replace(before, old, new, 1)), Known: true}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	must(t, err)
	return string(b)
}

// taskEvents returns the data of task's events of kind.
func taskEvents(t *testing.T, a *App, task, kind string) []string {
	t.Helper()
	evs, err := a.Store.Events(1000)
	must(t, err)
	var out []string
	for _, e := range evs {
		if e.Task == task && e.Kind == kind {
			out = append(out, e.Data)
		}
	}
	return out
}

// #590: integration changed Foo after B's base. B editing Foo is denied,
// naming Foo, the landed commit and saddle sync; B editing Bar, far from
// it, is allowed.
func TestStaleOverwriteDeniesChangedSymbol(t *testing.T) {
	a, tk := staleSetup(t)
	d := a.CheckEdit(staleEdit(t, tk, "pkg/f.go", "return 1", "return 100"))
	if d.Allow {
		t.Fatalf("edit of Foo allowed: %+v", d)
	}
	for _, want := range []string{"`Foo`", "pkg/f.go", staleSubject, "saddle sync"} {
		if !strings.Contains(d.Reason, want) {
			t.Errorf("reason %q lacks %q", d.Reason, want)
		}
	}
	if d.Owner != "" {
		t.Errorf("Owner = %q, want empty: no symbol rule may override a stale deny", d.Owner)
	}
	if ev := taskEvents(t, a, tk.ID, "symbol_deny"); len(ev) != 1 || !strings.Contains(ev[0], "stale-overwrite") {
		t.Errorf("symbol_deny events = %q", ev)
	}
	if d := a.CheckEdit(staleEdit(t, tk, "pkg/f.go", "return 3", "return 30")); !d.Allow {
		t.Fatalf("edit of Bar denied: %+v", d)
	}
}

// #590: an edit in a different symbol within the margin of integration's
// change still conflicts textually, so it is denied too.
func TestStaleOverwriteDeniesAdjacentLine(t *testing.T) {
	a, tk := staleSetup(t)
	d := a.CheckEdit(staleEdit(t, tk, "pkg/f.go", "return 2", "return 20"))
	if d.Allow {
		t.Fatalf("edit two lines below integration's change allowed: %+v", d)
	}
	if !strings.Contains(d.Reason, "pkg/f.go") || !strings.Contains(d.Reason, "saddle sync") {
		t.Errorf("reason = %q", d.Reason)
	}
}

// #590: a task cut after the landing has nothing stale, and file mode never
// runs the rule.
func TestStaleOverwriteAllowsCurrentBaseAndFileMode(t *testing.T) {
	a, _ := staleSetup(t)
	fresh, err := a.Spawn(SpawnReq{Title: "fresh"})
	must(t, err)
	if d := a.CheckEdit(staleEdit(t, fresh, "pkg/f.go", "return 10", "return 100")); !d.Allow {
		t.Fatalf("task on the current integration denied: %+v", d)
	}
	if ev := taskEvents(t, a, fresh.ID, "symbol_deny"); len(ev) != 0 {
		t.Errorf("symbol_deny events = %q", ev)
	}

	a, tk := staleSetup(t)
	a.Cfg.Claims.Granularity = config.GranularityFile
	if d := a.CheckEdit(staleEdit(t, tk, "pkg/f.go", "return 1", "return 100")); !d.Allow {
		t.Fatalf("file mode denied: %+v", d)
	}
	if ev := taskEvents(t, a, tk.ID, "symbol_deny"); len(ev) != 0 {
		t.Errorf("symbol_deny events = %q", ev)
	}
}

// #590: an unsupported file is left to the train as today, even when
// integration changed the same line.
func TestStaleOverwriteSkipsUnsupportedFile(t *testing.T) {
	a, tk := staleSetup(t)
	if d := a.CheckEdit(staleEdit(t, tk, "README.md", "hi", "bye")); !d.Allow {
		t.Fatalf("README.md edit denied: %+v", d)
	}
	if ev := taskEvents(t, a, tk.ID, "symbol_fallback"); len(ev) != 0 {
		t.Errorf("symbol_fallback events = %q for an unsupported file", ev)
	}
}

// #590: the rule falls back to today's verdict (allow), logging why, when it
// can't decide: unknown proposed content, a parse error in a changed range,
// or the budget running out.
func TestStaleOverwriteFallsBack(t *testing.T) {
	a, tk := staleSetup(t)

	unknown := staleEdit(t, tk, "pkg/f.go", "return 1", "return 100")
	unknown.After, unknown.Known = nil, false
	if d := a.CheckEdit(unknown); !d.Allow {
		t.Fatalf("unknown proposed content denied: %+v", d)
	}
	if d := a.CheckEdit(staleEdit(t, tk, "pkg/f.go", "return 1\n", "return 1 +\n")); !d.Allow {
		t.Fatalf("half-written Foo denied: %+v", d)
	}
	withStaleBudget(t, time.Nanosecond)
	if d := a.CheckEdit(staleEdit(t, tk, "pkg/f.go", "return 1", "return 100")); !d.Allow {
		t.Fatalf("over budget denied: %+v", d)
	}

	// Rule 3 logs its own symbol_fallback on the same edits. This test is
	// about rule 2's reasons, in order.
	var ev []string
	for _, e := range taskEvents(t, a, tk.ID, "symbol_fallback") {
		if strings.Contains(e, "stale-overwrite") {
			ev = append(ev, e)
		}
	}
	want := []string{"unknown proposed content", "parse error", "budget"}
	if len(ev) != len(want) {
		t.Fatalf("symbol_fallback events = %q, want %d", ev, len(want))
	}
	for i, w := range want {
		if !strings.Contains(ev[i], "stale-overwrite") || !strings.Contains(ev[i], w) {
			t.Errorf("event %d = %q, want stale-overwrite and %q", i, ev[i], w)
		}
	}
}

// #590: a stale edit to a file another task claims is still a deny, and it
// drops the owner so rule 1 can't turn it into a symbol-level allow.
func TestStaleOverwriteBeatsClaimDeny(t *testing.T) {
	a, tk := staleSetup(t)
	owner, err := a.Spawn(SpawnReq{Title: "owner", Claims: []string{"pkg/f.go"}})
	must(t, err)
	d := a.CheckEdit(staleEdit(t, tk, "pkg/f.go", "return 1", "return 100"))
	if d.Allow || d.Owner != "" || !strings.Contains(d.Reason, "`Foo`") || !strings.Contains(d.Reason, owner.ID) {
		t.Fatalf("decision = %+v, want a stale deny without Owner that still names %s", d, owner.ID)
	}
}
