package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/config"
)

// GitLab is Forge through the glab CLI (#645). Run is glab; New uses the
// real one. Every read and write goes through `glab api` against the REST
// API (v4), whose JSON and parameters don't change between glab releases
// the way `glab mr` flags have (--when-pipeline-succeeds became
// --auto-merge); only `auth status` uses a glab subcommand. Projects are
// addressed with glab's :id placeholder, which glab fills in from the
// checkout's remote.
type GitLab struct {
	Run Runner
	// Host is [forge] host: glab is pointed at it with --hostname. "" lets
	// glab take the host from the remote, or GITLAB_HOST.
	Host string
}

var _ Forge = (*GitLab)(nil)

func (g *GitLab) Provider() string { return config.ForgeGitLab }

// project is the API path of repo ("group/name"; "" is this repo).
func project(repo string) string {
	if repo == "" {
		return "projects/:id"
	}
	return "projects/" + url.PathEscape(repo)
}

// api calls `glab api` with method ("" is GET) on endpoint, relative to
// /api/v4, passing args (-f/-F fields) after it.
func (g *GitLab) api(ctx context.Context, method, endpoint string, args ...string) (string, error) {
	a := []string{"api"}
	if g.Host != "" {
		a = append(a, "--hostname", g.Host)
	}
	if method != "" {
		a = append(a, "-X", method)
	}
	a = append(a, endpoint)
	return g.Run(ctx, append(a, args...)...)
}

// apiJSON is api decoding the response into v.
func (g *GitLab) apiJSON(ctx context.Context, v any, method, endpoint string, args ...string) error {
	out, err := g.api(ctx, method, endpoint, args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(out), v); err != nil {
		return fmt.Errorf("parse glab api %s: %w", endpoint, err)
	}
	return nil
}

// query is an endpoint with its query string; empty values are left out.
func query(endpoint string, kv ...string) string {
	q := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			q.Set(kv[i], kv[i+1])
		}
	}
	if len(q) == 0 {
		return endpoint
	}
	return endpoint + "?" + q.Encode()
}

var mrURLRe = regexp.MustCompile(`/-/merge_requests/(\d+)(?:[/?#].*)?$`)

// mrIID is the MR number (its iid) ref names: "12", "!12", "#12" or an MR
// URL. A branch name has none.
func mrIID(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if m := mrURLRe.FindStringSubmatch(ref); m != nil {
		return m[1], true
	}
	n := strings.TrimLeft(ref, "!#")
	if _, err := strconv.Atoi(n); err == nil && n != "" {
		return n, true
	}
	return "", false
}

// iid resolves ref to an MR number: an MR reference, else the newest open
// MR from that branch, else the newest from it in any state.
func (g *GitLab) iid(ctx context.Context, ref string) (string, error) {
	if n, ok := mrIID(ref); ok {
		return n, nil
	}
	for _, state := range []string{"opened", "all"} {
		var mrs []mrJSON
		ep := query("projects/:id/merge_requests", "source_branch", ref, "state", state, "order_by", "created_at", "sort", "desc", "per_page", "1")
		if err := g.apiJSON(ctx, &mrs, "", ep); err != nil {
			return "", err
		}
		if len(mrs) > 0 {
			return strconv.Itoa(mrs[0].IID), nil
		}
	}
	return "", fmt.Errorf("no merge request found for branch %q", ref)
}

func mrPath(iid string) string { return "projects/:id/merge_requests/" + iid }

// mrJSON is GitLab's merge request JSON, mapped onto PR.
type mrJSON struct {
	IID                 int      `json:"iid"`
	WebURL              string   `json:"web_url"`
	Title               string   `json:"title"`
	State               string   `json:"state"` // opened, closed, merged, locked
	Draft               bool     `json:"draft"`
	WorkInProgress      bool     `json:"work_in_progress"`
	HasConflicts        bool     `json:"has_conflicts"`
	MergeStatus         string   `json:"merge_status"`
	DetailedMergeStatus string   `json:"detailed_merge_status"`
	TargetBranch        string   `json:"target_branch"`
	SourceBranch        string   `json:"source_branch"`
	SHA                 string   `json:"sha"`
	Description         string   `json:"description"`
	Labels              []string `json:"labels"`
	HeadPipeline        *struct {
		ID     int    `json:"id"`
		Status string `json:"status"`
		WebURL string `json:"web_url"`
	} `json:"head_pipeline"`
}

