package fakegh

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// RepoEnv names the bare repo directly, for gh calls made outside a checkout.
const RepoEnv = "FAKEGH_REPO"

// Exit codes gh uses.
const (
	exitOK      = 0
	exitErr     = 1
	exitPending = 8 // gh pr checks with pending checks
)

// Locate finds the fake GitHub for a gh call run in dir: $FAKEGH_REPO, else
// the origin remote of the checkout dir is in.
func Locate(dir string) (Repo, error) {
	if d := os.Getenv(RepoEnv); d != "" {
		return Repo{Dir: d}, nil
	}
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return Repo{}, fmt.Errorf("fakegh: no origin remote in %s: %w", dir, err)
	}
	url := strings.TrimSpace(string(out))
	url = strings.TrimPrefix(url, "file://")
	if !filepath.IsAbs(url) {
		url = filepath.Join(dir, url)
	}
	return Repo{Dir: url}, nil
}

// Main runs one gh invocation in dir and returns its exit code.
func Main(dir string, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "--version" {
		fmt.Fprintln(stdout, "gh version 2.99.0 (fakegh)")
		return exitOK
	}
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		fmt.Fprintln(stdout, "github.com\n  ✓ Logged in to github.com account e2e (fakegh)\n  - Token scopes: 'repo', 'workflow'")
		return exitOK
	}
	r, err := Locate(dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitErr
	}
	code := exitOK
	err = r.Update(func(s *State) error {
		s.Calls = append(s.Calls, args)
		for prefix, msg := range s.Fail {
			if strings.HasPrefix(strings.Join(args, " "), prefix) {
				return &ghError{msg: msg, code: exitErr}
			}
		}
		c := &call{dir: r.Dir, s: s, out: stdout}
		code, err = c.run(args)
		return err
	})
	var ge *ghError
	if errors.As(err, &ge) {
		// The calls log must keep failed calls too.
		_ = r.Update(func(s *State) error { s.Calls = append(s.Calls, args); return nil })
		fmt.Fprintln(stderr, ge.msg)
		return ge.code
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitErr
	}
	return code
}

// ghError is a gh failure: printed to stderr, exit code, and the state is
// not saved.
type ghError struct {
	msg  string
	code int
}

func (e *ghError) Error() string { return e.msg }

func fail(format string, a ...any) error {
	return &ghError{msg: fmt.Sprintf(format, a...), code: exitErr}
}

type call struct {
	dir string
	s   *State
	out io.Writer
}

// flags splits args into positionals and flag values. Repeated flags keep
// every value. Boolean flags are those in bools.
func flags(args []string, bools ...string) (pos []string, f map[string][]string) {
	f = map[string][]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		if k, v, ok := strings.Cut(name, "="); ok {
			f[k] = append(f[k], v)
			continue
		}
		if slices.Contains(bools, name) || i+1 >= len(args) {
			f[name] = append(f[name], "")
			continue
		}
		f[name] = append(f[name], args[i+1])
		i++
	}
	return pos, f
}

func one(f map[string][]string, names ...string) string {
	for _, n := range names {
		if v := f[n]; len(v) > 0 {
			return v[len(v)-1]
		}
	}
	return ""
}

func has(f map[string][]string, name string) bool { _, ok := f[name]; return ok }

func (c *call) run(args []string) (int, error) {
	if len(args) < 2 {
		return exitErr, fail("fakegh: unsupported: gh %s", strings.Join(args, " "))
	}
	switch args[0] {
	case "pr":
		return c.pr(args[1], args[2:])
	case "issue":
		return exitOK, c.issue(args[1], args[2:])
	case "label":
		return exitOK, c.label(args[1], args[2:])
	case "api":
		return exitOK, c.api(args[1:])
	case "repo":
		if args[1] == "view" {
			_, f := flags(args[2:])
			v := map[string]any{"name": c.s.Name, "url": c.s.URL(), "nameWithOwner": c.s.Owner + "/" + c.s.Name,
				"defaultBranchRef": map[string]any{"name": c.s.Default}}
			return exitOK, c.print(pick(v, one(f, "json")), one(f, "q", "jq"))
		}
	case "run":
		if args[1] == "view" {
			_, f := flags(args[2:], "log-failed")
			job := one(f, "job")
			for _, p := range c.s.PRs {
				for i, ch := range p.Checks {
					if jobID(p.Number, i) == job {
						fmt.Fprintln(c.out, ch.Log)
						return exitOK, nil
					}
				}
			}
			return exitErr, fail("could not find job %s", job)
		}
	}
	return exitErr, fail("fakegh: unsupported: gh %s", strings.Join(args, " "))
}

