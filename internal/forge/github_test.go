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
	if err != nil || pr != (PR{State: StateOpen, Mergeable: Conflicting, Base: "main"}) {
		t.Fatalf("PR = %+v, %v", pr, err)
	}
	pr, _ = g.PR(ctx, "u", PRState|PRBase|PRHead)
	if pr != (PR{State: StateMerged, Base: "main", HeadSHA: "abc"}) {
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
		github   bool
	}{
		{config.Forge{}, config.ForgeGitHub, true},
		{config.Forge{Provider: config.ForgeGitHub, CLI: config.ForgeGH}, config.ForgeGitHub, true},
		{config.Forge{Provider: config.ForgeGitLab}, config.ForgeGitLab, false},
		{config.Forge{CLI: config.ForgeGlab}, config.ForgeGitLab, false},
		{config.Forge{CLI: config.ForgeNone}, "", false},
	} {
		f := New(tc.fc, t.TempDir())
		if _, ok := f.(*GitHub); ok != tc.github || f.Provider() != tc.provider {
			t.Errorf("New(%+v) = %T provider %q, want github=%v provider %q", tc.fc, f, f.Provider(), tc.github, tc.provider)
		}
		if tc.github {
			continue
		}
		if _, err := f.CreatePR(context.Background(), NewPR{}); !errors.Is(err, ErrUnsupported) {
			t.Errorf("New(%+v).CreatePR = %v, want ErrUnsupported", tc.fc, err)
		}
	}
}