// mrState maps GitLab's MR state onto StateOpen, StateClosed or StateMerged.
func mrState(s string) string {
	switch s {
	case "merged":
		return StateMerged
	case "closed":
		return StateClosed
	}
	return StateOpen // opened, locked
}

func (m mrJSON) pr() PR {
	return PR{Number: m.IID, URL: m.WebURL, Title: m.Title, State: mrState(m.State), Draft: m.Draft || m.WorkInProgress,
		Mergeable: m.mergeable(), MergeState: m.mergeState(), Base: m.TargetBranch, Head: m.SourceBranch,
		HeadSHA: m.SHA, Body: m.Description, Labels: m.Labels}
}

// checking reports whether GitLab hasn't worked out the MR's mergeability.
func (m mrJSON) checking() bool {
	switch m.DetailedMergeStatus {
	case "unchecked", "checking", "preparing", "approvals_syncing":
		return true
	case "":
		return m.MergeStatus == "unchecked" || m.MergeStatus == "checking" || m.MergeStatus == ""
	}
	return false
}

func (m mrJSON) mergeable() string {
	switch {
	case m.State != "opened":
		return Unknown
	case m.HasConflicts || m.DetailedMergeStatus == "conflict" || m.MergeStatus == "cannot_be_merged":
		return Conflicting
	case m.checking():
		return Unknown
	}
	return Mergeable
}

// mergeState maps detailed_merge_status onto GitHub's mergeStateStatus
// words, which auto-merge reads: only CLEAN merges. "mergeable" with a
// failed or unfinished head pipeline (the project doesn't require pipelines
// to succeed) is UNSTABLE or BLOCKED, as GitHub reports non-passing checks.
func (m mrJSON) mergeState() string {
	switch {
	case m.State != "opened":
		return "UNKNOWN"
	case m.HasConflicts || m.DetailedMergeStatus == "conflict":
		return "DIRTY"
	case m.checking():
		return "UNKNOWN"
	}
	switch m.DetailedMergeStatus {
	case "need_rebase":
		return "BEHIND"
	case "draft_status":
		return "DRAFT"
	case "not_open":
		return "UNKNOWN"
	case "mergeable", "":
		if m.DetailedMergeStatus == "" && m.MergeStatus != "can_be_merged" {
			return "BLOCKED"
		}
		if m.HeadPipeline != nil {
			switch pipelineBucket(m.HeadPipeline.Status) {
			case "fail":
				return "UNSTABLE"
			case "pending":
				return "BLOCKED"
			}
		}
		return "CLEAN"
	}
	// ci_must_pass, ci_still_running, not_approved, discussions_not_resolved,
	// blocked_status, requested_changes, external_status_checks…
	return "BLOCKED"
}

func (g *GitLab) CreatePR(ctx context.Context, p NewPR) (string, error) {
	title := p.Title
	if p.Draft {
		title = "Draft: " + title
	}
	out, err := g.api(ctx, "POST", "projects/:id/merge_requests",
		"-f", "source_branch="+p.Head, "-f", "target_branch="+p.Base, "-f", "title="+title, "-f", "description="+p.Body)
	if err != nil {
		// GitLab refuses a second open MR from one branch (409, its reason
		// in the body glab prints); hand back the one that is there, as
		// gh's "already exists" error does.
		if strings.Contains(out+err.Error(), "already exists") {
			if prs, lerr := g.ListPRs(ctx, PRQuery{State: "open", Head: p.Head, Limit: 1}, PRURL); lerr == nil && len(prs) > 0 {
				return prs[0].URL, nil
			}
		}
		return "", err
	}
	var m mrJSON
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		return "", fmt.Errorf("parse glab api merge request: %w", err)
	}
	return m.WebURL, nil
}

