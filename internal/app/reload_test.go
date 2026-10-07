package app

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
)

// #228: the hand fix for a misdetected gate was lint.cmd in
// .saddle/config.toml, but a running train kept the gate it read at
// startup. Land re-reads the config, so lint.cmd edited between two lands
// takes effect on the second.
func TestLandReloadsLintCmdBetweenLands(t *testing.T) {
	a := trainSetup(t)
	tk := queueTask(t, a, "t1", "lint", map[string]string{"x.go": "LINTBAD\n"})
	write(t, a.Root, ".saddle/config.toml", "[test]\ncmd = \"none\"\n[train]\nlint.cmd = '"+badLint+"'\n")
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].State != store.TestFailed || rs[0].Note != "lint failed" {
		t.Fatalf("first land, with the red gate configured after Open: %+v", rs)
	}

	write(t, a.Root, ".saddle/config.toml", "[test]\ncmd = \"none\"\n[train]\nlint.cmd = \"true\"\n")
	must(t, a.Store.Enqueue(tk.ID))
	rs, err = a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].State != store.TrainOK {
		t.Fatalf("second land, after lint.cmd changed: %+v", rs)
	}
	if a.Cfg.Train.Lint.Cmd != "true" {
		t.Fatalf("Cfg.Train.Lint = %+v", a.Cfg.Train.Lint)
	}
}

// Only what the file changed is applied: a setting made in memory stands
// while the file leaves it alone, and other sections are untouched.
func TestReloadTrainConfigAppliesOnlyChanges(t *testing.T) {
	a := trainSetup(t)
	must(t, a.Init()) // writes the commented-out template
	a.Cfg.Train.Lint = config.Lint{Cmd: "in-memory", Set: true}
	if changed, err := a.ReloadTrainConfig(); err != nil || changed || a.Cfg.Train.Lint.Cmd != "in-memory" || a.Cfg.Test.Cmd != "none" {
		t.Fatalf("template only: changed=%v err=%v cfg=%+v %+v", changed, err, a.Cfg.Train.Lint, a.Cfg.Test)
	}
	write(t, a.Root, ".saddle/config.toml", "[train]\nmax_attempts = 5\n")
	if changed, err := a.ReloadTrainConfig(); err != nil || !changed || a.Cfg.Train.MaxAttempts != 5 || a.Cfg.Train.Lint.Cmd != "in-memory" {
		t.Fatalf("max_attempts edit: changed=%v err=%v cfg=%+v", changed, err, a.Cfg.Train)
	}
	write(t, a.Root, ".saddle/config.toml", "[train]\nmax_attempts = 5\nlint.cmd = \"\"\n")
	if _, err := a.ReloadTrainConfig(); err != nil || !a.Cfg.Train.Lint.Disabled() {
		t.Fatalf("lint.cmd = \"\": err=%v lint=%+v", err, a.Cfg.Train.Lint)
	}
	write(t, a.Root, ".saddle/config.toml", "[train\n")
	if _, err := a.ReloadTrainConfig(); err == nil {
		t.Fatal("a broken config must be reported")
	}
}

// SIGHUP to saddle mcp reloads the config without waiting for a land.
func TestReloadOnSignal(t *testing.T) {
	a := trainSetup(t)
	must(t, a.Init())
	_, err := a.ReloadTrainConfig() // baseline
	must(t, err)
	ch := make(chan os.Signal, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.reloadOn(ctx, ch); close(done) }()
	write(t, a.Root, ".saddle/config.toml", "[train]\nlint.cmd = \"make check\"\n")
	ch <- os.Interrupt
	deadline := time.Now().Add(5 * time.Second)
	for {
		evs, err := a.Store.Events(20)
		must(t, err)
		found := false
		for _, e := range evs {
			found = found || e.Kind == "config_reloaded"
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no config_reloaded event")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if a.Cfg.Train.Lint.Cmd != "make check" {
		t.Fatalf("lint = %+v", a.Cfg.Train.Lint)
	}
}

// The hand fix for a gate nested in the repo was [train] tmpdir, applied
// with SIGHUP, but the reload skipped it and the train kept .saddle/tmp.
func TestReloadTrainConfigAppliesTmpdir(t *testing.T) {
	a := trainSetup(t)
	must(t, a.Init())
	_, err := a.ReloadTrainConfig() // baseline
	must(t, err)
	dir := t.TempDir()
	write(t, a.Root, ".saddle/config.toml", "[train]\ntmpdir = \""+dir+"\"\n")
	if changed, err := a.ReloadTrainConfig(); err != nil || !changed || a.Cfg.Train.Tmpdir != dir || a.GateTmpdir() != dir {
		t.Fatalf("tmpdir edit: changed=%v err=%v tmpdir=%q gate=%q", changed, err, a.Cfg.Train.Tmpdir, a.GateTmpdir())
	}
}
