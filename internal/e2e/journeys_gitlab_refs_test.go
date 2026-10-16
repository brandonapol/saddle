//go:build e2e

package e2e

import (
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/e2e/fakeglab"
)

// TestJourneyGitLabMRRefs (#652): the commands that take a task or PR find
// GitLab MRs by "!N" and by URL, with a page after the number or without:
// automerge hold and release, unstack and repair. An MR saddle doesn't know
// is refused by name.
func TestJourneyGitLabMRRefs(t *testing.T) {
	if Fork(t) {
		return
	}
	w := gitlabWorld(t)
	urls := landTwoMRs(t, w)

	if r := w.MustSaddle("automerge", "hold", urls["t1"]+"/diffs"); !strings.Contains(r.Stdout, "t1's stack is held") {
		t.Fatalf("hold by MR URL: %s", r)
	}
	if out := w.MustSaddle("automerge", "status").Stdout; !strings.Contains(out, "held") {
		t.Fatalf("status doesn't say t1 is held:\n%s", out)
	}
	if r := w.MustSaddle("automerge", "release", "!1"); !strings.Contains(r.Stdout, "t1's stack is released") {
		t.Fatalf("release by !1: %s", r)
	}
	if r := w.Saddle("automerge", "hold", "!99"); r.Code == 0 || !strings.Contains(r.Stderr, `no task or PR "!99"`) {
		t.Fatalf("hold of an unknown MR: %s", r)
	}

	if r := w.MustSaddle("unstack", "!2"); !strings.Contains(r.Stdout, "t2 is out of the PR stack") {
		t.Fatalf("unstack !2: %s", r)
	}
	if v := w.Task("t2"); !strings.HasPrefix(v.Train, "superseded") {
		t.Fatalf("t2 after unstack !2 = %+v", v)
	}

	must(t, fa.Script{Steps: []fa.Step{fa.Done("Nothing to redo.")}}.Save(w.Scripts, "t3"))
	if r := w.MustSaddle("repair", urls["t1"]); !strings.Contains(r.Stdout, "t1") {
		t.Fatalf("repair by MR URL: %s", r)
	}
	if v := w.Task("t1"); !strings.Contains(v.Train, "repairing") {
		t.Fatalf("t1 after repair = %+v, want out of the stack for repair", v)
	}
}

// TestJourneyGitLabCIRedRetriesPipeline (#652): t1's job fails on a GitLab
// runner system failure. The ci-red watcher retries its pipeline once
// instead of spawning a repair, as it reruns an Actions run on GitHub; when
// the retried job fails the same way it counts as red.
func TestJourneyGitLabCIRedRetriesPipeline(t *testing.T) {
	if Fork(t) {
		return
	}
	w := gitlabWorld(t)
	urls := landTwoMRs(t, w)
	low, high := mrAt(t, w, urls["t1"]), mrAt(t, w, urls["t2"])
	infra := fakeglab.Job{Name: "test", Stage: "test", Status: fakeglab.Failed,
		Trace: "Running with gitlab-runner 17.4.0\nERROR: Job failed (system failure): prepare environment: exit code 1\n"}
	must(t, w.GL.SetJobs(low.IID, infra))
	must(t, w.GL.SetJobs(high.IID, fakeglab.Job{Name: "test", Stage: "test", Status: fakeglab.Success}))
	pipeline := mrAt(t, w, urls["t1"]).Pipeline.ID

	rep := w.CIRedTick()
	if len(rep.Red) != 0 {
		t.Fatalf("an infra failure went red before its retry: %+v", rep.Red)
	}
	m := mrAt(t, w, urls["t1"])
	if m.Pipeline.ID != pipeline || m.Pipeline.Jobs[0].Retried != 1 {
		t.Fatalf("pipeline %d jobs = %+v, want pipeline %d's job retried once", m.Pipeline.ID, m.Pipeline.Jobs, pipeline)
	}
	if slices.Contains(m.Labels, app.LabelCIRed) || len(repairTasks(w, "t1")) != 0 {
		t.Fatalf("a retried pipeline was labeled (%v) or repaired (%+v)", m.Labels, repairTasks(w, "t1"))
	}
	var retries []string
	for _, c := range w.GLState().Calls {
		if j := strings.Join(c, " "); strings.Contains(j, "/retry") {
			retries = append(retries, j)
		}
	}
	if len(retries) != 1 || !strings.Contains(retries[0], "POST") || !strings.Contains(retries[0], "pipelines/"+itoa(pipeline)+"/retry") {
		t.Fatalf("retry calls = %q, want one POST of pipeline %d", retries, pipeline)
	}

	// The retried job fails the same way on the same head: now it is red.
	must(t, fa.Script{Steps: []fa.Step{fa.Done("Nothing to fix in the code.")}}.Save(w.Scripts, "t3"))
	must(t, w.GL.Update(func(s *fakeglab.State) error {
		s.MR(low.IID).Pipeline.Jobs[0].Status = fakeglab.Failed
		return nil
	}))
	rep = w.CIRedTick()
	if len(rep.Red) != 1 || rep.Red[0].Task != "t1" {
		t.Fatalf("after the retry failed too: red = %+v, want t1", rep.Red)
	}
}