func (g *GitLab) PR(ctx context.Context, ref string, f PRFields) (PR, error) {
	iid, err := g.iid(ctx, ref)
	if err != nil {
		return PR{}, err
	}
	var m mrJSON
	if err := g.apiJSON(ctx, &m, "", mrPath(iid)); err != nil {
		return PR{}, err
	}
	pr := m.pr()
	if f&PRFiles != 0 {
		if pr.Files, err = g.files(ctx, iid); err != nil {
			return PR{}, err
		}
	}
	if f&PRChecks != 0 && m.HeadPipeline != nil {
		jobs, err := g.jobs(ctx, m.HeadPipeline.ID)
		if err != nil {
			return PR{}, err
		}
		for _, j := range jobs {
			pr.Checks = append(pr.Checks, j.checkRun())
		}
	}
	return pr, nil
}

// files lists the paths MR iid changes.
func (g *GitLab) files(ctx context.Context, iid string) ([]string, error) {
	var ds []struct {
		NewPath string `json:"new_path"`
		OldPath string `json:"old_path"`
	}
	if err := g.apiJSON(ctx, &ds, "", mrPath(iid)+"/diffs?per_page=100"); err != nil {
		return nil, err
	}
	var fs []string
	for _, d := range ds {
		p := d.NewPath
		if p == "" {
			p = d.OldPath
		}
		fs = append(fs, p)
	}
	return fs, nil
}

// gitlabState is PRQuery.State as the MR list API spells it.
func gitlabState(s string) string {
	switch s {
	case "open", "":
		return "opened"
	case "closed", "merged", "all":
		return s
	}
	return s
}

func (g *GitLab) ListPRs(ctx context.Context, q PRQuery, f PRFields) ([]PR, error) {
	limit := ""
	if q.Limit > 0 {
		limit = strconv.Itoa(min(q.Limit, 100))
	}
	var ms []mrJSON
	ep := query("projects/:id/merge_requests", "state", gitlabState(q.State), "source_branch", q.Head,
		"order_by", "created_at", "sort", "desc", "per_page", limit)
	if err := g.apiJSON(ctx, &ms, "", ep); err != nil {
		return nil, err
	}
	prs := make([]PR, len(ms))
	for i, m := range ms {
		// The list leaves out the head pipeline: read each MR for it.
		if f&(PRFiles|PRChecks|PRMergeState) != 0 {
			p, err := g.PR(ctx, strconv.Itoa(m.IID), f)
			if err != nil {
				return nil, err
			}
			prs[i] = p
			continue
		}
		prs[i] = m.pr()
	}
	return prs, nil
}

// openPRPage is how many merge requests one list page asks for. A short
// page is the last.
const openPRPage = 100

// OpenPRs lists open merge requests and their head pipelines in one list
// per page (#711). The list carries the pipeline's status, which is the
// pass, fail or pending the jobs fold into, so this does not read each MR.
func (g *GitLab) OpenPRs(ctx context.Context) ([]PR, error) {
	var all []PR
	for page := 1; page <= 50; page++ {
		var ms []mrJSON
		ep := query("projects/:id/merge_requests",
			"state", "opened",
			"with_merge_status_recheck", "true",
			"order_by", "updated_at",
			"sort", "desc",
			"per_page", strconv.Itoa(openPRPage),
			"page", strconv.Itoa(page))
		if err := g.apiJSON(ctx, &ms, "", ep); err != nil {
			return nil, err
		}
		for _, m := range ms {
			p := m.pr()
			p.Checks = pipelineChecks(m)
			all = append(all, p)
		}
		if len(ms) < openPRPage {
			return all, nil
		}
	}
	return nil, fmt.Errorf("glab api: open merge requests did not finish paging")
}

