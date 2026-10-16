package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/config"
)

// GitHub is Forge through the gh CLI. Run is gh; New uses the real one.
type GitHub struct {
	Run Runner
}

var _ Forge = (*GitHub)(nil)

func (g *GitHub) Provider() string { return config.ForgeGitHub }

// prJSON is gh's PR JSON, mapped onto PR.
type prJSON struct {
	Number           int    `json:"number"`
	URL              string `json:"url"`
	Title            string `json:"title"`
	State            string `json:"state"`
	IsDraft          bool   `json:"isDraft"`
	Mergeable        string `json:"mergeable"`
	MergeStateStatus string `json:"mergeStateStatus"`
	BaseRefName      string `json:"baseRefName"`
	HeadRefName      string `json:"headRefName"`
	HeadRefOid       string `json:"headRefOid"`
	Body             string `json:"body"`
	Labels           []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Files []struct {
		Path string `json:"path"`
	} `json:"files"`
	Rollup []rollupJSON `json:"statusCheckRollup"`
}

// rollupJSON is one statusCheckRollup entry: a CheckRun or a StatusContext.
type rollupJSON struct {
	Typename   string `json:"__typename"`
	Name       string `json:"name"`
	Workflow   string `json:"workflowName"`
	Context    string `json:"context"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
	StartedAt  string `json:"startedAt"`
}

func (p prJSON) pr() PR {
	pr := PR{Number: p.Number, URL: p.URL, Title: p.Title, State: p.State, Draft: p.IsDraft,
		Mergeable: p.Mergeable, MergeState: p.MergeStateStatus, Base: p.BaseRefName,
		Head: p.HeadRefName, HeadSHA: p.HeadRefOid, Body: p.Body}
	for _, l := range p.Labels {
		pr.Labels = append(pr.Labels, l.Name)
	}
	for _, f := range p.Files {
		pr.Files = append(pr.Files, f.Path)
	}
	for _, r := range p.Rollup {
		kind := map[string]string{"CheckRun": KindJob, "StatusContext": KindStatus}[r.Typename]
		pr.Checks = append(pr.Checks, CheckRun{Kind: kind, Name: r.Name, Workflow: r.Workflow, Context: r.Context,
			Status: r.Status, Conclusion: r.Conclusion, State: r.State, StartedAt: r.StartedAt})
	}
	return pr
}

// prFields is f as gh's --json list, in a fixed order.
func prFields(f PRFields) string {
	var fs []string
	for _, x := range []struct {
		bit  PRFields
		name string
	}{{PRNumber, "number"}, {PRURL, "url"}, {PRTitle, "title"}, {PRState, "state"}, {PRDraft, "isDraft"},
		{PRMergeable, "mergeable"}, {PRMergeState, "mergeStateStatus"}, {PRBase, "baseRefName"},
		{PRHeadBranch, "headRefName"}, {PRHead, "headRefOid"}, {PRBody, "body"}, {PRLabels, "labels"},
		{PRFiles, "files"}, {PRChecks, "statusCheckRollup"}} {
		if f&x.bit != 0 {
			fs = append(fs, x.name)
		}
	}
	return strings.Join(fs, ",")
}

func (g *GitHub) CreatePR(ctx context.Context, p NewPR) (string, error) {
	args := []string{"pr", "create", "--base", p.Base, "--head", p.Head, "--title", p.Title, "--body", p.Body}
	if p.Draft {
		args = append(args, "--draft")
	}
	out, err := g.Run(ctx, args...)
	if err != nil {
		return "", err
	}
	return lastLine(out), nil
}

func (g *GitHub) PR(ctx context.Context, ref string, f PRFields) (PR, error) {
	out, err := g.Run(ctx, "pr", "view", ref, "--json", prFields(f))
	if err != nil {
		return PR{}, err
	}
	var p prJSON
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		return PR{}, fmt.Errorf("gh pr view %s: %w", ref, err)
	}
	return p.pr(), nil
}

func (g *GitHub) ListPRs(ctx context.Context, q PRQuery, f PRFields) ([]PR, error) {
	args := []string{"pr", "list", "--state", q.State}
	if q.Head != "" {
		args = append(args, "--head", q.Head)
	}
	out, err := g.Run(ctx, append(args, "--limit", strconv.Itoa(q.Limit), "--json", prFields(f))...)
	if err != nil {
		return nil, err
	}
	var raw []prJSON
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse gh pr list: %w", err)
	}
	prs := make([]PR, len(raw))
	for i, p := range raw {
		prs[i] = p.pr()
	}
	return prs, nil
}

func (g *GitHub) SetPRBase(ctx context.Context, ref, base string) error {
	_, err := g.Run(ctx, "pr", "edit", ref, "--base", base)
	return err
}

func (g *GitHub) SetPRBody(ctx context.Context, ref, body string) error {
	_, err := g.Run(ctx, "pr", "edit", ref, "--body", body)
	return err
}

func (g *GitHub) ClosePR(ctx context.Context, ref string) error {
	_, err := g.Run(ctx, "pr", "close", ref)
	return err
}

func (g *GitHub) Comments(ctx context.Context, ref string) ([]Comment, error) {
	out, err := g.Run(ctx, "pr", "view", ref, "--json", "comments")
	if err != nil {
		return nil, err
	}
	var v struct {
		Comments []struct {
			ID   string `json:"id"`
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, fmt.Errorf("gh pr view %s: %w", ref, err)
	}
	cs := make([]Comment, len(v.Comments))
	for i, c := range v.Comments {
		cs[i] = Comment{ID: c.ID, Body: c.Body}
	}
	return cs, nil
}

func (g *GitHub) AddComment(ctx context.Context, ref, body string) error {
	_, err := g.Run(ctx, "pr", "comment", ref, "--body", body)
	return err
}

// EditComment edits the comment with GraphQL node id.
func (g *GitHub) EditComment(ctx context.Context, id, body string) error {
	_, err := g.Run(ctx, "api", "graphql",
		"-f", "query=mutation($id: ID!, $body: String!) { updateIssueComment(input: {id: $id, body: $body}) { issueComment { id } } }",
		"-f", "id="+id, "-f", "body="+body)
	return err
}

func (g *GitHub) AddLabel(ctx context.Context, ref, label string) error {
	_, err := g.Run(ctx, "pr", "edit", ref, "--add-label", label)
	return err
}

func (g *GitHub) RemoveLabel(ctx context.Context, ref, label string) error {
	_, err := g.Run(ctx, "pr", "edit", ref, "--remove-label", label)
	return err
}

// MergePR never passes --admin, so branch protection always applies.
func (g *GitHub) MergePR(ctx context.Context, ref, method, head string) error {
	args := []string{"pr", "merge", ref, "--" + method}
	if head != "" {
		args = append(args, "--match-head-commit", head)
	}
	_, err := g.Run(ctx, args...)
	return err
}

// EnsureLabel creates l, or updates it when it exists.
func (g *GitHub) EnsureLabel(ctx context.Context, l Label) error {
	args := []string{"label", "create", l.Name}
	if l.Color != "" {
		args = append(args, "--color", l.Color)
	}
	args = append(args, "--force")
	if l.Description != "" {
		args = append(args, "--description", l.Description)
	}
	_, err := g.Run(ctx, args...)
	return err
}

func (g *GitHub) RerunFailed(ctx context.Context, id string) error {
	_, err := g.Run(ctx, "run", "rerun", id, "--failed")
	return err
}

// MergeSettings reads `gh api repos/{owner}/{repo}`. GitHub omits the allow_*
// fields for users without admin or push rights; that is an error, not "all
// false".
func (g *GitHub) MergeSettings(ctx context.Context) (MergeSettings, error) {
	out, err := g.Run(ctx, "api", "repos/{owner}/{repo}")
	if err != nil {
		return MergeSettings{}, err
	}
	var r struct {
		FullName string `json:"full_name"`
		HTMLURL  string `json:"html_url"`
		Merge    *bool  `json:"allow_merge_commit"`
		Squash   *bool  `json:"allow_squash_merge"`
		Rebase   *bool  `json:"allow_rebase_merge"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return MergeSettings{}, fmt.Errorf("parse gh api output: %w", err)
	}
	if r.Merge == nil || r.Squash == nil || r.Rebase == nil {
		return MergeSettings{}, fmt.Errorf("gh api did not return the allow_*_merge fields (they need admin or push access)")
	}
	return MergeSettings{Repo: r.FullName, URL: r.HTMLURL, Merge: *r.Merge, Squash: *r.Squash, Rebase: *r.Rebase}, nil
}

