package app

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
)

// claimBase is the shared package at every task's base. Claim's signature
// and the one-line f are what the rule distinguishes from body edits.
const claimBase = "package store\n\n" +
	"type Store struct{}\n\n" +
	"func (s *Store) Claim(path string) error {\n\treturn nil\n}\n\n" +
	"func f() int { return 1 }\n"

const claimUse = "package store\n\nfunc Use(s *Store) error {\n\treturn nil\n}\n"

const claimOther = "package other\n\nfunc Use() {}\n"

const claimSig = "package store\n\n" +
	"type Store struct{}\n\n" +
	"func (s *Store) Claim(path string, force bool) error {\n\treturn nil\n}\n\n" +
	"func f() int { return 1 }\n"

const claimBody = "package store\n\n" +
	"type Store struct{}\n\n" +
	"func (s *Store) Claim(path string) error {\n\treturn errors.New(\"no\")\n}\n\n" +
	"func f() int { return 2 }\n"

const (
	claimOldSig = "func (s *Store) Claim(path string) error"
	claimNewSig = "func (s *Store) Claim(path string, force bool) error"
	claimPath   = "internal/store/store.go"
	claimUseRel = "internal/store/use.go"
)

// assumeSetup puts two workers on a base that has Store.Claim. The rule
// runs with the default budget; tests that need a different one set it.
func assumeSetup(t *testing.T) (*App, store.Task, store.Task) {
	t.Helper()
	t.Cleanup(SetAssumeBudget(time.Minute))
	a, _ := setup(t)
	a.Cfg.Claims.Granularity = config.GranularitySymbol
	write(t, a.Root, claimPath, claimBase)
	write(t, a.Root, claimUseRel, claimUse)
	write(t, a.Root, "internal/other/use.go", claimOther)
	commitAll(t, a.Root, "add store")
	must(t, a.Init())
	must(t, a.ensureIntegration())
	t1, err := a.Spawn(SpawnReq{Title: "change claim"})
	must(t, err)
	t2, err := a.Spawn(SpawnReq{Title: "use claim"})
	must(t, err)
	return a, t1, t2
}

// assumeCall is t2 adding a call on one line of rel.
func assumeCall(t *testing.T, tk store.Task, rel, old, new string) Edit {
	t.Helper()
	before := readFile(t, filepath.Join(tk.Worktree, rel))
	if !strings.Contains(before, old) {
		t.Fatalf("%s has no %q", rel, old)
	}
	return Edit{Task: tk.ID, Abs: filepath.Join(tk.Worktree, rel), Before: []byte(before),
		After: []byte(strings.Replace(before, old, new, 1)), Known: true}
}

// #592: t1 changes Store.Claim's parameters. t2's edit calls Claim in the
// same package, so the note names t1 and both signatures. A second edit
// that calls Claim again is silent.
func TestAssumeNotesLiveSignatureChangeOnce(t *testing.T) {
	a, t1, t2 := assumeSetup(t)
	write(t, t1.Worktree, claimPath, claimSig)

	call := assumeCall(t, t2, claimUseRel, "return nil", `return s.Claim("a")`)
	note := a.EditNotes(call)
	for _, want := range []string{t1.ID, "`Store.Claim`", claimPath, claimOldSig, claimNewSig, "Depend on the new shape"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q lacks %q", note, want)
		}
	}
	if ev := taskEvents(t, a, t2.ID, "symbol_note"); len(ev) != 1 {
		t.Errorf("symbol_note events = %q, want one", ev)
	}

	again := assumeCall(t, t2, claimUseRel, "return nil", `return s.Claim("b")`)
	if n := a.EditNotes(again); n != "" {
		t.Fatalf("second edit noted again: %q", n)
	}
	if ev := taskEvents(t, a, t2.ID, "symbol_note"); len(ev) != 1 {
		t.Errorf("symbol_note events = %q, want the first one only", ev)
	}

	// A different task's first edit hits the blob-pair cache and still notes.
	t3, err := a.Spawn(SpawnReq{Title: "also use claim"})
	must(t, err)
	third := assumeCall(t, t3, claimUseRel, "return nil", `return s.Claim("c")`)
	if n := a.EditNotes(third); !strings.Contains(n, t1.ID) || !strings.Contains(n, claimOldSig) || !strings.Contains(n, claimNewSig) {
		t.Fatalf("cached signature diff note = %q", n)
	}
}

