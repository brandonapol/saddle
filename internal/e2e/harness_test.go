//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/runq"
)

func TestHarnessRepoFactory(t *testing.T) {
	if Fork(t) {
		return
	}
	w := world(t, Options{})
	if got := w.OriginFile("main", "README.md"); got != "# demo\n" {
		t.Fatalf("origin main README = %q", got)
	}
	if _, err := os.Stat(filepath.Join(w.Repo, ".saddle", "config.toml")); err != nil {
		t.Fatalf("saddle init didn't run: %v", err)
	}
	if head := w.Git(w.Repo, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); head != "origin/main" {
		t.Fatalf("origin/HEAD = %q", head)
	}
	if s := w.GHState(); s.Owner != "e2e" || s.Name != "demo" || s.Repo.AllowMerge {
		t.Fatalf("fake GitHub = %+v", s)
	}
	if os.Getenv("HOME") != w.Home {
		t.Fatalf("HOME = %s, want the world's", os.Getenv("HOME"))
	}
}

func TestHarnessCLIRunner(t *testing.T) {
	if Fork(t) {
		return
	}
	w := world(t, Options{NoInit: true})
	if r := w.Saddle("version"); r.Code != 0 || strings.TrimSpace(r.Stdout) == "" {
		t.Fatalf("version: %s", r)
	}
	if r := w.Saddle("no-such-command"); r.Code == 0 {
		t.Fatalf("unknown command exited 0: %s", r)
	}
	r := w.Exec(w.Repo, "sh", "-c", `echo "$HOME|$TMUX_TMPDIR|$(command -v gh)|$(command -v claude)"`)
	want := w.Home + "|" + w.Tmux.Dir + "|" + bins.GH + "|" + filepath.Join(w.Bin, "claude")
	if strings.TrimSpace(r.Stdout) != want {
		t.Fatalf("env = %q, want %q", r.Stdout, want)
	}
}

func TestHarnessPrivateTmux(t *testing.T) {
	if Fork(t) {
		return
	}
	w := world(t, Options{NoInit: true})
	if err := w.Tmux.NewSession("probe", 50, 10, w.Repo, "cat"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(w.Tmux.Dir, "tmux-"+itoa(os.Getuid()), "default")); err != nil {
		t.Fatalf("socket not in the private dir: %v", err)
	}
	if err := w.Tmux.Type("probe:0", "hello tmux"); err != nil {
		t.Fatal(err)
	}
	if err := w.Tmux.SendKeys("probe:0", "Enter"); err != nil {
		t.Fatal(err)
	}
	Eventually(t, "cat to echo", func() error {
		s, err := w.Tmux.Capture("probe:0")
		if err != nil {
			return err
		}
		if strings.Count(s, "hello tmux") < 2 { // typed, then echoed by cat
			return errorf("screen:\n%s", s)
		}
		return nil
	})
	if ws := w.Tmux.Windows(); len(ws) != 1 || ws[0].Session != "probe" {
		t.Fatalf("windows = %+v", ws)
	}
}

func TestHarnessFakeAgentThroughSpawn(t *testing.T) {
	if Fork(t) {
		return
	}
	w := world(t, Options{})
	w.Spawn("t1", "Add hello", []string{"hello.txt"},
		fa.Write("hello.txt", "hi\n"), fa.Commit("add hello"), fa.Done("Adds hello.txt."))
	w.WaitAgentLog("t1", "idle: script finished")
	tv := w.WaitStatus("t1", "done")
	if tv.Train != "queued" {
		t.Fatalf("train = %q, want queued", tv.Train)
	}
	log := w.AgentLog("t1")
	if !strings.Contains(log, "prompt: ") || !strings.Contains(log, "step: 3 done ok") {
		t.Fatalf("agent log:\n%s", log)
	}
	if !w.Tmux.Alive(tv.Window) {
		t.Fatalf("agent window %s not in the private tmux server: %+v", tv.Window, w.Tmux.Windows())
	}
}