// ref finds the PR a gh pr argument names: number, #number, URL or head branch.
func (c *call) ref(arg string) (*PR, error) {
	n := strings.TrimPrefix(arg, "#")
	if i := strings.LastIndex(n, "/pull/"); i >= 0 {
		n = n[i+len("/pull/"):]
	}
	if num, err := strconv.Atoi(n); err == nil {
		if p := c.s.PR(num); p != nil {
			return p, nil
		}
		return nil, fail("GraphQL: Could not resolve to a PullRequest with the number of %d. (repository.pullRequest)", num)
	}
	for i := len(c.s.PRs) - 1; i >= 0; i-- {
		if c.s.PRs[i].Head == arg {
			return c.s.PRs[i], nil
		}
	}
	return nil, fail("no pull requests found for branch %q", arg)
}

func (c *call) pr(sub string, args []string) (int, error) {
	pos, f := flags(args, "squash", "rebase", "merge", "delete-branch", "admin", "auto", "draft")
	if sub == "create" {
		return exitOK, c.prCreate(f)
	}
	if sub == "list" {
		return exitOK, c.prList(f)
	}
	ref := ""
	if len(pos) > 0 {
		ref = pos[0]
	}
	p, err := c.ref(ref)
	if err != nil {
		return exitErr, err
	}
	switch sub {
	case "view":
		return exitOK, c.print(pick(c.prJSON(p), one(f, "json")), one(f, "q", "jq"))
	case "edit":
		if b := one(f, "base"); b != "" {
			if branchSHA(c.dir, b) == "" {
				return exitErr, fail("could not edit PR #%d: base branch %s does not exist", p.Number, b)
			}
			p.Base = b
		}
		if has(f, "body") {
			p.Body = one(f, "body")
		}
		if has(f, "title") {
			p.Title = one(f, "title")
		}
		for _, l := range f["add-label"] {
			if !slices.Contains(c.s.Labels, l) {
				return exitErr, fail("could not add label: '%s' not found", l)
			}
			if !slices.Contains(p.Labels, l) {
				p.Labels = append(p.Labels, l)
			}
		}
		for _, l := range f["remove-label"] {
			p.Labels = slices.DeleteFunc(p.Labels, func(x string) bool { return x == l })
		}
		fmt.Fprintln(c.out, c.s.PRURL(p.Number))
		return exitOK, nil
	case "comment":
		c.s.NextComment++
		p.Comments = append(p.Comments, Comment{ID: fmt.Sprintf("IC_%d", c.s.NextComment), Body: one(f, "body", "b")})
		fmt.Fprintf(c.out, "%s#issuecomment-%d\n", c.s.PRURL(p.Number), c.s.NextComment)
		return exitOK, nil
	case "close":
		if p.State != "OPEN" {
			return exitErr, fail("Pull request #%d is already %s", p.Number, strings.ToLower(p.State))
		}
		p.State, p.HeadOid = "CLOSED", branchSHA(c.dir, p.Head)
		return exitOK, nil
	case "merge":
		method := ""
		for _, m := range []string{"squash", "rebase", "merge"} {
			if has(f, m) {
				method = m
			}
		}
		if method == "" {
			return exitErr, fail("--merge, --rebase, or --squash required when not running interactively")
		}
		if err := merge(c.dir, c.s, p, method, one(f, "match-head-commit")); err != nil {
			return exitErr, fail("%v", err)
		}
		return exitOK, nil
	case "checks":
		if len(p.Checks) == 0 {
			return exitErr, fail("no checks reported on the '%s' branch", p.Head)
		}
		var out []map[string]any
		code := exitOK
		for i, ch := range p.Checks {
			out = append(out, map[string]any{"name": ch.Name, "workflow": ch.Workflow, "bucket": ch.State,
				"link": fmt.Sprintf("%s/actions/runs/%d/job/%s", c.s.URL(), 1000+p.Number, jobID(p.Number, i))})
			switch ch.State {
			case Fail:
				code = exitErr
			case Pending:
				if code == exitOK {
					code = exitPending
				}
			}
		}
		// gh prints the JSON and still exits non-zero for failing or pending checks.
		if err := c.print(out, ""); err != nil {
			return exitErr, err
		}
		return code, nil
	}
	return exitErr, fail("fakegh: unsupported: gh pr %s", sub)
}

