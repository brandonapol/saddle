package sweeper

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/forge"
)

var (
	green   = []Check{{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"}, {Context: "lint", State: "SUCCESS"}}
	pending = []Check{{Name: "test", Status: "IN_PROGRESS"}}
	failed  = []Check{{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"}}
	tested  = []File{{Path: "internal/x/x.go"}, {Path: "internal/x/x_test.go"}}
)

// pr builds a ready PR on main; tests change what they need.
func pr(n int, head, base string) PR {
	return PR{
		Number: n, Title: head, HeadRefName: head, HeadRefOid: fmt.Sprintf("sha%d", n), BaseRefName: base,
		Mergeable: "MERGEABLE", Checks: green, Files: tested,
	}
}

func TestDecide(t *testing.T) {
	o := Options{}
	many := make([]File, 0, DefaultMaxFiles+1)
	for i := range DefaultMaxFiles + 1 {
		many = append(many, File{Path: fmt.Sprintf("docs/f%d.md", i)})
	}
	var wide []File
	for i := range DefaultMaxPackages + 1 {
		wide = append(wide, File{Path: fmt.Sprintf("internal/p%d/p_test.go", i)})
	}
	for _, tc := range []struct {
		name   string
		edit   func(*PR)
		action Action
		flag   bool
		reason string
	}{
		{"green", func(*PR) {}, WouldMerge, false, "green"},
		{"pending", func(p *PR) { p.Checks = pending }, Waiting, false, "pending checks: test"},
		{"pending status context", func(p *PR) { p.Checks = []Check{{Context: "ci", State: "PENDING"}} }, Waiting, false, "pending"},
		{"queued", func(p *PR) { p.Checks = []Check{{Name: "test", Status: "QUEUED"}} }, Waiting, false, "pending"},
		{"failed", func(p *PR) { p.Checks = append(slices.Clone(green), failed...) }, Blocked, false, "failing checks: test"},
		{"failed beats pending", func(p *PR) { p.Checks = append(slices.Clone(pending), failed...) }, Blocked, false, "failing"},
		{"cancelled", func(p *PR) { p.Checks = []Check{{Name: "t", Status: "COMPLETED", Conclusion: "CANCELLED"}} }, Blocked, false, "failing"},
		{"error status", func(p *PR) { p.Checks = []Check{{Context: "ci", State: "ERROR"}} }, Blocked, false, "failing"},
		{"skipped counts as pass", func(p *PR) {
			p.Checks = append(slices.Clone(green), Check{Name: "s", Status: "COMPLETED", Conclusion: "SKIPPED"})
		}, WouldMerge, false, ""},
		{"no checks", func(p *PR) { p.Checks = nil }, Waiting, false, "no CI checks"},
		{"conflicting", func(p *PR) { p.Mergeable = "CONFLICTING" }, Blocked, false, "conflicts"},
		{"mergeable unknown", func(p *PR) { p.Mergeable = "UNKNOWN" }, Waiting, false, "mergeability"},
		{"draft", func(p *PR) { p.IsDraft = true }, Blocked, false, "draft"},
		{"labeled", func(p *PR) { p.Labels = []Label{{Name: "Requires Review"}} }, Blocked, false, "a human merges it"},
		{"labeled even when risky", func(p *PR) {
			p.Labels = []Label{{Name: "requires review"}}
			p.Files = append(p.Files, File{Path: "go.mod"})
		}, Blocked, false, "labeled"},
		{"no tests for code", func(p *PR) { p.Files = []File{{Path: "internal/x/x.go"}} }, Blocked, false, "_test.go"},
		{"docs only needs no tests", func(p *PR) { p.Files = []File{{Path: "README.md"}} }, WouldMerge, false, ""},
		{"tests only", func(p *PR) { p.Files = []File{{Path: "internal/x/x_test.go"}} }, WouldMerge, false, ""},
		{"go.mod is risky", func(p *PR) { p.Files = append(p.Files, File{Path: "go.mod"}) }, Blocked, true, "changes go.mod"},
		{"go.sum is risky", func(p *PR) { p.Files = append(p.Files, File{Path: "go.sum"}) }, Blocked, true, "go.sum"},
		{"workflow is risky", func(p *PR) { p.Files = append(p.Files, File{Path: ".github/workflows/ci.yml"}) }, Blocked, true, "workflow"},
		{"risky even while failing", func(p *PR) {
			p.Checks = failed
			p.Files = append(p.Files, File{Path: "go.sum"})
		}, Blocked, true, "go.sum"},
		{"many files", func(p *PR) { p.Files = many }, Blocked, true, "files"},
		{"many packages", func(p *PR) { p.Files = wide }, Blocked, true, "packages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pr(1, "saddle/t1-x", "main")
			tc.edit(&p)
			v := Decide(p, o)
			if v.Action != tc.action || v.Flag != tc.flag || !strings.Contains(v.Reason, tc.reason) {
				t.Fatalf("got %+v, want action %q flag %v reason containing %q", v, tc.action, tc.flag, tc.reason)
			}
		})
	}
}

// fakeForge answers from canned PRs and records every call. Mutating calls
// are recorded as "merge N METHOD HEAD", "base N BRANCH", "label NAME" and
// "add-label N NAME".
type fakeForge struct {
	open   []PR
	merged map[string]int // head branch -> merged PR number
	fail   map[string]error
	calls  []string
}

func (f *fakeForge) call(c string) error {
	f.calls = append(f.calls, c)
	for prefix, err := range f.fail {
		if strings.HasPrefix(c, prefix) {
			return err
		}
	}
	return nil
}

func (f *fakeForge) ListPRs(_ context.Context, q forge.PRQuery, _ forge.PRFields) ([]forge.PR, error) {
	if err := f.call("list " + q.State); err != nil {
		return nil, err
	}
	if q.State == "merged" {
		if n, ok := f.merged[q.Head]; ok {
			return []forge.PR{{Number: n}}, nil
		}
		return nil, nil
	}
	var out []forge.PR
	for _, p := range f.open {
		fp := forge.PR{Number: p.Number, Title: p.Title, Head: p.HeadRefName, HeadSHA: p.HeadRefOid, Base: p.BaseRefName,
			Draft: p.IsDraft, Mergeable: p.Mergeable}
		for _, l := range p.Labels {
			fp.Labels = append(fp.Labels, l.Name)
		}
		for _, x := range p.Files {
			fp.Files = append(fp.Files, x.Path)
		}
		for _, c := range p.Checks {
			fp.Checks = append(fp.Checks, forge.CheckRun{Name: c.Name, Context: c.Context, Status: c.Status, Conclusion: c.Conclusion, State: c.State})
		}
		out = append(out, fp)
	}
	return out, nil
}

func (f *fakeForge) MergePR(_ context.Context, ref, method, head string) error {
	return f.call(strings.TrimSpace("merge " + ref + " " + method + " " + head))
}

func (f *fakeForge) SetPRBase(_ context.Context, ref, base string) error {
	return f.call("base " + ref + " " + base)
}

func (f *fakeForge) EnsureLabel(_ context.Context, l forge.Label) error {
	return f.call("label " + l.Name + " " + l.Color + " " + l.Description)
}

func (f *fakeForge) AddLabel(_ context.Context, ref, label string) error {
	return f.call("add-label " + ref + " " + label)
}

// mutating returns the calls that change the forge's state.
func (f *fakeForge) mutating() []string {
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "list ") {
			out = append(out, c)
		}
	}
	return out
}

func byNumber(rs []Result) map[int]Result {
	m := map[int]Result{}
	for _, r := range rs {
		m[r.Number] = r
	}
	return m
}

func TestSweepStackBottomFirstAndRetarget(t *testing.T) {
	// #12 is stacked on #11 is stacked on #10; #20 stands alone. Listed out of
	// order to prove the sweep sorts them.
	gh := &fakeForge{open: []PR{
		pr(12, "saddle/t3-c", "saddle/t2-b"),
		pr(20, "saddle/t9-solo", "main"),
		pr(11, "saddle/t2-b", "saddle/t1-a"),
		pr(10, "saddle/t1-a", "main"),
		pr(30, "feature/not-saddle", "main"),
	}}
	var logged []string
	rs, err := Sweep(context.Background(), Options{Forge: gh, Log: func(branch, kind, data string) {
		logged = append(logged, kind+" "+branch)
	}})
	if err != nil {
		t.Fatal(err)
	}
	var order []int
	for _, r := range rs {
		order = append(order, r.Number)
	}
	if want := []int{10, 20, 11, 12}; !slices.Equal(order, want) {
		t.Fatalf("order %v, want %v", order, want)
	}
	got := byNumber(rs)
	if got[10].Action != Merged || got[20].Action != Merged {
		t.Fatalf("bottoms should merge: %+v %+v", got[10], got[20])
	}
	if r := got[11]; r.Action != Waiting || !strings.Contains(r.Reason, "retargeted to main") {
		t.Fatalf("#11 should be retargeted and wait for CI: %+v", r)
	}
	if r := got[12]; r.Action != Waiting || !strings.Contains(r.Reason, "stacked on #11") {
		t.Fatalf("#12 should wait on #11: %+v", r)
	}
	want := []string{
		"merge 10 squash sha10",
		"base 11 main",
		"merge 20 squash sha20",
	}
	if m := gh.mutating(); !slices.Equal(m, want) {
		t.Fatalf("mutating calls\n got %q\nwant %q", m, want)
	}
	for _, k := range []string{"sweep.merged saddle/t1-a", "sweep.retarget saddle/t2-b", "sweep.waiting saddle/t3-c"} {
		if !slices.Contains(logged, k) {
			t.Errorf("missing log %q in %q", k, logged)
		}
	}
}

func TestSweepParentMergedEarlier(t *testing.T) {
	// #11's parent merged in an earlier sweep without being retargeted.
	gh := &fakeForge{open: []PR{pr(11, "saddle/t2-b", "saddle/t1-a")}, merged: map[string]int{"saddle/t1-a": 10}}
	rs, err := Sweep(context.Background(), Options{Forge: gh})
	if err != nil {
		t.Fatal(err)
	}
	if r := rs[0]; r.Action != Waiting || !strings.Contains(r.Reason, "parent #10 merged; retargeted") {
		t.Fatalf("got %+v", r)
	}
	if m := gh.mutating(); !slices.Equal(m, []string{"base 11 main"}) {
		t.Fatalf("mutating calls %q", m)
	}
}

func TestSweepOrphanBaseBlocked(t *testing.T) {
	gh := &fakeForge{open: []PR{pr(11, "saddle/t2-b", "saddle/gone")}}
	rs, _ := Sweep(context.Background(), Options{Forge: gh})
	if rs[0].Action != Blocked || len(gh.mutating()) != 0 {
		t.Fatalf("got %+v, calls %q", rs[0], gh.mutating())
	}
}

func TestSweepRetargetedChildWaitsEvenIfGreen(t *testing.T) {
	// The child's checks are green against the old base; it must still wait.
	gh := &fakeForge{open: []PR{pr(10, "saddle/t1-a", "main"), pr(11, "saddle/t2-b", "saddle/t1-a")}}
	rs, _ := Sweep(context.Background(), Options{Forge: gh})
	got := byNumber(rs)
	if got[11].Action != Waiting {
		t.Fatalf("#11 merged in the same pass as its parent: %+v", got[11])
	}
	for _, c := range gh.calls {
		if strings.HasPrefix(c, "merge 11") {
			t.Fatal("#11 was merged")
		}
	}
}

func TestSweepLabelsRiskyOnce(t *testing.T) {
	a, b := pr(10, "saddle/t1-a", "main"), pr(11, "saddle/t2-b", "main")
	a.Files = append(a.Files, File{Path: "go.mod"})
	b.Files = append(b.Files, File{Path: ".github/workflows/ci.yml"})
	gh := &fakeForge{open: []PR{a, b}}
	rs, _ := Sweep(context.Background(), Options{Forge: gh, ReviewLabel: "needs eyes"})
	for _, r := range rs {
		if r.Action != Blocked || !r.Labeled {
			t.Fatalf("risky PR not blocked and labeled: %+v", r)
		}
	}
	want := []string{
		"label needs eyes D93F0B saddle sweep will not merge this; a human should review it",
		"add-label 10 needs eyes",
		"add-label 11 needs eyes",
	}
	if m := gh.mutating(); !slices.Equal(m, want) {
		t.Fatalf("mutating calls\n got %q\nwant %q", m, want)
	}
}

func TestSweepMethodAndMergeFailure(t *testing.T) {
	gh := &fakeForge{
		open: []PR{pr(10, "saddle/t1-a", "main"), pr(11, "saddle/t2-b", "saddle/t1-a")},
		fail: map[string]error{"merge 10": fmt.Errorf("head moved")},
	}
	rs, _ := Sweep(context.Background(), Options{Forge: gh, Method: "rebase"})
	got := byNumber(rs)
	if got[10].Action != Blocked || !strings.Contains(got[10].Reason, "head moved") {
		t.Fatalf("#10: %+v", got[10])
	}
	if got[11].Action != Waiting || !strings.Contains(got[11].Reason, "stacked on #10") {
		t.Fatalf("#11 must not be retargeted when its parent failed to merge: %+v", got[11])
	}
	if m := gh.mutating(); !slices.Equal(m, []string{"merge 10 rebase sha10"}) {
		t.Fatalf("mutating calls %q", m)
	}
}

func TestSweepDryRunMakesNoMutatingCalls(t *testing.T) {
	risky := pr(13, "saddle/t4-d", "main")
	risky.Files = append(risky.Files, File{Path: "go.sum"})
	gh := &fakeForge{
		open: []PR{
			pr(10, "saddle/t1-a", "main"),
			pr(11, "saddle/t2-b", "saddle/t1-a"),
			pr(12, "saddle/t3-c", "saddle/t0-old"),
			risky,
		},
		merged: map[string]int{"saddle/t0-old": 9},
	}
	rs, err := Sweep(context.Background(), Options{Forge: gh, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if m := gh.mutating(); len(m) != 0 {
		t.Fatalf("dry run made mutating calls: %q", m)
	}
	got := byNumber(rs)
	if got[10].Action != WouldMerge {
		t.Fatalf("#10: %+v", got[10])
	}
	if r := got[11]; r.Action != Waiting || !strings.Contains(r.Reason, "would be retargeted") {
		t.Fatalf("#11: %+v", r)
	}
	if r := got[12]; r.Action != Waiting || !strings.Contains(r.Reason, "would retarget") {
		t.Fatalf("#12: %+v", r)
	}
	if r := got[13]; r.Action != Blocked || !r.Labeled {
		t.Fatalf("#13: %+v", r)
	}
	rep := Report(rs, true)
	for _, s := range []string{"dry run", "would merge (1)", "waiting (2)", "blocked (1)"} {
		if !strings.Contains(rep, s) {
			t.Errorf("report missing %q:\n%s", s, rep)
		}
	}
}

func TestSweepListError(t *testing.T) {
	gh := &fakeForge{fail: map[string]error{"list": fmt.Errorf("not logged in")}}
	if _, err := Sweep(context.Background(), Options{Forge: gh}); err == nil {
		t.Fatal("want error")
	}
}

func TestReport(t *testing.T) {
	rep := Report([]Result{
		{Number: 10, Branch: "saddle/t1-a", Action: Merged, Reason: "squash merged"},
		{Number: 11, Branch: "saddle/t2-b", Action: Blocked, Reason: "changes go.mod; flagged for review", Labeled: true},
	}, false)
	want := "merged (1)\n  #10    saddle/t1-a: squash merged\nblocked (1)\n  #11    saddle/t2-b: changes go.mod; flagged for review [labeled]\n"
	if rep != want {
		t.Fatalf("got\n%s\nwant\n%s", rep, want)
	}
	if got := Report(nil, false); got != "no open saddle PRs\n" {
		t.Fatalf("empty: %q", got)
	}
}
