package app

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/runq"
)

// queueWaiter queues label from repo in class until the test ends, and
// returns once status shows it waiting.
func queueWaiter(t *testing.T, a *App, q *runq.Queue, class, label, repo string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	gone := make(chan struct{})
	t.Cleanup(func() { cancel(); <-gone })
	go func() {
		defer close(gone)
		_, _ = q.Acquire(ctx, runq.Request{Class: class, Prio: runq.PrioWorker, Label: label, Repo: repo, Cmd: "go test ./..."})
	}()
	waitHeavy(t, label+" queued", func() bool {
		v, err := a.HeavyRuns()
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range v.Classes {
			for _, w := range c.Waiters {
				if w.Label == label {
					return true
				}
			}
		}
		return false
	})
}

func waitHeavy(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// heavyClass returns class from v, failing when it is missing.
func heavyClass(t *testing.T, v HeavyRuns, class string) HeavyClass {
	t.Helper()
	for _, c := range v.Classes {
		if c.Class == class {
			return c
		}
	}
	t.Fatalf("no class %s in %+v", class, v)
	return HeavyClass{}
}

// TestHeavyRunsView: one holder in this repo and two waiters, one from
// another repo and one with no task. The view maps each label to its task
// and repo, keeps the queue's positions, and marks the holder overdue once
// it runs past max_run (#242).
func TestHeavyRunsView(t *testing.T) {
	a, _ := setup(t)
	db := heavyTest(t, a, "mode = \"enforce\"\nmax_load_per_cpu = 0\nmax_cpu_pressure = 0\nheartbeat = \"200ms\"\n[classes.go-test]\nslots = 1\nmax_run = \"1ms\"\n")
	repo := runq.RepoLabel(a.Root)
	q := openQueue(t, db)
	h, err := q.Acquire(context.Background(), runq.Request{Class: "go-test", Label: "t83", Repo: repo, Cmd: "make check"})
	must(t, err)
	t.Cleanup(func() { _ = h.Release() })
	queueWaiter(t, a, q, "go-test", "t84", "other")
	queueWaiter(t, a, q, "go-test", "pid 4242", repo)

	v, err := a.HeavyRuns()
	must(t, err)
	if v.Mode != "enforce" || v.Repo != repo {
		t.Fatalf("view %+v", v)
	}
	c := heavyClass(t, v, "go-test")
	if c.Slots != 1 || c.MaxRunMS != 1 || len(c.Holders) != 1 || len(c.Waiters) != 2 {
		t.Fatalf("class %+v", c)
	}
	ho := c.Holders[0]
	if ho.Task != "t83" || ho.Repo != repo || ho.Cmd != "make check" || !ho.Overdue || ho.Who(repo) != "t83" || ho.Lease == "" {
		t.Fatalf("holder %+v", ho)
	}
	w1, w2 := c.Waiters[0], c.Waiters[1]
	if w1.Position != 1 || w1.Task != "t84" || w1.Repo != "other" || w1.Who(repo) != "other/t84" || w1.Overdue {
		t.Fatalf("first waiter %+v", w1)
	}
	if w2.Position != 2 || w2.Task != "" || w2.Who(repo) != "pid 4242" {
		t.Fatalf("second waiter %+v", w2)
	}
	if !v.Busy() {
		t.Fatal("a queue with a holder should be busy")
	}
	if got := c.Segment(repo); !strings.HasPrefix(got, "go-test ▸t83 ") || !strings.HasSuffix(got, "s! · 2 waiting") {
		t.Fatalf("segment %q", got)
	}
}

// TestHeavyRunCarriesTaskAndRepo: saddle run labels its lease with
// SADDLE_TASK and the repo it runs in, so other repos' status can tell
// whose run it is.
func TestHeavyRunCarriesTaskAndRepo(t *testing.T) {
	a, _ := setup(t)
	db := heavyTest(t, a, "mode = \"enforce\"\nmax_load_per_cpu = 0\nmax_cpu_pressure = 0\nheartbeat = \"200ms\"\n[classes.go-test]\nslots = 1\n")
	t.Setenv("SADDLE_TASK", "t7")
	q := openQueue(t, db)
	h, err := q.Acquire(context.Background(), runq.Request{Class: "go-test", Label: "t83", Cmd: "make check"})
	must(t, err)
	done := make(chan error, 1)
	go func() { done <- a.RunHeavy(context.Background(), "go-test", runq.PrioWorker, exec.Command("true")) }()
	var w HeavyEntry
	waitHeavy(t, "t7 queued", func() bool {
		v, err := a.HeavyRuns()
		must(t, err)
		ws := heavyClass(t, v, "go-test").Waiters
		if len(ws) == 1 {
			w = ws[0]
		}
		return len(ws) == 1
	})
	if w.Task != "t7" || w.Repo != runq.RepoLabel(a.Root) {
		t.Fatalf("waiter %+v", w)
	}
	must(t, h.Release())
	must(t, <-done)
}

// TestKillHeavyLease: the TUI's kill key removes a lease by token.
func TestKillHeavyLease(t *testing.T) {
	a, _ := setup(t)
	db := heavyTest(t, a, "mode = \"enforce\"\nmax_load_per_cpu = 0\nmax_cpu_pressure = 0\n[classes.go-test]\nslots = 1\n")
	q := openQueue(t, db)
	h, err := q.Acquire(context.Background(), runq.Request{Class: "go-test", Label: "t83", Cmd: "make check"})
	must(t, err)
	must(t, a.KillHeavyLease(h.Token()))
	select {
	case <-h.Lost():
	case <-time.After(10 * time.Second):
		t.Fatal("the killed holder never saw its lease lost")
	}
	v, err := a.HeavyRuns()
	must(t, err)
	if c := heavyClass(t, v, "go-test"); len(c.Holders) != 0 {
		t.Fatalf("holders after kill %+v", c.Holders)
	}
	if err := a.KillHeavyLease("nope"); err == nil {
		t.Fatal("killing an unknown lease should fail")
	}
}

// TestNarratorHeavyLeases: the narrator sees each lease with its class,
// who it is, the class's max_run and, for waiters, who holds the class.
func TestNarratorHeavyLeases(t *testing.T) {
	a, _ := setup(t)
	db := heavyTest(t, a, "mode = \"enforce\"\nmax_load_per_cpu = 0\nmax_cpu_pressure = 0\nheartbeat = \"200ms\"\n[classes.go-test]\nslots = 1\nmax_run = \"1ms\"\n")
	q := openQueue(t, db)
	h, err := q.Acquire(context.Background(), runq.Request{Class: "go-test", Label: "t83", Repo: runq.RepoLabel(a.Root), Cmd: "make check"})
	must(t, err)
	t.Cleanup(func() { _ = h.Release() })
	queueWaiter(t, a, q, "go-test", "t84", "other")
	ls, err := narratorHeavy{a}.HeavyLeases()
	must(t, err)
	if len(ls) != 2 {
		t.Fatalf("leases %+v", ls)
	}
	ho, w := ls[0], ls[1]
	if !ho.Holder || ho.Who != "t83" || ho.Class != "go-test" || !ho.Overdue || ho.MaxRun != time.Millisecond || ho.Cmd != "make check" {
		t.Fatalf("holder %+v", ho)
	}
	if w.Holder || w.Who != "other/t84" || w.Position != 1 || w.Holders != "t83" {
		t.Fatalf("waiter %+v", w)
	}
}
