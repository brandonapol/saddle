//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

// usageBanner is Claude Code's session-limit screen from #180.
const usageBanner = `You've hit your session limit · resets 12:10pm (America/New_York)

● Usage limit reached · continuing automatically at 12:10pm · esc to cancel

  ⚠ Usage limit reached · limit resets 12:10pm
    Continuing automatically at 12:10pm · esc to cancel
`

// TestJourneyUsageLimitParksAndResumes (#180): a worker's pane sits on
// Claude Code's usage-limit banner. The engine marks it paused, tells the
// orchestrator once with the reset time and types nothing into the pane.
// When the banner clears the worker is running again with no second
// notice, and it finishes.
func TestJourneyUsageLimitParksAndResumes(t *testing.T) {
	w := world(t, Options{})
	reset, finish := filepath.Join(w.Scripts, "t1.reset"), filepath.Join(w.Scripts, "t1.finish")
	// The agent draws the banner on its own terminal, then waits for the
	// "reset", clears it the way Claude Code redraws and works on.
	park := "printf '%s' " + shq(usageBanner) + " > /dev/tty" +
		"; until [ -f " + shq(reset) + " ]; do sleep 0.2; done" +
		"; clear > /dev/tty; echo '⏺ Picking up where I left off' > /dev/tty" +
		"; until [ -f " + shq(finish) + " ]; do sleep 0.2; done"
	w.Spawn("t1", "Limited work", []string{"limited/**"},
		fa.Step{Run: park, Unhooked: true},
		fa.Write("limited/a.txt", "a\n"), fa.Commit("limited work"), fa.Done("limited work"))
	startEngine(w)

	w.WaitStatus("t1", "paused")
	r := w.MustSaddle("plugin", "wait", "--timeout", "30s")
	if !strings.Contains(r.Stdout, "t1 (Limited work) is parked on a Claude usage limit") ||
		!strings.Contains(r.Stdout, "12:10pm (America/New_York)") {
		t.Fatalf("plugin wait:\n%s", r.Stdout)
	}
	if got := w.MustSaddle("status").Stdout; !strings.Contains(got, "paused") {
		t.Fatalf("status while parked:\n%s", got)
	}
	if log := w.AgentLog("t1"); strings.Contains(log, "input") {
		t.Fatalf("something typed into the parked pane:\n%s", log)
	}

	must(t, os.WriteFile(reset, nil, 0o644))
	w.WaitStatus("t1", "running")
	r = w.MustSaddle("plugin", "wait", "--timeout", "6s")
	if !strings.Contains(r.Stdout, "No saddle events need you yet") {
		t.Fatalf("nagged after the reset:\n%s", r.Stdout)
	}

	must(t, os.WriteFile(finish, nil, 0o644))
	w.WaitTask("t1", "queued in the train", func(v mcpserver.TaskView) bool { return queued(v) })
}
