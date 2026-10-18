package forge

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/config"
)

// fakeGH records gh's args and answers each call with the next reply.
type fakeGH struct {
	calls   [][]string
	replies []string
	err     error
}

func (f *fakeGH) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if f.err != nil {
		return "", f.err
	}
	if len(f.replies) == 0 {
		return "", nil
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	return r, nil
}

func (f *fakeGH) github() *GitHub { return &GitHub{Run: f.run} }

func (f *fakeGH) want(t *testing.T, want ...string) {
	t.Helper()
	var got []string
	for _, c := range f.calls {
		got = append(got, strings.Join(c, " "))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gh calls:\n got %q\nwant %q", got, want)
	}
}

// TestGitHubPRArgs pins each PR operation to the gh call saddle made before
// the forge layer, so GitHub behaviour doesn't change.
func TestGitHubPRArgs(t *testing.T) {
	ctx := context.Background()
	f := &fakeGH{replies: []string{"Creating pull request…\nhttps://github.com/o/r/pull/7"}}
	g := f.github()
	url, err := g.CreatePR(ctx, NewPR{Base: "main", Head: "b", Title: "T", Body: "B", Draft: true})
	if err != nil || url != "https://github.com/o/r/pull/7" {
		t.Fatalf("CreatePR = %q, %v", url, err)
	}
	_ = g.SetPRBase(ctx, "u", "main")
	_ = g.SetPRBody(ctx, "u", "body")
	_ = g.ClosePR(ctx, "u")
	_ = g.AddComment(ctx, "u", "hi")
	_ = g.EditComment(ctx, "IC_1", "new")
	_ = g.AddLabel(ctx, "u", "saddle:needs-human")
	_ = g.EnsureLabel(ctx, Label{Name: "l", Description: "d"})
	_ = g.EnsureLabel(ctx, Label{Name: "l", Color: "D93F0B", Description: "d"})
	_ = g.RerunFailed(ctx, "42")
	_ = g.AuthStatus(ctx)
	f.want(t,
		"pr create --base main --head b --title T --body B --draft",
		"pr edit u --base main",
		"pr edit u --body body",
		"pr close u",
		"pr comment u --body hi",
		"api graphql -f query=mutation($id: ID!, $body: String!) { updateIssueComment(input: {id: $id, body: $body}) { issueComment { id } } } -f id=IC_1 -f body=new",
		"pr edit u --add-label saddle:needs-human",
		"label create l --force --description d",
		"label create l --color D93F0B --force --description d",
		"run rerun 42 --failed",
		"auth status",
	)
}

func TestGitHubPRRead(t *testing.T) {
	ctx := context.Background()
	f := &fakeGH{replies: []string{
		`{"state":"OPEN","mergeable":"CONFLICTING","baseRefName":"main"}`,
		`{"state":"MERGED","baseRefName":"main","headRefOid":"abc"}`,
		`{"body":"hello"}`,
		`[{"body":"Closes #3"},{"body":"x"}]`,
		`{"comments":[{"id":"IC_1","body":"one"}]}`,
	}}
	g := f.github()
	pr, err := g.PR(ctx, "u", PRState|PRMergeable|PRBase)
	if err != nil || !reflect.DeepEqual(pr, PR{State: StateOpen, Mergeable: Conflicting, Base: "main"}) {
		t.Fatalf("PR = %+v, %v", pr, err)
	}
	pr, _ = g.PR(ctx, "u", PRState|PRBase|PRHead)
	if !reflect.DeepEqual(pr, PR{State: StateMerged, Base: "main", HeadSHA: "abc"}) {
		t.Fatalf("PR head = %+v", pr)
	}
	pr, _ = g.PR(ctx, "u", PRBody)
	if pr.Body != "hello" {
		t.Fatalf("PR body = %+v", pr)
	}
	prs, _ := g.ListPRs(ctx, PRQuery{State: "open", Limit: 200}, PRBody)
	if len(prs) != 2 || prs[0].Body != "Closes #3" {
		t.Fatalf("ListPRs = %+v", prs)
	}
	cs, _ := g.Comments(ctx, "u")
	if !reflect.DeepEqual(cs, []Comment{{ID: "IC_1", Body: "one"}}) {
		t.Fatalf("Comments = %+v", cs)
	}
	f.want(t,
		"pr view u --json state,mergeable,baseRefName",
		"pr view u --json state,baseRefName,headRefOid",
		"pr view u --json body",
		"pr list --state open --limit 200 --json body",
		"pr view u --json comments",
	)
}

