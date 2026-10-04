// Package doctor runs saddle's preflight checks on a repo: everything that
// has to be true before agents, the merge train and stacked PRs work. Each
// check returns ok, warn or fail with a fix the owner can act on. Every
// outside call goes through Env so the checks run against fakes in tests.
package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/ghstack"
	"github.com/brandonapol/saddle/internal/lintgate"
	"github.com/brandonapol/saddle/internal/refguard"
)

// Status is a check's outcome. Only Fail makes saddle doctor exit non-zero.
type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
)

// Check names, in the order Run reports them.
const (
	CheckConfig        = "config"
	CheckRemote        = "git remote"
	CheckDefaultBranch = "default branch"
	CheckGH            = "gh auth"
	CheckMergeSettings = "merge settings"
	CheckProtection    = "branch protection"
	CheckGhStack       = "gh stack"
	CheckTestCmd       = "test.cmd"
	CheckTmux          = "tmux"
	CheckClaude        = "claude"
	CheckHooks         = "ref guard hooks"
	CheckGate          = "lint gate"
	CheckIgnored       = ".saddle ignored"
	CheckStateDB       = "state.db"
	CheckLeftovers     = "leftovers"
)

// Result is one check's outcome. Fix is set whenever Status isn't OK.
type Result struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// Env is everything the checks read from outside the process.
type Env interface {
	// Root is the repo's main checkout.
	Root() string
	Config() (config.Config, error)
	// Git and GH run in Root and return trimmed stdout.
	Git(args ...string) (string, error)
	GH(args ...string) (string, error)
	// Exec runs a program in Root and returns its trimmed combined output.
	Exec(name string, args ...string) (string, error)
	LookPath(name string) (string, error)
	Exists(path string) bool
	Hooks() ([]refguard.HookState, error)
	// OpenStore opens, migrates and closes the state database at path.
	OpenStore(path string) error
	// Leftovers counts the worktrees, branches and refs saddle gc would
	// remove, and those it would keep because they hold unmerged work.
	Leftovers() (remove, kept int, err error)
}

// Failed reports whether any check failed.
func Failed(rs []Result) bool {
	for _, r := range rs {
		if r.Status == Fail {
			return true
		}
	}
	return false
}

// run carries what earlier checks learned to later ones.
type run struct {
	env    Env
	cfg    config.Config
	remote string
	base   string // the detected default branch, else cfg.Base
	gh     bool   // gh is installed and logged in
}

// Run runs every check in order.
func Run(env Env) []Result {
	r := &run{env: env}
	return []Result{
		r.config(), r.gitRemote(), r.defaultBranch(), r.ghAuth(), r.mergeSettings(), r.protection(), r.ghStack(),
		r.testCmd(), r.tool(CheckTmux, "tmux", "-V", "install tmux (e.g. `brew install tmux` or your package manager)"),
		r.tool(CheckClaude, r.cfg.Claude.Cmd, "--version", "install Claude Code (https://claude.com/claude-code) or set [claude] cmd in .saddle/config.toml"),
		r.hooks(), r.gate(), r.ignored(), r.stateDB(), r.leftovers(),
	}
}

func ok(name, detail string) Result { return Result{Name: name, Status: OK, Detail: detail} }

func warn(name, detail, fix string) Result {
	return Result{Name: name, Status: Warn, Detail: detail, Fix: fix}
}

func fail(name, detail, fix string) Result {
	return Result{Name: name, Status: Fail, Detail: detail, Fix: fix}
}

func (r *run) config() Result {
	cfg, err := r.env.Config()
	if err != nil {
		r.cfg = config.Default()
		return fail(CheckConfig, err.Error(), "fix .saddle/config.toml (or ~/.config/saddle/config.toml); the other checks used the defaults")
	}
	r.cfg = cfg
	return ok(CheckConfig, fmt.Sprintf("base %s, integration %s", cfg.Base, cfg.Integration))
}

// gitRemote picks the remote base tracks, else origin, else the only one,
// as the app does when it fetches base.
func (r *run) gitRemote() Result {
	out, _ := r.env.Git("remote")
	remotes := strings.Fields(out)
	if len(remotes) == 0 {
		return fail(CheckRemote, "no git remote", "git remote add origin <github-url>; saddle pushes branches and opens PRs there")
	}
	if up, err := r.env.Git("config", "branch."+r.cfg.Base+".remote"); err == nil && up != "" && up != "." {
		r.remote = up
	}
	for _, rm := range remotes {
		if r.remote == "" && rm == "origin" {
			r.remote = rm
		}
	}
	if r.remote == "" {
		r.remote = remotes[0]
	}
	url, _ := r.env.Git("remote", "get-url", r.remote)
	return ok(CheckRemote, strings.TrimSpace(r.remote+" "+url))
}

