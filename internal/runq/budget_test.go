package runq

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// budgetOpts is a queue with roomy class slots, so only the budget binds.
func budgetOpts(t *testing.T, budget int) Options {
	o := testOpts(t)
	o.Slots = map[string]int{"go-test": 8, "flutter-test": 4}
	o.Weights = map[string]int{"flutter-test": 3}
	o.Budget = budget
	return o
}

// pending is an Acquire running in the background.
type pending struct {
	lines chan string
	got   chan *Lease
}

func acquireAsync(t *testing.T, q *Queue, req Request) *pending {
	t.Helper()
	p := &pending{lines: make(chan string, 20), got: make(chan *Lease, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	req.OnWait = func(w Wait) {
		select {
		case p.lines <- w.String():
		case <-ctx.Done():
		}
	}
	done := make(chan struct{})
	// Wait for Acquire to return, so it isn't still creating or removing
	// its lock file when the queue closes and TempDir is removed.
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		l, err := q.Acquire(ctx, req)
		if err != nil {
			if ctx.Err() == nil {
				t.Error(err)
			}
			return
		}
		p.got <- l
	}()
	return p
}

// line waits for a queue line containing want.
func (p *pending) line(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case l := <-p.lines:
			if strings.Contains(l, want) {
				return
			}
		case <-deadline:
			t.Fatalf("no queue line with %q", want)
		}
	}
}

func (p *pending) granted(t *testing.T) *Lease {
	t.Helper()
	select {
	case l := <-p.got:
		t.Cleanup(func() { _ = l.Release() })
		return l
	case <-time.After(10 * time.Second):
		t.Fatal("not granted")
		return nil
	}
}

