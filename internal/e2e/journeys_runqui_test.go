//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
)

// heavyRunsOf decodes the heavy_runs of saddle status --json or the MCP
// status tool's output.
func heavyRunsOf(t *testing.T, js string) *app.HeavyRuns {
	t.Helper()
	var st struct {
		HeavyRuns *app.HeavyRuns `json:"heavy_runs"`
	}
	if err := json.Unmarshal([]byte(js), &st); err != nil {
		t.Fatalf("%v\n%s", err, js)
	}
	return st.HeavyRuns
}

// checkHeavyPositions fails unless go-test has t83 holding (overdue) and
// otherrepo/t84 then t85 waiting at positions 1 and 2.
func checkHeavyPositions(t *testing.T, where string, v *app.HeavyRuns) {
	t.Helper()
	if v == nil || len(v.Classes) != 1 {
		t.Fatalf("%s: heavy_runs %+v", where, v)
	}
	c := v.Classes[0]
	if c.Class != "go-test" || c.Slots != 1 || len(c.Holders) != 1 || len(c.Waiters) != 2 {
		t.Fatalf("%s: class %+v", where, c)
	}
	h, w1, w2 := c.Holders[0], c.Waiters[0], c.Waiters[1]
	if h.Who(v.Repo) != "t83" || !h.Overdue || w1.Position != 1 || w1.Who(v.Repo) != "otherrepo/t84" || w2.Position != 2 || w2.Who(v.Repo) != "t85" {
		t.Fatalf("%s: holder %+v, waiters %+v %+v", where, h, w1, w2)
	}
}

// TestJourneyHeavyRunsQueueVisible (#242): t83 holds the only go-test slot
// past its max_run while t84 from another repo and then t85 wait. saddle
// status, its --json, the MCP status tool and the TUI all show the same
// holder and positions. In the TUI's runs view x asks before killing, n
// keeps the lease, and x then y kills it: the next waiter gets the slot.
func TestJourneyHeavyRunsQueueVisible(t *testing.T) {
	w := world(t, Options{})
	must(t, os.MkdirAll(filepath.Join(w.Home, ".config", "saddle"), 0o755))
	must(t, os.WriteFile(filepath.Join(w.Home, ".config", "saddle", "runq.toml"),
		[]byte("mode = \"enforce\"\nmax_load_per_cpu = 0\nmax_cpu_pressure = 0\nheartbeat = \"1s\"\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(w.Repo, ".saddle", "runq.toml"),
		[]byte("[classes.go-test]\nslots = 1\nmax_run = \"1s\"\n"), 0o644))
	other := filepath.Join(t.TempDir(), "otherrepo")
	must(t, os.MkdirAll(filepath.Join(other, ".saddle"), 0o755))
	must(t, os.WriteFile(filepath.Join(other, ".saddle", "config.toml"), nil, 0o644))

	dir := t.TempDir()
	run := func(task, in string) {
		started := filepath.Join(dir, task)
		cmd := exec.Command(w.Bins.Saddle, "run", "--class", "go-test", "--", "sh", "-c",
			"touch "+shq(started)+"; while [ ! -e "+shq(filepath.Join(dir, "release"))+" ]; do sleep 0.1; done")
		cmd.Env, cmd.Dir = append(w.Env(), "SADDLE_TASK="+task), in
		must(t, cmd.Start())
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	}
	queued := func(label string) func() bool {
		return func() bool { return strings.Contains(w.MustSaddle("runq", "status").Stdout, label) }
	}
	run("t83", w.Repo)
	waitFor(t, "t83 holds go-test", func() bool { _, err := os.Stat(filepath.Join(dir, "t83")); return err == nil })
	run("t84", other)
	waitFor(t, "t84 queued", queued("#1 t84"))
	run("t85", w.Repo)
	waitFor(t, "t85 queued", queued("#2 t85"))

	var text string
	waitFor(t, "t83 overdue past max_run 1s", func() bool {
		text = w.MustSaddle("status").Stdout
		return strings.Contains(text, "overdue")
	})
	_, sect, ok := strings.Cut(text, "Heavy runs (enforce)\n")
	if !ok {
		t.Fatalf("saddle status has no Heavy runs section:\n%s", text)
	}
	for _, want := range []string{"go-test: 1/1 slots busy, 2 waiting", "▸ t83", "#1 otherrepo/t84", "#2 t85"} {
		if !strings.Contains(sect, want) {
			t.Fatalf("saddle status lacks %q:\n%s", want, text)
		}
	}
	checkHeavyPositions(t, "saddle status --json", heavyRunsOf(t, w.MustSaddle("status", "--json").Stdout))
	checkHeavyPositions(t, "mcp status", heavyRunsOf(t, w.MustMCP("t0", "status", nil)))

	u := w.StartTUI(160, 40)
	u.WaitScreen("go-test ▸t83", "· 2 waiting")
	u.Keys("M-4")
	u.WaitScreen("HEAVY RUNS · enforce", "▸ t83", "#1 otherrepo/t84", "#2 t85")
	if err := u.Fits(160); err != nil {
		t.Fatal(err)
	}
	u.Keys("x")
	u.WaitScreen("Kill lease", "(t83, ")
	u.Keys("n")
	u.WaitScreen("kept lease")
	u.Keys("x")
	u.WaitScreen("Kill lease")
	u.Keys("y")
	u.WaitScreen("killed lease")
	waitFor(t, "t84 gets the slot after the kill", func() bool { _, err := os.Stat(filepath.Join(dir, "t84")); return err == nil })
	u.WaitScreen("#1 t85")
	u.Quit()
}