// DefaultBranch reads the repo's default branch with `gh repo view`, which
// needs only read access.
func (g *GitHub) DefaultBranch(ctx context.Context) (string, error) {
	return g.Run(ctx, "repo", "view", "--json", "defaultBranchRef", "-q", ".defaultBranchRef.name")
}

// RateLimit reads the core REST quota every gh call spends.
func (g *GitHub) RateLimit(ctx context.Context) (RateLimit, error) {
	out, err := g.Run(ctx, "api", "rate_limit")
	if err != nil {
		return RateLimit{}, err
	}
	var rl struct {
		Resources struct {
			Core struct {
				Limit     int   `json:"limit"`
				Remaining int   `json:"remaining"`
				Reset     int64 `json:"reset"`
			} `json:"core"`
		} `json:"resources"`
	}
	if err := json.Unmarshal([]byte(out), &rl); err != nil {
		return RateLimit{}, fmt.Errorf("parse gh api output: %w", err)
	}
	c := rl.Resources.Core
	return RateLimit{Limit: c.Limit, Remaining: c.Remaining, Reset: time.Unix(c.Reset, 0)}, nil
}

func (g *GitHub) AuthStatus(ctx context.Context) error {
	_, err := g.Run(ctx, "auth", "status")
	return err
}

// issueJSON is gh's issue JSON, mapped onto Issue.
type issueJSON struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	Body      string    `json:"body"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"createdAt"`
	Labels    []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (i issueJSON) issue() Issue {
	is := Issue{Number: i.Number, Title: i.Title, State: i.State, Body: i.Body, URL: i.URL, Created: i.CreatedAt}
	for _, l := range i.Labels {
		is.Labels = append(is.Labels, l.Name)
	}
	return is
}

