package doctor

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/refguard"
)

type res struct {
	out string
	err error
}

// fakeEnv is a healthy repo; tests break one thing at a time.
type fakeEnv struct {
	root      string
	cfg       config.Config
	cfgErr    error
	git       map[string]res
	gh        map[string]res
	exec      map[string]res
	paths     map[string]bool // LookPath and Exists
	hooks     []refguard.HookState
	hooksErr  error
	storeErr  error
	leftovers int
	kept      int
	leftErr   error
	ghCalls   []string
}

const repoJSON = `{"full_name":"o/r","html_url":"https://github.com/o/r","allow_merge_commit":false,"allow_squash_merge":true,"allow_rebase_merge":false}`

const protectionJSON = `{"required_status_checks":{"contexts":["test"]},"required_pull_request_reviews":{"required_approving_review_count":1},"enforce_admins":{"enabled":true}}`

func healthy(t *testing.T) *fakeEnv {
	cfg := config.Default()
	cfg.Test.Cmd = "go test ./..."
	root := t.TempDir()
	return &fakeEnv{
		root: root,
		cfg:  cfg,
		git: map[string]res{
			"remote":                {out: "origin"},
			"remote get-url origin": {out: "git@github.com:o/r.git"},
			"symbolic-ref --short refs/remotes/origin/HEAD": {out: "origin/main"},
			"check-ignore -q .saddle/":                      {},
		},
		gh: map[string]res{
			"api repos/{owner}/{repo}":                          {out: repoJSON},
			"api repos/{owner}/{repo}/branches/main/protection": {out: protectionJSON},
		},
		exec: map[string]res{
			"gh --version":     {out: "gh version 2.101.0 (2026-09-15)\nhttps://github.com/cli/cli"},
			"gh auth status":   {out: "github.com\n  ✓ Logged in to github.com account me (keyring)\n  - Token scopes: 'gist', 'read:org', 'repo', 'workflow'"},
			"tmux -V":          {out: "tmux 3.7c"},
			"claude --version": {out: "2.1.284 (Claude Code)"},
		},
		paths: map[string]bool{
			"gh": true, "tmux": true, "claude": true, "go": true,
			filepath.Join(root, ".saddle"): true,
			"/usr/bin/saddle":              true,
		},
		hooks: []refguard.HookState{
			{Name: "reference-transaction", Path: "/r/.git/hooks/reference-transaction", Present: true, Saddle: true, Bin: "/usr/bin/saddle"},
			{Name: "pre-push", Path: "/r/.git/hooks/pre-push", Present: true, Saddle: true, Bin: "/usr/bin/saddle"},
		},
	}
}

func lookup(m map[string]res, args []string) (string, error) {
	r, ok := m[strings.Join(args, " ")]
	if !ok {
		return "", errors.New("exit status 1: not faked: " + strings.Join(args, " "))
	}
	return r.out, r.err
}

func (f *fakeEnv) Root() string                       { return f.root }
func (f *fakeEnv) Config() (config.Config, error)     { return f.cfg, f.cfgErr }
func (f *fakeEnv) Git(args ...string) (string, error) { return lookup(f.git, args) }
func (f *fakeEnv) GH(args ...string) (string, error) {
	f.ghCalls = append(f.ghCalls, strings.Join(args, " "))
	return lookup(f.gh, args)
}
func (f *fakeEnv) Exec(name string, args ...string) (string, error) {
	return lookup(f.exec, append([]string{name}, args...))
}
func (f *fakeEnv) LookPath(name string) (string, error) {
	if f.paths[name] {
		return "/usr/bin/" + name, nil
	}
	return "", errors.New("executable file not found in $PATH")
}
func (f *fakeEnv) Exists(path string) bool              { return f.paths[path] }
func (f *fakeEnv) Hooks() ([]refguard.HookState, error) { return f.hooks, f.hooksErr }
func (f *fakeEnv) OpenStore(string) error               { return f.storeErr }
func (f *fakeEnv) Leftovers() (int, int, error)         { return f.leftovers, f.kept, f.leftErr }