// #592: a body-only edit, including the one-line `func f() int { return 1 }`
// shape, is not a signature change and produces no note.
func TestAssumeBodyOnlyChangeIsSilent(t *testing.T) {
	a, t1, t2 := assumeSetup(t)
	write(t, t1.Worktree, claimPath, claimBody)
	call := assumeCall(t, t2, claimUseRel, "return nil", `return s.Claim("a") + f()`)
	if n := a.EditNotes(call); n != "" {
		t.Fatalf("body-only change noted: %q", n)
	}
	if ev := taskEvents(t, a, t2.ID, "symbol_note"); len(ev) != 0 {
		t.Errorf("symbol_note events = %q", ev)
	}
}

// #592: a call through a selector matches only when the qualifier is the
// last element of the symbol's directory. db.Claim does not; store.Claim does.
func TestAssumeSelectorMatchesPackageDir(t *testing.T) {
	a, t1, t2 := assumeSetup(t)
	write(t, t1.Worktree, claimPath, claimSig)

	miss := assumeCall(t, t2, "internal/other/use.go", "func Use() {}", "func Use() { db.Claim(\"a\") }")
	if n := a.EditNotes(miss); n != "" {
		t.Fatalf("mismatched qualifier noted: %q", n)
	}

	hit := assumeCall(t, t2, "internal/other/use.go", "func Use() {}", "func Use() { store.Claim(\"a\") }")
	note := a.EditNotes(hit)
	for _, want := range []string{t1.ID, "`Store.Claim`", claimPath, claimOldSig, claimNewSig} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q lacks %q", note, want)
		}
	}
}

// #592: a signature that landed on integration since t2's base is Q2's
// recommended PreToolUse deny (rule 2's sync wording). A body-only landing
// is not, and the landed change is not also a live note.
func TestAssumeLandedSignatureDeniesUntilSync(t *testing.T) {
	a, _, t2 := assumeSetup(t)
	const subject = "feat: Claim takes force"
	commitOnIntegration(t, a, subject, map[string]string{claimPath: claimSig})

	call := assumeCall(t, t2, claimUseRel, "return nil", `return s.Claim("a")`)
	if n := a.EditNotes(call); n != "" {
		t.Fatalf("landed change produced a live note: %q", n)
	}
	d := a.CheckEdit(call)
	if d.Allow || d.Owner != "" {
		t.Fatalf("decision = %+v, want a deny with no owner", d)
	}
	for _, want := range []string{"`Store.Claim`", claimPath, a.Cfg.Integration, subject, "saddle sync"} {
		if !strings.Contains(d.Reason, want) {
			t.Errorf("reason %q lacks %q", d.Reason, want)
		}
	}
	if ev := taskEvents(t, a, t2.ID, "symbol_deny"); len(ev) != 1 || !strings.Contains(ev[0], RuleStaleAssumption) {
		t.Errorf("symbol_deny events = %q", ev)
	}

	a, _, t2 = assumeSetup(t)
	commitOnIntegration(t, a, "chore: Claim body", map[string]string{claimPath: claimBody})
	call = assumeCall(t, t2, claimUseRel, "return nil", `return s.Claim("a") + f()`)
	if d := a.CheckEdit(call); !d.Allow {
		t.Fatalf("body-only landing denied: %+v", d)
	}
}

// #592: file mode is the kill switch. The same live signature change that
// warns in symbol mode is invisible here.
func TestAssumeFileModeIsSilent(t *testing.T) {
	a, t1, t2 := assumeSetup(t)
	a.Cfg.Claims.Granularity = config.GranularityFile
	write(t, t1.Worktree, claimPath, claimSig)
	call := assumeCall(t, t2, claimUseRel, "return nil", `return s.Claim("a")`)
	if n := a.EditNotes(call); n != "" {
		t.Fatalf("file mode note = %q", n)
	}
	if d := a.CheckEdit(call); !d.Allow {
		t.Fatalf("file mode denied: %+v", d)
	}
}

// #592: past the budget the rule says nothing, the same as today's hook.
func TestAssumeSilentPastBudget(t *testing.T) {
	a, t1, t2 := assumeSetup(t)
	t.Cleanup(SetAssumeBudget(0))
	write(t, t1.Worktree, claimPath, claimSig)
	call := assumeCall(t, t2, claimUseRel, "return nil", `return s.Claim("a")`)
	if n := a.EditNotes(call); n != "" {
		t.Fatalf("over budget noted: %q", n)
	}
	ev := taskEvents(t, a, t2.ID, "symbol_fallback")
	if len(ev) != 1 || !strings.Contains(ev[0], RuleStaleAssumption) || !strings.Contains(ev[0], "budget") {
		t.Fatalf("symbol_fallback events = %q", ev)
	}
}
