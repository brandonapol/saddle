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

// TestJourneyAdapterRotation (#321): a claude worker hits its usage limit.
// The engine parks it and marks claude out of quota, the orchestrator hears
// once who ran out, who takes over and when claude resets, and the next
// spawn without an adapter runs grok. Status shows claude out of quota.
// When the banner clears claude is back, the parked worker finishes, and
// the next spawn runs claude again.
func TestJourneyAdapterRotation(t *testing.T) {
	w := world(t, Options{})
	args := filepath.Join(w.Root, "grok-args")
	fake := "#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" > " + args + "\nexec sleep 600\n"
	must(t, os.WriteFile(filepath.Join(w.Bin, "grok"), []byte(fake), 0o755))
	must(t, os.MkdirAll(filepath.Join(w.Home, ".grok"), 0o755))
	must(t, os.WriteFile(filepath.Join(w.Home, ".grok", "auth.json"), []byte("{}"), 0o600))

	reset, finish := filepath.Join(w.Scripts, "t1.reset"), filepath.Join(w.Scripts, "t1.finish")
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
	if !strings.Contains(r.Stdout, "Adapter claude is out of quota until 12:10pm") || !strings.Contains(r.Stdout, "go to grok") {
		t.Fatalf("plugin wait:\n%s", r.Stdout)
	}
	if n := strings.Count(r.Stdout, "out of quota until"); n != 1 {
		t.Fatalf("rotation told %d times:\n%s", n, r.Stdout)
	}
	out := map[string]string{}
	for _, s := range w.Status().Adapters {
		out[s.Name] = s.OutOfQuotaUntil
	}
	if !strings.Contains(out["claude"], "12:10pm") || out["grok"] != "" {
		t.Fatalf("status adapters while claude is out: %+v", w.Status().Adapters)
	}

	if o, isErr := w.MCP("t0", "spawn", map[string]any{"title": "Rotated work", "prompt": "fix the meter"}); isErr {
		t.Fatalf("spawn while claude is out: %s", o)
	}
	waitFile(t, args)
	rotated := taskTitled(t, w, "Rotated work")
	if b, err := os.ReadFile(filepath.Join(w.Repo, ".saddle", "run", rotated.ID, "adapter")); err != nil || strings.TrimSpace(string(b)) != "grok" {
		t.Fatalf("rotated spawn adapter = %q %v", b, err)
	}

	must(t, os.WriteFile(reset, nil, 0o644))
	w.WaitStatus("t1", "running")
	Eventually(t, "claude back", func() error {
		for _, s := range w.Status().Adapters {
			if s.Name == "claude" && s.OutOfQuotaUntil != "" {
				return errorf("claude still out until %s", s.OutOfQuotaUntil)
			}
		}
		return nil
	})
	must(t, os.WriteFile(finish, nil, 0o644))
	w.WaitTask("t1", "queued in the train", func(v mcpserver.TaskView) bool { return queued(v) })

	if o, isErr := w.MCP("t0", "spawn", map[string]any{"title": "Back on claude", "prompt": "x"}); isErr {
		t.Fatalf("spawn after the reset: %s", o)
	}
	back := taskTitled(t, w, "Back on claude")
	if b, err := os.ReadFile(filepath.Join(w.Repo, ".saddle", "run", back.ID, "adapter")); err != nil || strings.TrimSpace(string(b)) != "claude" {
		t.Fatalf("spawn after the reset adapter = %q %v", b, err)
	}
}

func taskTitled(t *testing.T, w *World, title string) mcpserver.TaskView {
	t.Helper()
	for _, v := range w.Status().Tasks {
		if v.Title == title {
			return v
		}
	}
	t.Fatalf("no task %q", title)
	return mcpserver.TaskView{}
}