func find(t *testing.T, rs []Result, name string) Result {
	t.Helper()
	for _, r := range rs {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no check %q in %+v", name, rs)
	return Result{}
}

func TestHealthyRepoAllOK(t *testing.T) {
	rs := Run(healthy(t))
	for _, r := range rs {
		if r.Status != OK {
			t.Errorf("%s: %s %q (fix %q)", r.Name, r.Status, r.Detail, r.Fix)
		}
	}
	if Failed(rs) {
		t.Fatal("Failed on a healthy repo")
	}
	want := []string{CheckConfig, CheckRemote, CheckDefaultBranch, CheckGH, CheckMergeSettings, CheckProtection, CheckGhStack,
		CheckTestCmd, CheckTmux, CheckClaude, CheckHooks, CheckGate, CheckIgnored, CheckStateDB, CheckLeftovers}
	if len(rs) != len(want) {
		t.Fatalf("got %d checks, want %d", len(rs), len(want))
	}
	for i, r := range rs {
		if r.Name != want[i] {
			t.Errorf("check %d = %q, want %q", i, r.Name, want[i])
		}
	}
	if d := find(t, rs, CheckTmux).Detail; !strings.Contains(d, "3.7c") {
		t.Errorf("tmux detail %q lacks the version", d)
	}
	if d := find(t, rs, CheckClaude).Detail; !strings.Contains(d, "2.1.284") {
		t.Errorf("claude detail %q lacks the version", d)
	}
}

// TestChecks covers each check's warn and fail paths; ok is TestHealthyRepoAllOK.
func TestChecks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		check  string
		break_ func(f *fakeEnv)
		want   Status
		fix    string // substring of the fix message
	}{
		{"config error", CheckConfig, func(f *fakeEnv) { f.cfgErr = errors.New("train.output \"x\"") }, Fail, "config.toml"},

		{"no remote", CheckRemote, func(f *fakeEnv) { f.git["remote"] = res{out: ""} }, Fail, "git remote add origin"},
		{"upstream remote preferred", CheckRemote, func(f *fakeEnv) {
			f.git["remote"] = res{out: "fork\nupstream"}
			f.git["config branch.main.remote"] = res{out: "upstream"}
			f.git["remote get-url upstream"] = res{out: "git@github.com:o/r.git"}
			f.git["symbolic-ref --short refs/remotes/upstream/HEAD"] = res{out: "upstream/main"}
		}, OK, ""},

		{"default branch differs from base", CheckDefaultBranch, func(f *fakeEnv) {
			f.git["symbolic-ref --short refs/remotes/origin/HEAD"] = res{out: "origin/trunk"}
		}, Fail, `base = "trunk"`},
		{"default branch from gh", CheckDefaultBranch, func(f *fakeEnv) {
			delete(f.git, "symbolic-ref --short refs/remotes/origin/HEAD")
			f.gh["repo view --json defaultBranchRef -q .defaultBranchRef.name"] = res{out: "main"}
		}, OK, ""},
		{"default branch unknown", CheckDefaultBranch, func(f *fakeEnv) {
			delete(f.git, "symbolic-ref --short refs/remotes/origin/HEAD")
		}, Warn, "git remote set-head origin --auto"},

		{"gh missing", CheckGH, func(f *fakeEnv) { f.paths["gh"] = false }, Fail, "cli.github.com"},
		{"gh logged out", CheckGH, func(f *fakeEnv) {
			f.exec["gh auth status"] = res{out: "You are not logged into any GitHub hosts.", err: errors.New("exit status 1")}
		}, Fail, "gh auth login"},
		{"gh lacks repo scope", CheckGH, func(f *fakeEnv) {
			f.exec["gh auth status"] = res{out: "  ✓ Logged in\n  - Token scopes: 'gist', 'workflow'"}
		}, Fail, "gh auth refresh -s repo"},
		{"gh lacks workflow scope", CheckGH, func(f *fakeEnv) {
			f.exec["gh auth status"] = res{out: "  ✓ Logged in\n  - Token scopes: 'repo', 'read:org'"}
		}, Warn, "gh auth refresh -s workflow"},
		{"gh scopes unlisted", CheckGH, func(f *fakeEnv) {
			f.exec["gh auth status"] = res{out: "  ✓ Logged in to github.com account me (GH_TOKEN)"}
		}, OK, ""},

		{"merge commits allowed", CheckMergeSettings, func(f *fakeEnv) {
			f.gh["api repos/{owner}/{repo}"] = res{out: strings.Replace(repoJSON, `"allow_merge_commit":false`, `"allow_merge_commit":true`, 1)}
		}, Fail, "allow_merge_commit=false"},
		{"squash off", CheckMergeSettings, func(f *fakeEnv) {
			f.gh["api repos/{owner}/{repo}"] = res{out: strings.Replace(repoJSON, `"allow_squash_merge":true`, `"allow_squash_merge":false`, 1)}
		}, Warn, "allow_squash_merge=true"},
		{"merge settings unreadable", CheckMergeSettings, func(f *fakeEnv) {
			f.gh["api repos/{owner}/{repo}"] = res{out: `{"full_name":"o/r"}`}
		}, Warn, "admin or push"},
		{"merge settings skipped without gh", CheckMergeSettings, func(f *fakeEnv) { f.paths["gh"] = false }, Warn, "gh"},

		{"base unprotected", CheckProtection, func(f *fakeEnv) {
			f.gh["api repos/{owner}/{repo}/branches/main/protection"] = res{err: errors.New("gh: Branch not protected (HTTP 404)")}
		}, Warn, "Settings -> Branches"},
		{"protection unreadable", CheckProtection, func(f *fakeEnv) {
			f.gh["api repos/{owner}/{repo}/branches/main/protection"] = res{err: errors.New("gh: Resource not accessible (HTTP 403)")}
		}, Warn, "admin"},
		{"no required checks", CheckProtection, func(f *fakeEnv) {
			f.gh["api repos/{owner}/{repo}/branches/main/protection"] = res{out: `{"enforce_admins":{"enabled":false}}`}
		}, Warn, "required status check"},
		{"protection uses detected default branch", CheckProtection, func(f *fakeEnv) {
			f.cfg.Base = "trunk"
			f.git["symbolic-ref --short refs/remotes/origin/HEAD"] = res{out: "origin/trunk"}
			f.gh["api repos/{owner}/{repo}/branches/trunk/protection"] = res{out: protectionJSON}
		}, OK, ""},

		{"no test cmd", CheckTestCmd, func(f *fakeEnv) { f.cfg.Test.Cmd = "" }, Warn, "[test]"},
		{"no test cmd, detectable", CheckTestCmd, func(f *fakeEnv) {
			f.cfg.Test.Cmd = ""
			if err := os.WriteFile(filepath.Join(f.root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, Warn, `cmd = "go test ./..."`},
		{"test cmd not runnable", CheckTestCmd, func(f *fakeEnv) { f.cfg.Test.Cmd = "CGO_ENABLED=0 pytest -q" }, Fail, "pytest"},

		{"tmux missing", CheckTmux, func(f *fakeEnv) { f.paths["tmux"] = false }, Fail, "install tmux"},
		{"claude missing", CheckClaude, func(f *fakeEnv) { f.paths["claude"] = false }, Fail, "claude"},
		{"custom claude cmd", CheckClaude, func(f *fakeEnv) {
			f.cfg.Claude.Cmd = "/opt/cc --verbose"
			f.paths["/opt/cc"] = true
			f.exec["/opt/cc --version"] = res{out: "2.2.0 (Claude Code)"}
		}, OK, ""},

		{"hooks missing", CheckHooks, func(f *fakeEnv) {
			f.hooks[0] = refguard.HookState{Name: "reference-transaction", Path: "/r/.git/hooks/reference-transaction"}
		}, Fail, "saddle init"},
		{"foreign hook", CheckHooks, func(f *fakeEnv) {
			f.hooks[1] = refguard.HookState{Name: "pre-push", Path: "/r/.git/hooks/pre-push", Present: true}
		}, Fail, "chains it"},
		{"hook binary gone", CheckHooks, func(f *fakeEnv) { f.paths["/usr/bin/saddle"] = false }, Warn, "make install"},
		{"hooks unreadable", CheckHooks, func(f *fakeEnv) { f.hooksErr = errors.New("not a git repo") }, Warn, ""},

		{"gate shipped but not installed", CheckGate, func(f *fakeEnv) {
			gateRepo(t, f, false)
		}, Warn, "make setup/hooks"},
		{"gate hook not active in worktrees", CheckGate, func(f *fakeEnv) {
			gateRepo(t, f, true)
			f.git["-C "+f.root+"/.saddle/worktrees/t1-x rev-parse --path-format=absolute --git-path hooks/pre-commit"] =
				res{out: f.root + "/.saddle/worktrees/t1-x/.githooks/pre-commit"}
		}, Warn, "core.hooksPath"},
		{"hooks dir inside the checkout", CheckGate, func(f *fakeEnv) {
			gateRepo(t, f, true)
			f.git["-C "+f.root+" rev-parse --path-format=absolute --git-path hooks"] = res{out: f.root + "/.githooks"}
			write(t, filepath.Join(f.root, ".githooks", "pre-commit"), "#!/bin/sh\nmake check\n")
		}, Warn, "clobber"},

		{".saddle tracked", CheckIgnored, func(f *fakeEnv) { delete(f.git, "check-ignore -q .saddle/") }, Fail, "/.saddle/"},

		{"not initialized", CheckStateDB, func(f *fakeEnv) { f.paths[filepath.Join(f.root, ".saddle")] = false }, Fail, "saddle init"},
		{"state.db broken", CheckStateDB, func(f *fakeEnv) { f.storeErr = errors.New("attempt to write a readonly database") }, Fail, "state.db"},

		{"leftovers", CheckLeftovers, func(f *fakeEnv) { f.leftovers = 3 }, Warn, "saddle gc"},
		{"leftovers with kept work", CheckLeftovers, func(f *fakeEnv) { f.leftovers, f.kept = 2, 5 }, Warn, "saddle gc"},
		{"leftovers unreadable", CheckLeftovers, func(f *fakeEnv) { f.leftErr = errors.New("boom") }, Warn, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := healthy(t)
			tc.break_(f)
			r := find(t, Run(f), tc.check)
			if r.Status != tc.want {
				t.Fatalf("status %s (detail %q, fix %q), want %s", r.Status, r.Detail, r.Fix, tc.want)
			}
			if tc.want != OK && r.Fix == "" && tc.fix != "" {
				t.Fatalf("no fix message")
			}
			if !strings.Contains(r.Fix, tc.fix) {
				t.Fatalf("fix %q lacks %q", r.Fix, tc.fix)
			}
			if tc.want == OK && r.Fix != "" {
				t.Fatalf("ok with a fix: %q", r.Fix)
			}
		})
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// gateRepo gives f quark's layout: git/hooks/pre-commit runs make check and
// `make setup/hooks` links it into .git/hooks; installed says whether it is.
// One agent worktree, t1-x, shares the common hooks directory.
func gateRepo(t *testing.T, f *fakeEnv, installed bool) {
	t.Helper()
	write(t, filepath.Join(f.root, "Makefile"), "check:\n\tgo test ./...\nfix:\n\tgofmt -w .\nsetup/hooks:\n\tln -sf ../../git/hooks/pre-commit .git/hooks/\n")
	write(t, filepath.Join(f.root, "git", "hooks", "pre-commit"), "#!/bin/sh\nmake check\n")
	hooks := filepath.Join(f.root, ".git", "hooks")
	if installed {
		write(t, filepath.Join(hooks, "pre-commit"), "#!/bin/sh\nmake check\n")
	}
	wt := filepath.Join(f.root, ".saddle", "worktrees", "t1-x")
	f.git["-C "+f.root+" rev-parse --path-format=absolute --git-path hooks"] = res{out: hooks}
	f.git["worktree list --porcelain"] = res{out: "worktree " + f.root + "\nHEAD abc\nbranch refs/heads/main\n\nworktree " + wt + "\nHEAD def\nbranch refs/heads/saddle/t1-x"}
	f.git["-C "+wt+" rev-parse --path-format=absolute --git-path hooks/pre-commit"] = res{out: filepath.Join(hooks, "pre-commit")}
}

func TestGateCheckReportsDetectedGate(t *testing.T) {
	f := healthy(t)
	if r := find(t, Run(f), CheckGate); r.Status != OK || !strings.Contains(r.Detail, "none detected") {
		t.Fatalf("no gate: %+v", r)
	}
	gateRepo(t, f, true)
	r := find(t, Run(f), CheckGate)
	if r.Status != OK {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{"pre-commit hook", "make check", "fix: make fix", "installed", "active in agent worktrees"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail %q lacks %q", r.Detail, want)
		}
	}
}

func TestEveryNonOKHasAFix(t *testing.T) {
	f := healthy(t)
	f.paths = map[string]bool{}
	f.git = map[string]res{}
	f.cfg.Test.Cmd = ""
	f.hooks = []refguard.HookState{{Name: "reference-transaction"}, {Name: "pre-push"}}
	f.leftovers = 1
	for _, r := range Run(f) {
		if r.Status != OK && r.Fix == "" {
			t.Errorf("%s is %s with no fix", r.Name, r.Status)
		}
	}
}

func TestFailed(t *testing.T) {
	if Failed([]Result{{Status: OK}, {Status: Warn}}) {
		t.Fatal("warnings alone must not fail")
	}
	if !Failed([]Result{{Status: OK}, {Status: Fail}}) {
		t.Fatal("a fail must fail")
	}
}

func TestWriteTableAndJSON(t *testing.T) {
	f := healthy(t)
	f.leftovers = 2
	f.git["symbolic-ref --short refs/remotes/origin/HEAD"] = res{out: "origin/trunk"}
	rs := Run(f)

	var tbl bytes.Buffer
	WriteTable(&tbl, rs)
	out := tbl.String()
	for _, want := range []string{"STATUS", "CHECK", "FAIL", "warn", CheckDefaultBranch, "fix:", `base = "trunk"`, "saddle gc"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}

	var js bytes.Buffer
	if err := WriteJSON(&js, rs); err != nil {
		t.Fatal(err)
	}
	var got struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
			Fix    string `json:"fix"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(js.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, js.String())
	}
	if got.OK || len(got.Checks) != len(rs) {
		t.Fatalf("ok %v, %d checks: %s", got.OK, len(got.Checks), js.String())
	}
	statuses := map[string]string{}
	for _, c := range got.Checks {
		statuses[c.Name] = c.Status
	}
	if statuses[CheckDefaultBranch] != "fail" || statuses[CheckLeftovers] != "warn" || statuses[CheckTmux] != "ok" {
		t.Fatalf("statuses %v", statuses)
	}
}

// #158: branches gc keeps because they hold unmerged work don't make doctor
// warn, since gc can't clear them; they're noted in the detail.
func TestLeftoversKeptOnlyIsOK(t *testing.T) {
	f := healthy(t)
	f.kept = 4
	r := find(t, Run(f), CheckLeftovers)
	if r.Status != OK || !strings.Contains(r.Detail, "4 kept") {
		t.Fatalf("kept-only leftovers = %s %q, want ok noting 4 kept", r.Status, r.Detail)
	}
}

// #211: the gh stack check reports the extension, whether the repo has
// Stacked PRs, and the stack backend. Missing pieces only warn: saddle falls
// back to chaining PR bases and never fails the train over them.
func TestGhStackCheck(t *testing.T) {
	const version, probe = "stack --version", "api repos/{owner}/{repo}/stacks?per_page=1"
	for _, c := range []struct {
		name    string
		backend string
		gh      map[string]res
		status  Status
		want    []string
	}{
		{"saddle without extension", "saddle", nil, OK, []string{"not installed", `stack_backend = "saddle"`}},
		{"saddle with extension", "saddle", map[string]res{version: {out: "gh stack version 0.1.1"}, probe: {out: "[]"}}, OK,
			[]string{"0.1.1", "Stacked PRs enabled", `stack_backend = "saddle"`}},
		{"gh-stack ready", "gh-stack", map[string]res{version: {out: "gh stack version 0.1.1"}, probe: {out: "[]"}}, OK,
			[]string{"0.1.1", "Stacked PRs enabled", `stack_backend = "gh-stack"`}},
		{"gh-stack missing", "gh-stack", nil, Warn, []string{"not installed", "gh extension install github/gh-stack"}},
		{"gh-stack not enabled", "gh-stack", map[string]res{version: {out: "gh stack version 0.1.1"},
			probe: {err: errors.New("exit status 1: Stacked PRs are not enabled for this repository (HTTP 404)")}}, Warn,
			[]string{"aren't enabled", "chained by PR bases"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := healthy(t)
			env.cfg.Train.StackBackend = c.backend
			for k, v := range c.gh {
				env.gh[k] = v
			}
			r := find(t, Run(env), CheckGhStack)
			text := r.Detail + " / " + r.Fix
			if r.Status != c.status {
				t.Fatalf("status = %s, want %s: %s", r.Status, c.status, text)
			}
			for _, w := range c.want {
				if !strings.Contains(text, w) {
					t.Fatalf("%q lacks %q", text, w)
				}
			}
			for _, call := range env.ghCalls {
				if strings.Contains(call, "-X") || strings.HasPrefix(call, "stack link") || strings.HasPrefix(call, "stack merge") {
					t.Fatalf("doctor must only read: %s", call)
				}
			}
		})
	}
}