func TestGitHubIssues(t *testing.T) {
	ctx := context.Background()
	f := &fakeGH{replies: []string{
		`{"number":5,"title":"T","state":"OPEN","body":"B","url":"https://github.com/o/r/issues/5","labels":[{"name":"bug"}]}`,
		`[{"number":6,"title":"S","state":"OPEN","body":"b"}]`,
		`[{"number":7,"title":"L","body":"x","createdAt":"2026-01-02T03:04:05Z"}]`,
		"https://github.com/o/r/issues/9",
	}}
	g := f.github()
	is, err := g.Issue(ctx, "o/r", 5, IssueNumber|IssueTitle|IssueState|IssueBody|IssueURL|IssueLabels)
	if err != nil || is.Number != 5 || !reflect.DeepEqual(is.Labels, []string{"bug"}) {
		t.Fatalf("Issue = %+v, %v", is, err)
	}
	subs, _ := g.SubIssues(ctx, "o/r", 5)
	if len(subs) != 1 || subs[0].Number != 6 {
		t.Fatalf("SubIssues = %+v", subs)
	}
	list, _ := g.ListIssues(ctx, IssueQuery{Label: "ready", State: "open", Limit: 200}, IssueNumber|IssueTitle|IssueBody|IssueCreated)
	if len(list) != 1 || !list[0].Created.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("ListIssues = %+v", list)
	}
	created, _ := g.CreateIssue(ctx, NewIssue{Title: "t", Body: "b", Labels: []string{"x", "y"}})
	if created.Number != 9 || created.URL != "https://github.com/o/r/issues/9" || created.State != StateOpen {
		t.Fatalf("CreateIssue = %+v", created)
	}
	_ = g.CommentIssue(ctx, 9, "c")
	_ = g.EditIssueBody(ctx, 9, "e")
	_ = g.CloseIssue(ctx, 9, "done")
	_ = g.ReopenIssue(ctx, 9)
	_, _ = g.Issue(ctx, "", 9, IssueState)
	f.want(t,
		"issue view 5 -R o/r --json number,title,state,body,url,labels",
		"api repos/o/r/issues/5/sub_issues --jq [.[] | {number, title, state, body}]",
		"issue list --label ready --state open --limit 200 --json number,title,body,createdAt",
		"issue create --title t --body b --label x --label y",
		"issue comment 9 --body c",
		"issue edit 9 --body e",
		"issue close 9 --comment done",
		"issue reopen 9",
		"issue view 9 --json state",
	)
}

func TestGitHubMergeSettings(t *testing.T) {
	f := &fakeGH{replies: []string{
		`{"full_name":"o/r","html_url":"https://github.com/o/r","allow_merge_commit":false,"allow_squash_merge":true,"allow_rebase_merge":false}`,
		`{"full_name":"o/r"}`,
	}}
	g := f.github()
	s, err := g.MergeSettings(context.Background())
	if err != nil || s != (MergeSettings{Repo: "o/r", URL: "https://github.com/o/r", Squash: true}) {
		t.Fatalf("MergeSettings = %+v, %v", s, err)
	}
	if _, err := g.MergeSettings(context.Background()); err == nil || !strings.Contains(err.Error(), "admin or push access") {
		t.Fatalf("MergeSettings without access = %v, want the access error", err)
	}
	f.want(t, "api repos/{owner}/{repo}", "api repos/{owner}/{repo}")
}

