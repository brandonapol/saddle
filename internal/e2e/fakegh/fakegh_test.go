package fakegh

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fixture is a bare origin with fake GitHub state and a clone of it.
type fixture struct {
	t     *testing.T
	bare  string
	clone string
	gh    Repo
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
	f := &fixture{t: t, bare: filepath.Join(d, "origin.git"), clone: filepath.Join(d, "work")}
	run(t, d, "git", "init", "-q", "--bare", "-b", "main", f.bare)
	run(t, d, "git", "clone", "-q", f.bare, f.clone)
	f.write("README", "hello\n")
	f.commit("init")
	run(t, f.clone, "git", "push", "-q", "origin", "HEAD:main")
	var err error
	if f.gh, err = Init(f.bare, "e2e", "demo"); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) write(name, body string) {
	if err := os.WriteFile(filepath.Join(f.clone, name), []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) commit(msg string) {
	run(f.t, f.clone, "git", "add", "-A")
	run(f.t, f.clone, "git", "commit", "-q", "-m", msg)
}

// branch commits name=body on a new branch off main and pushes it.
func (f *fixture) branch(br, name, body string) {
	run(f.t, f.clone, "git", "checkout", "-q", "-B", br, "origin/main")
	f.write(name, body)
	f.commit(br)
	run(f.t, f.clone, "git", "push", "-q", "-f", "origin", br)
}

// gh runs the fake gh in the clone and returns stdout and the exit code.
func (f *fixture) ghRun(args ...string) (string, string, int) {
	var out, errb bytes.Buffer
	code := Main(f.clone, args, &out, &errb)
	return strings.TrimSpace(out.String()), strings.TrimSpace(errb.String()), code
}

func (f *fixture) must(args ...string) string {
	f.t.Helper()
	out, errs, code := f.ghRun(args...)
	if code != 0 {
		f.t.Fatalf("gh %v exited %d: %s", args, code, errs)
	}
	return out
}

func (f *fixture) view(ref string) map[string]any {
	f.t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(f.must("pr", "view", ref, "--json", "state,mergeable,mergeStateStatus,baseRefName,headRefOid,labels,statusCheckRollup")), &v); err != nil {
		f.t.Fatal(err)
	}
	return v
}

func TestCreateViewAndSquashMerge(t *testing.T) {
	f := newFixture(t)
	f.branch("feat", "a.txt", "a\n")
	url := f.must("pr", "create", "--base", "main", "--head", "feat", "--title", "Add a", "--body", "b")
	if url != "https://github.com/e2e/demo/pull/1" {
		t.Fatalf("url = %q", url)
	}
	v := f.view(url)
	if v["state"] != "OPEN" || v["mergeable"] != "MERGEABLE" || v["mergeStateStatus"] != "CLEAN" || v["baseRefName"] != "main" {
		t.Fatalf("view = %v", v)
	}
	head := run(t, f.clone, "git", "rev-parse", "feat")
	if v["headRefOid"] != head {
		t.Fatalf("headRefOid = %v, want %s", v["headRefOid"], head)
	}
	if _, errs, code := f.ghRun("pr", "merge", "1", "--squash", "--match-head-commit", "deadbeef"); code == 0 || !strings.Contains(errs, "head branch was modified") {
		t.Fatalf("stale head merged: %d %s", code, errs)
	}
	f.must("pr", "merge", url, "--squash", "--match-head-commit", head)
	if v := f.view("1"); v["state"] != "MERGED" {
		t.Fatalf("after merge state = %v", v["state"])
	}
	run(t, f.clone, "git", "fetch", "-q")
	if got := run(t, f.clone, "git", "show", "origin/main:a.txt"); got != "a" {
		t.Fatalf("main a.txt = %q", got)
	}
	if n := run(t, f.clone, "git", "rev-list", "--count", "origin/main"); n != "2" {
		t.Fatalf("squash made %s commits on main, want 2", n)
	}
	if parents := run(t, f.clone, "git", "log", "-1", "--format=%P", "origin/main"); strings.Contains(parents, " ") {
		t.Fatalf("squash commit has two parents: %s", parents)
	}
}

