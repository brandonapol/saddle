package fakeglab

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/forge"
)

// fixture is a bare origin with fake GitLab state, a clone of it, and
// forge.GitLab driving the fake in process.
type fixture struct {
	t     *testing.T
	bare  string
	clone string
	gl    Repo
	f     *forge.GitLab
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d := t.TempDir()
	x := &fixture{t: t, bare: filepath.Join(d, "origin.git"), clone: filepath.Join(d, "work")}
	run(t, d, "git", "init", "-q", "--bare", "-b", "main", x.bare)
	run(t, d, "git", "clone", "-q", x.bare, x.clone)
	x.write("README", "hello\n")
	run(t, x.clone, "git", "add", "-A")
	run(t, x.clone, "git", "commit", "-q", "-m", "init")
	run(t, x.clone, "git", "push", "-q", "origin", "HEAD:main")
	var err error
	if x.gl, err = Init(x.bare, "e2e", "demo"); err != nil {
		t.Fatal(err)
	}
	// forge.CLI's error wording, with Main standing in for the glab binary.
	x.f = &forge.GitLab{Run: func(_ context.Context, args ...string) (string, error) {
		var out, errb bytes.Buffer
		if code := Main(x.clone, args, &out, &errb); code != 0 {
			return strings.TrimSpace(out.String()), errors.New("glab " + strings.Join(args[:min(2, len(args))], " ") + ": " + strings.TrimSpace(errb.String()))
		}
		return strings.TrimSpace(out.String()), nil
	}}
	return x
}

func (x *fixture) write(name, body string) {
	if err := os.WriteFile(filepath.Join(x.clone, name), []byte(body), 0o644); err != nil {
		x.t.Fatal(err)
	}
}

// branch commits name=body on br off base and pushes it.
func (x *fixture) branch(br, base, name, body string) {
	run(x.t, x.clone, "git", "checkout", "-q", "-B", br, "origin/"+base)
	x.write(name, body)
	run(x.t, x.clone, "git", "add", "-A")
	run(x.t, x.clone, "git", "commit", "-q", "-m", br)
	run(x.t, x.clone, "git", "push", "-q", "-f", "origin", br)
	run(x.t, x.clone, "git", "fetch", "-q", "origin")
}

func (x *fixture) state() *State {
	s, err := x.gl.Load()
	if err != nil {
		x.t.Fatal(err)
	}
	return s
}

