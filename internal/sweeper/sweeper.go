// Package sweeper merges open saddle PRs that are ready, bottom of a stack
// first, and reports why the rest are not.
//
// It is deterministic and shells out to the gh CLI through an injected
// Runner. Ready means: every CI check passed, GitHub says the PR is mergeable,
// its base is the trunk (or its parent PR already merged), it does not carry
// the review label, and it touches a _test.go file whenever it touches other
// Go files. A PR that changes something risky (go.mod, workflows, many
// packages) gets the review label so a human looks at it; the sweeper never
// merges a labeled PR, never forces a merge and never pushes to the trunk.
package sweeper

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Runner runs gh with args and returns its stdout. ciwatch.ExecRunner is the
// real one.
type Runner func(ctx context.Context, args ...string) (string, error)

// Options configures a sweep.
type Options struct {
	// GH runs the gh CLI. Required.
	GH Runner
	// Trunk is the branch stacks bottom out on, normally main.
	Trunk string
	// Method is the merge method: squash, merge or rebase.
	Method string
	// ReviewLabel is never merged and is applied to risky PRs.
	ReviewLabel string
	// DryRun makes no mutating gh calls: no merge, retarget or label.
	DryRun bool
	// Prefix selects PRs by head branch. Default "saddle/".
	Prefix string
	// MaxFiles and MaxPackages bound how wide a PR may be before it is
	// flagged for review. Zero picks the defaults.
	MaxFiles    int
	MaxPackages int
	// Log records each merge, skip, label and retarget. Optional.
	Log func(branch, kind, data string)
}

// Defaults for how wide a PR may be before a human should look.
const (
	DefaultMaxFiles    = 40
	DefaultMaxPackages = 8
)

// Action is what the sweep did with a PR.
type Action string

const (
	Merged     Action = "merged"
	WouldMerge Action = "would merge"
	Waiting    Action = "waiting"
	Blocked    Action = "blocked"
)

// Result is the outcome for one PR.
type Result struct {
	Number int
	Branch string
	Title  string
	Action Action
	Reason string
	// Labeled is set when this sweep applied (or, in a dry run, would apply)
	// the review label.
	Labeled bool
}

// PR is the slice of `gh pr list --json` the sweeper reads.
type PR struct {
	Number      int     `json:"number"`
	Title       string  `json:"title"`
	HeadRefName string  `json:"headRefName"`
	HeadRefOid  string  `json:"headRefOid"`
	BaseRefName string  `json:"baseRefName"`
	IsDraft     bool    `json:"isDraft"`
	Mergeable   string  `json:"mergeable"`
	Labels      []Label `json:"labels"`
	Files       []File  `json:"files"`
	Checks      []Check `json:"statusCheckRollup"`
}

type Label struct {
	Name string `json:"name"`
}

type File struct {
	Path string `json:"path"`
}

