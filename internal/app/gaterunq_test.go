package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/runq"
	"github.com/brandonapol/saddle/internal/store"
)

func TestGateClass(t *testing.T) {
	t.Parallel()
	cfg := runq.Config{Classes: map[string]runq.ClassConfig{
		"go-test":       {Match: []string{"go test*", "make check"}},
		"flutter-test":  {Match: []string{"flutter test*"}},
		"golangci-lint": {Match: []string{"golangci-lint run*", "make *"}},
		"slots-only":    {Slots: 3},
	}}
	for cmd, want := range map[string]string{
		"go test ./...":          "go-test",
		"go  test   -race ./...": "go-test",
		"make check":             "go-test", // golangci-lint's "make *" matches too, but go-test comes first
		"make lint":              "golangci-lint",
		"flutter test":           "flutter-test",
		"./run-tests.sh":         DefaultGateClass,
		"make checks":            "golangci-lint",
		"":                       DefaultGateClass,
	} {
		if got := GateClass(cfg, cmd); got != want {
			t.Errorf("GateClass(%q) = %q, want %q", cmd, got, want)
		}
	}
	if got := GateClass(runq.Config{}, "go test ./..."); got != DefaultGateClass {
		t.Errorf("no classes: %q", got)
	}
}

func TestMatchCmd(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		p, s string
		ok   bool
	}{
		{"go test*", "go test ./...", true},
		{"go test*", "go vet ./...", false},
		{"*lint*", "golangci-lint run", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxbyy", false},
		{"make check", "make check/lint", false},
		{"make ?heck", "make check", true},
		{"*", "", true},
	} {
		if got := matchCmd(c.p, c.s); got != c.ok {
			t.Errorf("matchCmd(%q, %q) = %v, want %v", c.p, c.s, got, c.ok)
		}
	}
}

