package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/autopilot"
	"github.com/brandonapol/saddle/internal/runq"
)

// backedUp fills class go-test (1 slot) with a holder and n waiters at prio
// in the queue file db. The returned func drops the waiters.
func backedUp(t *testing.T, db string, n, prio int) (drain func()) {
	t.Helper()
	q, err := runq.Open(runq.Options{Path: db, Mode: runq.ModeEnforce, Slots: map[string]int{"go-test": 1},
		Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	ctx, stop := context.WithCancel(context.Background())
	holder, err := q.Acquire(ctx, runq.Request{Class: "go-test", Label: "holder", Prio: runq.PrioWorker})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, n)
	for range n {
		go func() {
			if l, err := q.Acquire(ctx, runq.Request{Class: "go-test", Label: "waiter", Prio: prio}); err == nil {
				_ = l.Release()
			}
			done <- struct{}{}
		}()
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		st, err := q.Status()
		if err == nil && len(st) == 1 && len(st[0].Waiters) == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiters never queued: %+v %v", st, err)
		}
	}
	var once sync.Once
	drop := func() {
		once.Do(func() {
			stop()
			for range n {
				<-done
			}
		})
	}
	t.Cleanup(func() { drop(); _ = holder.Release() })
	return drop
}

const enforceNoLoadGate = "mode = \"enforce\"\nmax_load_per_cpu = 0\nmax_cpu_pressure = 0\n"

// TestSpawnRefusesWhenQueueBackedUp: with more than 2 × slots worker runs
// waiting, spawn refuses naming the class, the queue length and the ETA,
// and tells the orchestrator; --force overrides it; once the queue drains,
// spawn works again.
func TestSpawnRefusesWhenQueueBackedUp(t *testing.T) {
	a, _ := setup(t)
	db := heavyTest(t, a, enforceNoLoadGate)
	drain := backedUp(t, db, 3, runq.PrioWorker)

	_, err := a.Spawn(SpawnReq{Title: "blocked"})
	if !errors.Is(err, ErrBackpressure) {
		t.Fatalf("spawn with a backed-up queue: err = %v, want ErrBackpressure", err)
	}
	for _, want := range []string{"go-test", "3 runs waiting for 1 slot", "more than 2×", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
	if ev := eventKinds(t, a, EventSpawnBackpressure)[EventSpawnBackpressure]; !strings.Contains(ev, "go-test") {
		t.Errorf("no %s event naming go-test: %q", EventSpawnBackpressure, ev)
	}
	if n := eventKinds(t, a, "notice_")[EventNoticeDigest]; !strings.Contains(n, "heavy-run queue for go-test is backed up") {
		t.Errorf("orchestrator not told: %q", n)
	}

	if _, err := a.Spawn(SpawnReq{Title: "forced", Force: true}); err != nil {
		t.Fatalf("--force should override backpressure: %v", err)
	}

	drain()
	if _, err := a.Spawn(SpawnReq{Title: "after drain"}); err != nil {
		t.Fatalf("spawn after the queue drained: %v", err)
	}
}

// TestSpawnRefusesSubTaskWhenQueueBackedUp: a worker's spawn (MCP, with a
// parent) is refused the same way.
func TestSpawnRefusesSubTaskWhenQueueBackedUp(t *testing.T) {
	a, _ := setup(t)
	p, err := a.Spawn(SpawnReq{Title: "parent"})
	must(t, err)
	db := heavyTest(t, a, enforceNoLoadGate)
	backedUp(t, db, 3, runq.PrioWorker)
	if _, err := a.Spawn(SpawnReq{Title: "child", Parent: p.ID}); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("sub-task spawn: err = %v, want ErrBackpressure", err)
	}
}

// TestSpawnIgnoresGateWaiters: gate runs (the train, pre-publish checks)
// never count toward backpressure.
func TestSpawnIgnoresGateWaiters(t *testing.T) {
	a, _ := setup(t)
	db := heavyTest(t, a, enforceNoLoadGate)
	backedUp(t, db, 3, runq.PrioGate)
	if _, err := a.Spawn(SpawnReq{Title: "free"}); err != nil {
		t.Fatalf("gate waiters held back a spawn: %v", err)
	}
}

// TestSpawnBackpressureWaitFromConfig: backpressure_wait trips on the
// oldest waiter's age even when the queue is short; the queue being off
// turns backpressure off.
func TestSpawnBackpressureWaitFromConfig(t *testing.T) {
	a, _ := setup(t)
	db := heavyTest(t, a, enforceNoLoadGate+"backpressure_wait = \"1ms\"\n")
	backedUp(t, db, 1, runq.PrioWorker)
	time.Sleep(5 * time.Millisecond)
	_, err := a.Spawn(SpawnReq{Title: "old waiter"})
	if !errors.Is(err, ErrBackpressure) || !strings.Contains(err.Error(), "oldest waiter has waited") {
		t.Fatalf("err = %v, want the age check", err)
	}
	t.Setenv(runq.EnvBypass, "off")
	if _, err := a.Spawn(SpawnReq{Title: "queue off"}); err != nil {
		t.Fatalf("SADDLE_RUNQ=off still refused: %v", err)
	}
}

// TestAutopilotWaitsOnBackpressure: autopilot treats backpressure like the
// concurrency cap, stopping its spawns for the tick instead of skipping
// issues one by one.
func TestAutopilotWaitsOnBackpressure(t *testing.T) {
	a, _ := setup(t)
	db := heavyTest(t, a, enforceNoLoadGate)
	backedUp(t, db, 3, runq.PrioWorker)
	env := &autopilotEnv{a: a}
	if _, err := env.Spawn(autopilot.SpawnReq{Title: "issue"}); !errors.Is(err, autopilot.ErrAtCap) {
		t.Fatalf("err = %v, want autopilot.ErrAtCap", err)
	}
}
