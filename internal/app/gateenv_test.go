package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// #184: environment failures are told apart from the branch's own.
func TestClassifyGateOutput(t *testing.T) {
	t.Parallel()
	for out, want := range map[string]bool{
		"open /tmp/TestX/001/db: disk quota exceeded":                 true,
		"write /tmp/go-build123/b001/x.a: no space left on device":    true,
		"sqlite: disk I/O error":                                      true,
		"go: cannot create temporary directory: mkdir /tmp/x: denied": true,
		"pattern ./...: cannot create temp file":                      true,
		"FAIL\tdemo/alpha\t1.2s\nsignal: killed":                      true,
		"runtime: cannot allocate memory":                             true,
		"--- FAIL: TestThing\n    want 1, got 2\nFAIL\tdemo/alpha":    false,
		"": false,
	} {
		p, env := ClassifyGateOutput(out)
		if env != want {
			t.Errorf("%q: env = %v (%+v), want %v", out, env, p, want)
		}
		if env && (p.Signature == "" || p.Free == "") {
			t.Errorf("%q: problem %+v names no signature or what to free", out, p)
		}
	}
}

// A gate run with fake output: fails with outs[i] on run i, passes after.
type fakeGate struct {
	outs []string
	envs [][]string
	runs int
}

func (g *fakeGate) run(_ context.Context, env []string) (string, error) {
	g.envs = append(g.envs, env)
	g.runs++
	if g.runs <= len(g.outs) {
		return g.outs[g.runs-1], errors.New("exit status 1")
	}
	return "ok", nil
}

func envOf(env []string, k string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, k+"="); ok {
			return v
		}
	}
	return ""
}

// #184: a gate that hits the disk quota once, then passes, passes: the
// branch isn't blamed and nothing is counted against it. The gate's TMPDIR
// and GOTMPDIR are a per-run dir under the user cache dir, gone afterwards.
func TestRunGateEnvRetriesEnvironmentFailure(t *testing.T) {
	a, _ := setup(t)
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	g := &fakeGate{outs: []string{"open /tmp/TestX/db: disk quota exceeded"}}
	res := a.RunGateEnv(context.Background(), "t1", g.run)
	if res.Err != nil || res.Env != nil || res.Retries != 1 || g.runs != 2 {
		t.Fatalf("result = %+v after %d runs; want a pass after one retry", res, g.runs)
	}
	base := a.GateTmpdir()
	for _, env := range g.envs {
		tmp := envOf(env, "TMPDIR")
		if !strings.HasPrefix(tmp, base+string(filepath.Separator)) || envOf(env, "GOTMPDIR") != tmp {
			t.Fatalf("gate env TMPDIR=%q GOTMPDIR=%q, want one dir under %s", tmp, envOf(env, "GOTMPDIR"), base)
		}
		if _, err := os.Stat(tmp); !os.IsNotExist(err) {
			t.Fatalf("per-run tmpdir %s left behind (%v)", tmp, err)
		}
	}
	if !strings.HasPrefix(base, filepath.Join(cache, "saddle", "tmp")+string(filepath.Separator)) {
		t.Fatalf("default tmpdir %s is not under the user cache dir", base)
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatalf("empty gate tmpdir %s left in the user cache dir (%v)", base, err)
	}
	es, err := a.Store.Events(100)
	must(t, err)
	found := false
	for _, e := range es {
		found = found || e.Kind == EventGateEnv && strings.Contains(e.Data, "disk quota exceeded")
	}
	if !found {
		t.Fatalf("no %s event naming the problem: %+v", EventGateEnv, es)
	}
}

// #184: a real failure is the branch's: no retry.
func TestRunGateEnvRealFailureIsNotRetried(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	g := &fakeGate{outs: []string{"--- FAIL: TestThing", "--- FAIL: TestThing"}}
	res := a.RunGateEnv(context.Background(), "t1", g.run)
	if res.Err == nil || res.Env != nil || g.runs != 1 || !strings.Contains(res.Output, "TestThing") {
		t.Fatalf("result = %+v after %d runs; want one red run blamed on the branch", res, g.runs)
	}
}

