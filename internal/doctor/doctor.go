// Package doctor runs saddle's preflight checks on a repo: everything that
// has to be true before agents, the merge train and stacked PRs work. Each
// check returns ok, warn or fail with a fix the owner can act on. Every
// outside call goes through Env so the checks run against fakes in tests.
package doctor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/ghstack"
	"github.com/brandonapol/saddle/internal/lintgate"
	"github.com/brandonapol/saddle/internal/refguard"
	"github.com/brandonapol/saddle/internal/trust"
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
	// CheckGH is the git provider and its CLI's login: gh's, with its token
	// scopes, on GitHub (#646).
	CheckGH            = "git provider"
	CheckMergeSettings = "merge settings"
	CheckProtection    = "branch protection"
	CheckGhStack       = "gh stack"
	CheckTestCmd       = "test.cmd"
	CheckTmux          = "tmux"
	CheckClaude        = "claude"
	CheckHooks         = "ref guard hooks"
	CheckPush          = "saddle can push"
	CheckOrchAllow     = "orchestrator allowlist"
	CheckGate          = "lint gate"
	CheckRepoHooks     = "repo hooks"
	CheckIgnored       = ".saddle ignored"
	CheckStateDB       = "state.db"
	CheckLeftovers     = "leftovers"
	CheckTrust         = "trust"
)

// Result is one check's outcome. Fix is set whenever Status isn't OK.
type Result struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
	// About says in plain words what the check is for.
	About string `json:"about,omitempty"`
	// Fixable means saddle doctor --fix (saddle init) repairs it locally.
	Fixable bool `json:"fixable,omitempty"`
	// Fixed means saddle repaired it during this run.
	Fixed bool `json:"fixed,omitempty"`
}

// Env is everything the checks read from outside the process.
type Env interface {
	// Root is the repo's main checkout.
	Root() string
	Config() (config.Config, error)
	// Git, GH and Glab run in Root and return trimmed stdout. Doctor
	// reaches gh and glab only through a forge.Forge over them (#648).
	Git(args ...string) (string, error)
	GH(args ...string) (string, error)
	Glab(args ...string) (string, error)
	// Exec runs a program in Root and returns its trimmed combined output.
	Exec(name string, args ...string) (string, error)
	LookPath(name string) (string, error)
	Exists(path string) bool
	Hooks() ([]refguard.HookState, error)
	// OpenStore opens, migrates and closes the state database at path.
	OpenStore(path string) error
	// TrainGit runs git in Root as the merge train (SADDLE_TRAIN=1), the
	// way saddle itself pushes, and returns trimmed combined output.
	TrainGit(args ...string) (string, error)
	// ClaudeAllow is every permission allow rule in the Claude settings a
	// session in Root reads: the user's, the repo's and the repo's local ones.
	ClaudeAllow() []string
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
	repo   string // owner/repo (group/project on GitLab), "" when unknown
	web    string // the repo's web page, "" when unknown
	url    string // the remote's URL
	// provider is the forge saddle drives: forge.Select's pick from [forge]
	// and the remote, then DetectForge's (#646). ready means its CLI is
	// installed and logged in.
	provider string
	ready    bool
	fg       forge.Forge // over Env's gh and glab; see forge()
	local    bool        // RunLocal: no forge calls
	// agents is AgentsEnv's answer, read once when agentsRead.
	agents     []agent.Status
	agentsRead bool
}

// slowChecks are the checks that call GitHub, push, start the agent CLI or
// walk every branch and worktree. Under load they take tens of seconds, so
// saddle up runs them after its first frame (#635).
var slowChecks = map[string]bool{
	CheckGH: true, CheckMergeSettings: true, CheckProtection: true, CheckGhStack: true,
	CheckPush: true, CheckLeftovers: true, CheckSkills: true, CheckScratch: true,
}

// Slow reports whether Run makes the named check and RunLocal skips it.
func Slow(name string) bool { return slowChecks[name] }

