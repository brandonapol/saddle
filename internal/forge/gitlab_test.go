package forge

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/config"
)

func (f *fakeGH) gitlab() *GitLab { return &GitLab{Run: f.run} }

// TestGitLabPRArgs pins each MR operation to the glab api call it makes.
func TestGitLabPRArgs(t *testing.T) {
	ctx := context.Background()
	f := &fakeGH{replies: []string{
		`{"iid":7,"web_url":"https://gitlab.com/g/r/-/merge_requests/7"}`, // create
		"", "", "", "", // retarget, body, close, comment
		"", "", "", // edit comment, label, unlabel
		"", "", "", // ensure label, retry, job retry
		"", // auth
	}}
	g := f.gitlab()
	url, err := g.CreatePR(ctx, NewPR{Base: "main", Head: "b", Title: "T", Body: "Closes #3", Draft: true})
	if err != nil || url != "https://gitlab.com/g/r/-/merge_requests/7" {
		t.Fatalf("CreatePR = %q, %v", url, err)
	}
	_ = g.SetPRBase(ctx, "https://gitlab.com/g/r/-/merge_requests/7", "main")
	_ = g.SetPRBody(ctx, "!7", "body")
	_ = g.ClosePR(ctx, "7")
	_ = g.AddComment(ctx, "#7", "hi")
	_ = g.EditComment(ctx, "7:55", "new")
	_ = g.AddLabel(ctx, "7", "saddle:needs-human")
	_ = g.RemoveLabel(ctx, "7", "ci-red")
	_ = g.EnsureLabel(ctx, Label{Name: "l", Color: "D93F0B", Description: "d"})
	_ = g.RerunFailed(ctx, "901")
	_ = g.RerunFailed(ctx, RunID("https://gitlab.com/g/r/-/jobs/31"))
	_ = g.AuthStatus(ctx)
	f.want(t,
		"api -X POST projects/:id/merge_requests -f source_branch=b -f target_branch=main -f title=Draft: T -f description=Closes #3",
		"api -X PUT projects/:id/merge_requests/7 -f target_branch=main",
		"api -X PUT projects/:id/merge_requests/7 -f description=body",
		"api -X PUT projects/:id/merge_requests/7 -f state_event=close",
		"api -X POST projects/:id/merge_requests/7/notes -f body=hi",
		"api -X PUT projects/:id/merge_requests/7/notes/55 -f body=new",
		"api -X PUT projects/:id/merge_requests/7 -f add_labels=saddle:needs-human",
		"api -X PUT projects/:id/merge_requests/7 -f remove_labels=ci-red",
		"api -X POST projects/:id/labels -f name=l -f color=#D93F0B -f description=d",
		"api -X POST projects/:id/pipelines/901/retry",
		"api -X POST projects/:id/jobs/31/retry",
		"auth status",
	)
}

// A self-hosted GitLab named in [forge] host gets --hostname on every call.
func TestGitLabHost(t *testing.T) {
	f := &fakeGH{}
	g := &GitLab{Run: f.run, Host: "git.corp"}
	_ = g.ClosePR(context.Background(), "3")
	_ = g.AuthStatus(context.Background())
	f.want(t, "api --hostname git.corp -X PUT projects/:id/merge_requests/3 -f state_event=close", "auth status --hostname git.corp")
}

// A branch ref resolves to the newest open MR from it, then to any.
func TestGitLabBranchRef(t *testing.T) {
	f := &fakeGH{replies: []string{`[]`, `[{"iid":4}]`, ""}}
	if err := f.gitlab().SetPRBase(context.Background(), "saddle/t1", "main"); err != nil {
		t.Fatal(err)
	}
	f.want(t,
		"api projects/:id/merge_requests?order_by=created_at&per_page=1&sort=desc&source_branch=saddle%2Ft1&state=opened",
		"api projects/:id/merge_requests?order_by=created_at&per_page=1&sort=desc&source_branch=saddle%2Ft1&state=all",
		"api -X PUT projects/:id/merge_requests/4 -f target_branch=main")
}

// A second MR from one branch hands back the open one, as gh's "already
// exists" does for publish.
func TestGitLabCreateExisting(t *testing.T) {
	calls := 0
	g := &GitLab{Run: func(_ context.Context, args ...string) (string, error) {
		calls++
		if calls == 1 {
			return `{"message":["Another open merge request already exists for this source branch: !2"]}`,
				errors.New("glab api: exit status 1: glab: 409 Conflict")
		}
		return `[{"iid":2,"web_url":"https://gitlab.com/g/r/-/merge_requests/2"}]`, nil
	}}
	url, err := g.CreatePR(context.Background(), NewPR{Base: "main", Head: "b", Title: "t"})
	if err != nil || url != "https://gitlab.com/g/r/-/merge_requests/2" {
		t.Fatalf("CreatePR on an existing MR = %q, %v", url, err)
	}
}