// #184: an environment that stays broken after two retries is the
// orchestrator's problem, raised as an action notice naming what to free.
func TestRunGateEnvEscalatesAfterTwoRetries(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	quota := "write /tmp/x: disk quota exceeded"
	g := &fakeGate{outs: []string{quota, quota, quota, quota}}
	res := a.RunGateEnv(context.Background(), "t1", g.run)
	if res.Err == nil || res.Env == nil || res.Retries != 2 || g.runs != 3 {
		t.Fatalf("result = %+v after %d runs; want an env failure after 2 retries", res, g.runs)
	}
	ns, err := a.Store.TakeNotices(OrchestratorID, false)
	must(t, err)
	var got *store.Notice
	for i := range ns {
		if strings.Contains(ns[i].Text, "disk quota exceeded") {
			got = &ns[i]
		}
	}
	if got == nil || got.Kind != store.NoticeAction || !strings.Contains(got.Text, "t1") || !strings.Contains(got.Text, a.GateTmpdir()) {
		t.Fatalf("orchestrator notices = %+v, want an action notice naming t1, the problem and the dir to free", ns)
	}
	if tn, _ := a.Store.TakeNotices("t1", false); len(tn) != 0 {
		t.Fatalf("the branch's producer was told: %+v", tn)
	}
}

// #184: day-old Test* and go-build* dirs in the gate's tmpdir are swept
// before a run; fresh ones and anything else stay.
func TestRunGateEnvSweepsStaleTempDirs(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	a.Cfg.Train.Tmpdir = filepath.Join(t.TempDir(), "scratch")
	base := a.GateTmpdir()
	if base != a.Cfg.Train.Tmpdir {
		t.Fatalf("tmpdir = %s, want %s", base, a.Cfg.Train.Tmpdir)
	}
	old := time.Now().Add(-25 * time.Hour)
	for _, d := range []string{"TestOld123", "go-build456", "TestFresh789", "keep"} {
		must(t, os.MkdirAll(filepath.Join(base, d, "sub"), 0o755))
		if d != "TestFresh789" {
			must(t, os.Chtimes(filepath.Join(base, d), old, old))
		}
	}
	g := &fakeGate{}
	if res := a.RunGateEnv(context.Background(), "t1", g.run); res.Err != nil {
		t.Fatal(res.Err)
	}
	for d, want := range map[string]bool{"TestOld123": false, "go-build456": false, "TestFresh789": true, "keep": true} {
		_, err := os.Stat(filepath.Join(base, d))
		if (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", d, err == nil, want)
		}
	}
}

// The gate's scratch dir is never inside the repo: a test temp dir there
// sits under the repo's .saddle/config.toml, and the plugin's walk up to
// find a saddle repo found the real one, so internal/cli failed the gate
// with the trust prompt for the owner's checkout. An in-repo [train] tmpdir
// falls back to the default.
func TestGateTmpdirStaysOutOfTheRepo(t *testing.T) {
	a, _ := setup(t)
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	def := a.GateTmpdir()
	if !strings.HasPrefix(def, filepath.Join(cache, "saddle", "tmp")+string(filepath.Separator)) {
		t.Fatalf("default tmpdir %s, want one under %s", def, cache)
	}
	for _, d := range []string{".saddle/tmp", filepath.Join(a.Root, "scratch"), a.Root} {
		a.Cfg.Train.Tmpdir = d
		if got := a.GateTmpdir(); got != def {
			t.Errorf("[train] tmpdir %q: gate tmpdir %s, want the default %s", d, got, def)
		}
	}
	out := filepath.Join(t.TempDir(), "gate")
	a.Cfg.Train.Tmpdir = out
	if got := a.GateTmpdir(); got != out {
		t.Fatalf("[train] tmpdir %q: gate tmpdir %s", out, got)
	}
}

// The train runs inside the orchestrator's saddle mcp (SADDLE_TASK=t0,
// SADDLE_ROOT, CLAUDE_PROJECT_DIR) and sometimes under git (GIT_DIR). A gate
// run through RunGateEnv in a task's worktree sees none of it, and its
// TMPDIR and GOTMPDIR are outside the repo, even with [train] tmpdir in it.
func TestRunGateEnvHostileEnvInWorktree(t *testing.T) {
	a, _ := setup(t)
	must(t, a.Init())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	wt := filepath.Join(a.Root, ".saddle", "worktrees", "t1")
	git(t, a.Root, "worktree", "add", "-q", "-b", "saddle/t1", wt)
	t.Setenv("GIT_DIR", filepath.Join(a.Root, ".git"))
	t.Setenv("GIT_WORK_TREE", a.Root)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(a.Root, ".git", "index"))
	t.Setenv("SADDLE_TASK", "t0")
	t.Setenv("SADDLE_ROOT", a.Root)
	t.Setenv("CLAUDE_PROJECT_DIR", a.Root)
	t.Setenv("TMPDIR", filepath.Join(a.Root, ".saddle", "tmp"))
	t.Setenv("GOTMPDIR", filepath.Join(a.Root, ".saddle", "tmp"))
	a.Cfg.Train.Tmpdir = ".saddle/tmp"

	gate := "ROOT=" + shellQuote(a.Root) + "\n" + `set -e
for v in GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE SADDLE_TASK SADDLE_ROOT CLAUDE_PROJECT_DIR; do
  eval "x=\${$v:-}"; [ -z "$x" ] || { echo "$v inherited: $x"; exit 1; }
done
top=$(git rev-parse --show-toplevel)
[ "$top" = "$PWD" ] || { echo "git toplevel $top, want $PWD"; exit 1; }
for t in "$TMPDIR" "$GOTMPDIR"; do
  case "$t/" in "$ROOT"/*) echo "temp dir $t is inside the repo $ROOT"; exit 1;; esac
done
echo gate ok`
	res := a.RunGateEnv(context.Background(), "t1", ShellGate(wt, gate, time.Minute))
	if res.Err != nil || !strings.Contains(res.Output, "gate ok") {
		t.Fatalf("gate in a hostile env: %v\n%s", res.Err, res.Output)
	}
}

