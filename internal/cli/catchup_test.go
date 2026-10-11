package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
)

func runCatchup(t *testing.T, a *app.App, args ...string) (string, error) {
	t.Helper()
	c := catchupCmd()
	t.Chdir(a.Root)
	t.Setenv("SADDLE_ROOT", a.Root)
	if err := os.WriteFile(a.Root+"/.saddle/config.toml", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	c.SetOut(&out)
	c.SetArgs(args)
	err := c.Execute()
	return out.String(), err
}

// saddle catchup lists what waits on the owner first and ends with the
// state now; --since takes a duration (#403).
func TestCatchupCommand(t *testing.T) {
	a := storeApp(t)
	addTask(t, a, store.Task{ID: "t1", Title: "x", Status: store.Landed})
	addTask(t, a, store.Task{ID: "t2", Title: "y", Status: store.NeedsYou})
	a.Store.Event("t1", "landed", "")
	a.Store.Event("t2", "notification", "allow git push?")
	out, err := runCatchup(t, a, "--since", "1h")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 || lines[1] != "- Waiting on you: t2 asks: allow git push?" || lines[2] != "- Done: 1 landed (t1)." || !strings.HasPrefix(lines[3], "- Now: Needs you") {
		t.Fatalf("catchup output:\n%s", out)
	}
	if _, err := runCatchup(t, a, "--since", "yesterday"); err == nil {
		t.Fatal("a bad --since was accepted")
	}
}

// saddle status leads with the headline (#403).
func TestStatusLeadsWithHeadline(t *testing.T) {
	a := storeApp(t)
	addTask(t, a, store.Task{ID: "t1", Title: "x", Status: store.Running})
	out := runStatus(t, a)
	if first, _, _ := strings.Cut(out, "\n"); first != "Working: 1 agent running. Nothing needs you." {
		t.Fatalf("status first line = %q\n%s", first, out)
	}
}
