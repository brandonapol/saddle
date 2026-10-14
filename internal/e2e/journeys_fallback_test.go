//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

// TestJourneyModelFallbackChain (#483): [fallback] gives claude:opus its
// own chain, so opus is tracked out of quota on its own. An opus worker
// hits its limit: only claude:opus goes out, the running task's branch is
// handed to grok (the chain's entry), which finishes it, and the owner
// hears once who ran out, who took over and the reset. A new opus spawn
// lands on grok while claude's other models (sonnet) stay usable, and
// status shows the chain and opus's out_of_quota_until. Once the reset
// passes, opus spawns run on claude again.
func TestJourneyModelFallbackChain(t *testing.T) {
	if Fork(t) {
		return
	}
	w := world(t, Options{Tables: "[fallback]\n\"claude:opus\" = [\"grok\"]\n"})
	fakeGrok(t, w)
	banner := "You've hit your session limit · resets in 3 hours\n\n● Usage limit reached · esc to cancel\n"
	must(t, fa.Script{Steps: []fa.Step{
		fa.Write("limited/a.txt", "a\n"), fa.Commit("opus's half"),
		{Run: "printf '%s' " + shq(banner) + " > /dev/tty; exec sleep 600", Unhooked: true},
	}}.Save(w.Scripts, "t1"))
	w.MustSaddle("spawn", "--id", "t1", "-m", "opus", "-c", "limited/**", "Limited work", "Prompt for Limited work")
	startEngine(w)

	args := waitFile(t, filepath.Join(w.Root, "grok-args"))
	for _, want := range []string{"You are taking over this task", "ran out of quota on claude:opus", "Prompt for Limited work"} {
		if !strings.Contains(args, want) {
			t.Fatalf("grok's prompt lacks %q:\n%s", want, args)
		}
	}
	w.WaitTask("t1", "queued in the train", func(v mcpserver.TaskView) bool { return queued(v) })
	r := w.MustSaddle("plugin", "wait", "--timeout", "30s")
	if n := strings.Count(r.Stdout, "Model claude:opus is out of quota until"); n != 1 || !strings.Contains(r.Stdout, "go to grok") ||
		!strings.Contains(r.Stdout, "other claude models stay usable") {
		t.Fatalf("owner told %d times:\n%s", n, r.Stdout)
	}

	st := w.Status()
	for _, s := range st.Adapters {
		if s.Name == "claude" && (s.OutOfQuotaUntil != "" || s.ModelsOutOfQuota["opus"] == "") {
			t.Fatalf("claude in status: %+v", s)
		}
	}
	if len(st.Fallback) != 1 || st.Fallback[0].From != "claude:opus" || st.Fallback[0].OutOfQuotaUntil == "" || st.Fallback[0].Now != "grok" {
		t.Fatalf("status fallback: %+v", st.Fallback)
	}
	if out := w.MustSaddle("status").Stdout; !strings.Contains(out, "fallback: claude:opus -> grok; out until") || !strings.Contains(out, "claude:opus out of quota until") {
		t.Fatalf("saddle status:\n%s", out)
	}

	adapterOf := func(title string) (string, string) {
		v := taskTitled(t, w, title)
		b, err := os.ReadFile(filepath.Join(w.Repo, ".saddle", "run", v.ID, "adapter"))
		must(t, err)
		return strings.TrimSpace(string(b)), v.Model
	}
	if o, isErr := w.MCP("t0", "spawn", map[string]any{"title": "More opus work", "prompt": "x", "model": "opus"}); isErr {
		t.Fatalf("opus spawn while opus is out: %s", o)
	}
	if ad, _ := adapterOf("More opus work"); ad != "grok" {
		t.Fatalf("opus spawn while opus is out ran %s, want grok", ad)
	}
	if o, isErr := w.MCP("t0", "spawn", map[string]any{"title": "Sonnet work", "prompt": "x", "model": "sonnet"}); isErr {
		t.Fatalf("sonnet spawn: %s", o)
	}
	if ad, m := adapterOf("Sonnet work"); ad != "claude" || m != "sonnet" {
		t.Fatalf("sonnet spawn ran %s model %q, want claude sonnet", ad, m)
	}

	// The reset passes.
	p := filepath.Join(w.Repo, ".saddle", "adapters-exhausted.json")
	var ex map[string]map[string]any
	b, err := os.ReadFile(p)
	must(t, err)
	must(t, json.Unmarshal(b, &ex))
	if _, ok := ex["claude:opus"]; !ok || len(ex) != 1 {
		t.Fatalf("exhausted file: %s", b)
	}
	ex["claude:opus"]["until"] = time.Now().Add(-time.Minute).Format(time.RFC3339Nano)
	b, err = json.Marshal(ex)
	must(t, err)
	must(t, os.WriteFile(p, b, 0o644))

	if o, isErr := w.MCP("t0", "spawn", map[string]any{"title": "Opus again", "prompt": "x", "model": "opus"}); isErr {
		t.Fatalf("opus spawn after the reset: %s", o)
	}
	if ad, m := adapterOf("Opus again"); ad != "claude" || m != "opus" {
		t.Fatalf("opus spawn after the reset ran %s model %q, want claude opus", ad, m)
	}
	for _, s := range w.Status().Adapters {
		if len(s.ModelsOutOfQuota) > 0 {
			t.Fatalf("%s still has models out: %v", s.Name, s.ModelsOutOfQuota)
		}
	}
}