// defaultBranch detects the repo's default branch, never assuming main:
// first the remote's HEAD, then GitHub.
func (r *run) defaultBranch() Result {
	r.base = r.cfg.Base
	var def string
	if r.remote != "" {
		if h, err := r.env.Git("symbolic-ref", "--short", "refs/remotes/"+r.remote+"/HEAD"); err == nil {
			def = strings.TrimPrefix(h, r.remote+"/")
		}
	}
	if def == "" {
		if b, err := r.env.GH("repo", "view", "--json", "defaultBranchRef", "-q", ".defaultBranchRef.name"); err == nil {
			def = strings.TrimSpace(b)
		}
	}
	if def == "" {
		remote := r.remote
		if remote == "" {
			remote = "origin"
		}
		return warn(CheckDefaultBranch, "could not detect it; using base "+r.cfg.Base,
			fmt.Sprintf("git remote set-head %s --auto, or check base in .saddle/config.toml", remote))
	}
	r.base = def
	if def != r.cfg.Base {
		return fail(CheckDefaultBranch, fmt.Sprintf("default branch is %s but base is %s", def, r.cfg.Base),
			fmt.Sprintf("set base = %q in .saddle/config.toml (or use a deliberate non-default base and ignore this)", def))
	}
	return ok(CheckDefaultBranch, def+" (matches base)")
}

func (r *run) ghAuth() Result {
	if _, err := r.env.LookPath("gh"); err != nil {
		return fail(CheckGH, "gh is not installed", "install the GitHub CLI: https://cli.github.com, then gh auth login")
	}
	ver, _ := r.env.Exec("gh", "--version")
	ver, _, _ = strings.Cut(ver, "\n")
	out, err := r.env.Exec("gh", "auth", "status")
	if err != nil {
		return fail(CheckGH, ver+": not logged in", "gh auth login")
	}
	r.gh = true
	scopes, listed := tokenScopes(out)
	switch {
	case !listed:
		return ok(CheckGH, ver+", logged in (token scopes not listed)")
	case !scopes["repo"]:
		return fail(CheckGH, ver+", token lacks the repo scope", "gh auth refresh -s repo")
	case !scopes["workflow"]:
		return warn(CheckGH, ver+", token lacks the workflow scope",
			"gh auth refresh -s workflow; without it GitHub rejects pushes of branches that change .github/workflows")
	}
	return ok(CheckGH, ver+", logged in")
}

// tokenScopes parses "Token scopes: 'repo', 'workflow'" from gh auth status.
func tokenScopes(status string) (map[string]bool, bool) {
	for _, line := range strings.Split(status, "\n") {
		_, list, found := strings.Cut(line, "Token scopes:")
		if !found {
			continue
		}
		scopes := map[string]bool{}
		for _, s := range strings.Split(list, ",") {
			scopes[strings.Trim(strings.TrimSpace(s), `'"`)] = true
		}
		return scopes, true
	}
	return nil, false
}

const skippedFix = "fix gh auth above, then run saddle doctor again"

func (r *run) mergeSettings() Result {
	if !r.gh {
		return warn(CheckMergeSettings, "skipped: gh unavailable", skippedFix)
	}
	out, err := r.env.GH("api", "repos/{owner}/{repo}")
	cached := func(...string) (string, error) { return out, err }
	s, err := app.ReadMergeSettings(cached)
	if err != nil {
		return warn(CheckMergeSettings, "could not read: "+err.Error(),
			"use a gh login with admin or push access, or check by hand that merge commits are off and squash is on")
	}
	if s.Merge {
		// CheckRepoMergeSettings words the refusal saddle up, land and prs give.
		e := app.CheckRepoMergeSettings(cached, io.Discard)
		return fail(CheckMergeSettings, "merge commits are allowed; stacked PRs need linear history", e.Error())
	}
	if !s.Squash {
		return warn(CheckMergeSettings, "squash merging is off; saddle sweep and auto-merge squash by default",
			fmt.Sprintf("gh api -X PATCH repos/%s -F allow_squash_merge=true", s.Repo))
	}
	return ok(CheckMergeSettings, "merge commits off, squash on")
}