// Run runs every check in order.
func Run(env Env) []Result { return runChecks(env, false) }

// RunLocal runs every check but the slow ones, in Run's order: what saddle
// up blocks on before its first frame. Run covers the rest after it (#635).
func RunLocal(env Env) []Result { return runChecks(env, true) }

func runChecks(env Env, local bool) []Result {
	r := &run{env: env, local: local}
	checks := []struct {
		name string
		run  func() Result
	}{
		{CheckConfig, r.config}, {CheckRemote, r.gitRemote}, {CheckDefaultBranch, r.defaultBranch},
		{CheckGH, r.ghAuth}, {CheckMergeSettings, r.mergeSettings}, {CheckProtection, r.protection}, {CheckGhStack, r.ghStack},
		{CheckTestCmd, r.testCmd},
		{CheckTmux, func() Result {
			return r.tool(CheckTmux, "tmux", "-V", "install tmux (e.g. `brew install tmux` or your package manager)")
		}},
		{CheckClaude, r.claude},
		{CheckHooks, r.hooks}, {CheckPush, r.push}, {CheckOrchAllow, r.orchAllow}, {CheckGate, r.gate}, {CheckIgnored, r.ignored},
		{CheckStateDB, r.stateDB}, {CheckLeftovers, r.leftovers}, {CheckInhibit, r.inhibitor},
	}
	var rs []Result
	// A check that doesn't apply to this run returns a zero Result.
	add := func(name string, check func() Result) {
		if !local || !slowChecks[name] {
			if res := check(); res.Name != "" {
				rs = append(rs, res)
			}
		}
	}
	for _, c := range checks {
		add(c.name, c.run)
	}
	if _, ok := env.(AgentsEnv); ok {
		add(CheckAgents, r.agentCLIs)
	}
	add(CheckOrchestrator, r.orchestrator)
	if r.wantsGrok() { // after config, which sets the harness
		add(CheckGrok, r.grok)
	}
	if he, ok := env.(RepoHooksEnv); ok {
		add(CheckRepoHooks, func() Result { return r.repoHooks(he) })
	}
	if te, ok := env.(TrustEnv); ok {
		add(CheckTrust, func() Result { return trustCheck(te) })
	}
	if pe, ok := env.(PathEnv); ok {
		add(CheckPath, func() Result { return pathCheck(pe) })
	}
	if se, ok := env.(ScratchEnv); ok {
		add(CheckScratch, func() Result { return scratchCheck(se) })
	}
	if re, ok := env.(RemoteEnv); ok {
		add(CheckRemoteControl, func() Result { return remoteCheck(re, time.Now()) })
	}
	if se, ok := env.(SkillsEnv); ok && r.orchAdapter() == config.HarnessClaude {
		add(CheckSkills, func() Result { return r.skills(se) })
	}
	return describe(rs)
}

// RepoHooksEnv is an Env that can read the hooks the repo ships (#223). The
// check is skipped for envs without it.
type RepoHooksEnv interface {
	RepoHooks() ([]refguard.RepoHook, error)
}