func TestConflictingPRCannotMerge(t *testing.T) {
	f := newFixture(t)
	f.branch("one", "x.txt", "one\n")
	f.branch("two", "x.txt", "two\n")
	f.must("pr", "create", "--base", "main", "--head", "one", "--title", "one")
	f.must("pr", "create", "--base", "main", "--head", "two", "--title", "two")
	f.must("pr", "merge", "1", "--squash")
	if v := f.view("2"); v["mergeable"] != "CONFLICTING" || v["mergeStateStatus"] != "DIRTY" {
		t.Fatalf("view = %v", v)
	}
	if _, _, code := f.ghRun("pr", "merge", "2", "--squash"); code == 0 {
		t.Fatal("conflicting PR merged")
	}
}

func TestRebaseMergeReplaysCommits(t *testing.T) {
	f := newFixture(t)
	run(t, f.clone, "git", "checkout", "-q", "-B", "feat", "origin/main")
	f.write("a", "1\n")
	f.commit("first")
	f.write("b", "2\n")
	f.commit("second")
	run(t, f.clone, "git", "push", "-q", "origin", "feat")
	f.must("pr", "create", "--base", "main", "--head", "feat", "--title", "t")
	f.must("pr", "merge", "1", "--rebase")
	run(t, f.clone, "git", "fetch", "-q")
	if got := run(t, f.clone, "git", "log", "--format=%s", "origin/main"); got != "second\nfirst\ninit" {
		t.Fatalf("log = %q", got)
	}
}

func TestMergeCommitsRefusedUnlessAllowed(t *testing.T) {
	f := newFixture(t)
	f.branch("feat", "a", "a\n")
	f.must("pr", "create", "--base", "main", "--head", "feat", "--title", "t")
	if _, errs, code := f.ghRun("pr", "merge", "1", "--merge"); code == 0 || !strings.Contains(errs, "not allowed") {
		t.Fatalf("merge commit allowed: %d %s", code, errs)
	}
}

func TestChecksFlipAndExitCodes(t *testing.T) {
	f := newFixture(t)
	f.branch("feat", "a", "a\n")
	f.must("pr", "create", "--base", "main", "--head", "feat", "--title", "t")
	if _, errs, code := f.ghRun("pr", "checks", "1", "--json", "name,workflow,bucket,link"); code != 1 || !strings.Contains(errs, "no checks reported") {
		t.Fatalf("no checks: %d %s", code, errs)
	}
	if err := f.gh.SetAllChecks(Pending); err != nil {
		t.Fatal(err)
	}
	out, _, code := f.ghRun("pr", "checks", "1", "--json", "name,workflow,bucket,link")
	if code != exitPending || !strings.Contains(out, `"bucket":"pending"`) {
		t.Fatalf("pending: %d %s", code, out)
	}
	if v := f.view("1"); v["mergeStateStatus"] != "BLOCKED" {
		t.Fatalf("pending merge state = %v", v["mergeStateStatus"])
	}
	if err := f.gh.SetChecks(1, Check{Name: "ci", State: Fail, Log: "boom"}); err != nil {
		t.Fatal(err)
	}
	out, _, code = f.ghRun("pr", "checks", "1", "--json", "name,workflow,bucket,link")
	if code != 1 || !strings.Contains(out, `"bucket":"fail"`) {
		t.Fatalf("fail: %d %s", code, out)
	}
	if got := f.must("run", "view", "--job", "100", "--log-failed"); got != "boom" {
		t.Fatalf("log = %q", got)
	}
	if err := f.gh.SetAllChecks(Pass); err != nil {
		t.Fatal(err)
	}
	if v := f.view("1"); v["mergeStateStatus"] != "CLEAN" {
		t.Fatalf("pass merge state = %v", v["mergeStateStatus"])
	}
}