// ghStack checks the gh-stack extension and whether the repo has Stacked
// PRs (a private preview, enabled per repo), read-only, against [train]
// stack_backend. Nothing here fails: without them saddle chains PR bases.
func (r *run) ghStack() Result {
	backend := r.cfg.Train.StackBackend
	if backend == "" {
		backend = config.StackBackendSaddle
	}
	want := backend == config.StackBackendGhStack
	tail := fmt.Sprintf("stack_backend = %q", backend)
	if !r.gh {
		if want {
			return warn(CheckGhStack, "skipped: gh unavailable; "+tail, skippedFix)
		}
		return ok(CheckGhStack, "skipped: gh unavailable; "+tail+" doesn't need it")
	}
	c := ghstack.Client{Run: r.env.GH}
	ver, err := c.Available()
	if err != nil {
		if want {
			return warn(CheckGhStack, "the gh-stack extension is not installed; "+tail+", so stacks are chained by PR bases instead",
				"gh extension install github/gh-stack")
		}
		return ok(CheckGhStack, "gh-stack not installed; "+tail+" doesn't need it")
	}
	enabled, err := c.Enabled()
	state := "Stacked PRs enabled"
	if !enabled {
		state = "Stacked PRs aren't enabled for this repo"
		if !errors.Is(err, ghstack.ErrNotEnabled) {
			state = "can't tell whether Stacked PRs are enabled (" + err.Error() + ")"
		}
	}
	detail := fmt.Sprintf("gh-stack %s, %s, %s", ver, state, tail)
	switch {
	case want && !enabled:
		return warn(CheckGhStack, detail+"; stacks are chained by PR bases instead",
			"Stacked PRs are a GitHub preview enabled per repo: ask for it on the repo, or set stack_backend = \"saddle\" under [train]")
	case !want && enabled:
		return ok(CheckGhStack, detail+` (set stack_backend = "gh-stack" under [train] to link stacks natively)`)
	}
	return ok(CheckGhStack, detail)
}

func (r *run) protection() Result {
	if !r.gh {
		return warn(CheckProtection, "skipped: gh unavailable", skippedFix)
	}
	out, err := r.env.GH("api", "repos/{owner}/{repo}/branches/"+r.base+"/protection")
	if err != nil {
		if msg := err.Error(); strings.Contains(msg, "404") || strings.Contains(msg, "not protected") {
			return warn(CheckProtection, r.base+" is not protected",
				"Settings -> Branches: protect "+r.base+" and require your CI checks, so nothing merges on red")
		}
		return warn(CheckProtection, "could not read: "+err.Error(), "needs admin access to the repo; check Settings -> Branches by hand")
	}
	var p struct {
		Checks *struct {
			Contexts []string          `json:"contexts"`
			Checks   []json.RawMessage `json:"checks"`
		} `json:"required_status_checks"`
		Reviews *struct {
			Count int `json:"required_approving_review_count"`
		} `json:"required_pull_request_reviews"`
		Admins struct {
			Enabled bool `json:"enabled"`
		} `json:"enforce_admins"`
	}
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		return warn(CheckProtection, "could not parse gh api output: "+err.Error(), "check Settings -> Branches by hand")
	}
	checks := 0
	if p.Checks != nil {
		checks = max(len(p.Checks.Contexts), len(p.Checks.Checks))
	}
	reviews := 0
	if p.Reviews != nil {
		reviews = p.Reviews.Count
	}
	detail := fmt.Sprintf("%s: %d required checks, %d approvals", r.base, checks, reviews)
	if p.Admins.Enabled {
		detail += ", admins included"
	}
	if checks == 0 {
		return warn(CheckProtection, detail,
			"Settings -> Branches: add a required status check on "+r.base+", so auto-merge can tell green from red")
	}
	return ok(CheckProtection, detail)
}

func (r *run) testCmd() Result {
	cmd := r.cfg.Test.Cmd
	if strings.TrimSpace(cmd) == "" {
		fix := "set cmd under [test] in .saddle/config.toml; without it the merge train lands untested"
		if d := app.DetectTestCmd(r.env.Root()); d != "" {
			fix = fmt.Sprintf("add cmd = %q under [test] in .saddle/config.toml; without it the merge train lands untested", d)
		}
		return warn(CheckTestCmd, "not set", fix)
	}
	prog := program(cmd)
	if _, err := r.env.LookPath(prog); err != nil {
		return fail(CheckTestCmd, cmd+": "+prog+" not found", "install "+prog+" or change cmd under [test] in .saddle/config.toml")
	}
	return ok(CheckTestCmd, cmd)
}