func TestGitHubPassesErrorsThrough(t *testing.T) {
	boom := errors.New("gh pr view: exit status 1: no pull requests found")
	g := (&fakeGH{err: boom}).github()
	if _, err := g.PR(context.Background(), "u", PRState); !errors.Is(err, boom) {
		t.Fatalf("PR error = %v, want gh's", err)
	}
	if _, err := g.SubIssues(context.Background(), "", 1); !errors.Is(err, boom) {
		t.Fatalf("SubIssues error = %v, want gh's", err)
	}
}

func TestNewSelectsByConfig(t *testing.T) {
	for _, tc := range []struct {
		fc       config.Forge
		provider string
	}{
		{config.Forge{}, config.ForgeGitHub},
		{config.Forge{Provider: config.ForgeGitHub, CLI: config.ForgeGH}, config.ForgeGitHub},
		{config.Forge{Provider: config.ForgeGitLab}, config.ForgeGitLab},
		{config.Forge{CLI: config.ForgeGlab}, config.ForgeGitLab},
		{config.Forge{CLI: config.ForgeNone}, ""},
	} {
		f := New(tc.fc, t.TempDir())
		_, github := f.(*GitHub)
		_, gitlab := f.(*GitLab)
		if github != (tc.provider == config.ForgeGitHub) || gitlab != (tc.provider == config.ForgeGitLab) || f.Provider() != tc.provider {
			t.Errorf("New(%+v) = %T provider %q, want provider %q", tc.fc, f, f.Provider(), tc.provider)
		}
		if tc.provider != "" {
			continue
		}
		if _, err := f.CreatePR(context.Background(), NewPR{}); !errors.Is(err, ErrUnsupported) {
			t.Errorf("New(%+v).CreatePR = %v, want ErrUnsupported", tc.fc, err)
		}
	}
}

// TestGitHubWatcherArgs pins the operations the CI watcher, sweeper,
// auto-merge, sentinel and planner use to the gh calls they made themselves
// before #647.
func TestGitHubWatcherArgs(t *testing.T) {
	ctx := context.Background()
	f := &fakeGH{replies: []string{
		`[{"number":4,"title":"T","headRefName":"saddle/t1","headRefOid":"abc","baseRefName":"main","isDraft":true,
		  "mergeable":"MERGEABLE","labels":[{"name":"l"}],"files":[{"path":"a.go"}],
		  "statusCheckRollup":[{"__typename":"CheckRun","name":"test","workflowName":"CI","status":"COMPLETED","conclusion":"FAILURE","startedAt":"2026-01-01T00:00:00Z"},
		                       {"__typename":"StatusContext","context":"ext","state":"PENDING"}]}]`,
		`[{"number":3}]`,
		`{"url":"u","state":"OPEN","mergeStateStatus":"CLEAN","headRefName":"saddle/t1"}`,
		"",
		"",
		"",
		"7\n8\n",
		"9008",
		"",
		"https://github.com/o/r/issues/12",
	}}
	g := f.github()
	all := PRNumber | PRTitle | PRHeadBranch | PRHead | PRBase | PRDraft | PRMergeable | PRLabels | PRFiles | PRChecks
	prs, err := g.ListPRs(ctx, PRQuery{State: "open", Limit: 200}, all)
	want := PR{Number: 4, Title: "T", Head: "saddle/t1", HeadSHA: "abc", Base: "main", Draft: true, Mergeable: Mergeable,
		Labels: []string{"l"}, Files: []string{"a.go"}, Checks: []CheckRun{
			{Kind: KindJob, Name: "test", Workflow: "CI", Status: "COMPLETED", Conclusion: "FAILURE", StartedAt: "2026-01-01T00:00:00Z"},
			{Kind: KindStatus, Context: "ext", State: "PENDING"}}}
	if err != nil || len(prs) != 1 || !reflect.DeepEqual(prs[0], want) {
		t.Fatalf("ListPRs = %+v, %v\nwant %+v", prs, err, want)
	}
	merged, _ := g.ListPRs(ctx, PRQuery{State: "merged", Head: "saddle/t0", Limit: 1}, PRNumber)
	if len(merged) != 1 || merged[0].Number != 3 {
		t.Fatalf("merged = %+v", merged)
	}
	pr, _ := g.PR(ctx, "u", PRURL|PRState|PRDraft|PRMergeable|PRMergeState|PRBase|PRHeadBranch|PRHead|PRLabels|PRChecks)
	if pr.MergeState != "CLEAN" || pr.Head != "saddle/t1" || pr.URL != "u" {
		t.Fatalf("PR = %+v", pr)
	}
	_ = g.MergePR(ctx, "u", "rebase", "abc")
	_ = g.MergePR(ctx, "4", "squash", "")
	_ = g.RemoveLabel(ctx, "u", "saddle:ci-red")
	ns, _ := g.SubIssueNumbers(ctx, "o/r", 5)
	if !reflect.DeepEqual(ns, []int{7, 8}) {
		t.Fatalf("SubIssueNumbers = %v", ns)
	}
	if err := g.AddSubIssue(ctx, "", 5, 8); err != nil {
		t.Fatal(err)
	}
	is, _ := g.CreateIssue(ctx, NewIssue{Repo: "o/r", Title: "T", Body: "B"})
	if is.Number != 12 {
		t.Fatalf("CreateIssue = %+v", is)
	}
	f.want(t,
		"pr list --state open --limit 200 --json number,title,isDraft,mergeable,baseRefName,headRefName,headRefOid,labels,files,statusCheckRollup",
		"pr list --state merged --head saddle/t0 --limit 1 --json number",
		"pr view u --json url,state,isDraft,mergeable,mergeStateStatus,baseRefName,headRefName,headRefOid,labels,statusCheckRollup",
		"pr merge u --rebase --match-head-commit abc",
		"pr merge 4 --squash",
		"pr edit u --remove-label saddle:ci-red",
		"api repos/o/r/issues/5/sub_issues --paginate --jq .[].number",
		"api repos/{owner}/{repo}/issues/8 --jq .id",
		"api repos/{owner}/{repo}/issues/5/sub_issues -X POST -F sub_issue_id=9008",
		"issue create --title T --body B -R o/r",
	)
}

