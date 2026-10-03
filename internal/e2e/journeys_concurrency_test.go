//go:build e2e

package e2e

import (
	"strings"
	"testing"

	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

// TestJourneyConcurrencyCap (#176): lowering the bots limit at runtime stops
// nothing that runs; spawn (CLI and MCP) refuses until a task lands and frees
// a slot, then works again.
func TestJourneyConcurrencyCap(t *testing.T) {
	w := world(t, Options{})
	w.Spawn("t1", "Alpha work", []string{"alpha/**"}, append([]fa.Step{fa.Wait("proceed-now")}, finished("alpha", "alpha\n")...)...)
	w.Spawn("t2", "Beta work", []string{"beta/**"}, fa.Wait("never-comes"))
	// Waiting on input reads as idle; both hold a slot either way.
	live := func(v mcpserver.TaskView) bool { return v.Status == "running" || v.Status == "idle" }
	for _, id := range []string{"t1", "t2"} {
		w.WaitTask(id, "live", live)
	}

	if r := w.MustSaddle("concurrency", "2"); !strings.Contains(r.Stdout, "bots: 2 running, limit 2 (runtime)") {
		t.Fatalf("saddle concurrency 2: %s", r)
	}
	for _, id := range []string{"t1", "t2"} {
		if v := w.Task(id); !live(v) {
			t.Fatalf("%s is %s after lowering the cap; nothing should stop", id, v.Status)
		}
	}

	if out, isErr := w.MCP("t0", "spawn", map[string]any{"title": "Gamma work", "prompt": "x", "claims": []string{"gamma/**"}}); !isErr ||
		!strings.Contains(out, "concurrency cap") || !strings.Contains(out, "limit 2") {
		t.Fatalf("MCP spawn over the cap: err=%v %s", isErr, out)
	}
	if r := w.Saddle("spawn", "--id", "t3", "Gamma work", "x"); r.Code == 0 || !strings.Contains(r.String(), "saddle concurrency") {
		t.Fatalf("CLI spawn over the cap: %s", r)
	}

	w.MustSaddle("message", "t1", "proceed-now")
	w.WaitTask("t1", "queued in the train", func(v mcpserver.TaskView) bool { return v.Status == "done" && v.Train == "queued" })
	w.MustSaddle("land")
	w.WaitStatus("t1", "landed")

	w.Spawn("t3", "Gamma work", []string{"gamma/**"}, finished("gamma", "gamma\n")...)
	w.WaitTask("t3", "queued in the train", func(v mcpserver.TaskView) bool { return v.Status == "done" && v.Train == "queued" })
	if r := w.MustSaddle("concurrency", "--json"); !strings.Contains(r.Stdout, `"limit": 2`) || !strings.Contains(r.Stdout, `"source": "runtime"`) {
		t.Fatalf("override didn't stick: %s", r)
	}
}