// repoHooks warns when the repo ships git hooks (git/hooks/*, .githooks,
// husky, lefthook) that agent commits don't run, naming each and where git
// looks for it.
func (r *run) repoHooks(he RepoHooksEnv) Result {
	hs, err := he.RepoHooks()
	if err != nil {
		return warn(CheckRepoHooks, "could not read: "+err.Error(), "run saddle init inside the repo")
	}
	if len(hs) == 0 {
		return ok(CheckRepoHooks, "the repo ships no git hooks")
	}
	wt := r.agentWorktree()
	var on, off, where []string
	for _, h := range hs {
		active := h.Active
		if active && wt != "" && filepath.Base(h.Path) == h.Name {
			// A relative core.hooksPath sends each worktree to its own copy.
			if p, err := r.env.Git("-C", wt, "rev-parse", "--path-format=absolute", "--git-path", "hooks/"+h.Name); err == nil && p != h.Path && !r.env.Exists(p) {
				active, h.Path = false, p
			}
		}
		name := h.Source
		if h.Kind == lintgate.KindLefthook {
			name = h.Name + " (" + h.Source + ")"
		}
		if active {
			on = append(on, name)
			continue
		}
		off = append(off, name)
		where = append(where, h.Path)
	}
	if len(off) == 0 {
		return ok(CheckRepoHooks, strings.Join(on, ", ")+" run on agent commits in every worktree")
	}
	return local(warn(CheckRepoHooks,
		fmt.Sprintf("%s ship with the repo but don't run on agent commits: git looks for %s and finds nothing", strings.Join(off, ", "), strings.Join(where, ", ")),
		"run `saddle init`: it installs a wrapper per hook in the shared hooks directory that runs each worktree's own copy, "+
			"the heavy ones (pre-commit, pre-push) one at a time across worktrees; SADDLE_FAST_HOOK=1 skips a tree that just passed. "+
			"Don't set core.hooksPath to the repo's hooks directory instead: that turns off saddle's ref guard"))
}

// TrustEnv is an Env that knows whether the user trusts the repo (#215).
// The check is skipped for envs without it.
type TrustEnv interface {
	// Trust returns the recorded decision and whether this process is
	// trusted regardless (a saddle agent, or SADDLE_TRUST=1).
	Trust() (rep trust.Report, inherited bool, err error)
}