// TestGitHubChecksAndLogs covers gh pr checks printing JSON while exiting
// non-zero, no checks, and failed-job logs.
func TestGitHubChecksAndLogs(t *testing.T) {
	ctx := context.Background()
	var calls []string
	reply := map[string]struct {
		out string
		err error
	}{
		"pr checks 7 --json name,workflow,bucket,link": {`[{"name":"test","workflow":"CI","bucket":"fail","link":"https://github.com/o/r/actions/runs/11/job/22"}]`, errors.New("exit status 8")},
		"pr checks 8 --json name,workflow,bucket,link": {"", errors.New("gh pr checks: no checks reported on the 'x' branch")},
		"pr checks 9 --json name,workflow,bucket,link": {"", errors.New("gh pr checks: boom")},
		"run view --job 22 --log-failed":               {"test\tRun go test\t2026-01-01T00:00:00.1Z --- FAIL: TestX\ntest\tRun go test\t2026-01-01T00:00:00.2Z FAIL", errors.New("exit status 1")},
		"run view --job 23 --log-failed":               {"", errors.New("gh run view: HTTP 410")},
	}
	g := &GitHub{Run: func(_ context.Context, args ...string) (string, error) {
		c := strings.Join(args, " ")
		calls = append(calls, c)
		return reply[c].out, reply[c].err
	}}
	cs, err := g.Checks(ctx, "7")
	if err != nil || !reflect.DeepEqual(cs, []CICheck{{Name: "test", Workflow: "CI", Bucket: "fail", Link: "https://github.com/o/r/actions/runs/11/job/22"}}) {
		t.Fatalf("Checks(7) = %+v, %v", cs, err)
	}
	if cs, err := g.Checks(ctx, "8"); err != nil || cs != nil {
		t.Fatalf("Checks(8) = %+v, %v; want none", cs, err)
	}
	if _, err := g.Checks(ctx, "9"); err == nil {
		t.Fatal("Checks(9): want the gh error")
	}
	l, err := g.FailedLog(ctx, cs[0].Link, 1)
	if err != nil || l != (JobLog{Run: "https://github.com/o/r/actions/runs/11", Step: "Run go test", Tail: "FAIL"}) {
		t.Fatalf("FailedLog = %+v, %v", l, err)
	}
	l, err = g.FailedLog(ctx, "https://github.com/o/r/actions/runs/11/attempts/2/job/23", 40)
	if err == nil || l.Run != "https://github.com/o/r/actions/runs/11" {
		t.Fatalf("FailedLog(no log) = %+v, %v", l, err)
	}
	if l, err := g.FailedLog(ctx, "https://ci.example.com/x", 40); err != nil || l != (JobLog{Run: "https://ci.example.com/x"}) {
		t.Fatalf("FailedLog(not a job) = %+v, %v", l, err)
	}
	want := []string{
		"pr checks 7 --json name,workflow,bucket,link",
		"pr checks 8 --json name,workflow,bucket,link",
		"pr checks 9 --json name,workflow,bucket,link",
		"run view --job 22 --log-failed",
		"run view --job 23 --log-failed",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %q\nwant %q", calls, want)
	}
}