// TestGitLabPRRead maps an MR, its diffs and its head pipeline's jobs onto
// PR in saddle's (GitHub's) words.
func TestGitLabPRRead(t *testing.T) {
	f := &fakeGH{replies: []string{
		`{"iid":4,"web_url":"https://gitlab.com/g/r/-/merge_requests/4","title":"T","state":"opened","draft":false,
		  "has_conflicts":false,"detailed_merge_status":"mergeable","target_branch":"main","source_branch":"saddle/t1",
		  "sha":"abc","description":"Closes #9","labels":["ci-red"],"head_pipeline":{"id":77,"status":"failed"}}`,
		`[{"new_path":"a.go","old_path":"a.go"},{"new_path":"","old_path":"gone.go"}]`,
		`[{"id":1,"name":"test","stage":"test","status":"failed","started_at":"2026-01-01T00:00:00Z"},
		  {"id":2,"name":"lint","stage":"test","status":"failed","allow_failure":true},
		  {"id":3,"name":"build","stage":"build","status":"running"},
		  {"id":4,"name":"deploy","stage":"deploy","status":"manual"}]`,
	}}
	pr, err := f.gitlab().PR(context.Background(), "4", PRState|PRFiles|PRChecks)
	if err != nil {
		t.Fatal(err)
	}
	want := PR{Number: 4, URL: "https://gitlab.com/g/r/-/merge_requests/4", Title: "T", State: StateOpen, Mergeable: Mergeable,
		MergeState: "UNSTABLE", Base: "main", Head: "saddle/t1", HeadSHA: "abc", Body: "Closes #9", Labels: []string{"ci-red"},
		Files: []string{"a.go", "gone.go"}, Checks: []CheckRun{
			{Kind: KindJob, Name: "test", Workflow: "test", Status: "COMPLETED", Conclusion: "FAILURE", StartedAt: "2026-01-01T00:00:00Z"},
			{Kind: KindJob, Name: "lint", Workflow: "test", Status: "COMPLETED", Conclusion: "NEUTRAL"},
			{Kind: KindJob, Name: "build", Workflow: "build", Status: "IN_PROGRESS"},
			{Kind: KindJob, Name: "deploy", Workflow: "deploy", Status: "COMPLETED", Conclusion: "SKIPPED"},
		}}
	if !reflect.DeepEqual(pr, want) {
		t.Fatalf("PR =\n %+v\nwant\n %+v", pr, want)
	}
	f.want(t, "api projects/:id/merge_requests/4", "api projects/:id/merge_requests/4/diffs?per_page=100",
		"api projects/:id/pipelines/77/jobs?per_page=100")
}

func TestGitLabMergeState(t *testing.T) {
	for _, tc := range []struct {
		m                 mrJSON
		mergeable, mstate string
	}{
		{mrJSON{State: "opened", DetailedMergeStatus: "mergeable"}, Mergeable, "CLEAN"},
		{mrJSON{State: "opened", DetailedMergeStatus: "conflict", HasConflicts: true}, Conflicting, "DIRTY"},
		{mrJSON{State: "opened", DetailedMergeStatus: "need_rebase"}, Mergeable, "BEHIND"},
		{mrJSON{State: "opened", DetailedMergeStatus: "draft_status"}, Mergeable, "DRAFT"},
		{mrJSON{State: "opened", DetailedMergeStatus: "checking"}, Unknown, "UNKNOWN"},
		{mrJSON{State: "opened", DetailedMergeStatus: "ci_must_pass"}, Mergeable, "BLOCKED"},
		{mrJSON{State: "opened", DetailedMergeStatus: "not_approved"}, Mergeable, "BLOCKED"},
		{mrJSON{State: "opened", MergeStatus: "can_be_merged"}, Mergeable, "CLEAN"}, // before detailed_merge_status
		{mrJSON{State: "opened", MergeStatus: "cannot_be_merged"}, Conflicting, "BLOCKED"},
		{mrJSON{State: "merged", DetailedMergeStatus: "not_open"}, Unknown, "UNKNOWN"},
	} {
		if got := tc.m.mergeable(); got != tc.mergeable {
			t.Errorf("%+v mergeable = %s, want %s", tc.m, got, tc.mergeable)
		}
		if got := tc.m.mergeState(); got != tc.mstate {
			t.Errorf("%+v merge state = %s, want %s", tc.m, got, tc.mstate)
		}
	}
}

