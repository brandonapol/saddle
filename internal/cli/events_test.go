package cli

import (
	"strings"
	"testing"
)

// #908: doctor sends you to the event log for a watcher's panic, so there
// is a command that reads it, filtered by kind and task, with the stack a
// watcher_panic event holds.
func TestEventsCommand(t *testing.T) {
	a := automergeRepo(t)
	t.Chdir(a.Root)
	a.Store.Event("", "watcher_panic", "remote: boom\ngoroutine 1 [running]:\nmain.go:12")
	a.Store.Event("t1", "landed", "t1 landed")
	a.Store.Event("t2", "restack", "moved")

	out := run(t, "events")
	for _, want := range []string{"watcher_panic", "landed", "restack"} {
		if !strings.Contains(out, want) {
			t.Fatalf("events lacks %q:\n%s", want, out)
		}
	}
	out = run(t, "events", "--kind", "watcher_panic")
	if !strings.Contains(out, "goroutine 1 [running]:") || strings.Contains(out, "landed") {
		t.Fatalf("--kind watcher_panic:\n%s", out)
	}
	out = run(t, "events", "--task", "t2")
	if !strings.Contains(out, "moved") || strings.Contains(out, "t1 landed") {
		t.Fatalf("--task t2:\n%s", out)
	}
	if out := run(t, "events", "--kind", "nope"); !strings.Contains(out, "No events") {
		t.Fatalf("no match:\n%s", out)
	}
}