func TestGitHubMergedPRs(t *testing.T) {
	f := &fakeGH{replies: []string{`[{"number": 7, "title": "Release workflow", "labels": [{"name": "enhancement"}], "mergedAt": "2026-10-03T00:00:00Z"}]`}}
	prs, err := ReadMergedPRs(context.Background(), f.github(), "main", 1000)
	if err != nil {
		t.Fatal(err)
	}
	f.want(t, "pr list --state merged --base main --limit 1000 --json number,title,labels,mergedAt")
	want := []MergedPR{{Number: 7, Title: "Release workflow", Labels: []string{"enhancement"}, MergedAt: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)}}
	if !reflect.DeepEqual(prs, want) {
		t.Fatalf("MergedPRs = %+v, want %+v", prs, want)
	}
}

// Listing merged PRs is optional: GitLab and a local-only forge refuse it
// with ErrUnsupported, which saddle changelog turns into a clear message.
func TestReadMergedPRsUnsupported(t *testing.T) {
	for _, f := range []Forge{&GitLab{}, Unsupported{Reason: "local-only"}} {
		if _, err := ReadMergedPRs(context.Background(), f, "main", 10); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("%T: err = %v, want ErrUnsupported", f, err)
		}
	}
}

// InRepo points every gh call at another repo, so blocker issues can be
// filed in saddle's repo from a checkout of the repo saddle works on.
func TestInRepo(t *testing.T) {
	ctx := context.Background()
	f := &fakeGH{replies: []string{"[]", "", "https://github.com/o/saddle/issues/3"}}
	g := &GitHub{Run: InRepo(f.run, "o/saddle")}
	_, _ = g.ListIssues(ctx, IssueQuery{Label: "b", State: "all", Limit: 5}, IssueNumber)
	_ = g.EnsureLabel(ctx, Label{Name: "b"})
	is, _ := g.CreateIssue(ctx, NewIssue{Title: "t", Body: "x"})
	_ = g.CommentIssue(ctx, 3, "c")
	_ = g.EditIssueBody(ctx, 3, "e")
	_ = g.ReopenIssue(ctx, 3)
	if is.Number != 3 {
		t.Fatalf("CreateIssue = %+v", is)
	}
	f.want(t,
		"issue list --label b --state all --limit 5 --json number -R o/saddle",
		"label create b --force -R o/saddle",
		"issue create --title t --body x -R o/saddle",
		"issue comment 3 --body c -R o/saddle",
		"issue edit 3 --body e -R o/saddle",
		"issue reopen 3 -R o/saddle",
	)
	g = &GitHub{Run: InRepo(f.run, "")}
	f.calls = nil
	_ = g.ReopenIssue(ctx, 3)
	f.want(t, "issue reopen 3")
}
