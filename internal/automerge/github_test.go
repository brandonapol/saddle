package automerge

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/brandonapol/saddle/internal/forge"
)

// fakeForge answers PR reads and merge settings, and records merges.
type fakeForge struct {
	pr       forge.PR
	fields   forge.PRFields
	settings forge.MergeSettings
	merges   []string
}

func (f *fakeForge) PR(_ context.Context, _ string, fs forge.PRFields) (forge.PR, error) {
	f.fields = fs
	return f.pr, nil
}

func (f *fakeForge) MergeSettings(context.Context) (forge.MergeSettings, error) {
	return f.settings, nil
}

func (f *fakeForge) MergePR(_ context.Context, ref, method, head string) error {
	f.merges = append(f.merges, ref+" "+method+" "+head)
	return nil
}

func TestGHReadsPR(t *testing.T) {
	f := &fakeForge{pr: forge.PR{State: forge.StateOpen, Draft: true, Mergeable: forge.Mergeable, MergeState: "CLEAN",
		Base: "main", Head: "saddle/t1", HeadSHA: "abc", Labels: []string{NeedsHuman},
		Checks: []forge.CheckRun{{Status: "COMPLETED", Conclusion: "SUCCESS"}, {Status: "QUEUED"}}}}
	pr, err := (&GH{Forge: f}).PR("u")
	if err != nil {
		t.Fatal(err)
	}
	if pr.URL != "u" || !pr.Draft || pr.Base != "main" || pr.HeadSHA != "abc" || !slices.Equal(pr.Labels, []string{NeedsHuman}) || pr.Checks != ChecksPending {
		t.Fatalf("pr = %+v", pr)
	}
	if f.fields&forge.PRChecks == 0 || f.fields&forge.PRMergeState == 0 {
		t.Fatalf("fields = %b, want the check rollup and merge state", f.fields)
	}
}

func TestGHMergeMethodAndMerge(t *testing.T) {
	f := &fakeForge{settings: forge.MergeSettings{Merge: true, Rebase: true}}
	g := &GH{Forge: f}
	m, err := g.MergeMethod()
	if err != nil || m != "rebase" {
		t.Fatalf("method = %q, %v; want rebase (never merge commits)", m, err)
	}
	if err := g.Merge("u", m, "abc"); err != nil {
		t.Fatal(err)
	}
	if err := g.Merge("u", "fast-forward", "abc"); err == nil {
		t.Fatal("unknown method: want error")
	}
	if !slices.Equal(f.merges, []string{"u rebase abc"}) {
		t.Fatalf("merges = %q", f.merges)
	}
	if _, err := (&GH{Forge: &fakeForge{settings: forge.MergeSettings{Merge: true}}}).MergeMethod(); err == nil {
		t.Fatal("merge-commit-only repo: want error")
	}
}

// openForge lists open PRs in one call.
type openForge struct {
	fakeForge
	prs   []forge.PR
	calls int
}

func (f *openForge) OpenPRs(context.Context) ([]forge.PR, error) {
	f.calls++
	return f.prs, nil
}

func TestGHOpenPRsSummarizes(t *testing.T) {
	f := &openForge{prs: []forge.PR{{
		URL: "https://github.com/o/r/pull/4", State: forge.StateOpen, Mergeable: forge.Mergeable, MergeState: "CLEAN",
		Base: "main", Head: "saddle/t1", HeadSHA: "abc", Labels: []string{NeedsHuman},
		Checks: []forge.CheckRun{{Status: "COMPLETED", Conclusion: "FAILURE"}, {Status: "IN_PROGRESS"}},
	}}}
	prs, err := (&GH{Forge: f}).OpenPRs()
	if err != nil || f.calls != 1 || len(prs) != 1 || prs[0].URL != "https://github.com/o/r/pull/4" || prs[0].Checks != ChecksFail || prs[0].HeadSHA != "abc" {
		t.Fatalf("OpenPRs = %+v, %v, calls %d", prs, err, f.calls)
	}
}

// The watcher asks GH once for every open PR, not once per PR.
func TestWatcherUsesGHOpenPRs(t *testing.T) {
	f := &openForge{prs: []forge.PR{
		{URL: "pr/a", State: forge.StateOpen, Mergeable: forge.Mergeable, MergeState: "CLEAN",
			Base: "main", Head: "saddle/a", HeadSHA: "sha-a",
			Checks: []forge.CheckRun{{Status: "COMPLETED", Conclusion: "SUCCESS"}}},
		{URL: "pr/b", State: forge.StateOpen, Mergeable: forge.Mergeable, MergeState: "CLEAN",
			Base: "saddle/a", Head: "saddle/b", HeadSHA: "sha-b",
			Checks: []forge.CheckRun{{Status: "COMPLETED", Conclusion: "SUCCESS"}}},
	}}
	w := &Watcher{
		Path: filepath.Join(t.TempDir(), "automerge.json"),
		Base: "main",
		GH:   &GH{Forge: f},
		Entries: func() ([]Entry, error) {
			return []Entry{{Task: "a", Branch: "saddle/a", PR: "pr/a"}, {Task: "b", Branch: "saddle/b", PR: "pr/b"}}, nil
		},
	}
	st, err := w.Check()
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Fatalf("OpenPRs calls = %d, want 1", f.calls)
	}
	if len(st.Stacks) != 1 || len(st.Stacks[0].Nodes) != 2 || !st.Stacks[0].Ready {
		t.Fatalf("stacks = %+v", st.Stacks)
	}
}