func jobID(pr, i int) string { return strconv.Itoa(pr*100 + i) }

func (c *call) prCreate(f map[string][]string) error {
	head, base := one(f, "head", "H"), one(f, "base", "B")
	if base == "" {
		base = c.s.Default
	}
	if branchSHA(c.dir, head) == "" {
		return fail("pull request create failed: GraphQL: Head sha can't be blank, No commits between %s and %s", base, head)
	}
	if branchSHA(c.dir, base) == "" {
		return fail("pull request create failed: GraphQL: Base ref must be a branch (createPullRequest)")
	}
	for _, p := range c.s.Open() {
		if p.Head == head {
			return fail("a pull request for branch %q into branch %q already exists:\n%s", head, p.Base, c.s.PRURL(p.Number))
		}
	}
	p := &PR{Number: c.s.Next, Title: one(f, "title", "t"), Body: one(f, "body", "b"), Head: head, Base: base,
		State: "OPEN", Draft: has(f, "draft"), Checks: slices.Clone(c.s.DefaultChecks)}
	c.s.Next++
	c.s.PRs = append(c.s.PRs, p)
	fmt.Fprintln(c.out, c.s.PRURL(p.Number))
	return nil
}

func (c *call) prList(f map[string][]string) error {
	state := strings.ToUpper(one(f, "state", "s"))
	if state == "" {
		state = "OPEN"
	}
	limit := 30
	if l, err := strconv.Atoi(one(f, "limit", "L")); err == nil {
		limit = l
	}
	var out []map[string]any
	for i := len(c.s.PRs) - 1; i >= 0 && len(out) < limit; i-- {
		p := c.s.PRs[i]
		if state != "ALL" && p.State != state {
			continue
		}
		if h := one(f, "head", "H"); h != "" && p.Head != h {
			continue
		}
		if b := one(f, "base", "B"); b != "" && p.Base != b {
			continue
		}
		out = append(out, pick(c.prJSON(p), one(f, "json")).(map[string]any))
	}
	if out == nil {
		out = []map[string]any{}
	}
	return c.print(out, one(f, "q", "jq"))
}

// prJSON is every field gh pr view --json can return that saddle asks for.
func (c *call) prJSON(p *PR) map[string]any {
	head := p.HeadOid
	if p.State == "OPEN" {
		head = branchSHA(c.dir, p.Head)
	}
	m := p.Mergeable
	if m == "" {
		m = "UNKNOWN"
		if p.State == "OPEN" {
			m = mergeable(c.dir, p)
		}
	}
	ms := p.MergeState
	if ms == "" {
		ms = mergeState(p, m)
	}
	labels := []map[string]any{}
	for _, l := range p.Labels {
		labels = append(labels, map[string]any{"name": l})
	}
	comments := []map[string]any{}
	for _, cm := range p.Comments {
		comments = append(comments, map[string]any{"id": cm.ID, "body": cm.Body, "author": map[string]any{"login": "e2e"}})
	}
	rollup := []map[string]any{}
	for _, ch := range p.Checks {
		status, concl := "COMPLETED", "SUCCESS"
		switch ch.State {
		case Fail:
			concl = "FAILURE"
		case Pending:
			status, concl = "IN_PROGRESS", ""
		}
		rollup = append(rollup, map[string]any{"__typename": "CheckRun", "name": ch.Name, "workflowName": ch.Workflow,
			"status": status, "conclusion": concl})
	}
	files := []map[string]any{}
	for _, f := range changedFiles(c.dir, p) {
		files = append(files, map[string]any{"path": f, "additions": 1, "deletions": 0})
	}
	var mc any
	if p.MergeCommit != "" {
		mc = map[string]any{"oid": p.MergeCommit}
	}
	return map[string]any{
		"number": p.Number, "url": c.s.PRURL(p.Number), "title": p.Title, "body": p.Body, "state": p.State,
		"isDraft": p.Draft, "mergeable": m, "mergeStateStatus": ms, "baseRefName": p.Base, "headRefName": p.Head,
		"headRefOid": head, "labels": labels, "comments": comments, "statusCheckRollup": rollup, "files": files,
		"mergeCommit": mc, "closed": p.State != "OPEN", "merged": p.State == "MERGED",
	}
}