// Check is one entry of statusCheckRollup: a CheckRun (name, status,
// conclusion) or a StatusContext (context, state).
type Check struct {
	Name       string `json:"name"`
	Context    string `json:"context"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

func (c Check) label() string {
	if c.Name != "" {
		return c.Name
	}
	return c.Context
}

// bucket folds a rollup entry into pass, pending or fail.
func (c Check) bucket() string {
	if c.Context != "" || (c.State != "" && c.Status == "") {
		switch c.State {
		case "SUCCESS":
			return "pass"
		case "PENDING", "EXPECTED":
			return "pending"
		default:
			return "fail"
		}
	}
	if c.Status != "COMPLETED" {
		return "pending"
	}
	switch c.Conclusion {
	case "SUCCESS", "NEUTRAL", "SKIPPED":
		return "pass"
	default:
		return "fail"
	}
}

const listFields = "number,title,headRefName,headRefOid,baseRefName,isDraft,mergeable,labels,files,statusCheckRollup"

// List returns the open PRs whose head branch starts with prefix.
func List(ctx context.Context, gh Runner, prefix string) ([]PR, error) {
	out, err := gh(ctx, "pr", "list", "--state", "open", "--limit", "200", "--json", listFields)
	if err != nil {
		return nil, err
	}
	var all []PR
	if err := json.Unmarshal([]byte(out), &all); err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	var prs []PR
	for _, p := range all {
		if strings.HasPrefix(p.HeadRefName, prefix) {
			prs = append(prs, p)
		}
	}
	return prs, nil
}

// Verdict is Decide's answer for one PR on its own, before stack position
// is taken into account.
type Verdict struct {
	Action Action // WouldMerge, Waiting or Blocked
	Reason string
	// Flag asks for the review label: the PR needs a human, not a fix.
	Flag bool
}

// Decide judges one PR by its own state: labels, risk, draft, conflicts,
// checks and tests. Its base is judged separately by the sweep.
func Decide(p PR, o Options) Verdict {
	o = o.withDefaults()
	if hasLabel(p, o.ReviewLabel) {
		return Verdict{Blocked, fmt.Sprintf("labeled %q; a human merges it", o.ReviewLabel), false}
	}
	if why := risky(p, o); why != "" {
		return Verdict{Blocked, why + "; flagged for review", true}
	}
	if p.IsDraft {
		return Verdict{Blocked, "draft", false}
	}
	switch p.Mergeable {
	case "CONFLICTING":
		return Verdict{Blocked, "has merge conflicts", false}
	case "MERGEABLE":
	default:
		return Verdict{Waiting, "GitHub has not computed mergeability yet", false}
	}
	if len(p.Checks) == 0 {
		return Verdict{Waiting, "no CI checks reported yet", false}
	}
	var failed, pending []string
	for _, c := range p.Checks {
		switch c.bucket() {
		case "fail":
			failed = append(failed, c.label())
		case "pending":
			pending = append(pending, c.label())
		}
	}
	if len(failed) > 0 {
		return Verdict{Blocked, "failing checks: " + strings.Join(failed, ", "), false}
	}
	if len(pending) > 0 {
		return Verdict{Waiting, "pending checks: " + strings.Join(pending, ", "), false}
	}
	if !hasTests(p) {
		return Verdict{Blocked, "changes Go code without touching a _test.go file", false}
	}
	return Verdict{WouldMerge, "green, mergeable, tested", false}
}

func hasLabel(p PR, name string) bool {
	for _, l := range p.Labels {
		if strings.EqualFold(l.Name, name) {
			return true
		}
	}
	return false
}

// risky names why a PR needs a human regardless of CI, or "".
func risky(p PR, o Options) string {
	pkgs := map[string]bool{}
	for _, f := range p.Files {
		switch {
		case f.Path == "go.mod" || f.Path == "go.sum" || strings.HasSuffix(f.Path, "/go.mod") || strings.HasSuffix(f.Path, "/go.sum"):
			return "changes " + f.Path
		case strings.HasPrefix(f.Path, ".github/workflows/"):
			return "changes CI workflow " + f.Path
		}
		if strings.HasSuffix(f.Path, ".go") {
			pkgs[path.Dir(f.Path)] = true
		}
	}
	if len(p.Files) > o.MaxFiles {
		return fmt.Sprintf("touches %d files (over %d)", len(p.Files), o.MaxFiles)
	}
	if len(pkgs) > o.MaxPackages {
		return fmt.Sprintf("touches %d Go packages (over %d)", len(pkgs), o.MaxPackages)
	}
	return ""
}

// hasTests reports whether a PR that changes Go code also changes a test.
func hasTests(p PR) bool {
	code := false
	for _, f := range p.Files {
		if strings.HasSuffix(f.Path, "_test.go") {
			return true
		}
		if strings.HasSuffix(f.Path, ".go") {
			code = true
		}
	}
	return !code
}

func (o Options) withDefaults() Options {
	if o.Trunk == "" {
		o.Trunk = "main"
	}
	if o.Method == "" {
		o.Method = "squash"
	}
	if o.ReviewLabel == "" {
		o.ReviewLabel = "requires review"
	}
	if o.Prefix == "" {
		o.Prefix = "saddle/"
	}
	if o.MaxFiles <= 0 {
		o.MaxFiles = DefaultMaxFiles
	}
	if o.MaxPackages <= 0 {
		o.MaxPackages = DefaultMaxPackages
	}
	if o.Log == nil {
		o.Log = func(string, string, string) {}
	}
	return o
}

// Sweep lists open saddle PRs, merges the ready ones bottom of each stack
// first, retargets their children to the trunk and labels risky PRs. A
// retargeted child waits for its CI to re-run; it is merged by a later sweep.
// Results come back in stack order.
func Sweep(ctx context.Context, o Options) ([]Result, error) {
	o = o.withDefaults()
	if o.GH == nil {
		return nil, fmt.Errorf("sweeper: no gh runner")
	}
	prs, err := List(ctx, o.GH, o.Prefix)
	if err != nil {
		return nil, err
	}
	s := &sweep{o: o, byHead: map[string]*PR{}}
	for i := range prs {
		s.byHead[prs[i].HeadRefName] = &prs[i]
	}
	var out []Result
	for _, p := range stackOrder(prs, o.Trunk) {
		r := s.one(ctx, p)
		o.Log(p.HeadRefName, "sweep."+strings.ReplaceAll(string(r.Action), " ", "_"), fmt.Sprintf("#%d %s", p.Number, r.Reason))
		out = append(out, r)
	}
	return out, nil
}

type sweep struct {
	o      Options
	byHead map[string]*PR
	// merged maps a head branch merged in this sweep to its PR number.
	merged map[string]int
	// retargeted holds PRs moved onto the trunk in this sweep; their CI
	// re-runs, so they wait for the next sweep.
	retargeted   map[int]bool
	labelCreated bool
}

func (s *sweep) one(ctx context.Context, p *PR) Result {
	r := Result{Number: p.Number, Branch: p.HeadRefName, Title: p.Title}
	v := Decide(*p, s.o)
	if v.Flag {
		r.Labeled = true
		if err := s.label(ctx, p); err != nil {
			v.Reason += " (labeling failed: " + err.Error() + ")"
		}
	}
	if s.retargeted[p.Number] {
		// Its checks ran against the old base; wait for the new run.
		r.Action, r.Reason = Waiting, "retargeted to "+s.o.Trunk+"; waiting for CI to re-run"
		if v.Action == Blocked {
			r.Action, r.Reason = Blocked, "retargeted to "+s.o.Trunk+"; "+v.Reason
		}
		return r
	}
	if v.Action != WouldMerge {
		r.Action, r.Reason = v.Action, v.Reason
		return r
	}
	if p.BaseRefName != s.o.Trunk {
		if parent, ok := s.byHead[p.BaseRefName]; ok {
			r.Action = Waiting
			r.Reason = fmt.Sprintf("stacked on #%d, which has not merged", parent.Number)
			if s.o.DryRun && s.merged[parent.HeadRefName] != 0 {
				r.Reason = fmt.Sprintf("would be retargeted to %s after #%d merges, then wait for CI", s.o.Trunk, parent.Number)
			}
			return r
		}
		n, err := s.mergedPR(ctx, p.BaseRefName)
		switch {
		case err != nil:
			r.Action, r.Reason = Blocked, "checking base "+p.BaseRefName+": "+err.Error()
		case n == 0:
			r.Action, r.Reason = Blocked, fmt.Sprintf("base %s is not %s and no merged PR heads it", p.BaseRefName, s.o.Trunk)
		default:
			r.Action, r.Reason = Waiting, s.retarget(ctx, p, n)
		}
		return r
	}
	if s.o.DryRun {
		r.Action, r.Reason = WouldMerge, v.Reason
		s.markMerged(p)
		return r
	}
	args := []string{"pr", "merge", strconv.Itoa(p.Number), "--" + s.o.Method}
	if p.HeadRefOid != "" {
		// Refuse to merge commits pushed after this sweep looked.
		args = append(args, "--match-head-commit", p.HeadRefOid)
	}
	if _, err := s.o.GH(ctx, args...); err != nil {
		r.Action, r.Reason = Blocked, "merge failed: "+err.Error()
		return r
	}
	r.Action, r.Reason = Merged, s.o.Method+" merged"
	s.markMerged(p)
	for _, c := range s.children(p.HeadRefName) {
		s.o.Log(c.HeadRefName, "sweep.retarget", s.retarget(ctx, c, p.Number))
	}
	return r
}

func (s *sweep) markMerged(p *PR) {
	if s.merged == nil {
		s.merged = map[string]int{}
	}
	s.merged[p.HeadRefName] = p.Number
}

func (s *sweep) children(head string) []*PR {
	var cs []*PR
	for _, c := range s.byHead {
		if c.BaseRefName == head {
			cs = append(cs, c)
		}
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].Number < cs[j].Number })
	return cs
}

// retarget moves p onto the trunk because its parent PR n merged, and says
// what happened.
func (s *sweep) retarget(ctx context.Context, p *PR, n int) string {
	if s.o.DryRun {
		return fmt.Sprintf("parent #%d merged; would retarget to %s, then wait for CI", n, s.o.Trunk)
	}
	if _, err := s.o.GH(ctx, "pr", "edit", strconv.Itoa(p.Number), "--base", s.o.Trunk); err != nil {
		return fmt.Sprintf("parent #%d merged; retarget to %s failed: %v", n, s.o.Trunk, err)
	}
	p.BaseRefName = s.o.Trunk
	if s.retargeted == nil {
		s.retargeted = map[int]bool{}
	}
	s.retargeted[p.Number] = true
	return fmt.Sprintf("parent #%d merged; retargeted to %s, waiting for CI", n, s.o.Trunk)
}

// mergedPR returns the number of a merged PR whose head is branch, or 0.
func (s *sweep) mergedPR(ctx context.Context, branch string) (int, error) {
	out, err := s.o.GH(ctx, "pr", "list", "--state", "merged", "--head", branch, "--limit", "1", "--json", "number")
	if err != nil {
		return 0, err
	}
	var ns []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal([]byte(out), &ns); err != nil {
		return 0, fmt.Errorf("gh pr list: %w", err)
	}
	if len(ns) == 0 {
		return 0, nil
	}
	return ns[0].Number, nil
}

// label applies the review label, creating it on first use.
func (s *sweep) label(ctx context.Context, p *PR) error {
	if s.o.DryRun {
		return nil
	}
	if !s.labelCreated {
		// --force makes this idempotent: it updates the label if it exists.
		if _, err := s.o.GH(ctx, "label", "create", s.o.ReviewLabel, "--force",
			"--color", "D93F0B", "--description", "saddle sweep will not merge this; a human should review it"); err != nil {
			return err
		}
		s.labelCreated = true
	}
	if _, err := s.o.GH(ctx, "pr", "edit", strconv.Itoa(p.Number), "--add-label", s.o.ReviewLabel); err != nil {
		return err
	}
	p.Labels = append(p.Labels, Label{Name: s.o.ReviewLabel})
	s.o.Log(p.HeadRefName, "sweep.label", fmt.Sprintf("#%d labeled %q", p.Number, s.o.ReviewLabel))
	return nil
}

// stackOrder sorts PRs so every PR comes after the PR its base branch heads:
// stacks bottom first, then by number. PRs in a base cycle come last.
func stackOrder(prs []PR, trunk string) []*PR {
	byHead := map[string]*PR{}
	for i := range prs {
		byHead[prs[i].HeadRefName] = &prs[i]
	}
	depth := map[int]int{}
	var depthOf func(p *PR, seen map[int]bool) int
	depthOf = func(p *PR, seen map[int]bool) int {
		if d, ok := depth[p.Number]; ok {
			return d
		}
		if seen[p.Number] {
			return len(prs)
		}
		seen[p.Number] = true
		d := 0
		if parent, ok := byHead[p.BaseRefName]; ok && p.BaseRefName != trunk {
			d = depthOf(parent, seen) + 1
		}
		depth[p.Number] = d
		return d
	}
	out := make([]*PR, len(prs))
	for i := range prs {
		out[i] = &prs[i]
		depthOf(out[i], map[int]bool{})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if depth[out[i].Number] != depth[out[j].Number] {
			return depth[out[i].Number] < depth[out[j].Number]
		}
		return out[i].Number < out[j].Number
	})
	return out
}

// Report formats results as merged / waiting / blocked sections with one
// line per PR.
func Report(rs []Result, dryRun bool) string {
	var b strings.Builder
	if dryRun {
		b.WriteString("dry run: nothing was merged, retargeted or labeled\n")
	}
	if len(rs) == 0 {
		b.WriteString("no open saddle PRs\n")
		return b.String()
	}
	sections := []struct {
		title string
		match func(Action) bool
	}{
		{"merged", func(a Action) bool { return a == Merged || a == WouldMerge }},
		{"waiting", func(a Action) bool { return a == Waiting }},
		{"blocked", func(a Action) bool { return a == Blocked }},
	}
	if dryRun {
		sections[0].title = "would merge"
	}
	for _, sec := range sections {
		var lines []string
		for _, r := range rs {
			if !sec.match(r.Action) {
				continue
			}
			l := fmt.Sprintf("  #%-5d %s: %s", r.Number, r.Branch, r.Reason)
			if r.Labeled && !dryRun {
				l += " [labeled]"
			}
			lines = append(lines, l)
		}
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s (%d)\n%s\n", sec.title, len(lines), strings.Join(lines, "\n"))
	}
	return b.String()
}
