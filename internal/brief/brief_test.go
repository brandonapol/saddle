package brief

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
)

const prompt = `Implement the usage meter.

Wire it into the TUI header.

## Done when
- [ ] meter tests pass
- [x] header shows the meter
- make check passes

## Notes
Anything else.`

func fixture() []mcpserver.TaskView {
	return []mcpserver.TaskView{
		{ID: "t1", Title: "usage meter", Status: store.Running, Parent: "t0", Branch: "saddle/t1-usage-meter",
			Claims: []string{"internal/usage/**", "internal/tui/header.go"}, Train: ""},
		{ID: "t2", Title: "meter cli", Status: store.Done, Parent: "t1", Claims: []string{"internal/cli/meter.go"}, Train: "queued"},
		{ID: "t3", Title: "meter docs", Status: store.Landed, Parent: "t1", Claims: []string{"docs/**"}},
		{ID: "t4", Title: "other work", Status: store.Running, Parent: "t0", Claims: []string{"internal/app/**"}},
		{ID: "t5", Title: "dead work", Status: store.Killed, Parent: "t0", Claims: []string{"internal/old/**"}},
	}
}

func TestBuildCollectsClaimsChildrenAndDoneWhen(t *testing.T) {
	b, err := Build("t1", prompt, fixture(), []string{"go.mod"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Goal != "Implement the usage meter." {
		t.Errorf("goal: %q", b.Goal)
	}
	if !slices.Equal(b.Owned, []string{"internal/usage/**", "internal/tui/header.go"}) {
		t.Errorf("owned: %q", b.Owned)
	}
	// Other live tasks' claims and serial files; never a landed or killed task's.
	want := []string{"go.mod", "internal/app/** (t4)", "internal/cli/meter.go (t2)"}
	if !slices.Equal(b.DoNotTouch, want) {
		t.Errorf("do-not-touch: %q, want %q", b.DoNotTouch, want)
	}
	if len(b.DoneWhen) != 3 || b.DoneWhen[0].Text != "meter tests pass" || b.DoneWhen[0].Checked ||
		!b.DoneWhen[1].Checked || b.DoneWhen[2].Text != "make check passes" {
		t.Errorf("done-when: %+v", b.DoneWhen)
	}
	if len(b.Children) != 2 || b.Children[0].ID != "t2" || b.Children[1].Status != store.Landed {
		t.Errorf("children: %+v", b.Children)
	}
	if _, err := Build("t9", "", fixture(), nil); err == nil {
		t.Error("an unknown task should be an error")
	}
}

// Without a done-when section, checklist items and "must" sentences stand in.
func TestDoneWhenFallbacks(t *testing.T) {
	if d := doneWhen("Do it.\n- [ ] a\n- [x] b\n"); len(d) != 2 || d[1].Text != "b" || !d[1].Checked {
		t.Errorf("checklist: %+v", d)
	}
	d := doneWhen("Add the thing. Failing tests first. `make check` must pass. Call done.")
	if len(d) != 1 || d[0].Text != "`make check` must pass." {
		t.Errorf("must sentence: %+v", d)
	}
	if d := doneWhen("Just do it."); len(d) != 0 {
		t.Errorf("nothing to check: %+v", d)
	}
}

func TestGoalFallsBackToTitle(t *testing.T) {
	ts := fixture()
	b, _ := Build("t4", "", ts, nil)
	if b.Goal != "other work" {
		t.Errorf("goal: %q", b.Goal)
	}
	b, _ = Build("t4", "## Heading\n\nFirst line of the goal\nwraps here. Then more.", ts, nil)
	if b.Goal != "First line of the goal wraps here." {
		t.Errorf("goal: %q", b.Goal)
	}
}

func TestLinesFitWidth(t *testing.T) {
	b, _ := Build("t1", prompt, fixture(), []string{"go.mod"})
	for _, w := range []int{24, 36, 80, 120} {
		ls := b.Lines(w)
		for _, l := range ls {
			if n := utf8.RuneCountInString(l); n > w {
				t.Errorf("width %d: %q is %d wide", w, l, n)
			}
		}
		out := strings.Join(ls, "\n")
		for _, want := range []string{"t1", "Goal", "Owns", "Hands off", "Done when", "Children", "t2"} {
			if !strings.Contains(out, want) {
				t.Errorf("width %d lacks %q:\n%s", w, want, out)
			}
		}
	}
	wide := strings.Join(b.Lines(120), "\n")
	for _, want := range []string{"[ ] meter tests pass", "[x] header shows the meter", "internal/usage/**", "t2 done meter cli"} {
		if !strings.Contains(wide, want) {
			t.Errorf("wide brief lacks %q:\n%s", want, wide)
		}
	}
}
