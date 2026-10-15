package app

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
)

// shareBase is pkg/x.go at both tasks' base: Foo near the top, Bar far
// below it, Tail last.
const shareBase = "package pkg\n\nfunc Foo() int {\n\treturn 1\n}\n\n" +
	"func M1() {}\n\nfunc M2() {}\n\nfunc M3() {}\n\nfunc M4() {}\n\n" +
	"func Bar() int {\n\treturn 3\n}\n\nfunc Tail() {}\n"

// shareSetup makes owner A, which claims pkg/x.go, and B, which doesn't,
// both cut from the same base in symbol mode.
func shareSetup(t *testing.T, ownerAdapter string) (a *App, owner, b store.Task) {
	t.Helper()
	t.Cleanup(SetShareBudget(time.Minute))
	withStaleBudget(t, time.Minute)
	a, _ = setup(t)
	a.Cfg.Claims.Granularity = config.GranularitySymbol
	write(t, a.Root, "pkg/x.go", shareBase)
	commitAll(t, a.Root, "add pkg")
	must(t, a.Init())
	must(t, a.ensureIntegration())
	owner, err := a.Spawn(SpawnReq{Title: "owner", Claims: []string{"pkg/x.go"}, Adapter: ownerAdapter})
	must(t, err)
	b, err = a.Spawn(SpawnReq{Title: "b"})
	must(t, err)
	return a, owner, b
}

// onDisk applies edit e to its file, as the tool call would after an allow.
func onDisk(t *testing.T, e Edit) {
	t.Helper()
	write(t, filepath.Dir(e.Abs), filepath.Base(e.Abs), string(e.After))
}

func heldSymbols(t *testing.T, a *App, rel string) []string {
	t.Helper()
	rows, err := a.Store.SymbolClaims(rel)
	must(t, err)
	var out []string
	for _, r := range rows {
		out = append(out, r.Task+":"+r.Symbol)
	}
	return out
}

// #591: A claims x.go and changed Foo. B may edit Bar, far away, and then
// holds Bar; B editing Foo is denied, naming Foo and A, with A as the task
// whose hunk the hook quotes.
func TestSymbolShareAllowsDisjointDeniesSameSymbol(t *testing.T) {
	a, owner, b := shareSetup(t, "")
	onDisk(t, staleEdit(t, owner, "pkg/x.go", "return 1", "return 10"))

	bar := staleEdit(t, b, "pkg/x.go", "return 3", "return 30")
	if d := a.CheckEdit(bar); !d.Allow {
		t.Fatalf("B's edit of Bar denied: %+v", d)
	}
	onDisk(t, bar)
	if got, want := heldSymbols(t, a, "pkg/x.go"), []string{b.ID + ":", b.ID + ":Bar"}; !slices.Equal(got, want) {
		t.Fatalf("symbol claims = %q, want %q", got, want)
	}
	if ev := taskEvents(t, a, b.ID, "symbol_allow"); len(ev) != 1 || !strings.Contains(ev[0], owner.ID) {
		t.Errorf("symbol_allow events = %q", ev)
	}

	d := a.CheckEdit(staleEdit(t, b, "pkg/x.go", "return 1", "return 100"))
	if d.Allow {
		t.Fatalf("B's edit of Foo allowed: %+v", d)
	}
	if !strings.Contains(d.Reason, "`Foo`") || !strings.Contains(d.Reason, owner.ID) || d.Owner != owner.ID {
		t.Fatalf("deny = %+v, want Foo and owner %s", d, owner.ID)
	}
	if ev := taskEvents(t, a, b.ID, "symbol_deny"); len(ev) != 1 || !strings.Contains(ev[0], "Foo") {
		t.Errorf("symbol_deny events = %q", ev)
	}
}

// #591: two appends at the end of the file touch no shared symbol but
// conflict in git, so the range test denies; two new functions of one name
// conflict by name even far apart.
func TestSymbolShareDeniesAdjacentAppendsAndSameNewName(t *testing.T) {
	a, owner, b := shareSetup(t, "")
	onDisk(t, staleEdit(t, owner, "pkg/x.go", "func Tail() {}\n", "func Tail() {}\n\nfunc FromA() {}\n"))
	d := a.CheckEdit(staleEdit(t, b, "pkg/x.go", "func Tail() {}\n", "func Tail() {}\n\nfunc FromB() {}\n"))
	if d.Allow || !strings.Contains(d.Reason, "within 2 lines") {
		t.Fatalf("both appending at EOF: %+v, want a range deny", d)
	}

	a, owner, b = shareSetup(t, "")
	onDisk(t, staleEdit(t, owner, "pkg/x.go", "package pkg\n", "package pkg\n\nfunc New() {}\n"))
	d = a.CheckEdit(staleEdit(t, b, "pkg/x.go", "func Tail() {}\n", "func Tail() {}\n\nfunc New() {}\n"))
	if d.Allow || !strings.Contains(d.Reason, "`New`") {
		t.Fatalf("both adding New: %+v, want a deny naming New", d)
	}
}