func (g *GitLab) updateMR(ctx context.Context, ref string, fields ...string) error {
	iid, err := g.iid(ctx, ref)
	if err != nil {
		return err
	}
	_, err = g.api(ctx, "PUT", mrPath(iid), fields...)
	return err
}

// SetPRBase retargets the MR. GitLab, unlike GitHub, lets an MR in a
// stack change its target freely.
func (g *GitLab) SetPRBase(ctx context.Context, ref, base string) error {
	return g.updateMR(ctx, ref, "-f", "target_branch="+base)
}

func (g *GitLab) SetPRBody(ctx context.Context, ref, body string) error {
	return g.updateMR(ctx, ref, "-f", "description="+body)
}

func (g *GitLab) ClosePR(ctx context.Context, ref string) error {
	return g.updateMR(ctx, ref, "-f", "state_event=close")
}

// noteID is a Comment's ID: the MR and the note, since the notes API needs
// both.
func noteID(iid string, note int) string { return iid + ":" + strconv.Itoa(note) }

// Comments lists the MR's notes, leaving out GitLab's system notes ("added
// 1 commit", "changed the description").
func (g *GitLab) Comments(ctx context.Context, ref string) ([]Comment, error) {
	iid, err := g.iid(ctx, ref)
	if err != nil {
		return nil, err
	}
	var ns []struct {
		ID     int    `json:"id"`
		Body   string `json:"body"`
		System bool   `json:"system"`
	}
	if err := g.apiJSON(ctx, &ns, "", mrPath(iid)+"/notes?sort=asc&order_by=created_at&per_page=100"); err != nil {
		return nil, err
	}
	var cs []Comment
	for _, n := range ns {
		if !n.System {
			cs = append(cs, Comment{ID: noteID(iid, n.ID), Body: n.Body})
		}
	}
	return cs, nil
}

func (g *GitLab) AddComment(ctx context.Context, ref, body string) error {
	iid, err := g.iid(ctx, ref)
	if err != nil {
		return err
	}
	_, err = g.api(ctx, "POST", mrPath(iid)+"/notes", "-f", "body="+body)
	return err
}

// EditComment edits a note by the ID Comments gave it.
func (g *GitLab) EditComment(ctx context.Context, id, body string) error {
	iid, note, ok := strings.Cut(id, ":")
	if !ok {
		return fmt.Errorf("edit comment %q: not a GitLab note id (mr:note)", id)
	}
	_, err := g.api(ctx, "PUT", mrPath(iid)+"/notes/"+note, "-f", "body="+body)
	return err
}

func (g *GitLab) AddLabel(ctx context.Context, ref, label string) error {
	return g.updateMR(ctx, ref, "-f", "add_labels="+label)
}

func (g *GitLab) RemoveLabel(ctx context.Context, ref, label string) error {
	return g.updateMR(ctx, ref, "-f", "remove_labels="+label)
}

// EnsureLabel creates l, or updates it when it exists. GitLab wants a
// "#rrggbb" color, which gh takes without the "#".
func (g *GitLab) EnsureLabel(ctx context.Context, l Label) error {
	color := l.Color
	if color == "" {
		color = "6699CC"
	}
	color = "#" + strings.TrimPrefix(color, "#")
	fields := []string{"-f", "color=" + color}
	if l.Description != "" {
		fields = append(fields, "-f", "description="+l.Description)
	}
	out, err := g.api(ctx, "POST", "projects/:id/labels", append([]string{"-f", "name=" + l.Name}, fields...)...)
	if err != nil && strings.Contains(out+err.Error(), "already exists") {
		_, err = g.api(ctx, "PUT", "projects/:id/labels/"+url.PathEscape(l.Name), fields...)
	}
	return err
}

