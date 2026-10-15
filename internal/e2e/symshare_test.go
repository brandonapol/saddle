//go:build e2e

package e2e

import (
	"strings"
	"testing"

	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

const shareGo = "package pkg\n\nfunc Foo() int {\n\treturn 1\n}\n\nfunc M1() {}\n\nfunc M2() {}\n\nfunc M3() {}\n\n" +
	"func Bar() int {\n\treturn 3\n}\n"

// TestJourneySymbolShareLandsBoth (#591): in symbol mode, t1 claims
// pkg/x.go and changes Foo; t2, which doesn't claim it, changes Bar in the
// same file. The write guard lets t2 through, and the train lands both.
func TestJourneySymbolShareLandsBoth(t *testing.T) {
	if Fork(t) {
		return
	}
	opts := Options{Tables: "[claims]\ngranularity = \"symbol\"\n"}
	w := world(t, Options{NoInit: true})
	w.WriteFile("pkg/x.go", shareGo)
	w.Git(w.Repo, "add", "-A")
	w.Git(w.Repo, "commit", "-q", "-m", "add pkg")
	w.Git(w.Repo, "push", "-q", "origin", "main")
	w.MustSaddle("init", "-q", "--trust")
	w.WriteConfig(opts)

	foo := strings.Replace(shareGo, "return 1", "return 10", 1)
	bar := strings.Replace(shareGo, "return 3", "return 30", 1)
	queued := func(v mcpserver.TaskView) bool { return v.Status == "done" && v.Train == "queued" }
	w.Spawn("t1", "Foo work", []string{"pkg/x.go"},
		fa.Write("pkg/x.go", foo), fa.Commit("change Foo"), fa.Done("Changes Foo."))
	w.WaitTask("t1", "queued in the train", queued)
	w.Spawn("t2", "Bar work", []string{"other/**"},
		fa.Write("pkg/x.go", bar), fa.Commit("change Bar"), fa.Done("Changes Bar."))
	w.WaitTask("t2", "queued in the train", queued)
	if log := w.AgentLog("t2"); strings.Contains(log, "denied") {
		t.Fatalf("t2's write to t1's file was denied:\n%s", log)
	}

	r := w.MustSaddle("land")
	for _, id := range []string{"t1", "t2"} {
		if w.Task(id).Status != "landed" {
			t.Fatalf("%s didn't land: %s", id, r)
		}
	}
	got := w.Git(w.Repo, "show", "saddle/integration:pkg/x.go")
	if !strings.Contains(got, "return 10") || !strings.Contains(got, "return 30") {
		t.Fatalf("integration pkg/x.go lacks one side:\n%s", got)
	}
}