// MergePR squashes through the merge API pinned to head, and asks GitLab
// to merge when the pipeline succeeds only while it is running, which is
// not yet a merge.
func TestGitLabMergePR(t *testing.T) {
	ctx := context.Background()
	f := &fakeGH{replies: []string{`{"iid":5,"head_pipeline":{"id":1,"status":"success"}}`, `{"state":"merged"}`}}
	if err := f.gitlab().MergePR(ctx, "5", "squash", "abc"); err != nil {
		t.Fatal(err)
	}
	f.want(t, "api projects/:id/merge_requests/5", "api -X PUT projects/:id/merge_requests/5/merge -F squash=true -f sha=abc")

	f = &fakeGH{replies: []string{`{"iid":5,"head_pipeline":{"id":1,"status":"running"}}`, `{"state":"opened"}`}}
	err := f.gitlab().MergePR(ctx, "5", "rebase", "")
	if err == nil || !strings.Contains(err.Error(), "merge when its pipeline succeeds") {
		t.Fatalf("MergePR on a running pipeline = %v, want it scheduled, not merged", err)
	}
	f.want(t, "api projects/:id/merge_requests/5", "api -X PUT projects/:id/merge_requests/5/merge -F squash=false -F merge_when_pipeline_succeeds=true")
}

func TestGitLabMergeSettings(t *testing.T) {
	for _, tc := range []struct {
		json string
		want MergeSettings
	}{
		{`{"path_with_namespace":"g/r","web_url":"u","merge_method":"ff","squash_option":"default_on"}`,
			MergeSettings{Repo: "g/r", URL: "u", Squash: true, Rebase: true}},
		{`{"path_with_namespace":"g/r","web_url":"u","merge_method":"merge","squash_option":"never"}`,
			MergeSettings{Repo: "g/r", URL: "u", Merge: true}},
		{`{"path_with_namespace":"g/r","web_url":"u","merge_method":"rebase_merge","squash_option":"always"}`,
			MergeSettings{Repo: "g/r", URL: "u", Merge: true, Squash: true}},
	} {
		f := &fakeGH{replies: []string{tc.json}}
		s, err := f.gitlab().MergeSettings(context.Background())
		if err != nil || s != tc.want {
			t.Errorf("MergeSettings(%s) = %+v, %v; want %+v", tc.json, s, err, tc.want)
		}
	}
}

func TestGitLabChecksAndLog(t *testing.T) {
	ctx := context.Background()
	f := &fakeGH{replies: []string{
		`{"iid":4,"head_pipeline":{"id":77,"status":"failed"}}`,
		`[{"id":11,"name":"test","stage":"test","status":"failed","web_url":"https://gitlab.com/g/r/-/jobs/11"},
		  {"id":12,"name":"build","stage":"build","status":"pending","web_url":"https://gitlab.com/g/r/-/jobs/12"},
		  {"id":13,"name":"old","stage":"test","status":"canceled","web_url":"https://gitlab.com/g/r/-/jobs/13"}]`,
		`{"id":11,"pipeline":{"id":77,"web_url":"https://gitlab.com/g/r/-/pipelines/77"}}`,
		"\x1b[0Ksection_start:1700000000:step_script\r\x1b[0K\x1b[32;1m$ go test ./...\x1b[0;m\n--- FAIL: TestThing\nprogress 10%\rprogress 100%\nFAIL\tdemo\nsection_end:1700000001:step_script\r\x1b[0K\n",
	}}
	g := f.gitlab()
	cs, err := g.Checks(ctx, "4")
	if err != nil {
		t.Fatal(err)
	}
	want := []CICheck{{"test", "test", "fail", "https://gitlab.com/g/r/-/jobs/11"}, {"build", "build", "pending", "https://gitlab.com/g/r/-/jobs/12"},
		{"old", "test", "cancel", "https://gitlab.com/g/r/-/jobs/13"}}
	if !reflect.DeepEqual(cs, want) {
		t.Fatalf("Checks = %+v", cs)
	}
	l, err := g.FailedLog(ctx, cs[0].Link, 3)
	if err != nil {
		t.Fatal(err)
	}
	if l.Run != "https://gitlab.com/g/r/-/pipelines/77" || l.Step != "step_script" || l.Tail != "--- FAIL: TestThing\nprogress 100%\nFAIL\tdemo" {
		t.Fatalf("FailedLog = %+v", l)
	}
	if l, err := g.FailedLog(ctx, "https://ci.example/build/1", 3); err != nil || l.Run != "https://ci.example/build/1" {
		t.Fatalf("FailedLog of a non-job link = %+v, %v", l, err)
	}
	f.want(t, "api projects/:id/merge_requests/4", "api projects/:id/pipelines/77/jobs?per_page=100",
		"api projects/:id/jobs/11", "api projects/:id/jobs/11/trace")

	f = &fakeGH{replies: []string{`{"iid":4}`}}
	if cs, err := f.gitlab().Checks(ctx, "4"); err != nil || cs != nil {
		t.Fatalf("Checks with no pipeline = %+v, %v; want none", cs, err)
	}
}