// program is the command a shell line runs, skipping VAR=value prefixes.
func program(line string) string {
	for _, f := range strings.Fields(line) {
		if !strings.Contains(f, "=") {
			return f
		}
	}
	return ""
}

func (r *run) tool(name, cmd, versionFlag, fix string) Result {
	prog := program(cmd)
	if _, err := r.env.LookPath(prog); err != nil {
		return fail(name, prog+" not on PATH", fix)
	}
	ver, err := r.env.Exec(prog, versionFlag)
	ver, _, _ = strings.Cut(ver, "\n")
	if err != nil || ver == "" {
		return ok(name, prog+" (version unknown)")
	}
	return ok(name, ver)
}

func (r *run) hooks() Result {
	hs, err := r.env.Hooks()
	if err != nil {
		return warn(CheckHooks, "could not read: "+err.Error(), "run saddle init inside the repo")
	}
	var names []string
	for _, h := range hs {
		switch {
		case !h.Present:
			return fail(CheckHooks, h.Name+" hook missing", "saddle init installs it; without it agents can move saddle's branches")
		case !h.Saddle:
			return fail(CheckHooks, h.Path+" was not written by saddle, so the guard is off",
				"saddle init chains it: it moves to "+h.Path+lintgate.ChainSuffix+" and saddle's hook runs it after its own check")
		case !r.env.Exists(h.Bin):
			return warn(CheckHooks, h.Name+" runs "+h.Bin+", which is gone, so the guard is off",
				"make install && saddle init, to point the hooks at the current binary")
		}
		name := h.Name
		if h.Chained != "" {
			name += " (chains " + filepath.Base(h.Chained) + ")"
		}
		names = append(names, name)
	}
	return ok(CheckHooks, strings.Join(names, ", "))
}

// gate reports the repo's own pre-commit/lint gate (#212): what it runs,
// whether its hook is installed and active in agent worktrees, and whether
// saddle's hooks would land among the repo's tracked ones.
func (r *run) gate() Result {
	root := r.env.Root()
	git := func(dir string, args ...string) (string, error) {
		return r.env.Git(append([]string{"-C", dir}, args...)...)
	}
	g := lintgate.Detect(root, r.cfg.Integration, git)
	if r.cfg.Train.Lint.Disabled() {
		return ok(CheckGate, "off: [train] lint.cmd = \"\", so done and the train skip the repo's gate")
	}
	if l := r.cfg.Train.Lint; l.Set {
		g.Kind, g.Source, g.Cmd = "lint.cmd", ".saddle/config.toml", l.Cmd
	}
	if g.Cmd == "" {
		return ok(CheckGate, "none detected (no pre-commit hook, .pre-commit-config.yaml, lefthook, husky or Makefile check/lint target)")
	}
	detail := fmt.Sprintf("%s (%s) runs `%s`", g.Kind, g.Source, g.Cmd)
	if g.Fix != "" {
		detail += ", fix: " + g.Fix
	}
	var why, fixes []string
	switch {
	case g.Hook != "":
		detail += "; installed at " + g.Hook
		if wt := r.agentWorktree(); wt == "" {
			detail += "; no agent worktrees to check yet"
		} else if p, err := git(wt, "rev-parse", "--path-format=absolute", "--git-path", "hooks/pre-commit"); err == nil && (p == g.Hook || r.env.Exists(p)) {
			detail += "; active in agent worktrees"
		} else {
			why = append(why, "agent worktrees don't run it: git resolves their pre-commit hook to "+p)
			fixes = append(fixes, "core.hooksPath is relative, so each worktree looks in its own copy; set it to an absolute path or unset it and link the hook into .git/hooks")
		}
	case g.Shipped != "":
		why = append(why, g.Shipped+" is not installed, so commits in this checkout skip it")
		fix := "link it: ln -s ../../" + g.Shipped + " .git/hooks/pre-commit"
		if hasSetupHooks(root) {
			fix = "make setup/hooks"
		}
		fixes = append(fixes, fix+" (saddle runs `"+g.Cmd+"` before done and landing either way)")
	case g.Kind != lintgate.KindMake:
		why = append(why, g.Kind+" is not installed as a git hook, so commits in this checkout skip it")
		fixes = append(fixes, installHint(g.Kind)+" (saddle runs `"+g.Cmd+"` before done and landing either way)")
	}
	if hooks, err := git(root, "rev-parse", "--path-format=absolute", "--git-path", "hooks"); err == nil &&
		strings.HasPrefix(hooks, root+string(filepath.Separator)) && !strings.HasPrefix(hooks, filepath.Join(root, ".git")+string(filepath.Separator)) {
		why = append(why, "core.hooksPath puts git hooks in "+hooks+", inside the checkout, where saddle's ref-guard hooks could clobber the repo's tracked ones")
		fixes = append(fixes, "unset core.hooksPath and link the repo's hooks into .git/hooks, where saddle chains them instead of risking a clobber")
	}
	if len(why) > 0 {
		return warn(CheckGate, detail+"; "+strings.Join(why, "; "), strings.Join(fixes, "; "))
	}
	return ok(CheckGate, detail)
}

