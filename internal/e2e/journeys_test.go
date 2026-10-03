//go:build e2e

package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/e2e/fakegh"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

// TestJourneyInitDoctorFreshRepo: a fresh repo fails doctor only on what
// init fixes, and passes after saddle init with no hand edits.
func TestJourneyInitDoctorFreshRepo(t *testing.T) {
	w := world(t, Options{NoInit: true})

	r := w.Saddle("doctor", "--json")
	if r.Code == 0 {
		t.Fatalf("doctor passed before init: %s", r)
	}
	checks := doctorChecks(t, r.Stdout)
	for _, name := range []string{"ref guard hooks", ".saddle ignored", "state.db"} {
		if c := checks[name]; c.Status != "fail" || !strings.Contains(c.Fix, "saddle init") {
			t.Errorf("before init, %s = %+v, want a fail fixed by saddle init", name, c)
		}
	}
	for _, name := range []string{"git remote", "default branch", "gh auth", "merge settings", "tmux", "claude"} {
		if c := checks[name]; c.Status != "ok" {
			t.Errorf("before init, %s = %+v, want ok", name, c)
		}
	}

	r = w.MustSaddle("init")
	if !strings.Contains(r.Stdout, "initialized "+w.Repo+"/.saddle") {
		t.Fatalf("init output: %s", r)
	}
	r = w.Saddle("doctor", "--json")
	if r.Code != 0 {
		t.Fatalf("doctor fails after init: %s", r)
	}
	for name, c := range doctorChecks(t, r.Stdout) {
		if c.Status == "fail" {
			t.Errorf("after init, %s failed: %+v", name, c)
		}
	}
	// Running init again is harmless.
	w.MustSaddle("init", "-q")
	if r := w.Saddle("doctor"); r.Code != 0 {
		t.Fatalf("doctor after a second init: %s", r)
	}
}

type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix"`
}

func doctorChecks(t *testing.T, out string) map[string]doctorCheck {
	t.Helper()
	var v struct {
		Checks []doctorCheck `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, out)
	}
	m := map[string]doctorCheck{}
	for _, c := range v.Checks {
		m[c.Name] = c
	}
	return m
}

// finished is a fake agent that writes one file under dir, commits and
// calls done.
func finished(dir, body string) []fa.Step {
	return []fa.Step{fa.Write(dir+"/work.txt", body), fa.Commit("work in " + dir), fa.Done("Adds " + dir + "/work.txt.")}
}

// landTwo spawns two agents on disjoint claims, waits for both to call
// done, lands them and opens their PRs. It returns the PR URLs by task.
func landTwo(t *testing.T, w *World) map[string]string {
	t.Helper()
	w.Spawn("t1", "Alpha work", []string{"alpha/**"}, finished("alpha", "alpha\n")...)
	w.Spawn("t2", "Beta work", []string{"beta/**"}, finished("beta", "beta\n")...)
	for _, id := range []string{"t1", "t2"} {
		w.WaitTask(id, "queued in the train", func(v mcpserver.TaskView) bool { return v.Status == "done" && v.Train == "queued" })
	}
	r := w.MustSaddle("land")
	for _, id := range []string{"t1", "t2"} {
		if !strings.Contains(r.Stdout, id) || w.Task(id).Status != "landed" {
			t.Fatalf("%s didn't land: %s", id, r)
		}
	}
	integ := w.Git(w.Repo, "ls-tree", "-r", "--name-only", "saddle/integration")
	if !strings.Contains(integ, "alpha/work.txt") || !strings.Contains(integ, "beta/work.txt") {
		t.Fatalf("integration lacks the work:\n%s", integ)
	}
	r = w.MustSaddle("prs")
	urls := map[string]string{}
	for _, id := range []string{"t1", "t2"} {
		urls[id] = w.Task(id).PR
		if urls[id] == "" || !strings.Contains(r.Stdout, urls[id]) {
			t.Fatalf("%s has no PR: %s", id, r)
		}
	}
	return urls
}

func prNumber(t *testing.T, s *fakegh.State, url string) *fakegh.PR {
	t.Helper()
	for _, p := range s.PRs {
		if s.PRURL(p.Number) == url {
			return p
		}
	}
	t.Fatalf("no PR %s in the fake GitHub", url)
	return nil
}

// TestJourneyParallelLandPRsAutomerge: two agents work in parallel on
// disjoint claims; the train lands both, prs opens a two-layer stack, and
// auto-merge waits for CI, then merges it bottom-up into main, restacking
// and retargeting the top PR after the bottom one merges.
func TestJourneyParallelLandPRsAutomerge(t *testing.T) {
	w := world(t, Options{Tables: "[train]\noutput = \"single\"\n"})
	must(t, w.GH.Update(func(s *fakegh.State) error {
		s.DefaultChecks = []fakegh.Check{{Name: "ci", Workflow: "CI", State: fakegh.Pending}}
		return nil
	}))
	urls := landTwo(t, w)

	s := w.GHState()
	low, high := prNumber(t, s, urls["t1"]), prNumber(t, s, urls["t2"])
	if low.Base != "main" || high.Base != low.Head {
		t.Fatalf("stack bases: %s on %s, %s on %s; want t2 stacked on t1 on main", low.Head, low.Base, high.Head, high.Base)
	}
	if !strings.Contains(high.Body, "Stack") {
		t.Fatalf("top PR body lacks the stack list:\n%s", high.Body)
	}

	// Off by default: a tick merges nothing.
	if st := w.AutomergeTick(); st.Enabled || st.Merged != "" {
		t.Fatalf("auto-merge acted while off: %+v", st)
	}
	w.MustSaddle("automerge", "on")
	st := w.AutomergeTick()
	if st.Merged != "" || len(st.Stacks) != 1 || !strings.Contains(st.Stacks[0].Why, "pending") {
		t.Fatalf("merged on pending CI: %+v", st)
	}
	must(t, w.GH.SetAllChecks(fakegh.Pass))

	st = w.AutomergeTick()
	if st.Merged != urls["t1"] {
		t.Fatalf("first tick merged %q, want t1's %s: %+v", st.Merged, urls["t1"], st)
	}
	s = w.GHState()
	if p := prNumber(t, s, urls["t2"]); p.Base != "main" {
		t.Fatalf("after t1 merged, t2's PR targets %s, want main (restack retargets it)", p.Base)
	}
	if p := prNumber(t, s, urls["t1"]); p.State != "MERGED" || p.MergedBy != "squash" {
		t.Fatalf("t1's PR = %+v", p)
	}
	// t2's branch was rebuilt on the new main: its PR has only its own change.
	if files := w.Git(w.Repo, "diff", "--name-only", "origin/main...origin/saddle/t2-beta-work"); files != "beta/work.txt" {
		t.Fatalf("t2's PR diff after restack = %q", files)
	}

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
	if st := w.AutomergeTick(); st.Merged != "" || len(st.Stacks) != 0 {
		t.Fatalf("stack not empty after both merged: %+v", st)
	}
}
