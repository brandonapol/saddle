//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/e2e/fakeglab"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

// The GitLab journeys (#645) run saddle on a repo whose [forge] names
// GitLab: every forge call goes to the fake glab, and the fake gh, still on
// PATH, must see none.

const gitlabTables = "[forge]\nprovider = \"gitlab\"\n\n[train]\noutput = \"single\"\n"

func gitlabWorld(t *testing.T) *World {
	t.Helper()
	w := world(t, Options{Tables: gitlabTables})
	t.Cleanup(func() { noGHCalls(t, w) })
	return w
}

// noGHCalls fails t when saddle reached the fake GitHub on a GitLab repo.
func noGHCalls(t *testing.T, w *World) {
	t.Helper()
	if s := w.GHState(); len(s.Calls) > 0 || len(s.PRs) > 0 {
		t.Errorf("saddle called gh on a GitLab repo: %v", s.Calls)
	}
}

// mrAt is the fake GitLab's MR at url.
func mrAt(t *testing.T, w *World, url string) *fakeglab.MR {
	t.Helper()
	m := w.GLState().ByURL(url)
	if m == nil {
		t.Fatalf("no MR %s in the fake GitLab", url)
	}
	return m
}

// landTwoMRs is landTwo on GitLab: t1 and t2 land as one stack and prs opens
// their MRs. It returns the MR URLs by task.
func landTwoMRs(t *testing.T, w *World) map[string]string {
	t.Helper()
	queued := func(v mcpserver.TaskView) bool { return v.Status == "done" && v.Train == "queued" }
	w.Spawn("t1", "Alpha work", []string{"alpha/**"}, finished("alpha", "alpha\n")...)
	w.WaitTask("t1", "queued in the train", queued)
	w.Spawn("t2", "Beta work", []string{"beta/**"}, finished("beta", "beta\n")...)
	w.WaitTask("t2", "queued in the train", queued)
	if r := w.MustSaddle("land"); w.Task("t1").Status != "landed" || w.Task("t2").Status != "landed" {
		t.Fatalf("t1 and t2 didn't land: %s", r)
	}
	r := w.MustSaddle("prs")
	urls := map[string]string{}
	for i, id := range []string{"t1", "t2"} {
		urls[id] = w.Task(id).PR
		if want := w.GLState().MRURL(i + 1); urls[id] != want || !strings.Contains(r.Stdout, want) {
			t.Fatalf("%s's MR = %q, want %s: %s", id, urls[id], want, r)
		}
	}
	return urls
}