func trustCheck(te TrustEnv) Result {
	rep, inherited, err := te.Trust()
	if err != nil {
		return fail(CheckTrust, err.Error(), "run `saddle trust` to rewrite the decision")
	}
	switch {
	case rep.State == trust.StateTrusted:
		return ok(CheckTrust, "trusted since "+rep.Recorded.TrustedAt.Format("2006-01-02"))
	case inherited:
		return ok(CheckTrust, string(rep.State)+", but this run is trusted (SADDLE_TASK or SADDLE_TRUST=1)")
	case rep.State == trust.StateOriginChanged:
		return fail(CheckTrust, fmt.Sprintf("origin changed: trusted with %s, now %s", rep.Recorded.Origin, rep.Repo.Origin),
			"run `saddle trust` to review and trust the new origin; saddle up refuses until then")
	}
	return fail(CheckTrust, "not trusted: saddle up and the plugin won't start here", "run `saddle trust` to review what saddle does here and trust it")
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
	if dir := filepath.Join(r.env.Root(), ".saddle"); !r.env.Exists(filepath.Join(dir, "config.toml")) {
		detail := fmt.Sprintf("defaults (no .saddle/config.toml): base %s, integration %s", cfg.Base, cfg.Integration)
		if r.env.Exists(filepath.Join(dir, "state.db")) {
			detail = "half set up: .saddle/state.db exists but saddle init never ran; using " + detail
		}
		return local(warn(CheckConfig, detail, fixInit+" writes it, with test.cmd detected from the project"))
	}
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
	r.url = url
	r.repo = repoSlug(url)
	r.provider = forge.Select(r.cfg.Forge, url)
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
	if def == "" && !r.local {
		if b, err := forge.ReadDefaultBranch(context.Background(), r.forge()); err == nil {
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

// ghAuth reports the git provider, the CLI saddle drives it with and its
// login (#646). On GitHub through gh it also checks the token's scopes.
// Without a usable CLI saddle runs local-only, so this only warns.
func (r *run) ghAuth() Result {
	f := DetectForge(r.env, r.url, r.cfg.Forge)
	if f.Provider != r.provider {
		r.provider, r.fg = f.Provider, nil
	}
	switch {
	case f.CLI == "" && f.Fix == "":
		return ok(CheckGH, f.Detail)
	case f.LocalOnly():
		return warn(CheckGH, f.Detail, f.Fix)
	}
	r.ready = true
	if f.CLI == config.ForgeGlab {
		return ok(CheckGH, f.Detail)
	}
	scopes, listed := tokenScopes(f.Status)
	switch {
	case !listed:
		return ok(CheckGH, f.Detail+" (token scopes not listed)")
	case !scopes["repo"]:
		return fail(CheckGH, f.Detail+", but the token lacks the repo scope", "gh auth refresh -s repo")
	case !scopes["workflow"]:
		return warn(CheckGH, f.Detail+", but the token lacks the workflow scope",
			"gh auth refresh -s workflow; without it GitHub rejects pushes of branches that change .github/workflows")
	}
	return ok(CheckGH, f.Detail)
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

const skippedFix = "fix git provider above, then run saddle doctor again"

// forge is the repo's forge over Env's gh and glab, for the provider
// doctor settled on: what forge.New would build at run time.
func (r *run) forge() forge.Forge {
	if r.fg == nil {
		fc := r.cfg.Forge
		if r.provider == config.ForgeGitHub || r.provider == config.ForgeGitLab {
			fc.Provider, fc.CLI = r.provider, ""
		}
		r.fg = forge.Over(fc, r.url, runner(r.env.GH), runner(r.env.Glab))
	}
	return r.fg
}

func (r *run) gitlab() bool { return r.provider == config.ForgeGitLab }

// providerName is the provider in prose.
func (r *run) providerName() string {
	if n := providerNames[r.provider]; n != "" {
		return n
	}
	return "this forge"
}

// skipped is a forge check that can't run: no logged-in CLI, or a forge
// that doesn't report what it reads.
func (r *run) skipped(name string, err error, fix string) (Result, bool) {
	switch {
	case !r.ready:
		return warn(name, "skipped: no logged-in "+r.cliName(), skippedFix), true
	case errors.Is(err, forge.ErrUnsupported):
		return warn(name, "skipped: "+r.providerName()+" doesn't report it ("+err.Error()+")", fix), true
	}
	return Result{}, false
}

func (r *run) cliName() string {
	if r.gitlab() {
		return config.ForgeGlab
	}
	return config.ForgeGH
}

func (r *run) mergeSettings() Result {
	byHand := "check by hand that merge commits are off and squash is on"
	if r.gitlab() {
		byHand = "check Settings -> Merge requests by hand: Fast-forward merge, squash allowed"
	}
	if res, skip := r.skipped(CheckMergeSettings, nil, byHand); skip {
		return res
	}
	s, err := r.forge().MergeSettings(context.Background())
	if res, skip := r.skipped(CheckMergeSettings, err, byHand); skip {
		return res
	}
	if err != nil {
		access := "use a gh login with admin or push access, or "
		if r.gitlab() {
			access = "use a glab login with Maintainer access, or "
		}
		return warn(CheckMergeSettings, "could not read: "+err.Error(), access+byHand)
	}
	if s.Repo != "" {
		r.repo = s.Repo
	}
	r.web = s.URL
	if r.gitlab() {
		where := r.gitlabSettings("merge_requests", "Settings -> Merge requests")
		switch {
		case s.Merge:
			return fail(CheckMergeSettings, "the merge method makes merge commits; stacked MRs need linear history",
				"in "+where+` set Merge method to "Fast-forward merge", then run saddle again; saddle will not change this setting for you`)
		case !s.Squash:
			return warn(CheckMergeSettings, `fast-forward merge, but squash is "Do not allow", so saddle merges each MR's commits as they are`,
				"in "+where+` set "Squash commits when merging" to Allow, Encourage or Require`)
		}
		return ok(CheckMergeSettings, "fast-forward merge, squash allowed")
	}
	if s.Merge {
		return fail(CheckMergeSettings, "merge commits are allowed; stacked PRs need linear history", githubMergeFix(s))
	}
	if !s.Squash {
		return warn(CheckMergeSettings, "squash merging is off; saddle sweep and auto-merge squash by default",
			fmt.Sprintf("gh api -X PATCH repos/%s -F allow_squash_merge=true", s.Repo))
	}
	return ok(CheckMergeSettings, "merge commits off, squash on")
}

// githubMergeFix is how to turn merge commits off, as saddle up, land and
// prs word their refusal.
func githubMergeFix(s forge.MergeSettings) string {
	repo, settings := cmp.Or(s.Repo, "{owner}/{repo}"), "the repo's GitHub settings"
	if s.URL != "" {
		settings = s.URL + "/settings"
	}
	squash, squashFlag := "", ""
	if !s.Squash {
		squash, squashFlag = ` (and check "Allow squash merging")`, " -F allow_squash_merge=true"
	}
	return fmt.Sprintf(`in %s: Settings -> General -> Pull Requests, uncheck "Allow merge commits"%s; `+
		"or: gh api -X PATCH repos/%s -F allow_merge_commit=false%s. Saddle will not change this setting for you",
		settings, squash, repo, squashFlag)
}

// gitlabSettings is the project's settings page named page, else what to
// open by hand.
func (r *run) gitlabSettings(page, byHand string) string {
	if r.web == "" {
		return byHand
	}
	return r.web + "/-/settings/" + page
}

// ghStack checks the gh-stack extension and whether the repo has Stacked
// PRs (a private preview, enabled per repo), read-only, against [train]
// stack_backend. Nothing here fails: without them saddle chains PR bases.
// gh-stack is GitHub-only, so on any other forge it isn't probed (#648).
func (r *run) ghStack() Result {
	backend := r.cfg.Train.StackBackend
	if backend == "" {
		backend = config.StackBackendSaddle
	}
	want := backend == config.StackBackendGhStack
	tail := fmt.Sprintf("stack_backend = %q", backend)
	on := ""
	switch {
	case r.cfg.Forge.CLI == config.ForgeNone:
		on = `[forge] cli = "none"`
	case r.provider != "" && r.provider != config.ForgeGitHub:
		on = "this repo is on " + r.providerName()
	}
	if on != "" {
		if want {
			return warn(CheckGhStack, "gh-stack is GitHub-only and "+on+"; "+tail,
				`set stack_backend = "saddle" under [train]; saddle chains PR and MR targets itself`)
		}
		return ok(CheckGhStack, "skipped: gh-stack is GitHub-only and "+on+"; "+tail+" doesn't need it")
	}
	if !r.ready {
		if want {
			return warn(CheckGhStack, "skipped: gh unavailable; "+tail, skippedFix)
		}
		return ok(CheckGhStack, "skipped: gh unavailable; "+tail+" doesn't need it")
	}
	c := ghstack.For(r.forge(), r.env.GH)
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

// protection reads base's protection through the forge's optional
// BranchProtection; a forge without it warns (#648).
func (r *run) protection() Result {
	byHand := "check Settings -> Branches by hand"
	if r.gitlab() {
		byHand = "check Settings -> Repository -> Protected branches by hand"
	}
	if res, skip := r.skipped(CheckProtection, nil, byHand); skip {
		return res
	}
	p, err := forge.ReadBranchProtection(context.Background(), r.forge(), r.base)
	if res, skip := r.skipped(CheckProtection, err, byHand); skip {
		return res
	}
	switch {
	case err != nil && r.gitlab():
		return warn(CheckProtection, "could not read: "+err.Error(), "needs Maintainer access to the project; "+byHand)
	case err != nil:
		return warn(CheckProtection, "could not read: "+err.Error(), "needs admin access to the repo; "+byHand)
	case !p.Protected:
		return warn(CheckProtection, r.base+" is not protected, so red or unreviewed work can merge", r.protectionSteps())
	}
	if r.gitlab() {
		return r.gitlabProtection(p)
	}
	detail := fmt.Sprintf("%s: %d required checks, %d approvals", r.base, len(p.Checks), p.Approvals)
	if p.Admins {
		detail += ", admins included"
	}
	if !p.RequireCI {
		return warn(CheckProtection, detail+"; auto-merge can't tell green from red", r.protectionSteps())
	}
	return ok(CheckProtection, detail)
}

// gitlabProtection words a protected branch on GitLab: who may push and
// merge, and whether pipelines must succeed. Saddle's auto-merge waits for
// a green pipeline itself, so without that setting only people can merge
// red work.
func (r *run) gitlabProtection(p forge.Protection) Result {
	detail := r.base + ": protected"
	var who []string
	if len(p.Push) > 0 {
		who = append(who, "push: "+strings.Join(p.Push, ", "))
	}
	if len(p.Merge) > 0 {
		who = append(who, "merge: "+strings.Join(p.Merge, ", "))
	}
	if len(who) > 0 {
		detail += " (" + strings.Join(who, "; ") + ")"
	}
	if p.Approvals > 0 {
		detail += fmt.Sprintf(", %d approvals", p.Approvals)
	}
	if p.ForcePush {
		detail += ", force push allowed"
	}
	if !p.RequireCI {
		return warn(CheckProtection, detail+`; "Pipelines must succeed" is off, so a person can merge a red MR`,
			"in "+r.gitlabSettings("merge_requests", "Settings -> Merge requests")+` tick "Pipelines must succeed"`)
	}
	return ok(CheckProtection, detail+", pipelines must succeed")
}

func (r *run) testCmd() Result {
	cmd := r.cfg.Test.Cmd
	if strings.TrimSpace(cmd) == "" {
		if d := app.DetectTestCmd(r.env.Root()); d != "" {
			return local(warn(CheckTestCmd, "not set; detected "+d,
				fmt.Sprintf("%s writes cmd = %q under [test] in .saddle/config.toml; without it the merge train lands untested", fixInit, d)))
		}
		return warn(CheckTestCmd, "not set", "set cmd under [test] in .saddle/config.toml; without it the merge train lands untested")
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
	if app.RestackInterrupted(r.env.Root()) {
		return fail(CheckHooks, "interrupted restack journal in .saddle/"+app.RestackJournalFile, "run saddle restack --continue or saddle restack --abort")
	}
	hs, err := r.env.Hooks()
	if err != nil {
		return warn(CheckHooks, "could not read: "+err.Error(), "run saddle init inside the repo")
	}
	var names []string
	for _, h := range hs {
		switch {
		case !h.Present:
			return local(fail(CheckHooks, h.Name+" hook missing", fixInit+" installs it; without it agents can move saddle's branches"))
		case !h.Saddle:
			return local(fail(CheckHooks, h.Path+" was not written by saddle, so the guard is off",
				fixInit+" chains it: it moves to "+h.Path+lintgate.ChainSuffix+" and saddle's hook runs it after its own check"))
		case !r.env.Exists(h.Bin):
			return local(warn(CheckHooks, h.Name+" runs "+h.Bin+", which is gone, so the guard is off",
				fixInit+" points the hooks at the current binary"))
		}
		name := h.Name
		if h.Chained != "" {
			name += " (chains " + filepath.Base(h.Chained) + ")"
		}
		names = append(names, name)
	}
	return ok(CheckHooks, strings.Join(names, ", ")+"; operator recovery: SADDLE_REFGUARD=off git ... (outside a task; logged)")
}

// pushScratch is the ref the push check pretends to create. --dry-run never
// creates it, so nothing is left behind.
const pushScratch = "refs/heads/saddle/doctor-push-check"

// push checks that the saddle process can push to the remote, as prs,
// restack and publish do (#220): a dry-run push of a scratch ref, run as the
// train so the ref guard's pre-push hook allows it.
func (r *run) push() Result {
	if r.remote == "" {
		return warn(CheckPush, "no remote to push to", "git remote add origin <github-url>")
	}
	out, err := r.env.TrainGit("push", "--dry-run", r.remote, "HEAD:"+pushScratch)
	if err != nil {
		return warn(CheckPush, "a dry-run push to "+r.remote+" failed: "+firstLine(out),
			"make `git push "+r.remote+"` work from this shell (gh auth setup-git, or an SSH key GitHub accepts); saddle prs, restack and publish push from here")
	}
	return ok(CheckPush, "dry-run push to "+r.remote+" works; saddle publish can push")
}

// orchAllowRules are what the orchestrator needs pre-approved so the
// auto-mode classifier doesn't block it. saddle up's orchestrator gets them
// from agent.OrchestratorAllow; a Claude session used as the orchestrator
// (the saddle plugin) needs them in its settings.
var orchAllowRules = []string{"Bash(saddle:*)", "mcp__saddle"}

// orchAllow checks that a Claude session in the repo has the orchestrator's
// allowlist (#220), and that it doesn't hand the model git push.
func (r *run) orchAllow() Result {
	if r.orchAdapter() != config.HarnessClaude {
		return Result{} // no Claude orchestrator to configure (#668)
	}
	invalid, warnings := settingsProblems(r.env.Root())
	if len(invalid) > 0 {
		return fail(CheckOrchAllow, strings.Join(invalid, "; "), "repair the named settings file; saddle doctor --fix preserves malformed files")
	}
	have := map[string]bool{}
	for _, a := range r.env.ClaudeAllow() {
		have[a] = true
	}
	var missing []string
	for _, rule := range orchAllowRules {
		if !have[rule] {
			missing = append(missing, rule)
		}
	}
	if len(missing) > 0 {
		q := make([]string, len(missing))
		for i, m := range missing {
			q[i] = fmt.Sprintf("%q", m)
		}
		return local(warn(CheckOrchAllow, strings.Join(append(warnings, "your Claude settings don't allow "+strings.Join(missing, ", ")+
			", so an orchestrator in your own Claude session (the saddle plugin) can be blocked by the auto-mode classifier; saddle up's orchestrator has them",
		), "; "), `run saddle doctor --fix to merge into .claude/settings.local.json: {"permissions": {"allow": [`+strings.Join(q, ", ")+`]}}; push through saddle publish, not git push`))
	}
	if len(warnings) > 0 {
		return warn(CheckOrchAllow, strings.Join(warnings, "; "), "correct the named entries; Saddle tool permissions use mcp__saddle and the server name is saddle")
	}
	return ok(CheckOrchAllow, strings.Join(orchAllowRules, ", ")+" allowed; git push goes through saddle publish")
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
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
		return local(fail(CheckIgnored, ".saddle/ is not ignored, so state and worktrees could be committed",
			fixInit+" adds /.saddle/ to .git/info/exclude; or add /.saddle/ to .gitignore"))
	}
	return ok(CheckIgnored, ".saddle/ is ignored")
}

func (r *run) stateDB() Result {
	dir := filepath.Join(r.env.Root(), ".saddle")
	if !r.env.Exists(dir) {
		return local(fail(CheckStateDB, "repo not initialized", fixInit+" creates it"))
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

// WriteTable prints one row per check, then every check that isn't plain ok
// grouped by who acts on it, with its fix, and the next steps once nothing
// blocks.
func WriteTable(w io.Writer, rs []Result) {
	rs = describe(rs)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tCHECK\tDETAIL")
	for _, r := range rs {
		st := string(r.Status)
		switch {
		case r.Fixed:
			st = "fixed"
		case r.Status == Fail:
			st = "FAIL"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", st, r.Name, r.Detail)
	}
	_ = tw.Flush()
	writeGroups(w, rs)
}

// WriteJSON prints {"ok": <no check failed>, "checks": [...]}, each check
// with its group (ok, fixed, fixable, manual or optional).
func WriteJSON(w io.Writer, rs []Result) error {
	type check struct {
		Result
		Group string `json:"group"`
	}
	cs := make([]check, len(rs))
	for i, r := range describe(rs) {
		cs[i] = check{r, r.Group()}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		OK     bool    `json:"ok"`
		Checks []check `json:"checks"`
	}{!Failed(rs), cs})
}