// mergeState is GitHub's mergeStateStatus for an open PR without an override.
func mergeState(p *PR, mergeable string) string {
	if p.State != "OPEN" {
		return "UNKNOWN"
	}
	switch mergeable {
	case "CONFLICTING":
		return "DIRTY"
	case "UNKNOWN":
		return "UNKNOWN"
	}
	if p.Draft {
		return "DRAFT"
	}
	for _, ch := range p.Checks {
		if ch.State == Fail {
			return "UNSTABLE"
		}
		if ch.State == Pending {
			return "BLOCKED"
		}
	}
	return "CLEAN"
}

func (c *call) issue(sub string, args []string) error {
	pos, f := flags(args)
	switch sub {
	case "create":
		n := c.s.Next
		c.s.Next++
		c.s.Issues = append(c.s.Issues, &Issue{Number: n, Title: one(f, "title", "t"), Body: one(f, "body", "b"), State: "OPEN"})
		fmt.Fprintf(c.out, "%s/issues/%d\n", c.s.URL(), n)
		return nil
	case "view":
		if len(pos) == 0 {
			return fail("issue number required")
		}
		is, err := c.findIssue(pos[0])
		if err != nil {
			return err
		}
		return c.print(pick(c.issueJSON(is), one(f, "json")), one(f, "q", "jq"))
	}
	return fail("fakegh: unsupported: gh issue %s", sub)
}

func (c *call) findIssue(arg string) (*Issue, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
	if err != nil {
		return nil, fail("invalid issue %q", arg)
	}
	for _, is := range c.s.Issues {
		if is.Number == n {
			return is, nil
		}
	}
	return nil, fail("GraphQL: Could not resolve to an issue or pull request with the number of %d. (repository.issue)", n)
}

func (c *call) issueJSON(is *Issue) map[string]any {
	labels := []map[string]any{}
	for _, l := range is.Labels {
		labels = append(labels, map[string]any{"name": l})
	}
	return map[string]any{"number": is.Number, "id": 9000 + is.Number, "title": is.Title, "state": is.State,
		"body": is.Body, "url": fmt.Sprintf("%s/issues/%d", c.s.URL(), is.Number), "labels": labels}
}

func (c *call) label(sub string, args []string) error {
	pos, f := flags(args, "force", "f")
	if sub != "create" || len(pos) == 0 {
		return fail("fakegh: unsupported: gh label %s", sub)
	}
	if slices.Contains(c.s.Labels, pos[0]) {
		if !has(f, "force") && !has(f, "f") {
			return fail("label with name %q already exists; use `--force` to update its color and description", pos[0])
		}
		return nil
	}
	c.s.Labels = append(c.s.Labels, pos[0])
	return nil
}