// agentWorktree is the first saddle agent worktree, "" when none.
func (r *run) agentWorktree() string {
	out, err := r.env.Git("worktree", "list", "--porcelain")
	if err != nil {
		return ""
	}
	prefix := filepath.Join(r.env.Root(), ".saddle", "worktrees") + string(filepath.Separator)
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok && strings.HasPrefix(p, prefix) {
			return p
		}
	}
	return ""
}

func hasSetupHooks(root string) bool {
	b, err := os.ReadFile(filepath.Join(root, "Makefile"))
	return err == nil && regexp.MustCompile(`(?m)^setup/hooks[ \t]*:([^=]|$)`).Match(b)
}

func installHint(kind string) string {
	switch kind {
	case lintgate.KindPreCommit:
		return "pre-commit install"
	case lintgate.KindLefthook:
		return "lefthook install"
	case lintgate.KindHusky:
		return "npx husky"
	}
	return "install the repo's pre-commit hook"
}

func (r *run) ignored() Result {
	if _, err := r.env.Git("check-ignore", "-q", ".saddle/"); err != nil {
		return fail(CheckIgnored, ".saddle/ is not ignored, so state and worktrees could be committed",
			"saddle init adds /.saddle/ to .git/info/exclude; or add /.saddle/ to .gitignore")
	}
	return ok(CheckIgnored, ".saddle/ is ignored")
}

func (r *run) stateDB() Result {
	dir := filepath.Join(r.env.Root(), ".saddle")
	if !r.env.Exists(dir) {
		return fail(CheckStateDB, "repo not initialized", "saddle init")
	}
	path := filepath.Join(dir, "state.db")
	if err := r.env.OpenStore(path); err != nil {
		return fail(CheckStateDB, err.Error(),
			"make "+dir+" writable; if state.db is corrupt, back it up and move it aside (saddle recreates it, losing task history)")
	}
	return ok(CheckStateDB, "writable, migrated")
}

func (r *run) leftovers() Result {
	n, kept, err := r.env.Leftovers()
	if err != nil {
		return warn(CheckLeftovers, "could not count: "+err.Error(), "saddle gc --dry-run")
	}
	note := ""
	if kept > 0 {
		note = fmt.Sprintf("; %d kept with unmerged work (saddle gc --dry-run says why)", kept)
	}
	if n > 0 {
		return warn(CheckLeftovers, fmt.Sprintf("%d stale worktrees, branches or refs%s", n, note), "saddle gc --dry-run to list them, saddle gc to remove them")
	}
	return ok(CheckLeftovers, "none"+note)
}

// WriteTable prints one row per check, then the fixes for every check that
// isn't ok.
func WriteTable(w io.Writer, rs []Result) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tCHECK\tDETAIL")
	for _, r := range rs {
		st := string(r.Status)
		if r.Status == Fail {
			st = "FAIL"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", st, r.Name, r.Detail)
	}
	_ = tw.Flush()
	for _, r := range rs {
		if r.Fix == "" {
			continue
		}
		fix := strings.ReplaceAll(r.Fix, "\n", "\n    ")
		fmt.Fprintf(w, "\n%s fix: %s", r.Name, fix)
	}
	if hasFix(rs) {
		fmt.Fprintln(w)
	}
}

func hasFix(rs []Result) bool {
	for _, r := range rs {
		if r.Fix != "" {
			return true
		}
	}
	return false
}

// WriteJSON prints {"ok": <no check failed>, "checks": [...]}.
func WriteJSON(w io.Writer, rs []Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		OK     bool     `json:"ok"`
		Checks []Result `json:"checks"`
	}{!Failed(rs), rs})
}