// issueFields is f as gh's --json list, in a fixed order.
func issueFields(f IssueFields) string {
	var fs []string
	for _, x := range []struct {
		bit  IssueFields
		name string
	}{{IssueNumber, "number"}, {IssueTitle, "title"}, {IssueState, "state"}, {IssueBody, "body"},
		{IssueURL, "url"}, {IssueLabels, "labels"}, {IssueCreated, "createdAt"}} {
		if f&x.bit != 0 {
			fs = append(fs, x.name)
		}
	}
	return strings.Join(fs, ",")
}

func (g *GitHub) Issue(ctx context.Context, repo string, n int, f IssueFields) (Issue, error) {
	args := []string{"issue", "view", strconv.Itoa(n)}
	if repo != "" {
		args = append(args, "-R", repo)
	}
	out, err := g.Run(ctx, append(args, "--json", issueFields(f))...)
	if err != nil {
		return Issue{}, err
	}
	var i issueJSON
	if err := json.Unmarshal([]byte(out), &i); err != nil {
		return Issue{}, fmt.Errorf("parse issue #%d: %w", n, err)
	}
	return i.issue(), nil
}

// SubIssues asks the REST API, since `gh issue view` doesn't list them.
func (g *GitHub) SubIssues(ctx context.Context, repo string, n int) ([]Issue, error) {
	out, err := g.Run(ctx, "api", repoAPI(repo)+"/issues/"+strconv.Itoa(n)+"/sub_issues",
		"--jq", "[.[] | {number, title, state, body}]")
	if err != nil || out == "" {
		return nil, err
	}
	var raw []issueJSON
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse sub-issues of #%d: %w", n, err)
	}
	is := make([]Issue, len(raw))
	for i, r := range raw {
		is[i] = r.issue()
	}
	return is, nil
}

