package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/brandonapol/saddle/internal/config"
)

// withRules swaps the symbol rule tables for the test. Tests that call it
// must not be parallel: the tables are package state.
func withRules(t *testing.T, edit []EditRule, note []NoteRule) {
	t.Helper()
	oldEdit, oldNote := editRules, noteRules
	editRules, noteRules = nil, nil
	for _, r := range edit {
		RegisterEditRule(r)
	}
	for _, r := range note {
		RegisterNoteRule(r)
	}
	t.Cleanup(func() { editRules, noteRules = oldEdit, oldNote })
}

// #588: a path-claim deny names its owner; serial-file and outside-worktree
// denies do not, so a symbol rule can tell them apart.
func TestCheckWriteDenySetsOwnerOnlyForClaims(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	owner, err := a.Spawn(SpawnReq{Title: "billing", Claims: []string{"billing/meter.go"}})
	must(t, err)
	other, err := a.Spawn(SpawnReq{Title: "other"})
	must(t, err)
	a.Cfg.Serial = []string{"go.sum"}
	if d := a.CheckWrite(other.ID, filepath.Join(other.Worktree, "billing/meter.go")); d.Allow || d.Owner != owner.ID {
		t.Fatalf("claim deny = %+v, want Owner %s", d, owner.ID)
	}
	if d := a.CheckWrite(other.ID, filepath.Join(other.Worktree, "go.sum")); d.Allow || d.Owner != "" {
		t.Fatalf("serial deny = %+v, want no Owner", d)
	}
	if d := a.CheckWrite(other.ID, filepath.Join(owner.Worktree, "README.md")); d.Allow || d.Owner != "" {
		t.Fatalf("outside-worktree deny = %+v, want no Owner", d)
	}
	if d := a.CheckWrite(other.ID, filepath.Join(other.Worktree, "README.md")); !d.Allow || d.Owner != "" {
		t.Fatalf("allow = %+v", d)
	}
}

// writeCases are CheckWrite's verdict shapes: a claim deny, a serial deny,
// an outside-worktree deny, a first-write auto-claim, a repeat write, and a
// write outside the repo.
func writeCases(t *testing.T, a *App) []Edit {
	t.Helper()
	owner, err := a.Spawn(SpawnReq{Title: "billing", Claims: []string{"billing/meter.go"}})
	must(t, err)
	other, err := a.Spawn(SpawnReq{Title: "other"})
	must(t, err)
	a.Cfg.Serial = []string{"go.sum"}
	scratch := filepath.Join(t.TempDir(), "x.txt")
	var es []Edit
	for _, c := range []struct{ task, abs string }{
		{other.ID, filepath.Join(other.Worktree, "billing/meter.go")},
		{other.ID, filepath.Join(other.Worktree, "go.sum")},
		{other.ID, filepath.Join(owner.Worktree, "README.md")},
		{other.ID, filepath.Join(other.Worktree, "billing/invoice.go")},
		{other.ID, filepath.Join(other.Worktree, "billing/invoice.go")},
		{owner.ID, filepath.Join(owner.Worktree, "billing/invoice.go")},
		{other.ID, scratch},
	} {
		before, _ := os.ReadFile(c.abs)
		es = append(es, Edit{Task: c.task, Abs: c.abs, Before: before, After: []byte("changed\n"), Known: true})
	}
	return es
}

// #588: with no symbol rules registered, CheckEdit is exactly CheckWrite in
// both modes.
func TestCheckEditEqualsCheckWriteWithoutRules(t *testing.T) {
	withRules(t, nil, nil)
	for _, mode := range config.Granularities() {
		t.Run(mode, func(t *testing.T) {
			a, _ := setup(t)
			a.Cfg.Claims.Granularity = mode
			for i, e := range writeCases(t, a) {
				got := a.CheckEdit(e)
				want := a.CheckWrite(e.Task, e.Abs)
				if got != want {
					t.Errorf("case %d (%s): CheckEdit = %+v, CheckWrite = %+v", i, e.Abs, got, want)
				}
			}
			if n := a.EditNotes(Edit{Task: "t1", Abs: filepath.Join(a.Root, "README.md")}); n != "" {
				t.Errorf("EditNotes = %q with no rules", n)
			}
		})
	}
}

// #588: a registered rule runs in symbol mode, in order, sees the edit and
// the verdict so far, and can flip it. In file mode it never runs.
func TestSymbolRulesRunOnlyInSymbolMode(t *testing.T) {
	var ran []string
	var seen []Edit
	withRules(t, []EditRule{
		{Name: "second", Order: 20, Check: func(_ *App, e Edit, d Decision) Decision {
			ran = append(ran, "second")
			seen = append(seen, e)
			if d.Owner != "" {
				return Decision{Allow: true} // a claim deny it overrides
			}
			return d
		}},
		{Name: "first", Order: 10, Check: func(_ *App, _ Edit, d Decision) Decision {
			ran = append(ran, "first")
			return d
		}},
	}, []NoteRule{
		{Name: "note", Note: func(_ *App, e Edit) string { return "note on " + e.Rel }},
	})
	a, _ := setup(t)
	es := writeCases(t, a)
	claimed, serial := es[0], es[1]

	a.Cfg.Claims.Granularity = config.GranularityFile
	if d := a.CheckEdit(claimed); d.Allow {
		t.Fatalf("file mode: claim deny flipped: %+v", d)
	}
	if n := a.EditNotes(claimed); n != "" {
		t.Fatalf("file mode: EditNotes = %q", n)
	}
	if len(ran) != 0 {
		t.Fatalf("file mode ran rules %v", ran)
	}

	a.Cfg.Claims.Granularity = config.GranularitySymbol
	if d := a.CheckEdit(claimed); !d.Allow {
		t.Fatalf("symbol mode: rule did not flip the claim deny: %+v", d)
	}
	if len(ran) != 2 || ran[0] != "first" || ran[1] != "second" {
		t.Fatalf("rules ran %v, want [first second]", ran)
	}
	if e := seen[0]; e.Rel != "billing/meter.go" || string(e.After) != "changed\n" || !e.Known {
		t.Fatalf("rule saw %+v", e)
	}
	if d := a.CheckEdit(serial); d.Allow {
		t.Fatalf("symbol mode: serial deny flipped: %+v", d)
	}
	if n := a.EditNotes(claimed); n != "note on billing/meter.go" {
		t.Fatalf("symbol mode: EditNotes = %q", n)
	}
}