func TestLabelsCommentsAndEdit(t *testing.T) {
	f := newFixture(t)
	f.branch("feat", "a", "a\n")
	f.branch("base2", "b", "b\n")
	f.must("pr", "create", "--base", "main", "--head", "feat", "--title", "t")
	if _, _, code := f.ghRun("pr", "edit", "1", "--add-label", "needs-human"); code == 0 {
		t.Fatal("added a label that doesn't exist")
	}
	f.must("label", "create", "needs-human", "--color", "D93F0B", "--force")
	f.must("pr", "edit", "1", "--add-label", "needs-human", "--base", "base2", "--body", "new body")
	s, err := f.gh.Load()
	if err != nil {
		t.Fatal(err)
	}
	p := s.PR(1)
	if p.Base != "base2" || p.Body != "new body" || len(p.Labels) != 1 {
		t.Fatalf("pr = %+v", p)
	}
	f.must("pr", "edit", "1", "--remove-label", "needs-human")
	f.must("pr", "comment", "1", "--body", "hi")
	out := f.must("pr", "view", "1", "--json", "comments")
	if !strings.Contains(out, `"id":"IC_1"`) {
		t.Fatalf("comments = %s", out)
	}
	f.must("api", "graphql", "-f", "query=mutation { updateIssueComment }", "-f", "id=IC_1", "-f", "body=edited")
	if s, _ := f.gh.Load(); s.PR(1).Comments[0].Body != "edited" || len(s.PR(1).Labels) != 0 {
		t.Fatalf("after edit: %+v", s.PR(1))
	}
}

func TestDeleteBranchOnMergeRetargets(t *testing.T) {
	f := newFixture(t)
	if err := f.gh.Update(func(s *State) error { s.Repo.DeleteBranch = true; return nil }); err != nil {
		t.Fatal(err)
	}
	f.branch("low", "a", "a\n")
	run(t, f.clone, "git", "checkout", "-q", "-B", "high", "low")
	f.write("b", "b\n")
	f.commit("high")
	run(t, f.clone, "git", "push", "-q", "origin", "high")
	f.must("pr", "create", "--base", "main", "--head", "low", "--title", "low")
	f.must("pr", "create", "--base", "low", "--head", "high", "--title", "high")
	f.must("pr", "merge", "1", "--squash")
	if v := f.view("2"); v["baseRefName"] != "main" {
		t.Fatalf("high base = %v", v["baseRefName"])
	}
	if out := run(t, f.clone, "git", "ls-remote", "origin", "refs/heads/low"); out != "" {
		t.Fatalf("low not deleted: %s", out)
	}
}

func TestMergeByHandAndList(t *testing.T) {
	f := newFixture(t)
	f.branch("feat", "a", "a\n")
	f.must("pr", "create", "--base", "main", "--head", "feat", "--title", "t")
	if err := f.gh.MergeByHand(1, "squash"); err != nil {
		t.Fatal(err)
	}
	if out := f.must("pr", "list", "--state", "merged", "--head", "feat", "--limit", "1", "--json", "number"); out != `[{"number":1}]` {
		t.Fatalf("list = %s", out)
	}
	if out := f.must("pr", "list", "--state", "open", "--json", "number"); out != `[]` {
		t.Fatalf("open list = %s", out)
	}
}

func TestRepoAPIAndDoctorCalls(t *testing.T) {
	f := newFixture(t)
	out := f.must("api", "repos/{owner}/{repo}")
	if !strings.Contains(out, `"allow_merge_commit":false`) || !strings.Contains(out, `"allow_squash_merge":true`) {
		t.Fatalf("repo = %s", out)
	}
	if _, errs, code := f.ghRun("api", "repos/{owner}/{repo}/branches/main/protection"); code == 0 || !strings.Contains(errs, "404") {
		t.Fatalf("protection: %d %s", code, errs)
	}
	if got := f.must("repo", "view", "--json", "defaultBranchRef", "-q", ".defaultBranchRef.name"); got != "main" {
		t.Fatalf("default branch = %q", got)
	}
	f.must("api", "-X", "PATCH", "repos/{owner}/{repo}", "-F", "allow_merge_commit=true")
	if s, _ := f.gh.Load(); !s.Repo.AllowMerge {
		t.Fatal("PATCH didn't stick")
	}
	if got := f.must("auth", "status"); !strings.Contains(got, "'repo'") {
		t.Fatalf("auth status = %q", got)
	}
}