// SubIssueNumbers pages through the REST API, since a plan's epic can have
// more sub-issues than one page holds.
func (g *GitHub) SubIssueNumbers(ctx context.Context, repo string, n int) ([]int, error) {
	out, err := g.Run(ctx, "api", repoAPI(repo)+"/issues/"+strconv.Itoa(n)+"/sub_issues", "--paginate", "--jq", ".[].number")
	if err != nil {
		return nil, err
	}
	var ns []int
	for f := range strings.FieldsSeq(out) {
		if x, err := strconv.Atoi(f); err == nil {
			ns = append(ns, x)
		}
	}
	return ns, nil
}

// AddSubIssue looks up child's database id, which the REST API links by.
func (g *GitHub) AddSubIssue(ctx context.Context, repo string, parent, child int) error {
	api := repoAPI(repo)
	id, err := g.Run(ctx, "api", api+"/issues/"+strconv.Itoa(child), "--jq", ".id")
	if err != nil {
		return fmt.Errorf("look up issue #%d: %w", child, err)
	}
	_, err = g.Run(ctx, "api", api+"/issues/"+strconv.Itoa(parent)+"/sub_issues", "-X", "POST", "-F", "sub_issue_id="+strings.TrimSpace(id))
	return err
}

// repoAPI is repo's REST path; gh fills in this repo's owner and name.
func repoAPI(repo string) string {
	if repo == "" {
		return "repos/{owner}/{repo}"
	}
	return "repos/" + repo
}

func (g *GitHub) ListIssues(ctx context.Context, q IssueQuery, f IssueFields) ([]Issue, error) {
	args := []string{"issue", "list"}
	if q.Label != "" {
		args = append(args, "--label", q.Label)
	}
	args = append(args, "--state", q.State, "--limit", strconv.Itoa(q.Limit), "--json", issueFields(f))
	out, err := g.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var raw []issueJSON
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse gh issue list: %w", err)
	}
	is := make([]Issue, len(raw))
	for i, r := range raw {
		is[i] = r.issue()
	}
	return is, nil
}

// CreateIssue files i; gh prints the new issue's URL last.
func (g *GitHub) CreateIssue(ctx context.Context, i NewIssue) (Issue, error) {
	args := []string{"issue", "create", "--title", i.Title, "--body", i.Body}
	for _, l := range i.Labels {
		args = append(args, "--label", l)
	}
	if i.Repo != "" {
		args = append(args, "-R", i.Repo)
	}
	out, err := g.Run(ctx, args...)
	if err != nil {
		return Issue{}, err
	}
	url := strings.TrimSpace(out[strings.LastIndex(out, "\n")+1:])
	n, _ := strconv.Atoi(url[strings.LastIndex(url, "/")+1:])
	return Issue{Number: n, Title: i.Title, Body: i.Body, State: StateOpen, URL: url, Labels: i.Labels}, nil
}

func (g *GitHub) CommentIssue(ctx context.Context, n int, body string) error {
	_, err := g.Run(ctx, "issue", "comment", strconv.Itoa(n), "--body", body)
	return err
}

func (g *GitHub) EditIssueBody(ctx context.Context, n int, body string) error {
	_, err := g.Run(ctx, "issue", "edit", strconv.Itoa(n), "--body", body)
	return err
}

// CloseIssue closes issue n, commenting comment when it isn't "".
func (g *GitHub) CloseIssue(ctx context.Context, n int, comment string) error {
	args := []string{"issue", "close", strconv.Itoa(n)}
	if comment != "" {
		args = append(args, "--comment", comment)
	}
	_, err := g.Run(ctx, args...)
	return err
}

func (g *GitHub) ReopenIssue(ctx context.Context, n int) error {
	_, err := g.Run(ctx, "issue", "reopen", strconv.Itoa(n))
	return err
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