// TestHarnessInProcessJourneyIgnoresHostRunQueue (#575): in-process saddle
// calls read the process environment. A host queue in enforce mode with its
// only slot held must not stop a journey's gate, which runs in this process.
func TestHarnessInProcessJourneyIgnoresHostRunQueue(t *testing.T) {
	if Fork(t) {
		return
	}
	host := t.TempDir()
	state := filepath.Join(host, "state")
	cache := filepath.Join(host, "cache")
	config := filepath.Join(host, "config")
	db := filepath.Join(state, "saddle", "runq.db")
	must(t, os.MkdirAll(filepath.Join(config, "saddle"), 0o755))
	// Load gate off, so the only reason to wait is the held slot.
	must(t, os.WriteFile(filepath.Join(config, "saddle", "runq.toml"), []byte(
		"mode = \"enforce\"\nheartbeat = \"200ms\"\nstale_after = \"30s\"\n"+
			"max_load_per_cpu = 0\nmax_cpu_pressure = 0\n"+
			"[classes.go-test]\nslots = 1\n"), 0o644))
	for k, v := range map[string]string{
		"XDG_STATE_HOME":  state,
		"XDG_CACHE_HOME":  cache,
		"XDG_CONFIG_HOME": config,
		runq.EnvPath:      db,
		runq.EnvBypass:    string(runq.ModeEnforce),
		runq.EnvLease:     "",
	} {
		t.Setenv(k, v)
	}

	release := filepath.Join(host, "release")
	holder := exec.Command(bins.Saddle, "run", "--class", "go-test", "--wait-max", "2m", "--",
		"sh", "-c", "while [ ! -e "+shq(release)+" ]; do sleep 0.05; done")
	holder.Dir = host
	holder.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + host,
		"XDG_STATE_HOME=" + state,
		"XDG_CACHE_HOME=" + cache,
		"XDG_CONFIG_HOME=" + config,
		runq.EnvPath + "=" + db,
		runq.EnvBypass + "=" + string(runq.ModeEnforce),
		"SADDLE_TASK=host",
		"LANG=C.UTF-8",
		"TMPDIR=" + os.TempDir(),
	}
	var holdErr strings.Builder
	holder.Stderr = &holdErr
	must(t, holder.Start())
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o644)
		_ = holder.Process.Kill()
		_ = holder.Wait()
	})
	Eventually(t, "the host queue to hold its only go-test slot", func() error {
		out, err := hostRunqStatus(holder.Env, host)
		if err != nil {
			return errorf("runq status: %v\n%s\nholder: %s", err, out, holdErr.String())
		}
		if !strings.Contains(out, "running host") {
			return errorf("status:\n%s\nholder: %s", out, holdErr.String())
		}
		return nil
	})

	w := world(t, Options{})
	w.Spawn("t1", "Add hello", []string{"hello.txt"},
		fa.Write("hello.txt", "hi\n"), fa.Commit("add hello"), fa.Done("Adds hello.txt."))
	w.WaitStatus("t1", "done")

	// Land's test gate takes a heavy-run slot in this process. Five seconds
	// is enough to be granted an empty queue and far too little to outwait
	// the host's holder.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := w.App().Heavy().Run(ctx, app.HeavyOpts{
		Class: "go-test", Prio: runq.PrioGate, WaitMax: 5 * time.Second,
	}, exec.Command("true"))
	if err != nil {
		t.Fatalf("in-process gate did not run while the host held go-test: %v", err)
	}
	if res.Waited > time.Second {
		t.Fatalf("in-process gate waited %s; it used the host queue", res.Waited)
	}
	for _, kv := range []struct{ key, got string }{
		{"XDG_STATE_HOME", os.Getenv("XDG_STATE_HOME")},
		{"XDG_CACHE_HOME", os.Getenv("XDG_CACHE_HOME")},
		{runq.EnvPath, os.Getenv(runq.EnvPath)},
		{"runq config", runq.UserConfigPath(os.Getenv)},
	} {
		if !strings.HasPrefix(kv.got, w.Root+string(filepath.Separator)) {
			t.Fatalf("%s = %s, want it under %s", kv.key, kv.got, w.Root)
		}
	}
	if err := holder.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("host holder exited before the gate finished: %v\n%s", err, holdErr.String())
	}
}

// hostRunqStatus runs `saddle runq status` with env, outside any repo.
func hostRunqStatus(env []string, dir string) (string, error) {
	cmd := exec.Command(bins.Saddle, "runq", "status")
	cmd.Env, cmd.Dir = env, dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
