//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/e2e/fakeagent"
)

// Skills from the TUI chat (#255): typing / lists the skills the
// orchestrator's session reports, tab completes one, and sending it reaches
// the orchestrator as typed, whose reply streams into the chat. /help stays
// in the TUI. saddle doctor lists what the orchestrator sees.
func TestJourneySkillsFromChat(t *testing.T) {
	w := world(t, Options{})
	skill := filepath.Join(w.Home, ".claude", "skills", "greet")
	must(t, os.MkdirAll(skill, 0o755))
	must(t, os.WriteFile(filepath.Join(skill, "SKILL.md"),
		[]byte("---\nname: greet\ndescription: Say hello to someone\n---\nGreet $ARGUMENTS.\n"), 0o644))

	r := w.Saddle("doctor", "--json")
	if c := doctorChecks(t, r.Stdout)["orchestrator skills"]; c.Status != "ok" || !strings.Contains(c.Detail, "/greet") {
		t.Fatalf("doctor skills check = %+v", c)
	}

	u := w.StartTUI(140, 40)
	// The session lists its commands before any message is sent.
	u.Type("/")
	u.WaitScreen("/greet", "Say hello to someone", "/help")
	u.Type("gr")
	u.Keys("Tab")
	u.Type("Ada")
	u.Keys("Enter")
	u.WaitScreen("fake skill greet ran with: Ada")
	Eventually(t, "the orchestrator to get /greet as typed", func() error {
		b, _ := os.ReadFile(fakeagent.OrchestratorLog(w.Scripts))
		if !strings.Contains(string(b), "user: /greet Ada") {
			return errorf("orchestrator log:\n%s", b)
		}
		return nil
	})

	// The TUI's own command opens help and never reaches the orchestrator.
	u.Type("/help")
	u.Keys("Enter")
	u.WaitScreen("skills & commands")
	if b, _ := os.ReadFile(fakeagent.OrchestratorLog(w.Scripts)); strings.Contains(string(b), "/help") {
		t.Fatalf("/help reached the orchestrator:\n%s", b)
	}
	u.Keys("Escape")
	u.WaitGone("skills & commands")
	u.Quit()
}
