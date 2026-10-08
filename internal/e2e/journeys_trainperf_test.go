//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/mcpserver"
)

// gitShim puts a git on the world's PATH that logs every call's arguments,
// one line each, then runs the real git. It returns a func reading the log.
func gitShim(t *testing.T, w *World) func() []string {
	t.Helper()
	real, err := exec.LookPath("git")
	must(t, err)
	log := filepath.Join(w.Root, "git.log")
	script := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %s\nexec %s \"$@\"\n", shq(log), shq(real))
	must(t, os.WriteFile(filepath.Join(w.Bin, "git"), []byte(script), 0o755))
	t.Cleanup(func() { _ = os.Remove(filepath.Join(w.Bin, "git")) })
	return func() []string {
		b, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
}

// rebases counts the git calls in log that start a rebase.
func rebases(log []string) int {
	n := 0
	for _, l := range log {
		fs := strings.Fields(l)
		for i, f := range fs {
			if f == "rebase" && (i == 0 || fs[i-1] != "-c") {
				if i+1 >= len(fs) || (fs[i+1] != "--abort" && fs[i+1] != "--continue") {
					n++
				}
				break
			}
		}
	}
	return n
}

// TestJourneyLandRebasesEachQueuedTaskOnce (#268): landing N disjoint queued
// tasks used to auto-rebase every task still waiting after each landing,
// N + N(N-1)/2 rebases in all. Now each lands with exactly one rebase, its
// own on its turn.
func TestJourneyLandRebasesEachQueuedTaskOnce(t *testing.T) {
	const n = 6
	w := world(t, Options{Top: "concurrency = 8\n"})
	var ids []string
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("t%d", i)
		dir := fmt.Sprintf("d%d", i)
		ids = append(ids, id)
		w.Spawn(id, "Work "+dir, []string{dir + "/**"}, finished(dir, dir+"\n")...)
	}
	for _, id := range ids {
		w.WaitTask(id, "queued in the train", func(v mcpserver.TaskView) bool { return v.Status == "done" && v.Train == "queued" })
	}

	log := gitShim(t, w)
	r := w.MustSaddle("land")
	for _, id := range ids {
		if w.Task(id).Status != "landed" {
			t.Fatalf("%s didn't land: %s", id, r)
		}
	}
	if got := rebases(log()); got != n {
		t.Fatalf("land ran %d rebases for %d queued tasks, want %d", got, n, n)
	}
}