// #591, Q1 (first writer wins): A has not touched x.go, so B may take Bar;
// then A, the path owner, is denied Bar with B named, but may still edit Foo.
func TestSymbolShareFirstWriterWins(t *testing.T) {
	a, owner, b := shareSetup(t, "")
	bar := staleEdit(t, b, "pkg/x.go", "return 3", "return 30")
	if d := a.CheckEdit(bar); !d.Allow {
		t.Fatalf("B's edit with an untouched owner denied: %+v", d)
	}
	onDisk(t, bar)

	d := a.CheckEdit(staleEdit(t, owner, "pkg/x.go", "return 3", "return 300"))
	if d.Allow || !strings.Contains(d.Reason, "`Bar`") || !strings.Contains(d.Reason, b.ID) || d.Owner != b.ID {
		t.Fatalf("owner's edit of B's Bar = %+v, want a deny naming Bar and %s", d, b.ID)
	}
	if d := a.CheckEdit(staleEdit(t, owner, "pkg/x.go", "return 1", "return 10")); !d.Allow {
		t.Fatalf("owner's edit of Foo denied: %+v", d)
	}
}

// #591: when the tasks' bases hold different versions of x.go the rule
// falls back to today's path-claim deny, and file mode never runs it.
func TestSymbolShareFallsBackToPathDeny(t *testing.T) {
	a, owner, _ := shareSetup(t, "")
	commitOnIntegration(t, a, "feat: Tail grows", map[string]string{
		"pkg/x.go": strings.Replace(shareBase, "func Tail() {}", "func Tail() { _ = 1 }", 1),
	})
	late, err := a.Spawn(SpawnReq{Title: "late"})
	must(t, err)
	d := a.CheckEdit(staleEdit(t, late, "pkg/x.go", "return 1", "return 10"))
	if d.Allow || d.Owner != owner.ID || !strings.Contains(d.Reason, "is claimed by") {
		t.Fatalf("different bases: %+v, want the path-claim deny", d)
	}
	if ev := taskEvents(t, a, late.ID, "symbol_fallback"); len(ev) != 1 || !strings.Contains(ev[0], "base of the file differs") {
		t.Errorf("symbol_fallback events = %q", ev)
	}

	a, owner, b := shareSetup(t, "")
	a.Cfg.Claims.Granularity = config.GranularityFile
	if d := a.CheckEdit(staleEdit(t, b, "pkg/x.go", "return 3", "return 30")); d.Allow || d.Owner != owner.ID {
		t.Fatalf("file mode: %+v, want the path-claim deny", d)
	}
	if got := heldSymbols(t, a, "pkg/x.go"); len(got) != 0 {
		t.Errorf("file mode wrote symbol claims %q", got)
	}
}

// #591: the rule falls back to the path-claim deny when the owner runs
// without hooks or the proposed content is unknown.
func TestSymbolShareFallsBackForHooklessOwnerAndUnknownEdit(t *testing.T) {
	a, owner, b := shareSetup(t, "codex")
	if d := a.CheckEdit(staleEdit(t, b, "pkg/x.go", "return 3", "return 30")); d.Allow || d.Owner != owner.ID {
		t.Fatalf("hookless owner: %+v, want the path-claim deny", d)
	}
	a, owner, b = shareSetup(t, "")
	unknown := staleEdit(t, b, "pkg/x.go", "return 3", "return 30")
	unknown.After, unknown.Known = nil, false
	if d := a.CheckEdit(unknown); d.Allow || d.Owner != owner.ID {
		t.Fatalf("unknown content: %+v, want the path-claim deny", d)
	}
	// Rule 3 logs its own fallback for the same unknown edit.
	var ev []string
	for _, e := range taskEvents(t, a, b.ID, "symbol_fallback") {
		if strings.Contains(e, "symbol-share") {
			ev = append(ev, e)
		}
	}
	if len(ev) != 1 || !strings.Contains(ev[0], "unknown proposed content") {
		t.Errorf("symbol_fallback events = %q", ev)
	}
}

// #591: an edit that touches only lines outside every symbol (a comment
// between functions) still leaves B a row on the file, so A's later edit
// next to it is compared against B and denied by the range test.
func TestSymbolShareTracksEditsOutsideSymbols(t *testing.T) {
	a, owner, b := shareSetup(t, "")
	note := staleEdit(t, b, "pkg/x.go", "func M2() {}\n", "// from b\nfunc M2() {}\n")
	if d := a.CheckEdit(note); !d.Allow {
		t.Fatalf("B's comment denied: %+v", d)
	}
	onDisk(t, note)
	d := a.CheckEdit(staleEdit(t, owner, "pkg/x.go", "func M2() {}\n", "// from a\nfunc M2() {}\n"))
	if d.Allow || d.Owner != b.ID || !strings.Contains(d.Reason, "within 2 lines") {
		t.Fatalf("owner's comment at the same spot = %+v, want a range deny naming %s", d, b.ID)
	}
}