// MergePR merges the MR through the merge API: squash for "squash", the
// project's own merge method (fast-forward on a linear-history project)
// otherwise, since GitLab sets rebase versus merge commits per project, not
// per merge. head pins the merge to that commit (sha). While the head
// pipeline is still running it asks GitLab to merge when the pipeline
// succeeds instead, and says so in an error: GitLab then merges on its own
// once it passes. It never bypasses protected branches or required
// approvals.
func (g *GitLab) MergePR(ctx context.Context, ref, method, head string) error {
	iid, err := g.iid(ctx, ref)
	if err != nil {
		return err
	}
	var m mrJSON
	if err := g.apiJSON(ctx, &m, "", mrPath(iid)); err != nil {
		return err
	}
	running := m.HeadPipeline != nil && pipelineBucket(m.HeadPipeline.Status) == "pending"
	args := []string{"-F", "squash=" + strconv.FormatBool(method == "squash")}
	if running {
		args = append(args, "-F", "merge_when_pipeline_succeeds=true")
	}
	if head != "" {
		args = append(args, "-f", "sha="+head)
	}
	if err := g.apiJSON(ctx, &m, "PUT", mrPath(iid)+"/merge", args...); err != nil {
		return err
	}
	if m.State != "merged" {
		return fmt.Errorf("merge request !%s is set to merge when its pipeline succeeds, not merged yet", iid)
	}
	return nil
}

// MergeSettings reads the project. Saddle needs linear history, which on
// GitLab is the "Fast-forward merge" method: "merge" and "rebase_merge"
// both make merge commits, so they read as Merge. Squash is allowed unless
// the project says never.
func (g *GitLab) MergeSettings(ctx context.Context) (MergeSettings, error) {
	var p struct {
		Path         string `json:"path_with_namespace"`
		WebURL       string `json:"web_url"`
		MergeMethod  string `json:"merge_method"`
		SquashOption string `json:"squash_option"`
	}
	if err := g.apiJSON(ctx, &p, "", "projects/:id"); err != nil {
		return MergeSettings{}, err
	}
	if p.MergeMethod == "" {
		return MergeSettings{}, errors.New("glab api did not return the project's merge_method")
	}
	return MergeSettings{Repo: p.Path, URL: p.WebURL, Merge: p.MergeMethod != "ff",
		Squash: p.SquashOption != "never", Rebase: p.MergeMethod == "ff"}, nil
}

func (g *GitLab) AuthStatus(ctx context.Context) error {
	_, err := g.Run(ctx, AuthStatusArgs(g.Host)...)
	return err
}

// DefaultBranch reads the project's default branch.
func (g *GitLab) DefaultBranch(ctx context.Context) (string, error) {
	var p struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.apiJSON(ctx, &p, "", "projects/:id"); err != nil {
		return "", err
	}
	if p.DefaultBranch == "" {
		return "", errors.New("glab api did not return the project's default_branch (an empty project has none)")
	}
	return p.DefaultBranch, nil
}

// gitlabIssue is GitLab's issue JSON, mapped onto Issue. GitLab numbers
// issues per project (iid) and spells them #N, as GitHub does.
type gitlabIssue struct {
	IID         int       `json:"iid"`
	Title       string    `json:"title"`
	State       string    `json:"state"` // opened or closed
	Description string    `json:"description"`
	WebURL      string    `json:"web_url"`
	Labels      []string  `json:"labels"`
	CreatedAt   time.Time `json:"created_at"`
}

func (i gitlabIssue) issue() Issue {
	st := StateOpen
	if i.State == "closed" {
		st = StateClosed
	}
	return Issue{Number: i.IID, Title: i.Title, State: st, Body: i.Description, URL: i.WebURL, Labels: i.Labels, Created: i.CreatedAt}
}

func issuePath(repo string, n int) string { return project(repo) + "/issues/" + strconv.Itoa(n) }

func (g *GitLab) Issue(ctx context.Context, repo string, n int, _ IssueFields) (Issue, error) {
	var i gitlabIssue
	if err := g.apiJSON(ctx, &i, "", issuePath(repo, n)); err != nil {
		return Issue{}, err
	}
	return i.issue(), nil
}