func TestIssuesAndSubIssues(t *testing.T) {
	f := newFixture(t)
	f.must("issue", "create", "--title", "epic", "--body", "b")
	f.must("issue", "create", "--title", "child", "--body", "b")
	id := f.must("api", "repos/{owner}/{repo}/issues/2", "--jq", ".id")
	f.must("api", "repos/{owner}/{repo}/issues/1/sub_issues", "-X", "POST", "-F", "sub_issue_id="+id)
	if got := f.must("api", "repos/{owner}/{repo}/issues/1/sub_issues", "--paginate", "--jq", ".[].number"); got != "2" {
		t.Fatalf("subs = %q", got)
	}
	got := f.must("api", "repos/{owner}/{repo}/issues/1/sub_issues", "--jq", "[.[] | {number, title, state, body}]")
	if got != `[{"body":"b","number":2,"state":"OPEN","title":"child"}]` {
		t.Fatalf("projected subs = %s", got)
	}
	if got := f.must("issue", "view", "1", "--json", "number,title"); got != `{"number":1,"title":"epic"}` {
		t.Fatalf("issue view = %s", got)
	}
}

func TestFailInjectionAndCallLog(t *testing.T) {
	f := newFixture(t)
	if err := f.gh.Update(func(s *State) error { s.Fail = map[string]string{"pr create": "HTTP 502"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, errs, code := f.ghRun("pr", "create", "--head", "x"); code == 0 || errs != "HTTP 502" {
		t.Fatalf("injected failure: %d %q", code, errs)
	}
	s, _ := f.gh.Load()
	if len(s.Calls) != 1 || s.Calls[0][0] != "pr" {
		t.Fatalf("calls = %v", s.Calls)
	}
}

func TestJQ(t *testing.T) {
	v := map[string]any{"a": map[string]any{"b": "x"}, "l": []any{map[string]any{"n": 1}, map[string]any{"n": 2}}}
	cases := map[string]string{
		".a.b":            `["x"]`,
		".l[].n":          `[1,2]`,
		".l | .[] | .n":   `[1,2]`,
		"[.l[] | {n}]":    `[[{"n":1},{"n":2}]]`,
		".missing.deeper": `[null]`,
	}
	for expr, want := range cases {
		got, err := evalJQ(expr, v)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		b, _ := json.Marshal(got)
		if string(b) != want {
			t.Errorf("%s = %s, want %s", expr, b, want)
		}
	}
}

// openStack opens PRs a (on main) and b (on a) and returns their numbers.
func (f *fixture) openStack() (int, int) {
	f.branch("a", "a.txt", "a\n")
	run(f.t, f.clone, "git", "checkout", "-q", "-B", "b", "a")
	f.write("b.txt", "b\n")
	f.commit("b")
	run(f.t, f.clone, "git", "push", "-q", "-f", "origin", "b")
	f.must("pr", "create", "--base", "main", "--head", "a", "--title", "A", "--body", "")
	f.must("pr", "create", "--base", "a", "--head", "b", "--title", "B", "--body", "")
	return 1, 2
}

func TestStackLinkViewMerge(t *testing.T) {
	f := newFixture(t)
	a, b := f.openStack()
	if out := f.must("stack", "--version"); out != "gh stack version 0.1.1" {
		t.Fatalf("version = %q", out)
	}
	f.must("stack", "link", "--base", "main", "https://github.com/e2e/demo/pull/1", "2")
	s, _ := f.gh.Load()
	if len(s.Stacks) != 1 || len(s.Stacks[0].PRs) != 2 || s.Stacks[0].PRs[0] != a || s.Stacks[0].PRs[1] != b {
		t.Fatalf("stacks = %+v", s.Stacks)
	}
	// Linking again is a no-op; the API lists it.
	f.must("stack", "link", "--base", "main", "1", "2")
	out := f.must("api", "repos/{owner}/{repo}/stacks?per_page=100", "--paginate")
	var list []struct {
		Number int  `json:"number"`
		Open   bool `json:"open"`
		PRs    []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list) != 1 || len(list[0].PRs) != 2 || !list[0].Open {
		t.Fatalf("api stacks = %s (%v)", out, err)
	}
	run(t, f.clone, "git", "checkout", "-q", "b")
	if v := f.must("stack", "view", "--json"); !strings.Contains(v, `"number":2`) {
		t.Fatalf("view = %s", v)
	}
	// merge by top PR number merges the whole stack into main in one call.
	f.must("stack", "merge", "2", "--yes", "--merge-method", "squash")
	s, _ = f.gh.Load()
	if s.PR(a).State != "MERGED" || s.PR(b).State != "MERGED" || s.Stacks[0].Open {
		t.Fatalf("after merge: %+v %+v open=%v", s.PR(a), s.PR(b), s.Stacks[0].Open)
	}
	if got := run(t, f.bare, "git", "show", "main:b.txt"); got != "b" {
		t.Fatalf("main lacks b's work: %q", got)
	}
}

func TestStackLinkRefusesBadChainAndBranches(t *testing.T) {
	f := newFixture(t)
	f.openStack()
	if _, errs, code := f.ghRun("stack", "link", "2", "1"); code == 0 || !strings.Contains(errs, "targets") {
		t.Fatalf("out-of-order link: code %d %s", code, errs)
	}
	if _, errs, code := f.ghRun("stack", "link", "a", "b"); code == 0 || !strings.Contains(errs, "push") {
		t.Fatalf("branch link must fail (it would push): code %d %s", code, errs)
	}
}

func TestStackMergeIsAllOrNothing(t *testing.T) {
	f := newFixture(t)
	a, b := f.openStack()
	f.must("stack", "link", "1", "2")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(f.gh.Update(func(s *State) error { s.PR(b).Mergeable = "CONFLICTING"; return nil }))
	before := run(t, f.bare, "git", "rev-parse", "main")
	if _, errs, code := f.ghRun("stack", "merge", "2", "--yes"); code == 0 || !strings.Contains(errs, "not mergeable") {
		t.Fatalf("merge: code %d %s", code, errs)
	}
	s, _ := f.gh.Load()
	if s.PR(a).State != "OPEN" || run(t, f.bare, "git", "rev-parse", "main") != before {
		t.Fatalf("a failed stack merge merged part of it: a=%s", s.PR(a).State)
	}
}

func TestStackUnavailable(t *testing.T) {
	f := newFixture(t)
	f.openStack()
	if err := f.gh.Update(func(s *State) error { s.StacksDisabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, errs, code := f.ghRun("stack", "link", "1", "2"); code == 0 || !strings.Contains(errs, "Stacked PRs are not enabled for this repository") {
		t.Fatalf("disabled link: %d %s", code, errs)
	}
	if _, errs, code := f.ghRun("api", "repos/{owner}/{repo}/stacks?per_page=1"); code == 0 || !strings.Contains(errs, "not enabled") {
		t.Fatalf("disabled probe: %d %s", code, errs)
	}
	if err := f.gh.Update(func(s *State) error { s.NoStackExt = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, errs, code := f.ghRun("stack", "--version"); code == 0 || !strings.Contains(errs, `unknown command "stack" for "gh"`) {
		t.Fatalf("missing extension: %d %s", code, errs)
	}
}