func (p *pending) notGranted(t *testing.T) {
	t.Helper()
	select {
	case l := <-p.got:
		_ = l.Release()
		t.Fatal("granted over the budget")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestBudgetCountsWeights: a flutter-test run uses 3 of 4 units, so one
// go-test fits beside it and a second waits on the budget until the
// flutter run ends.
func TestBudgetCountsWeights(t *testing.T) {
	q := open(t, budgetOpts(t, 4))
	f := acquire(t, q, Request{Class: "flutter-test", Label: "f"})
	g1 := acquire(t, q, Request{Class: "go-test", Label: "g1"})
	defer func() { _ = g1.Release() }()
	if used, budget, err := q.Budget(); err != nil || used != 4 || budget != 4 {
		t.Fatalf("budget %d/%d %v, want 4/4", used, budget, err)
	}
	g2 := acquireAsync(t, q, Request{Class: "go-test", Label: "g2"})
	g2.line(t, "waiting on budget: 4/4 units busy, go-test needs 1")
	g2.notGranted(t)
	_ = f.Release()
	g2.granted(t)
	if used, _, _ := q.Budget(); used != 2 {
		t.Fatalf("used %d after the swap, want 2", used)
	}
	if e := class(t, q, "go-test").Holders; len(e) != 2 || e[0].Weight != 1 {
		t.Fatalf("holders %+v", e)
	}
}

// TestBudgetBlocksLighterRunsBehindHeavyWaiter: a flutter-test waiting for 3
// units holds back a go-test that arrived after it, even though one unit is
// free, so a stream of light runs can't starve the heavy one.
func TestBudgetBlocksLighterRunsBehindHeavyWaiter(t *testing.T) {
	q := open(t, budgetOpts(t, 4))
	a := acquire(t, q, Request{Class: "go-test", Label: "a"})
	b := acquire(t, q, Request{Class: "go-test", Label: "b"})
	defer func() { _ = b.Release() }()
	f := acquireAsync(t, q, Request{Class: "flutter-test", Label: "f"})
	f.line(t, "flutter-test needs 3")
	g := acquireAsync(t, q, Request{Class: "go-test", Label: "g"})
	g.line(t, "behind f (flutter-test, 3 units)")
	g.notGranted(t)
	_ = a.Release()
	f.granted(t)
	g.notGranted(t) // 1 (b) + 3 (f) = 4: still full
}

// TestOverBudgetRunStartsAlone: a run heavier than the whole budget waits
// for the machine to empty, then runs alone instead of never.
func TestOverBudgetRunStartsAlone(t *testing.T) {
	q := open(t, budgetOpts(t, 2))
	a := acquire(t, q, Request{Class: "go-test", Label: "a"})
	f := acquireAsync(t, q, Request{Class: "flutter-test", Label: "f"})
	f.line(t, "1/2 units busy, flutter-test needs 3")
	f.notGranted(t)
	_ = a.Release()
	f.granted(t)
	g := acquireAsync(t, q, Request{Class: "go-test", Label: "g"})
	g.line(t, "3/2 units busy")
	g.notGranted(t)
}

// TestBudgetOff: a negative budget lets every class's slots run at once.
func TestBudgetOff(t *testing.T) {
	q := open(t, budgetOpts(t, -1))
	for _, c := range []string{"flutter-test", "flutter-test", "go-test"} {
		_ = acquire(t, q, Request{Class: c})
	}
}

// TestLeaseExpiresWhenProcessDies: a heavy run held by another process that
// is SIGKILLed gives its units back on the next poll, so a waiter in
// another class starts without anyone cleaning up.
func TestLeaseExpiresWhenProcessDies(t *testing.T) {
	o := budgetOpts(t, 3)
	q := open(t, o)
	h := startHelper(t, "hold", helperEnv(o, "RUNQ_CLASS=flutter-test", "RUNQ_LABEL=hook", "RUNQ_HOLD_MS=forever")...)
	h.expect(t, "acquired", 10*time.Second)
	if e := class(t, q, "flutter-test").Holders; len(e) != 1 || e[0].Weight != 3 {
		t.Fatalf("holder %+v, want weight 3 from the helper's own default", e)
	}
	g := acquireAsync(t, q, Request{Class: "go-test", Label: "g"})
	g.line(t, "waiting on budget: 3/3 units busy")
	h.kill(t)
	g.granted(t)
}

// TestForeignLeaseShowsInStatus: a run marked foreign (no saddle agent
// started it) says so in status, with its weight.
func TestForeignLeaseShowsInStatus(t *testing.T) {
	q := open(t, budgetOpts(t, 4))
	_ = acquire(t, q, Request{Class: "flutter-test", Label: "pid 7", Foreign: true})
	_ = acquire(t, q, Request{Class: "go-test", Label: "t5"})
	if h := class(t, q, "flutter-test").Holders; len(h) != 1 || !h[0].Foreign || h[0].Weight != 3 {
		t.Fatalf("flutter holders %+v", h)
	}
	if h := class(t, q, "go-test").Holders; len(h) != 1 || h[0].Foreign {
		t.Fatalf("go-test holders %+v", h)
	}
}

// TestLoadGateHoldsOnLowRAM: the gate holds while MemAvailable is under
// min_free_ram_pct of MemTotal, and reads it from /proc/meminfo.
func TestLoadGateHoldsOnLowRAM(t *testing.T) {
	g := LoadGate{Source: &fakeLoad{l: Load{Load1: 1, CPUs: 8, MemTotalKB: 1000, MemAvailKB: 50, HasMem: true}}, MinFreeRAMPct: 10}
	if ok, why := g.Admit(); ok || why != "free ram 5% < 10%" {
		t.Fatalf("admit %v %q", ok, why)
	}
	g.Source = &fakeLoad{l: Load{Load1: 1, CPUs: 8, MemTotalKB: 1000, MemAvailKB: 500, HasMem: true}}
	if ok, why := g.Admit(); !ok {
		t.Fatalf("held at 50%% free: %q", why)
	}
	root := procRoot(t, "1.00", -1)
	_ = os.WriteFile(filepath.Join(root, "proc", "meminfo"),
		[]byte("MemTotal:       32000000 kB\nMemFree:          100000 kB\nMemAvailable:    1600000 kB\n"), 0o644)
	l, err := ProcLoad{Root: root}.Sample()
	if err != nil || !l.HasMem || l.MemTotalKB != 32000000 || l.MemAvailKB != 1600000 {
		t.Fatalf("%+v %v", l, err)
	}
	if ok, why := (LoadGate{Source: ProcLoad{Root: root}, MinFreeRAMPct: 10}).Admit(); ok || !strings.Contains(why, "free ram 5%") {
		t.Fatalf("admit %v %q", ok, why)
	}
	if ok, _ := (LoadGate{Source: ProcLoad{Root: procRoot(t, "1.00", -1)}, MinFreeRAMPct: 10}).Admit(); !ok {
		t.Fatal("a box without /proc/meminfo must fail open")
	}
}

// TestAcquireWaitsOnLowRAM: under simulated memory pressure a free slot
// and free budget still wait, and the run starts once RAM frees up.
func TestAcquireWaitsOnLowRAM(t *testing.T) {
	src := &fakeLoad{l: Load{Load1: 1, CPUs: 8, MemTotalKB: 1000, MemAvailKB: 20, HasMem: true}}
	o := budgetOpts(t, 4)
	o.Gate = LoadGate{Source: src, MaxLoadPerCPU: 1, MinFreeRAMPct: 10}
	q := open(t, o)
	p := acquireAsync(t, q, Request{Class: "go-test"})
	p.line(t, "waiting on load: free ram 2% < 10%")
	p.notGranted(t)
	src.set(Load{Load1: 1, CPUs: 8, MemTotalKB: 1000, MemAvailKB: 400, HasMem: true})
	p.granted(t)
}

// TestConfigWeightsAndBudget: weights and the budget come from runq.toml,
// the user file winning; budget = 0 turns it off; the defaults weigh
// flutter-test 3.
func TestConfigWeightsAndBudget(t *testing.T) {
	o := Config{}.Apply(Options{})
	if o.Weights["flutter-test"] != 3 || o.Budget != 0 {
		t.Fatalf("defaults: weights %v budget %d", o.Weights, o.Budget)
	}
	if o := withDefaults(Options{CPUs: 16}); o.Budget != 8 {
		t.Fatalf("default budget on 16 cores %d, want 8", o.Budget)
	}
	if o := withDefaults(Options{CPUs: 2}); o.Budget != 3 {
		t.Fatalf("default budget on 2 cores %d, want 3 (the heaviest weight)", o.Budget)
	}
	repo := writeConfig(t, "budget = 6\n[classes.sqlc]\nweight = 2\n[classes.flutter-test]\nweight = 4\n")
	user := writeConfig(t, "budget = 5\n[classes.flutter-test]\nweight = 2\n")
	c, err := LoadConfig(repo, user)
	if err != nil {
		t.Fatal(err)
	}
	o = c.Apply(Options{})
	if o.Budget != 5 || o.Weights["flutter-test"] != 2 || o.Weights["sqlc"] != 2 {
		t.Fatalf("budget %d weights %v", o.Budget, o.Weights)
	}
	if c, _ = LoadConfig(writeConfig(t, "budget = 0\n")); c.Apply(Options{}).Budget >= 0 {
		t.Fatal("budget = 0 should turn the budget off")
	}
	if _, err := LoadConfig(writeConfig(t, "min_free_ram_pct = 120\n")); err == nil {
		t.Fatal("min_free_ram_pct over 100 should be rejected")
	}
}