func (c *call) api(args []string) error {
	pos, f := flags(args, "paginate", "i", "include", "silent")
	if len(pos) == 0 {
		return fail("api: endpoint required")
	}
	ep := strings.TrimPrefix(pos[0], "/")
	ep = strings.ReplaceAll(ep, "{owner}/{repo}", c.s.Owner+"/"+c.s.Name)
	method := strings.ToUpper(one(f, "X", "method"))
	fields := map[string]string{}
	for _, k := range []string{"f", "F", "field", "raw-field"} {
		for _, kv := range f[k] {
			name, v, _ := strings.Cut(kv, "=")
			fields[name] = v
		}
	}
	if method == "" {
		method = "GET"
		if len(fields) > 0 && ep != "graphql" {
			method = "POST"
		}
	}
	jq := one(f, "q", "jq")
	if ep == "graphql" {
		return c.graphql(fields, jq)
	}
	parts := strings.Split(ep, "/")
	if len(parts) < 3 || parts[0] != "repos" {
		return fail("gh: Not Found (HTTP 404)")
	}
	rest := parts[3:]
	switch {
	case len(rest) == 0:
		if method == "PATCH" {
			for k, v := range fields {
				b := v == "true"
				switch k {
				case "allow_merge_commit":
					c.s.Repo.AllowMerge = b
				case "allow_squash_merge":
					c.s.Repo.AllowSquash = b
				case "allow_rebase_merge":
					c.s.Repo.AllowRebase = b
				case "delete_branch_on_merge":
					c.s.Repo.DeleteBranch = b
				}
			}
		}
		return c.print(map[string]any{"full_name": c.s.Owner + "/" + c.s.Name, "html_url": c.s.URL(),
			"default_branch": c.s.Default, "allow_merge_commit": c.s.Repo.AllowMerge, "allow_squash_merge": c.s.Repo.AllowSquash,
			"allow_rebase_merge": c.s.Repo.AllowRebase, "delete_branch_on_merge": c.s.Repo.DeleteBranch}, jq)
	case len(rest) == 3 && rest[0] == "branches" && rest[2] == "protection":
		if !c.s.Protected || rest[1] != c.s.Default {
			return fail("gh: Branch not protected (HTTP 404)")
		}
		return c.print(map[string]any{"required_status_checks": map[string]any{"contexts": []string{"ci"}, "checks": []any{}}}, jq)
	case len(rest) >= 2 && rest[0] == "issues":
		is, err := c.findIssue(rest[1])
		if err != nil {
			return fail("gh: Not Found (HTTP 404)")
		}
		if len(rest) == 2 {
			return c.print(c.issueJSON(is), jq)
		}
		if rest[2] == "sub_issues" {
			if method == "POST" {
				id, _ := strconv.Atoi(fields["sub_issue_id"])
				is.Subs = append(is.Subs, id-9000)
				return c.print(c.issueJSON(is), jq)
			}
			subs := []map[string]any{}
			for _, n := range is.Subs {
				if sub, err := c.findIssue(strconv.Itoa(n)); err == nil {
					subs = append(subs, c.issueJSON(sub))
				}
			}
			return c.print(subs, jq)
		}
	}
	return fail("gh: Not Found (HTTP 404)")
}

func (c *call) graphql(fields map[string]string, jq string) error {
	q := fields["query"]
	if strings.Contains(q, "updateIssueComment") {
		for _, p := range c.s.PRs {
			for i := range p.Comments {
				if p.Comments[i].ID == fields["id"] {
					p.Comments[i].Body = fields["body"]
					return c.print(map[string]any{"data": map[string]any{"updateIssueComment": map[string]any{"issueComment": map[string]any{"id": fields["id"]}}}}, jq)
				}
			}
		}
		return fail("GraphQL: Could not resolve to a node with the global id of '%s'", fields["id"])
	}
	return fail("fakegh: unsupported graphql query: %s", q)
}

// pick keeps only the comma-separated fields of v; gh --json does the same.
func pick(v map[string]any, fields string) any {
	if fields == "" {
		return v
	}
	out := map[string]any{}
	for _, k := range strings.Split(fields, ",") {
		k = strings.TrimSpace(k)
		out[k] = v[k]
	}
	return out
}

// print writes v as gh does: JSON, or through a jq expression.
func (c *call) print(v any, jq string) error {
	if jq == "" {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		fmt.Fprintln(c.out, string(b))
		return nil
	}
	res, err := evalJQ(jq, v)
	if err != nil {
		return fail("%v", err)
	}
	for _, r := range res {
		if s, ok := r.(string); ok {
			fmt.Fprintln(c.out, s)
			continue
		}
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		fmt.Fprintln(c.out, string(b))
	}
	return nil
}