// TestJourneyGitLabPublish: saddle publish opens a landed task's own MR on
// GitLab, recorded as its PR; publishing again finds it.
func TestJourneyGitLabPublish(t *testing.T) {
	if Fork(t) {
		return
	}
	w := gitlabWorld(t)
	w.Spawn("t1", "Alpha work", []string{"alpha/**"}, finished("alpha", "alpha\n")...)
	w.WaitTask("t1", "queued in the train", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	w.MustSaddle("land")

	r := w.MustSaddle("publish", "t1")
	url := w.Task("t1").PR
	if url != w.GLState().MRURL(1) || !strings.Contains(r.Stdout, url) {
		t.Fatalf("publish: PR %q, want MR !1: %s", url, r)
	}
	m := mrAt(t, w, url)
	if m.Target != "main" || m.State != "opened" || w.OriginFile(m.Source, "alpha/work.txt") != "alpha\n" {
		t.Fatalf("MR = %+v, want t1's work against main", m)
	}
	if !strings.Contains(m.Description, "t1") {
		t.Fatalf("MR description lacks the task trailer:\n%s", m.Description)
	}
	if again := w.MustSaddle("publish", "t1"); !strings.Contains(again.Stdout, "already has a PR: "+url) {
		t.Fatalf("publish again: %s", again)
	}
	if n := len(w.GLState().MRs); n != 1 {
		t.Fatalf("%d MRs, want 1", n)
	}
}

// TestJourneyGitLabStackRetarget: prs opens a two-MR stack, t2's MR
// targeting t1's branch with saddle's stack note; once t1's MR is merged in
// GitLab by hand, restack retargets t2's MR to main and rebuilds its branch,
// updating the note rather than adding one.
func TestJourneyGitLabStackRetarget(t *testing.T) {
	if Fork(t) {
		return
	}
	w := gitlabWorld(t)
	urls := landTwoMRs(t, w)
	low, high := mrAt(t, w, urls["t1"]), mrAt(t, w, urls["t2"])
	if low.Target != "main" || high.Target != low.Source {
		t.Fatalf("stack targets: %s into %s, %s into %s; want t2 on t1 on main", low.Source, low.Target, high.Source, high.Target)
	}
	notes := high.UserNotes()
	if len(notes) != 1 || !strings.Contains(notes[0].Body, "<!-- saddle:stack -->") || !strings.Contains(notes[0].Body, "Base: `"+low.Source+"`") {
		t.Fatalf("top MR lacks its stack note: %+v", high.Notes)
	}
	w.MustSaddle("prs")
	if n := mrAt(t, w, urls["t2"]).UserNotes(); len(n) != 1 {
		t.Fatalf("repeat prs duplicated the stack note: %+v", n)
	}

	must(t, w.GL.MergeByHand(low.IID))
	if out := w.MustMCP("t0", "restack", nil); !strings.Contains(out, "retargeted") {
		t.Fatalf("restack: %s", out)
	}
	high = mrAt(t, w, urls["t2"])
	if high.Target != "main" {
		t.Fatalf("t2's MR targets %s after restack, want main", high.Target)
	}
	if files := w.Git(w.Repo, "diff", "--name-only", "origin/main...origin/"+high.Source); files != "beta/work.txt" {
		t.Fatalf("t2's MR diff = %q, want only its own file", files)
	}
	if n := high.UserNotes(); len(n) != 1 || !strings.Contains(n[0].Body, "Base: `main`") {
		t.Fatalf("stack note after restack = %+v, want it updated to main", n)
	}
	if !slices.ContainsFunc(high.Notes, func(n fakeglab.Note) bool { return n.System && strings.Contains(n.Body, "changed target branch") }) {
		t.Fatalf("GitLab logged no retarget: %+v", high.Notes)
	}
}

// TestJourneyGitLabCIRedThenRepair: t1's pipeline fails. The ci-red watcher
// holds t2 above it, labels both MRs ci-red and spawns one repair seeded
// with the failed job's trace; the repair lands and folds into t1's layer,
// and when the new pipeline passes the hold and labels clear.
func TestJourneyGitLabCIRedThenRepair(t *testing.T) {
	if Fork(t) {
		return
	}
	w := gitlabWorld(t)
	urls := landTwoMRs(t, w)
	low, high := mrAt(t, w, urls["t1"]), mrAt(t, w, urls["t2"])
	must(t, w.GL.SetJobs(low.IID, fakeglab.Job{Name: "test", Stage: "test", Status: fakeglab.Failed,
		Trace: "\x1b[0Ksection_start:1759572000:step_script\r\x1b[0K$ go test ./...\n--- FAIL: TestAlpha\nFAIL\tdemo/alpha\n"}))
	must(t, w.GL.SetJobs(high.IID, fakeglab.Job{Name: "test", Stage: "test", Status: fakeglab.Success}))
	must(t, fa.Script{Steps: []fa.Step{
		fa.Append("alpha/work.txt", "fixed\n"), fa.Commit("fix alpha"), fa.Done("Fixes TestAlpha.")}}.Save(w.Scripts, "t3"))

	rep := w.CIRedTick()
	if len(rep.Red) != 1 || rep.Red[0].Task != "t1" || !slices.Equal(rep.Red[0].Held, []string{"t2"}) {
		t.Fatalf("red layers = %+v, want t1 holding t2", rep.Red)
	}
	for _, u := range []string{urls["t1"], urls["t2"]} {
		if m := mrAt(t, w, u); !slices.Contains(m.Labels, app.LabelCIRed) {
			t.Fatalf("MR !%d labels = %v, want %s", m.IID, m.Labels, app.LabelCIRed)
		}
	}
	rs := repairTasks(w, "t1")
	if len(rs) != 1 || rs[0].ID != "t3" {
		t.Fatalf("repairs = %+v, want t3", rs)
	}
	t3, err := w.App().Store.Task("t3")
	must(t, err)
	if !strings.Contains(t3.Prompt, "--- FAIL: TestAlpha") {
		t.Fatalf("repair prompt lacks the job trace:\n%s", t3.Prompt)
	}
	if r := w.Saddle("prs"); r.Code == 0 || !strings.Contains(r.Stderr, "CI is red on t1") {
		t.Fatalf("prs over a red layer = %s", r)
	}

	w.WaitTask("t3", "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	if r := w.MustSaddle("land"); w.Task("t3").Status != "landed" {
		t.Fatalf("the repair didn't land: %s", r)
	}
	must(t, w.GL.SetJobs(low.IID, fakeglab.Job{Name: "test", Stage: "test", Status: fakeglab.Success}))
	rep = w.CIRedTick()
	if len(rep.Red) != 0 || !slices.Equal(rep.Cleared, []string{"t1"}) {
		t.Fatalf("after the repair went green: %+v", rep)
	}
	if got := w.OriginFile(w.Task("t1").Branch, "alpha/work.txt"); got != "alpha\nfixed\n" {
		t.Fatalf("t1's MR branch has alpha/work.txt = %q, want the fix folded in", got)
	}
	for _, u := range []string{urls["t1"], urls["t2"]} {
		if m := mrAt(t, w, u); slices.Contains(m.Labels, app.LabelCIRed) {
			t.Fatalf("MR !%d still labeled %s", m.IID, app.LabelCIRed)
		}
	}
	if n := len(w.GLState().MRs); n != 2 {
		t.Fatalf("%d MRs, want t1's and t2's only (the repair folds)", n)
	}
}

// TestJourneyGitLabAutomerge: auto-merge waits while the pipelines run,
// then squash-merges the stack bottom-up on a fast-forward project, pinned
// to each MR's head, retargeting and rebuilding t2's MR in between.
func TestJourneyGitLabAutomerge(t *testing.T) {
	if Fork(t) {
		return
	}
	w := gitlabWorld(t)
	must(t, w.GL.Update(func(s *fakeglab.State) error {
		s.DefaultJobs = []fakeglab.Job{{Name: "test", Stage: "test", Status: fakeglab.Running}}
		s.PipelineMustSucceed = true
		return nil
	}))
	urls := landTwoMRs(t, w)

	w.MustSaddle("automerge", "on")
	st := w.AutomergeTick()
	if st.Merged != "" || len(st.Stacks) != 1 || !strings.Contains(st.Stacks[0].Why, "pending") {
		t.Fatalf("merged on a running pipeline: %+v", st)
	}
	must(t, w.GL.SetAllJobs(fakeglab.Success))

	st = w.AutomergeTick()
	if st.Merged != urls["t1"] {
		t.Fatalf("first tick merged %q, want t1's %s: %+v", st.Merged, urls["t1"], st)
	}
	if m := mrAt(t, w, urls["t1"]); m.State != "merged" || !m.Squash {
		t.Fatalf("t1's MR = %+v, want squash-merged", m)
	}
	if m := mrAt(t, w, urls["t2"]); m.Target != "main" {
		t.Fatalf("after t1 merged, t2's MR targets %s, want main", m.Target)
	}
	// The rebuilt branch is a push: GitLab runs a new pipeline on it.
	must(t, w.GL.SetAllJobs(fakeglab.Success))
	st = w.AutomergeTick()
	if st.Merged != urls["t2"] {
		t.Fatalf("second tick merged %q, want t2's %s: %+v", st.Merged, urls["t2"], st)
	}
	if w.OriginFile("main", "alpha/work.txt") != "alpha\n" || w.OriginFile("main", "beta/work.txt") != "beta\n" {
		t.Fatal("main lacks the merged work")
	}
	if n := w.Git(w.Repo, "rev-list", "--count", "origin/main"); n != "3" {
		t.Fatalf("main has %s commits, want 3 (initial + two squashes)", n)
	}
	var merges []string
	for _, c := range w.GLState().Calls {
		if j := strings.Join(c, " "); strings.Contains(j, "/merge ") {
			merges = append(merges, j)
		}
	}
	if len(merges) != 2 || !strings.Contains(merges[0], "squash=true") || !strings.Contains(merges[0], "sha=") {
		t.Fatalf("merge calls = %q, want two squash merges pinned to a head", merges)
	}
}

// TestJourneyGitLabMergeConflict: someone pushes a conflicting change to
// main. GitLab reports t1's MR as conflicting, and auto-merge leaves it
// alone and says why instead of merging.
func TestJourneyGitLabMergeConflict(t *testing.T) {
	if Fork(t) {
		return
	}
	w := gitlabWorld(t)
	w.Spawn("t1", "Alpha work", []string{"alpha/**"}, finished("alpha", "alpha\n")...)
	w.WaitTask("t1", "queued in the train", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	w.MustSaddle("land")
	w.MustSaddle("prs")
	url := w.Task("t1").PR
	m := mrAt(t, w, url)
	must(t, w.GL.SetAllJobs(fakeglab.Success))

	// A person lands a different alpha/work.txt on main behind saddle's back.
	clone := filepath.Join(w.Root, "other")
	w.Git(w.Root, "clone", "-q", w.Origin, clone)
	must(t, osWrite(filepath.Join(clone, "alpha", "work.txt"), "someone else's alpha\n"))
	w.Git(clone, "add", "-A")
	w.Git(clone, "commit", "-q", "-m", "conflicting alpha")
	w.Git(clone, "push", "-q", "origin", "HEAD:main")

	w.MustSaddle("automerge", "on")
	st := w.AutomergeTick()
	if st.Merged != "" {
		t.Fatalf("auto-merge merged a conflicting MR: %+v", st)
	}
	if len(st.Stacks) != 1 || !strings.Contains(st.Stacks[0].Why, "conflicts with main") {
		t.Fatalf("stacks = %+v, want t1 held for its conflict", st.Stacks)
	}
	if got := mrAt(t, w, url); got.State != "opened" || got.IID != m.IID {
		t.Fatalf("MR = %+v, want it open", got)
	}
	if w.OriginFile("main", "alpha/work.txt") != "someone else's alpha\n" {
		t.Fatal("main changed")
	}
}

func osWrite(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}
