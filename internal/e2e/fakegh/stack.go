package fakegh

import (
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// Stack is a native stacked PR on GitHub (the gh-stack extension, #211).
type Stack struct {
	Number int    `json:"number"`
	Base   string `json:"base"`
	Open   bool   `json:"open"`
	PRs    []int  `json:"prs"` // bottom to top
}

// stackVersion is what the fake extension reports.
const stackVersion = "0.1.1"

// stackOf returns the stack holding PR n, preferring an open one.
func (s *State) stackOf(n int) *Stack {
	var found *Stack
	for _, st := range s.Stacks {
		if slices.Contains(st.PRs, n) && (found == nil || st.Open) {
			found = st
		}
	}
	return found
}

// stack runs gh stack: --version, link, view and merge. The commands that
// track stacks locally or push (init, add, submit, sync, push) aren't
// faked, so a saddle that called them would fail loudly.
func (c *call) stack(args []string) (int, error) {
	if c.s.NoStackExt {
		return exitErr, fail("unknown command %q for %q", "stack", "gh")
	}
	if len(args) > 0 && (args[0] == "--version" || args[0] == "-v") {
		fmt.Fprintf(c.out, "gh stack version %s\n", stackVersion)
		return exitOK, nil
	}
	if len(args) == 0 {
		return exitErr, fail("fakegh: unsupported: gh stack")
	}
	switch args[0] {
	case "link":
		return exitOK, c.stackLink(args[1:])
	case "view":
		return exitOK, c.stackView(args[1:])
	case "merge":
		return exitOK, c.stackMerge(args[1:])
	}
	return exitErr, fail("fakegh: unsupported: gh stack %s (saddle may only link, view and merge)", args[0])
}

func (c *call) stacksEnabled() error {
	if c.s.StacksDisabled {
		return fail("Stacked PRs are not enabled for this repository")
	}
	return nil
}

// stackLink is gh stack link: PR numbers or URLs, bottom to top. A branch
// argument would make gh-stack push it, which only saddle's train may do,
// so the fake refuses one. Bases must already chain: the bottom PR on
// --base, each other on the head of the PR below.
func (c *call) stackLink(args []string) error {
	pos, f := flags(args, "open")
	if err := c.stacksEnabled(); err != nil {
		return err
	}
	if len(pos) < 2 {
		return fail("accepts at least 2 arg(s), received %d", len(pos))
	}
	base := one(f, "base")
	if base == "" {
		base = c.s.Default
	}
	var prs []*PR
	for _, a := range pos {
		n := strings.TrimPrefix(a, "#")
		if i := strings.LastIndex(n, "/pull/"); i >= 0 {
			n = n[i+len("/pull/"):]
		}
		num, err := strconv.Atoi(n)
		if err != nil || c.s.PR(num) == nil {
			return fail("fakegh: gh stack link %q: a branch argument makes gh-stack push it; pass PR numbers or URLs", a)
		}
		p := c.s.PR(num)
		if p.State != "OPEN" {
			return fail("pull request #%d is %s", num, strings.ToLower(p.State))
		}
		prs = append(prs, p)
	}
	for i, p := range prs {
		want := base
		if i > 0 {
			want = prs[i-1].Head
		}
		if p.Base != want {
			return fail("fakegh: PR #%d targets %s, not %s, so it can't sit there in the stack", p.Number, p.Base, want)
		}
	}
	var st *Stack
	for _, p := range prs {
		o := c.s.stackOf(p.Number)
		if o == nil || !o.Open {
			continue
		}
		if st != nil && o != st {
			return fail("#%d belongs to a different stack (#%d) than #%d", p.Number, o.Number, st.Number)
		}
		st = o
	}
	if st == nil {
		st = &Stack{Number: c.s.Next, Base: base, Open: true}
		c.s.Next++
		c.s.Stacks = append(c.s.Stacks, st)
		for _, p := range prs {
			st.PRs = append(st.PRs, p.Number)
		}
		fmt.Fprintf(c.out, "Created stack #%d with %d PRs\n", st.Number, len(st.PRs))
		return nil
	}
	for _, p := range prs {
		if !slices.Contains(st.PRs, p.Number) {
			st.PRs = append(st.PRs, p.Number)
		}
	}
	fmt.Fprintf(c.out, "Updated stack to %d PRs\n", len(st.PRs))
	return nil
}

// stackJSON is a stack as the REST API returns it.
func (c *call) stackJSON(st *Stack) map[string]any {
	prs := []map[string]any{}
	for _, n := range st.PRs {
		p := c.s.PR(n)
		state, merged := "open", any(nil)
		if p.State != "OPEN" {
			state = "closed"
		}
		if p.State == "MERGED" {
			merged = "2026-10-04T00:00:00Z"
		}
		head := p.HeadOid
		if p.State == "OPEN" {
			head = branchSHA(c.dir, p.Head)
		}
		prs = append(prs, map[string]any{"number": n, "state": state, "draft": p.Draft, "merged_at": merged,
			"head": map[string]any{"ref": p.Head, "sha": head}})
	}
	return map[string]any{"number": st.Number, "url": fmt.Sprintf("https://api.github.com/repos/%s/%s/stacks/%d", c.s.Owner, c.s.Name, st.Number),
		"base": map[string]any{"ref": st.Base}, "open": st.Open, "pull_requests": prs}
}

// stacksAPI answers GET repos/o/r/stacks and repos/o/r/stacks/n.
func (c *call) stacksAPI(rest []string, jq string) error {
	if err := c.stacksEnabled(); err != nil {
		return fail("gh: Stacked PRs are not enabled for this repository (HTTP 404)")
	}
	if len(rest) == 2 {
		n, _ := strconv.Atoi(rest[1])
		for _, st := range c.s.Stacks {
			if st.Number == n {
				return c.print(c.stackJSON(st), jq)
			}
		}
		return fail("gh: Not Found (HTTP 404)")
	}
	out := []map[string]any{}
	for _, st := range c.s.Stacks {
		out = append(out, c.stackJSON(st))
	}
	return c.print(out, jq)
}

// stackView is gh stack view --json for the stack of the checked-out branch.
func (c *call) stackView(args []string) error {
	_, f := flags(args, "json", "short", "s")
	if !has(f, "json") {
		return fail("fakegh: gh stack view needs --json")
	}
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = c.cwd
	b, err := cmd.Output()
	if err != nil {
		return fail("not on a branch")
	}
	br := strings.TrimSpace(string(b))
	for i := len(c.s.PRs) - 1; i >= 0; i-- {
		p := c.s.PRs[i]
		if p.Head != br {
			continue
		}
		if st := c.s.stackOf(p.Number); st != nil {
			return c.print(c.stackJSON(st), "")
		}
	}
	return fail("current branch %q is not part of a stack", br)
}

// stackMerge is gh stack merge <stack|PR> --yes: an atomic merge of the
// stack up to the PR into its base. Nothing moves unless every PR merges.
func (c *call) stackMerge(args []string) error {
	pos, f := flags(args, "yes", "y", "squash", "rebase", "merge")
	if err := c.stacksEnabled(); err != nil {
		return err
	}
	if !has(f, "yes") && !has(f, "y") {
		return fail("--yes required when not running interactively")
	}
	if len(pos) != 1 {
		return fail("fakegh: gh stack merge needs a stack or PR number")
	}
	n, err := strconv.Atoi(strings.TrimPrefix(pos[0], "#"))
	if err != nil {
		return fail("invalid stack or PR number %q", pos[0])
	}
	var st *Stack
	upTo := 0
	for _, s := range c.s.Stacks {
		if s.Number == n {
			st, upTo = s, s.PRs[len(s.PRs)-1]
		}
	}
	if st == nil {
		if st = c.s.stackOf(n); st == nil {
			return fail("no stack found for %d", n)
		}
		upTo = n
	}
	if !st.Open {
		return fail("stack #%d is already merged", st.Number)
	}
	method := one(f, "merge-method")
	for _, m := range []string{"squash", "rebase", "merge"} {
		if has(f, m) {
			method = m
		}
	}
	if method == "" {
		method = "squash"
	}
	var todo []*PR
	for _, num := range st.PRs {
		p := c.s.PR(num)
		if p.State == "OPEN" {
			if p.Draft {
				return fail("pull request #%d is a draft", num)
			}
			todo = append(todo, p)
		}
		if num == upTo {
			break
		}
	}
	// All or nothing: remember every branch and PR, put them back on failure.
	refs := map[string]string{}
	for _, b := range append([]string{st.Base}, prHeads(todo)...) {
		refs[b] = branchSHA(c.dir, b)
	}
	saved := map[int]PR{}
	for _, p := range c.s.Open() {
		saved[p.Number] = *p
	}
	for _, p := range todo {
		p.Base = st.Base
		if err := merge(c.dir, c.s, p, method, ""); err != nil {
			for b, sha := range refs {
				if sha != "" {
					_, _ = git(c.dir, "update-ref", "refs/heads/"+b, sha)
				}
			}
			for _, o := range c.s.PRs {
				if v, ok := saved[o.Number]; ok {
					*o = v
				}
			}
			return fail("stack merge failed, nothing was merged: %v", err)
		}
	}
	open := false
	for _, num := range st.PRs {
		if c.s.PR(num).State == "OPEN" {
			open = true
		}
	}
	st.Open = open
	fmt.Fprintf(c.out, "Merged %d PRs into %s\n", len(todo), st.Base)
	return nil
}

func prHeads(ps []*PR) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Head)
	}
	return out
}
