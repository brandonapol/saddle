//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestJourneyFirstUpSetsUpTheRepo (#163): in a fresh repo with gh, tmux and
// claude ready, one `saddle up` runs init's local setup itself, then the
// doctor, and opens the TUI. No --skip-doctor, no saddle init first.
func TestJourneyFirstUpSetsUpTheRepo(t *testing.T) {
	t.Setenv("SADDLE_TRUST", "")
	w := world(t, Options{NoInit: true})

	cmd := shq(w.Bins.Saddle) + ` up --trust; echo "[saddle up exited $?]"; exec cat`
	must(t, w.Tmux.NewSession(tuiSession, max(260, len(w.Repo)+200), 45, w.Repo, cmd))
	u := &TUI{w: w, Target: tuiSession + ":0"}
	u.WaitScreen("orchestrator")

	if _, err := os.Stat(filepath.Join(w.Repo, ".saddle", "config.toml")); err != nil {
		t.Fatalf("first up wrote no config.toml: %v", err)
	}
	for _, h := range []string{"reference-transaction", "pre-push"} {
		if _, err := os.Stat(filepath.Join(w.Repo, ".git", "hooks", h)); err != nil {
			t.Errorf("first up didn't install %s: %v", h, err)
		}
	}
	w.Git(w.Repo, "check-ignore", "-q", ".saddle/")

	u.Quit()
	u.WaitScreen("saddle set up this repo: wrote .saddle/config.toml", "added /.saddle/ to .git/info/exclude", "[saddle up exited 0]")

	// Nothing the doctor checks fails after that one command.
	r := w.MustSaddle("doctor", "--json")
	for name, c := range doctorChecks(t, r.Stdout) {
		if c.Status == "fail" {
			t.Errorf("after first up, %s failed: %+v", name, c)
		}
	}
}

// TestJourneyDoctorFixHalfInit (#163): a repo where something created
// .saddle/state.db but saddle init never ran. doctor says so and groups the
// local fixes; doctor --fix refuses in an untrusted folder, then repairs
// everything once trusted, keeps an existing config.toml on a rerun, and
// shows the next steps.
func TestJourneyDoctorFixHalfInit(t *testing.T) {
	t.Setenv("SADDLE_TRUST", "")
	w := world(t, Options{NoInit: true})
	// Any app command creates state.db without running init.
	w.MustSaddle("status")
	if _, err := os.Stat(filepath.Join(w.Repo, ".saddle", "state.db")); err != nil {
		t.Fatalf("status didn't create state.db: %v", err)
	}

	r := w.Saddle("doctor", "--json")
	checks := groupedChecks(t, r.Stdout)
	if c := checks["config"]; c.Status != "warn" || c.Group != "fixable" || !strings.Contains(c.Detail, "half set up") {
		t.Errorf("half-initialized config = %+v", c)
	}
	for _, name := range []string{"ref guard hooks", ".saddle ignored"} {
		if c := checks[name]; c.Status != "fail" || c.Group != "fixable" || c.About == "" {
			t.Errorf("%s = %+v, want a fixable fail with an explanation", name, c)
		}
	}
	r = w.Saddle("doctor")
	if !strings.Contains(r.Stdout, "Saddle can fix these") || !strings.Contains(r.Stdout, "saddle doctor --fix") {
		t.Errorf("doctor table doesn't point at --fix:\n%s", r.Stdout)
	}

	// Untrusted: --fix writes nothing.
	r = w.Saddle("doctor", "--fix")
	if r.Code == 0 || !strings.Contains(r.Stderr, "trust") {
		t.Fatalf("untrusted doctor --fix: %s", r)
	}
	if _, err := os.Stat(filepath.Join(w.Repo, ".git", "hooks", "reference-transaction")); !os.IsNotExist(err) {
		t.Fatalf("untrusted doctor --fix installed a hook: %v", err)
	}

	r = w.MustSaddle("doctor", "--fix", "--trust")
	for _, want := range []string{"Fixed automatically", "ref guard hooks", ".saddle ignored", "Next steps", "saddle up"} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("doctor --fix output lacks %q:\n%s", want, r.Stdout)
		}
	}

	// A rerun keeps the owner's config.toml and has nothing to fix.
	w.WriteConfig(Options{})
	cfg := filepath.Join(w.Repo, ".saddle", "config.toml")
	before, err := os.ReadFile(cfg)
	must(t, err)
	r = w.MustSaddle("doctor", "--fix")
	if strings.Contains(r.Stdout, "Fixed automatically") {
		t.Errorf("second --fix fixed something:\n%s", r.Stdout)
	}
	if after, _ := os.ReadFile(cfg); string(after) != string(before) {
		t.Errorf("doctor --fix rewrote config.toml:\n%s", after)
	}
	r = w.MustSaddle("doctor", "--json")
	if c := groupedChecks(t, r.Stdout)["config"]; c.Status != "ok" || c.Group != "ok" {
		t.Errorf("config after fix = %+v", c)
	}
}

type groupedCheck struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
	About  string `json:"about"`
	Group  string `json:"group"`
}

// groupedChecks reads doctor --json with each check's group and explanation.
func groupedChecks(t *testing.T, out string) map[string]groupedCheck {
	t.Helper()
	var v struct {
		Checks []struct {
			Name string `json:"name"`
			groupedCheck
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, out)
	}
	m := map[string]groupedCheck{}
	for _, c := range v.Checks {
		m[c.Name] = c.groupedCheck
	}
	return m
}
