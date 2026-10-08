//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/e2e/fakegh"
)

const howdyLine = "Howdy, partner!"

// trustRepo answers saddle's trust prompt (#215) with yes, as a user does
// before a first plugin command or init can set the repo up.
func (w *World) trustRepo() {
	w.T.Helper()
	w.MustSaddle("trust", "--yes")
}

// marker is the plugin's onboarding marker, "" when missing.
func (w *World) marker() string {
	b, _ := os.ReadFile(filepath.Join(w.Repo, ".saddle", "plugin-onboarded"))
	return strings.TrimSpace(string(b))
}

// TestJourneyPluginOnboarding: the first plugin command in a repo that never
// ran saddle init greets the user, runs init and the doctor and goes on. It
// happens once per repo: later commands go straight to work. The plugin's
// hook, which runs in every project, never sets a repo up by itself.
func TestJourneyPluginOnboarding(t *testing.T) {
	w := world(t, Options{NoInit: true})

	// The hook runs on every tool call in every project: it stays out.
	if r := w.Exec(w.Repo, "sh", "-c", `echo '{"hook_event_name":"PostToolUse"}' | "$0" plugin hook`, w.Bins.Saddle); r.Code != 0 || r.Stdout != "" {
		t.Fatalf("plugin hook in an uninitialized repo: %s", r)
	}
	if _, err := os.Stat(filepath.Join(w.Repo, ".saddle")); err == nil {
		t.Fatal("the plugin hook created .saddle in a repo that never ran saddle init")
	}
	w.trustRepo()

	r := w.MustSaddle("plugin", "setup")
	for _, want := range []string{howdyLine, "First use of saddle in " + w.Repo, "STATUS", "CHECK", "Saddle is set up in " + w.Repo} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("first plugin setup lacks %q:\n%s", want, r.Stdout)
		}
	}
	// Claude Code captures the output: the banner is never colored.
	if strings.Contains(r.Stdout, "\x1b[") {
		t.Errorf("plugin setup printed escape codes:\n%q", r.Stdout)
	}
	if m := w.marker(); m != "ok" {
		t.Fatalf("onboarding marker = %q, want ok", m)
	}
	if r := w.Saddle("doctor"); r.Code != 0 {
		t.Fatalf("doctor after plugin onboarding: %s", r)
	}

	// Once per repo: no banner, no doctor table.
	for _, cmd := range [][]string{{"plugin", "setup"}, {"plugin", "brief"}} {
		r := w.MustSaddle(cmd...)
		if strings.Contains(r.Stdout, howdyLine) || strings.Contains(r.Stdout, "First use") || strings.Contains(r.Stdout, "STATUS") {
			t.Errorf("saddle %s onboarded again:\n%s", strings.Join(cmd, " "), r.Stdout)
		}
	}
	if r := w.MustSaddle("plugin", "brief"); !strings.Contains(r.Stdout, "# Saddle orchestrator") || !strings.Contains(r.Stdout, "## Right now") {
		t.Fatalf("plugin brief:\n%s", r.Stdout)
	}
}

// TestJourneyPluginOnboardingBlockedByFailingDoctor: when a doctor check
// fails on first use, the command stops with the table and says to fix it;
// each later use reruns only the doctor (no second greeting) until it
// passes, then carries on.
func TestJourneyPluginOnboardingBlockedByFailingDoctor(t *testing.T) {
	w := world(t, Options{NoInit: true})
	must(t, w.GH.Update(func(s *fakegh.State) error {
		s.Repo.AllowMerge = true // stacked PRs need linear history: a hard fail
		return nil
	}))
	w.trustRepo()

	r := w.MustSaddle("plugin", "brief")
	for _, want := range []string{howdyLine, "First use of saddle", "merge commits are allowed", "saddle doctor found failing checks"} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("blocked first use lacks %q:\n%s", want, r.Stdout)
		}
	}
	if strings.Contains(r.Stdout, "# Saddle orchestrator") {
		t.Fatalf("the brief was printed past a failing doctor:\n%s", r.Stdout)
	}
	if m := w.marker(); m != "pending" {
		t.Fatalf("marker after a failing doctor = %q, want pending", m)
	}

	// Still failing: the doctor reruns, the greeting doesn't.
	r = w.MustSaddle("plugin", "brief")
	if strings.Contains(r.Stdout, howdyLine) || !strings.Contains(r.Stdout, "saddle doctor found failing checks") {
		t.Fatalf("second blocked use:\n%s", r.Stdout)
	}

	must(t, w.GH.Update(func(s *fakegh.State) error { s.Repo.AllowMerge = false; return nil }))
	r = w.MustSaddle("plugin", "brief")
	if strings.Contains(r.Stdout, howdyLine) || strings.Contains(r.Stdout, "found failing checks") || !strings.Contains(r.Stdout, "# Saddle orchestrator") {
		t.Fatalf("use after fixing the doctor:\n%s", r.Stdout)
	}
	if m := w.marker(); m != "ok" {
		t.Fatalf("marker after the doctor passed = %q, want ok", m)
	}
	if r := w.MustSaddle("plugin", "brief"); strings.Contains(r.Stdout, "STATUS") {
		t.Fatalf("doctor ran again after onboarding finished:\n%s", r.Stdout)
	}
}

// TestJourneyInitBannerOnlyOnTTY: saddle init greets a person at a terminal,
// in color, and prints no banner into a pipe or with --quiet.
func TestJourneyInitBannerOnlyOnTTY(t *testing.T) {
	w := world(t, Options{NoInit: true})
	w.trustRepo()
	if r := w.MustSaddle("init"); strings.Contains(r.Stdout, howdyLine) {
		t.Fatalf("saddle init printed the banner into a pipe:\n%s", r.Stdout)
	}

	w2 := world(t, Options{NoInit: true})
	w2.trustRepo()
	must(t, w2.Tmux.NewSession("init", 100, 30, w2.Repo, shq(w2.Bins.Saddle)+` init; echo "[init exited $?]"; exec cat`))
	Eventually(t, "saddle init on a terminal to greet", func() error {
		s, err := w2.Tmux.Capture("init:0")
		if err != nil {
			return err
		}
		if !strings.Contains(s, "[init exited 0]") || !strings.Contains(s, howdyLine) {
			return errorf("screen:\n%s", s)
		}
		return nil
	})
	esc, err := w2.Tmux.Run("capture-pane", "-p", "-e", "-t", "init:0")
	must(t, err)
	if !strings.Contains(esc, "\x1b[33m") {
		t.Fatalf("banner on a terminal isn't colored:\n%q", esc)
	}

	w3 := world(t, Options{NoInit: true})
	w3.trustRepo()
	must(t, w3.Tmux.NewSession("init", 100, 30, w3.Repo, shq(w3.Bins.Saddle)+` init --quiet; echo "[init exited $?]"; exec cat`))
	Eventually(t, "saddle init --quiet to finish", func() error {
		s, err := w3.Tmux.Capture("init:0")
		if err != nil {
			return err
		}
		if !strings.Contains(s, "[init exited 0]") {
			return errorf("screen:\n%s", s)
		}
		return nil
	})
	if s, _ := w3.Tmux.Capture("init:0"); strings.Contains(s, howdyLine) {
		t.Fatalf("saddle init --quiet greeted:\n%s", s)
	}
}