// openQueue opens db in enforce mode with go-test's slots set by the repo's
// runq.toml.
func openQueue(t *testing.T, db string) *runq.Queue {
	t.Helper()
	q, err := runq.Open(runq.Options{Path: db, Mode: runq.ModeEnforce, Slots: map[string]int{}, Heartbeat: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

const gateRunqToml = "mode = \"enforce\"\nmax_load_per_cpu = 0\nmax_cpu_pressure = 0\nheartbeat = \"200ms\"\n[classes.go-test]\nslots = 1\nmatch = [\"true\", \"go test*\"]\n"

// TestRunGateEnvWaitsForSlotAndShowsQueued: the train's gate queues behind a
// worker's run in its class (test.cmd "true" matches go-test). While it
// waits, the train entry reads "queued: position 1 of 1 for go-test, holder
// t83 …"; once the worker releases, the gate runs and the note is restored.
func TestRunGateEnvWaitsForSlotAndShowsQueued(t *testing.T) {
	a, _ := setup(t)
	db := heavyTest(t, a, gateRunqToml)
	must(t, a.Store.Enqueue("t1"))
	q := openQueue(t, db)
	hold, err := q.Acquire(context.Background(), runq.Request{Class: "go-test", Prio: runq.PrioWorker, Label: "t83", Cmd: "make check"})
	must(t, err)

	g := &fakeGate{}
	done := make(chan GateEnvResult, 1)
	go func() { done <- a.RunGateEnv(context.Background(), "t1", g.run) }()

	deadline := time.Now().Add(10 * time.Second)
	var note string
	for time.Now().Before(deadline) {
		if note = a.trainNote("t1"); note != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.HasPrefix(note, "position 1 of 1 for go-test, holder t83 (make check") {
		t.Fatalf("train note while queued = %q", note)
	}
	select {
	case <-done:
		t.Fatal("the gate ran while a worker held the only slot")
	default:
	}
	es, err := a.Store.Train()
	must(t, err)
	if es[0].State != store.Queued {
		t.Fatalf("train state %q", es[0].State)
	}
	if st, _ := a.Queue(); len(st) == 0 || !strings.Contains(st[0].State+": "+st[0].Note, "queued: position 1 of 1") {
		t.Fatalf("train queue doesn't show the wait: %+v", st)
	}

	must(t, hold.Release())
	var res GateEnvResult
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the gate didn't start after the slot freed")
	}
	if res.Err != nil || g.runs != 1 {
		t.Fatalf("gate: %+v, runs %d", res, g.runs)
	}
	if n := a.trainNote("t1"); n != "" {
		t.Fatalf("note after the wait = %q, want it restored", n)
	}
	ev := eventKinds(t, a, "run_")
	if !strings.Contains(ev[EventRunQueued], "go-test") || !strings.Contains(ev[EventRunStarted], "go-test waited") ||
		!strings.Contains(ev[EventRunFinished], "go-test ok") {
		t.Errorf("events %v", ev)
	}
}

// TestRunGateEnvExportsLease: the gate's children get the lease's token and
// the queue's path, and a nested request of any class made with them rides
// on the gate's lease with no slot of its own. The lease is held at
// PrioGate in the class test.cmd matches, and released after.
func TestRunGateEnvExportsLease(t *testing.T) {
	a, _ := setup(t)
	db := heavyTest(t, a, gateRunqToml)
	q := openQueue(t, db)
	var nested *runq.Lease
	var holders []runq.Entry
	run := func(_ context.Context, env []string) (string, error) {
		st, err := q.Status()
		if err != nil {
			return "", err
		}
		for _, c := range st {
			if c.Class == "go-test" {
				holders = c.Holders
			}
		}
		if envOf(env, runq.EnvPath) != db {
			return "", errors.New("no queue path in the gate's env: " + strings.Join(env, " "))
		}
		inner, err := runq.Open(runq.Options{Path: envOf(env, runq.EnvPath), Mode: runq.ModeEnforce,
			Slots:  map[string]int{"e2e": 0},
			Getenv: func(k string) string { return envOf(env, k) }})
		if err != nil {
			return "", err
		}
		defer func() { _ = inner.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// A drained class: only riding the lease can grant it.
		nested, err = inner.Acquire(ctx, runq.Request{Class: "e2e", Label: "hook"})
		if err != nil {
			return "", err
		}
		return "ok", nested.Release()
	}
	res := a.RunGateEnv(context.Background(), "t1", run)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if nested == nil || !nested.Nested() {
		t.Fatal("the nested request didn't ride the gate's lease")
	}
	if len(holders) != 1 || holders[0].Label != "t1" || holders[0].Repo != runq.RepoLabel(a.Root) || holders[0].Prio < runq.PrioGate {
		t.Fatalf("go-test holders while the gate ran: %+v", holders)
	}
	st, err := q.Status()
	must(t, err)
	for _, c := range st {
		if len(c.Holders)+len(c.Waiters) > 0 {
			t.Fatalf("%s still has leases after the gate: %+v", c.Class, c)
		}
	}
}

// TestRunGateEnvGatePriority: a gate that queues behind an earlier worker
// request is granted first.
func TestRunGateEnvGatePriority(t *testing.T) {
	a, _ := setup(t)
	db := heavyTest(t, a, gateRunqToml)
	q := openQueue(t, db)
	hold, err := q.Acquire(context.Background(), runq.Request{Class: "go-test", Label: "holder"})
	must(t, err)
	order := make(chan string, 2)
	// Both runs must be over, leases released, before TempDir's cleanup.
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Add(2)
	go func() {
		defer wg.Done()
		l, err := q.Acquire(context.Background(), runq.Request{Class: "go-test", Prio: runq.PrioWorker, Label: "worker"})
		if err == nil {
			order <- "worker"
			_ = l.Release()
		}
	}()
	waitWaiters(t, q, 1)
	go func() {
		defer wg.Done()
		a.RunGateEnv(context.Background(), "t1", func(context.Context, []string) (string, error) {
			order <- "gate"
			return "ok", nil
		})
	}()
	waitWaiters(t, q, 2)
	must(t, hold.Release())
	if first := <-order; first != "gate" {
		t.Fatalf("%s went first, want the gate", first)
	}
	<-order
}

func waitWaiters(t *testing.T, q *runq.Queue, n int) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		st, err := q.Status()
		must(t, err)
		for _, c := range st {
			if c.Class == "go-test" && len(c.Waiters) == n {
				return
			}
		}
	}
	t.Fatalf("go-test never had %d waiters", n)
}

// TestRunGateEnvLostLeaseIsEnvironment: killing the gate's lease (saddle
// runq kill) stops the gate, and the failure is the environment's, not the
// branch's, without a retry.
func TestRunGateEnvLostLeaseIsEnvironment(t *testing.T) {
	a, _ := setup(t)
	db := heavyTest(t, a, gateRunqToml)
	q := openQueue(t, db)
	runs := 0
	run := func(ctx context.Context, env []string) (string, error) {
		runs++
		must(t, q.Kill(envOf(env, runq.EnvLease)))
		select {
		case <-ctx.Done():
			return "killed", ctx.Err()
		case <-time.After(10 * time.Second):
			return "the gate wasn't stopped", errors.New("exit status 1")
		}
	}
	res := a.RunGateEnv(context.Background(), "t1", run)
	if res.Err == nil || res.Env == nil || res.Env.Signature != gateLeaseLost.Signature {
		t.Fatalf("lost lease: %+v", res)
	}
	if runs != 1 {
		t.Fatalf("ran %d times, want 1", runs)
	}
	if ev := eventKinds(t, a, "run_"); !strings.Contains(ev[EventRunFinished], "go-test lost") {
		t.Errorf("run_finished %q", ev[EventRunFinished])
	}
}

// TestRunGateEnvUnqueuedWhenQueueBroken: a queue that can't be opened never
// blocks the gate.
func TestRunGateEnvUnqueuedWhenQueueBroken(t *testing.T) {
	a, _ := setup(t)
	heavyTest(t, a, gateRunqToml)
	blocker := filepath.Join(t.TempDir(), "file")
	must(t, os.WriteFile(blocker, nil, 0o644))
	t.Setenv(runq.EnvPath, filepath.Join(blocker, "sub", "runq.db"))
	g := &fakeGate{}
	if res := a.RunGateEnv(context.Background(), "t1", g.run); res.Err != nil || g.runs != 1 {
		t.Fatalf("%+v runs %d", res, g.runs)
	}
	if envOf(g.envs[0], runq.EnvLease) != "" {
		t.Fatal("exported a lease it doesn't hold")
	}
}