// TestGitLabStackLifecycle drives forge.GitLab through a two-MR stack on
// the fake: open both, comment and edit the stack note, retarget, read
// mergeability and CI, squash-merge the bottom, retarget the top and merge
// it.
func TestGitLabStackLifecycle(t *testing.T) {
	x := newFixture(t)
	ctx := context.Background()
	x.branch("a", "main", "a.txt", "a\n")
	x.branch("b", "a", "b.txt", "b\n")
	ua, err := x.f.CreatePR(ctx, forge.NewPR{Base: "main", Head: "a", Title: "A", Body: "Closes #1"})
	if err != nil || ua != "https://gitlab.com/e2e/demo/-/merge_requests/1" {
		t.Fatalf("CreatePR a = %q, %v", ua, err)
	}
	ub, err := x.f.CreatePR(ctx, forge.NewPR{Base: "a", Head: "b", Title: "B", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if again, err := x.f.CreatePR(ctx, forge.NewPR{Base: "a", Head: "b", Title: "B"}); err != nil || again != ub {
		t.Fatalf("second CreatePR from b = %q, %v; want the open MR %s", again, err, ub)
	}
	if err := x.f.AddComment(ctx, ub, "<!-- saddle:stack --> base a"); err != nil {
		t.Fatal(err)
	}
	if err := x.f.SetPRBody(ctx, ub, "b2"); err != nil { // adds a system note
		t.Fatal(err)
	}
	cs, err := x.f.Comments(ctx, ub)
	if err != nil || len(cs) != 1 {
		t.Fatalf("Comments = %+v, %v; want saddle's note only", cs, err)
	}
	if err := x.f.EditComment(ctx, cs[0].ID, "<!-- saddle:stack --> base main"); err != nil {
		t.Fatal(err)
	}
	if n := x.state().MR(2).UserNotes(); n[0].Body != "<!-- saddle:stack --> base main" {
		t.Fatalf("note after edit = %+v", n)
	}
	if err := x.f.EnsureLabel(ctx, forge.Label{Name: "ci-red", Color: "B60205"}); err != nil {
		t.Fatal(err)
	}
	if err := x.f.EnsureLabel(ctx, forge.Label{Name: "ci-red", Color: "B60205"}); err != nil {
		t.Fatalf("EnsureLabel on an existing label = %v", err)
	}
	if err := x.f.AddLabel(ctx, ua, "ci-red"); err != nil {
		t.Fatal(err)
	}

	if err := x.gl.SetJobs(1, Job{Name: "test", Stage: "test", Status: Failed, Trace: "\x1b[0Ksection_start:1:step_script\r\x1b[0K$ make check\n--- FAIL: TestX\n"}); err != nil {
		t.Fatal(err)
	}
	pr, err := x.f.PR(ctx, ua, forge.PRState|forge.PRChecks|forge.PRFiles|forge.PRLabels)
	if err != nil {
		t.Fatal(err)
	}
	if pr.State != forge.StateOpen || pr.Mergeable != forge.Mergeable || pr.MergeState != "UNSTABLE" ||
		len(pr.Checks) != 1 || pr.Checks[0].Conclusion != "FAILURE" || pr.Files[0] != "a.txt" || pr.Labels[0] != "ci-red" {
		t.Fatalf("PR a on red CI = %+v", pr)
	}
	checks, err := x.f.Checks(ctx, "a")
	if err != nil || len(checks) != 1 || checks[0].Bucket != "fail" {
		t.Fatalf("Checks by branch = %+v, %v", checks, err)
	}
	l, err := x.f.FailedLog(ctx, checks[0].Link, 10)
	if err != nil || l.Step != "step_script" || !strings.Contains(l.Tail, "--- FAIL: TestX") || !strings.Contains(l.Run, "/-/pipelines/") {
		t.Fatalf("FailedLog = %+v, %v", l, err)
	}
	if err := x.f.RerunFailed(ctx, l.Run[strings.LastIndex(l.Run, "/")+1:]); err != nil {
		t.Fatal(err)
	}
	if j := x.state().MR(1).Pipeline.Jobs[0]; j.Status != Pending || j.Retried != 1 {
		t.Fatalf("job after retry = %+v", j)
	}
	if err := x.gl.SetJobs(1, Job{Name: "test", Stage: "test", Status: Success}); err != nil {
		t.Fatal(err)
	}
	if pr, _ := x.f.PR(ctx, ua, forge.PRMergeState); pr.MergeState != "CLEAN" {
		t.Fatalf("merge state on green = %s", pr.MergeState)
	}
	ms, err := x.f.MergeSettings(ctx)
	if err != nil || ms.Merge || !ms.Squash || ms.Repo != "e2e/demo" {
		t.Fatalf("MergeSettings = %+v, %v", ms, err)
	}
	if err := x.f.MergePR(ctx, ua, "squash", "0000"); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("MergePR on a stale head = %v, want 409", err)
	}
	if err := x.f.MergePR(ctx, ua, "squash", pr.HeadSHA); err != nil {
		t.Fatal(err)
	}
	if pr, _ := x.f.PR(ctx, ua, forge.PRState); pr.State != forge.StateMerged {
		t.Fatalf("a after merge = %s", pr.State)
	}

	// b still targets a, and holds a's commit, which main now has squashed:
	// on a fast-forward project it needs a rebase before it can merge.
	if err := x.f.SetPRBase(ctx, ub, "main"); err != nil {
		t.Fatal(err)
	}
	if pr, _ := x.f.PR(ctx, ub, forge.PRMergeState|forge.PRBase); pr.Base != "main" || pr.MergeState != "BEHIND" {
		t.Fatalf("b retargeted = %+v, want main and BEHIND", pr)
	}
	run(t, x.clone, "git", "fetch", "-q", "origin")
	run(t, x.clone, "git", "checkout", "-q", "b")
	run(t, x.clone, "git", "rebase", "-q", "--onto", "origin/main", "origin/a")
	run(t, x.clone, "git", "push", "-q", "-f", "origin", "b")
	pr, _ = x.f.PR(ctx, ub, forge.PRMergeState|forge.PRHead|forge.PRFiles)
	if pr.MergeState != "CLEAN" || len(pr.Files) != 1 || pr.Files[0] != "b.txt" {
		t.Fatalf("b restacked = %+v", pr)
	}
	if err := x.f.MergePR(ctx, ub, "squash", pr.HeadSHA); err != nil {
		t.Fatal(err)
	}
	if got := run(t, x.bare, "git", "log", "--format=%s", "main"); got != "B\nA\ninit" {
		t.Fatalf("main history = %q, want two squash commits", got)
	}
	open, err := x.f.ListPRs(ctx, forge.PRQuery{State: "open", Limit: 10}, forge.PRURL)
	if err != nil || len(open) != 0 {
		t.Fatalf("open MRs = %+v, %v", open, err)
	}
	merged, err := x.f.ListPRs(ctx, forge.PRQuery{State: "merged", Head: "a", Limit: 1}, forge.PRURL|forge.PRMergeState)
	if err != nil || len(merged) != 1 || merged[0].URL != ua {
		t.Fatalf("merged MR from a = %+v, %v", merged, err)
	}
}

// A merge while the pipeline runs sets "merge when pipeline succeeds";
// GitLab merges once it passes.
func TestMergeWhenPipelineSucceeds(t *testing.T) {
	x := newFixture(t)
	ctx := context.Background()
	x.branch("a", "main", "a.txt", "a\n")
	u, err := x.f.CreatePR(ctx, forge.NewPR{Base: "main", Head: "a", Title: "A"})
	if err != nil {
		t.Fatal(err)
	}
	must(t, x.gl.SetAllJobs(Running))
	err = x.f.MergePR(ctx, u, "squash", "")
	if err == nil || !strings.Contains(err.Error(), "merge when its pipeline succeeds") {
		t.Fatalf("MergePR on a running pipeline = %v", err)
	}
	if m := x.state().MR(1); m.State != "opened" || !m.AutoMerge {
		t.Fatalf("MR = %+v, want open and set to auto-merge", m)
	}
	must(t, x.gl.SetAllJobs(Success))
	if m := x.state().MR(1); m.State != "merged" || !m.Squash {
		t.Fatalf("MR after the pipeline passed = %+v, want squash-merged", m)
	}
}

// A conflicting MR reads CONFLICTING/DIRTY and refuses to merge.
func TestConflict(t *testing.T) {
	x := newFixture(t)
	ctx := context.Background()
	x.branch("a", "main", "README", "mine\n")
	x.branch("main", "main", "README", "theirs\n")
	u, err := x.f.CreatePR(ctx, forge.NewPR{Base: "main", Head: "a", Title: "A"})
	if err != nil {
		t.Fatal(err)
	}
	pr, err := x.f.PR(ctx, u, forge.PRMergeable|forge.PRMergeState)
	if err != nil || pr.Mergeable != forge.Conflicting || pr.MergeState != "DIRTY" {
		t.Fatalf("PR = %+v, %v; want CONFLICTING and DIRTY", pr, err)
	}
	if err := x.f.MergePR(ctx, u, "squash", ""); err == nil || !strings.Contains(err.Error(), "406") {
		t.Fatalf("MergePR on a conflict = %v, want 406", err)
	}
}

func TestIssuesAndAuth(t *testing.T) {
	x := newFixture(t)
	ctx := context.Background()
	is, err := x.f.CreateIssue(ctx, forge.NewIssue{Title: "Blocked", Body: "why", Labels: []string{"saddle:blocker"}})
	if err != nil || is.Number != 1 || !strings.HasSuffix(is.URL, "/-/issues/1") {
		t.Fatalf("CreateIssue = %+v, %v", is, err)
	}
	if l, err := x.f.ListIssues(ctx, forge.IssueQuery{Label: "saddle:blocker", State: "open", Limit: 10}, forge.IssueNumber); err != nil || len(l) != 1 {
		t.Fatalf("ListIssues = %+v, %v", l, err)
	}
	must(t, x.f.CloseIssue(ctx, 1, "fixed"))
	if got, _ := x.f.Issue(ctx, "", 1, forge.IssueState); got.State != forge.StateClosed {
		t.Fatalf("issue after close = %+v", got)
	}
	if got, _ := x.f.Issue(ctx, "e2e/demo", 1, forge.IssueBody); got.Body != "why" {
		t.Fatalf("issue by project path = %+v", got)
	}
	must(t, x.f.AuthStatus(ctx))
	if _, err := x.f.Issue(ctx, "", 99, forge.IssueState); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("missing issue = %v, want 404", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