func TestGateEnvironDropsTheTrainsVars(t *testing.T) {
	t.Parallel()
	got := GateEnviron([]string{"PATH=/bin", "GIT_DIR=/r/.git", "SADDLE_TASK=t0", "SADDLE_TRUST_FILE=/x",
		"GIT_AUTHOR_NAME=t", "CLAUDE_PROJECT_DIR=/r", "HOME=/h"})
	if want := []string{"PATH=/bin", "GIT_AUTHOR_NAME=t", "HOME=/h"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("GateEnviron = %q, want %q", got, want)
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// setGateTimeout sets [train] gate_timeout in a user config private to t.
func setGateTimeout(t *testing.T, d string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "saddle")
	must(t, os.MkdirAll(dir, 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[train]\ngate_timeout = \""+d+"\"\n"), 0o644))
}

// gone waits for the process pid wrote to file to be gone.
func gone(t *testing.T, file string) {
	t.Helper()
	b, err := os.ReadFile(file)
	must(t, err)
	var pid int
	if _, err := fmt.Sscan(string(b), &pid); err != nil || pid <= 0 {
		t.Fatalf("pid file %s: %q", file, b)
	}
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if !pidAlive(pid) {
			return
		}
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("the gate's child %d outlived it", pid)
}

// #269: a hung gate is killed with everything it started once it runs past
// [train] gate_timeout, and the timeout is the branch's failure, not the
// environment's: nothing is retried.
func TestRunGateEnvTimeoutKillsTheGroup(t *testing.T) {
	a, _ := setup(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	setGateTimeout(t, "300ms")
	if got := a.GateTimeout(); got != 300*time.Millisecond {
		t.Fatalf("GateTimeout = %s", got)
	}
	pidf := filepath.Join(t.TempDir(), "child")
	start := time.Now()
	res := a.RunGateEnv(context.Background(), "t1", ShellGate(a.Root, "sleep 600 & echo $! > "+pidf+"; echo hanging; trap '' TERM; wait", a.GateTimeout()))
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("gate took %s", took)
	}
	if res.Err == nil || res.Env != nil || res.Retries != 0 {
		t.Fatalf("timeout: %+v", res)
	}
	var to *GateTimeoutError
	if !errors.As(res.Err, &to) {
		t.Fatalf("err = %v, want a GateTimeoutError", res.Err)
	}
	for _, want := range []string{"hanging", "gate timed out after 300ms"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("output %q lacks %q", res.Output, want)
		}
	}
	gone(t, pidf)
}

// #269: a gate that leaves a helper holding its stdout returns when its
// shell exits, not when the helper does.
func TestShellGateReturnsWhenTheShellExits(t *testing.T) {
	t.Parallel()
	start := time.Now()
	out, err := ShellGate(t.TempDir(), "(sleep 20; echo helper-exit) & echo gate-ok", time.Minute)(context.Background(), nil)
	if err != nil || !strings.Contains(out, "gate-ok") {
		t.Fatalf("gate: %v\n%s", err, out)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("gate took %s: it waited on its helper", took)
	}
}

// #269: when the gate's caller gives up (land is interrupted), the gate's
// whole group goes with it.
func TestShellGateCancelKillsTheGroup(t *testing.T) {
	t.Parallel()
	pidf := filepath.Join(t.TempDir(), "child")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
			if b, _ := os.ReadFile(pidf); len(b) > 0 {
				break
			}
		}
		cancel()
	}()
	_, err := ShellGate(t.TempDir(), "sleep 600 & echo $! > "+pidf+"; wait", time.Hour)(ctx, nil)
	if err == nil {
		t.Fatal("a cancelled gate passed")
	}
	gone(t, pidf)
}
