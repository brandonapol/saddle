//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
)

func TestHarnessRepoFactory(t *testing.T) {
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