// GitLab Free has no sub-issues (child items are Premium epics and tasks),
// so saddle files an epic's sub-issues as linked issues ("relates to"),
// which every tier has: SubIssues lists the issues linked to n.
func (g *GitLab) SubIssues(ctx context.Context, repo string, n int) ([]Issue, error) {
	var raw []gitlabIssue
	if err := g.apiJSON(ctx, &raw, "", issuePath(repo, n)+"/links"); err != nil {
		return nil, err
	}
	is := make([]Issue, len(raw))
	for i, r := range raw {
		is[i] = r.issue()
	}
	return is, nil
}

func (g *GitLab) SubIssueNumbers(ctx context.Context, repo string, n int) ([]int, error) {
	is, err := g.SubIssues(ctx, repo, n)
	if err != nil {
		return nil, err
	}
	ns := make([]int, len(is))
	for i, x := range is {
		ns[i] = x.Number
	}
	return ns, nil
}

// AddSubIssue links child to parent. The links API names the target
// project by id, which it reads from the child.
func (g *GitLab) AddSubIssue(ctx context.Context, repo string, parent, child int) error {
	var c struct {
		ProjectID int `json:"project_id"`
	}
	if err := g.apiJSON(ctx, &c, "", issuePath(repo, child)); err != nil {
		return fmt.Errorf("look up issue #%d: %w", child, err)
	}
	_, err := g.api(ctx, "POST", issuePath(repo, parent)+"/links",
		"-f", "target_project_id="+strconv.Itoa(c.ProjectID), "-f", "target_issue_iid="+strconv.Itoa(child))
	return err
}

func (g *GitLab) ListIssues(ctx context.Context, q IssueQuery, _ IssueFields) ([]Issue, error) {
	state := q.State
	if state == "open" || state == "" {
		state = "opened"
	}
	limit := ""
	if q.Limit > 0 {
		limit = strconv.Itoa(min(q.Limit, 100))
	}
	var raw []gitlabIssue
	ep := query("projects/:id/issues", "labels", q.Label, "state", state, "order_by", "created_at", "sort", "desc", "per_page", limit)
	if err := g.apiJSON(ctx, &raw, "", ep); err != nil {
		return nil, err
	}
	is := make([]Issue, len(raw))
	for i, r := range raw {
		is[i] = r.issue()
	}
	return is, nil
}

func (g *GitLab) CreateIssue(ctx context.Context, i NewIssue) (Issue, error) {
	args := []string{"-f", "title=" + i.Title, "-f", "description=" + i.Body}
	if len(i.Labels) > 0 {
		args = append(args, "-f", "labels="+strings.Join(i.Labels, ","))
	}
	var r gitlabIssue
	if err := g.apiJSON(ctx, &r, "POST", project(i.Repo)+"/issues", args...); err != nil {
		return Issue{}, err
	}
	is := r.issue()
	if is.Labels == nil {
		is.Labels = i.Labels
	}
	return is, nil
}

func (g *GitLab) CommentIssue(ctx context.Context, n int, body string) error {
	_, err := g.api(ctx, "POST", issuePath("", n)+"/notes", "-f", "body="+body)
	return err
}

func (g *GitLab) EditIssueBody(ctx context.Context, n int, body string) error {
	_, err := g.api(ctx, "PUT", issuePath("", n), "-f", "description="+body)
	return err
}

// CloseIssue closes issue n, commenting comment first when it isn't "".
func (g *GitLab) CloseIssue(ctx context.Context, n int, comment string) error {
	if comment != "" {
		if err := g.CommentIssue(ctx, n, comment); err != nil {
			return err
		}
	}
	_, err := g.api(ctx, "PUT", issuePath("", n), "-f", "state_event=close")
	return err
}

func (g *GitLab) ReopenIssue(ctx context.Context, n int) error {
	_, err := g.api(ctx, "PUT", issuePath("", n), "-f", "state_event=reopen")
	return err
}