func TestGitLabIssues(t *testing.T) {
	ctx := context.Background()
	f := &fakeGH{replies: []string{
		`{"iid":9,"title":"T","state":"closed","description":"B","web_url":"https://gitlab.com/g/r/-/issues/9","labels":["x"],"created_at":"2026-01-02T03:04:05Z"}`,
		`[{"iid":1,"state":"opened"}]`,
		`{"iid":10,"web_url":"https://gitlab.com/o/p/-/issues/10","title":"N","state":"opened"}`,
		"", "", "", "", "",
	}}
	g := f.gitlab()
	is, err := g.Issue(ctx, "", 9, IssueBody)
	if err != nil || is.Number != 9 || is.State != StateClosed || is.Body != "B" || is.Created.IsZero() || is.Labels[0] != "x" {
		t.Fatalf("Issue = %+v, %v", is, err)
	}
	if l, err := g.ListIssues(ctx, IssueQuery{Label: "saddle:blocker", State: "open", Limit: 5}, IssueNumber); err != nil || len(l) != 1 || l[0].State != StateOpen {
		t.Fatalf("ListIssues = %+v, %v", l, err)
	}
	n, err := g.CreateIssue(ctx, NewIssue{Repo: "o/p", Title: "N", Body: "b", Labels: []string{"a", "b"}})
	if err != nil || n.Number != 10 || n.URL != "https://gitlab.com/o/p/-/issues/10" || len(n.Labels) != 2 {
		t.Fatalf("CreateIssue = %+v, %v", n, err)
	}
	_ = g.CommentIssue(ctx, 9, "c")
	_ = g.EditIssueBody(ctx, 9, "b2")
	_ = g.CloseIssue(ctx, 9, "done")
	_ = g.ReopenIssue(ctx, 9)
	f.want(t,
		"api projects/:id/issues/9",
		"api projects/:id/issues?labels=saddle%3Ablocker&order_by=created_at&per_page=5&sort=desc&state=opened",
		"api -X POST projects/o%2Fp/issues -f title=N -f description=b -f labels=a,b",
		"api -X POST projects/:id/issues/9/notes -f body=c",
		"api -X PUT projects/:id/issues/9 -f description=b2",
		"api -X POST projects/:id/issues/9/notes -f body=done",
		"api -X PUT projects/:id/issues/9 -f state_event=close",
		"api -X PUT projects/:id/issues/9 -f state_event=reopen",
	)
}

// Sub-issues are linked issues on GitLab.
func TestGitLabSubIssues(t *testing.T) {
	ctx := context.Background()
	f := &fakeGH{replies: []string{`[{"iid":4,"title":"a","state":"opened"},{"iid":6,"state":"closed"}]`, `{"iid":7,"project_id":42}`, ""}}
	g := f.gitlab()
	if ns, err := g.SubIssueNumbers(ctx, "", 3); err != nil || !reflect.DeepEqual(ns, []int{4, 6}) {
		t.Fatalf("SubIssueNumbers = %v, %v", ns, err)
	}
	if err := g.AddSubIssue(ctx, "g/r", 3, 7); err != nil {
		t.Fatal(err)
	}
	f.want(t, "api projects/:id/issues/3/links", "api projects/g%2Fr/issues/7",
		"api -X POST projects/g%2Fr/issues/3/links -f target_project_id=42 -f target_issue_iid=7")
}

func TestGitLabComments(t *testing.T) {
	f := &fakeGH{replies: []string{`[{"id":1,"body":"added 1 commit","system":true},{"id":2,"body":"<!-- saddle:stack -->"}]`}}
	cs, err := f.gitlab().Comments(context.Background(), "!3")
	if err != nil || len(cs) != 1 || cs[0].ID != "3:2" {
		t.Fatalf("Comments = %+v, %v; want the one user note, id mr:note", cs, err)
	}
	if err := f.gitlab().EditComment(context.Background(), "IC_1", "x"); err == nil {
		t.Fatal("EditComment took a GitHub comment id")
	}
}

func TestMRIID(t *testing.T) {
	for ref, want := range map[string]string{
		"12": "12", "!12": "12", "#12": "12",
		"https://gitlab.com/g/s/r/-/merge_requests/12":       "12",
		"https://git.corp/g/r/-/merge_requests/12/diffs":     "12",
		"https://git.corp/g/r/-/merge_requests/12#note_3443": "12",
		"saddle/t1-x": "", "": "",
	} {
		if got, _ := mrIID(ref); got != want {
			t.Errorf("mrIID(%q) = %q, want %q", ref, got, want)
		}
	}
}

// TestNewPicksGitLabFromOrigin: with [forge] unset, a checkout (or a
// worktree of one) whose origin is on a GitLab host gets GitLab, without
// starting a process; [forge] still wins.
func TestNewPicksGitLabFromOrigin(t *testing.T) {
	repo := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(repo, "init", "-q", "-b", "main")
	git(repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x")
	git(repo, "remote", "add", "upstream", "git@github.com:o/r.git")
	git(repo, "remote", "add", "origin", "git@gitlab.example.com:g/r.git")
	wt := filepath.Join(t.TempDir(), "wt")
	git(repo, "worktree", "add", "-q", wt)
	sub := filepath.Join(wt, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "") // no git, gh or glab: selection must not need them
	for _, dir := range []string{repo, sub} {
		if got := OriginURL(dir); got != "git@gitlab.example.com:g/r.git" {
			t.Fatalf("OriginURL(%s) = %q", dir, got)
		}
		if f := New(config.Forge{}, dir); f.Provider() != config.ForgeGitLab {
			t.Fatalf("New in %s = %T, want GitLab", dir, f)
		}
	}
	if f := New(config.Forge{Provider: config.ForgeGitHub}, repo); f.Provider() != config.ForgeGitHub {
		t.Fatalf("[forge] provider = github on a GitLab origin = %T", f)
	}
	if f, ok := New(config.Forge{Host: "gitlab.example.com"}, repo).(*GitLab); !ok || f.Host != "gitlab.example.com" {
		t.Fatalf("[forge] host not passed to GitLab: %+v", f)
	}
	if got := OriginURL(t.TempDir()); got != "" {
		t.Fatalf("OriginURL outside a repo = %q", got)
	}
}

func TestRemoteHost(t *testing.T) {
	for url, want := range map[string]string{
		"git@github.com:o/r.git":                "github.com",
		"https://github.com/o/r":                "github.com",
		"ssh://git@gitlab.example.com:2222/o/r": "gitlab.example.com",
		"https://user:tok@gitlab.com/g/s/r.git": "gitlab.com",
		"gitlab.com:o/r.git":                    "gitlab.com",
		"/srv/git/r.git":                        "",
		"":                                      "",
	} {
		if got := RemoteHost(url); got != want {
			t.Errorf("RemoteHost(%q) = %q, want %q", url, got, want)
		}
	}
}

// TestNewGitLabUsesGlabOnPATH drives the selected GitLab forge through a
// glab on PATH, as the e2e harness does with internal/e2e/fakeglab.
func TestNewGitLabUsesGlabOnPATH(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, Glab), []byte("#!/bin/sh\necho \"{\\\"web_url\\\":\\\"$*\\\"}\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	url, err := New(config.Forge{Provider: config.ForgeGitLab}, t.TempDir()).CreatePR(context.Background(), NewPR{Base: "main", Head: "b", Title: "t", Body: "x"})
	if err != nil || url != "api -X POST projects/:id/merge_requests -f source_branch=b -f target_branch=main -f title=t -f description=x" {
		t.Fatalf("CreatePR via fake glab = %q, %v", url, err)
	}
}

// glab prints GitLab's reason ("Label already exists") on stdout and only
// the HTTP status on stderr, so EnsureLabel must read both.
func TestGitLabEnsureLabelExists(t *testing.T) {
	var calls []string
	g := &GitLab{Run: func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		if len(calls) == 1 {
			return `{"message":"Label already exists"}`, errors.New("glab api -X: exit status 1: glab: 409 Conflict (HTTP 409)")
		}
		return "", nil
	}}
	if err := g.EnsureLabel(context.Background(), Label{Name: "ci red", Color: "B60205"}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[1] != "api -X PUT projects/:id/labels/ci%20red -f color=#B60205" {
		t.Fatalf("calls = %q, want an update after the 409", calls)
	}
}
