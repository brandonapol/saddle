package runq

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func open(t *testing.T, o Options) *Queue {
	t.Helper()
	q, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	return q
}

func acquire(t *testing.T, q *Queue, req Request) *Lease {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	l, err := q.Acquire(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// class returns one class's status.
func class(t *testing.T, q *Queue, name string) ClassStatus {
	t.Helper()
	st, err := q.Status()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range st {
		if c.Class == name {
			return c
		}
	}
	return ClassStatus{Class: name}
}

// waitFor polls cond (a test-side condition, not product code) until it holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func labels(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Label)
	}
	return out
}

// TestTwoProcessesTakeTurns: two OS processes contend for one flutter-test
// slot; the second sees its position and the holder, then starts by itself
// after the first ends. Their runs never overlap.
func TestTwoProcessesTakeTurns(t *testing.T) {
	o := testOpts(t)
	log := filepath.Join(t.TempDir(), "log")
	a := startHelper(t, "hold", helperEnv(o, "RUNQ_CLASS=flutter-test", "RUNQ_LABEL=a", "RUNQ_HOLD_MS=700", "RUNQ_LOG="+log)...)
	a.expect(t, "acquired", 10*time.Second)
	b := startHelper(t, "hold", helperEnv(o, "RUNQ_CLASS=flutter-test", "RUNQ_LABEL=b", "RUNQ_HOLD_MS=50", "RUNQ_LOG="+log)...)
	seen := b.expect(t, "acquired", 10*time.Second)
	if len(seen) < 2 || !strings.HasPrefix(seen[0], "queued: position 1 of 1 for flutter-test, holder a (hold,") {
		t.Fatalf("b's status lines: %q", seen)
	}
	a.wait(t, 10*time.Second)
	b.wait(t, 10*time.Second)
	spans := readLog(t, log)
	if len(spans) != 2 || spans[0].label != "a" || spans[1].label != "b" {
		t.Fatalf("spans: %+v", spans)
	}
	if spans[1].start < spans[0].end {
		t.Fatalf("b started %v before a ended", time.Duration(spans[0].end-spans[1].start))
	}
}

// TestKilledHolderFreesSlotWithinOneHeartbeat: SIGKILL the holder; the
// waiter (another process) starts within one heartbeat interval.
func TestKilledHolderFreesSlotWithinOneHeartbeat(t *testing.T) {
	o := testOpts(t)
	o.Heartbeat = time.Second
	log := filepath.Join(t.TempDir(), "log")
	a := startHelper(t, "hold", helperEnv(o, "RUNQ_CLASS=go-test", "RUNQ_LABEL=a", "RUNQ_HOLD_MS=forever", "RUNQ_LOG="+log)...)
	a.expect(t, "acquired", 10*time.Second)
	b := startHelper(t, "hold", helperEnv(o, "RUNQ_CLASS=go-test", "RUNQ_LABEL=b", "RUNQ_HOLD_MS=0", "RUNQ_LOG="+log)...)
	b.expect(t, "queued: position 1 of 1", 10*time.Second)
	// Let b's backoff grow to its maximum so the test measures the worst case.
	time.Sleep(2 * o.Heartbeat)
	killed := time.Now()
	a.kill(t)
	b.expect(t, "acquired", 10*time.Second)
	b.wait(t, 10*time.Second)
	var bStart int64
	for _, s := range readLog(t, log) {
		if s.label == "b" {
			bStart = s.start
		}
	}
	took := time.Unix(0, bStart).Sub(killed)
	t.Logf("b started %s after the kill", took)
	if took > o.Heartbeat {
		t.Fatalf("b started %s after the kill; want within one heartbeat (%s)", took, o.Heartbeat)
	}
	q := open(t, o)
	if c := class(t, q, "go-test"); len(c.Holders)+len(c.Waiters) != 0 {
		t.Fatalf("leftover leases: %+v", c)
	}
	var how string
	if err := q.db.QueryRow(`SELECT how FROM history WHERE label='a'`).Scan(&how); err != nil || how != "reaped" {
		t.Fatalf("a's history: %q %v", how, err)
	}
}

// TestNestedLeaseDoesNotDeadlock: a run holding the only slot starts a child
// that asks for the same class (a pre-commit hook's make check); the child
// rides on the parent's lease through SADDLE_RUNQ_LEASE.
func TestNestedLeaseDoesNotDeadlock(t *testing.T) {
	o := testOpts(t)
	log := filepath.Join(t.TempDir(), "log")
	p := startHelper(t, "run", helperEnv(o, "RUNQ_CLASS=go-test", "RUNQ_LABEL=outer", "RUNQ_HOLD_MS=50", "RUNQ_LOG="+log)...)
	p.wait(t, 10*time.Second)
	spans := readLog(t, log)
	if len(spans) != 1 || spans[0].label != "outer-child" || !spans[0].nested {
		t.Fatalf("spans: %+v", spans)
	}
}

func TestNestedLeaseInProcess(t *testing.T) {
	o := testOpts(t)
	q := open(t, o)
	outer := acquire(t, q, Request{Class: "go-test", Label: "outer"})
	env := map[string]string{EnvLease: outer.Token()}
	o.Getenv = func(k string) string { return env[k] }
	inner := open(t, o)
	l := acquire(t, inner, Request{Class: "golangci-lint", Label: "inner"})
	if !l.Nested() || l.Token() != outer.Token() {
		t.Fatalf("inner lease not nested: %+v", l)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if c := class(t, q, "go-test"); len(c.Holders) != 1 {
		t.Fatalf("releasing the nested lease freed the outer one: %+v", c)
	}
	// Once the outer lease ends, its token in a stale environment queues
	// normally instead of riding on nothing.
	if err := outer.Release(); err != nil {
		t.Fatal(err)
	}
	l = acquire(t, inner, Request{Class: "go-test", Label: "late"})
	if l.Nested() {
		t.Fatal("stale token still nested")
	}
	_ = l.Release()
}

// TestPriorityOrdering: with the slot busy, background, worker and gate
// requests arrive in that order and are granted gate, worker, background.
func TestPriorityOrdering(t *testing.T) {
	o := testOpts(t)
	o.AgingStep = time.Hour
	q := open(t, o)
	h := acquire(t, q, Request{Class: "go-test", Label: "holder"})
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	for i, r := range []struct {
		label string
		prio  int
	}{{"background", PrioBackground}, {"worker", PrioWorker}, {"gate", PrioGate}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := q.Acquire(context.Background(), Request{Class: "go-test", Prio: r.prio, Label: r.label})
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, r.label)
			mu.Unlock()
			_ = l.Release()
		}()
		waitFor(t, r.label+" queued", func() bool { return len(class(t, q, "go-test").Waiters) == i+1 })
	}
	if got := labels(class(t, q, "go-test").Waiters); strings.Join(got, ",") != "gate,worker,background" {
		t.Fatalf("queue order %v", got)
	}
	_ = h.Release()
	wg.Wait()
	if strings.Join(order, ",") != "gate,worker,background" {
		t.Fatalf("grant order %v", order)
	}
}

// TestAgingPreventsStarvation: waiting raises priority, so a background
// request outranks any worker request that arrives more than
// (PrioWorker-PrioBackground) aging steps after it. A stream of fresh
// higher-priority work can't starve it.
func TestAgingPreventsStarvation(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	o := testOpts(t)
	o.AgingStep = time.Minute
	o.StaleAfter = 24 * time.Hour
	o.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	advance := func(d time.Duration) { clock.Add(int64(d)) }
	q := open(t, o)
	h := acquire(t, q, Request{Class: "go-test", Label: "holder"})
	got := make(chan string, 2)
	enqueue := func(ctx context.Context, label string, prio int) {
		go func() {
			l, err := q.Acquire(ctx, Request{Class: "go-test", Prio: prio, Label: label})
			if errors.Is(err, context.Canceled) {
				return
			} else if err != nil {
				t.Error(err)
				return
			}
			got <- label
			_ = l.Release()
		}()
	}
	waiters := func() []string { return labels(class(t, q, "go-test").Waiters) }
	enqueue(context.Background(), "background", PrioBackground)
	waitFor(t, "background queued", func() bool { return len(waiters()) == 1 })

	advance(time.Minute)
	early, cancel := context.WithCancel(context.Background())
	enqueue(early, "early-worker", PrioWorker)
	waitFor(t, "early worker queued", func() bool { return len(waiters()) == 2 })
	if w := waiters(); w[0] != "early-worker" {
		t.Fatalf("a worker one step behind should lead: %v", w)
	}
	cancel()
	waitFor(t, "early worker gone", func() bool { return len(waiters()) == 1 })

	advance(10 * time.Minute) // background has waited 11 steps
	enqueue(context.Background(), "late-worker", PrioWorker)
	waitFor(t, "late worker queued", func() bool { return len(waiters()) == 2 })
	if w := waiters(); w[0] != "background" {
		t.Fatalf("background should now lead: %v", w)
	}
	_ = h.Release()
	if first := <-got; first != "background" {
		t.Fatalf("first grant went to %s", first)
	}
	<-got
}

// TestFIFOWithinPriority: a holder that releases and asks again goes behind
// whoever was already waiting, so one agent can't hog a class.
func TestFIFOWithinPriority(t *testing.T) {
	q := open(t, testOpts(t))
	a := acquire(t, q, Request{Class: "go-test", Label: "a"})
	bGot := make(chan *Lease, 1)
	go func() {
		l, err := q.Acquire(context.Background(), Request{Class: "go-test", Label: "b"})
		if err != nil {
			t.Error(err)
		}
		bGot <- l
	}()
	waitFor(t, "b queued", func() bool { return len(class(t, q, "go-test").Waiters) == 1 })
	_ = a.Release()
	aGot := make(chan *Lease, 1)
	go func() {
		l, err := q.Acquire(context.Background(), Request{Class: "go-test", Label: "a"})
		if err != nil {
			t.Error(err)
		}
		aGot <- l
	}()
	b := <-bGot
	select {
	case <-aGot:
		t.Fatal("a got the slot back while b held it")
	case <-time.After(300 * time.Millisecond):
	}
	_ = b.Release()
	_ = (<-aGot).Release()
}

// TestCrashSafety: contenders SIGKILLed at random points mid-churn never
// corrupt the database or leave a lease behind.
func TestCrashSafety(t *testing.T) {
	o := testOpts(t)
	for round := range 4 {
		var ps []*proc
		for i := range 3 {
			ps = append(ps, startHelper(t, "churn", helperEnv(o, "RUNQ_CLASS=go-test", "RUNQ_LABEL=c"+string(rune('0'+i)))...))
		}
		time.Sleep(time.Duration(150+rand.IntN(200)) * time.Millisecond)
		for _, p := range ps {
			p.kill(t)
		}
		for _, p := range ps {
			<-p.done
			p.done <- nil
		}
		t.Logf("round %d killed", round)
	}
	q := open(t, o)
	var ok string
	if err := q.db.QueryRow(`PRAGMA integrity_check`).Scan(&ok); err != nil || ok != "ok" {
		t.Fatalf("integrity_check: %q %v", ok, err)
	}
	if c := class(t, q, "go-test"); len(c.Holders)+len(c.Waiters) != 0 {
		t.Fatalf("dead contenders still queued: %+v", c)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	l, err := q.Acquire(ctx, Request{Class: "go-test"})
	if err != nil {
		t.Fatalf("acquire after the crashes: %v", err)
	}
	_ = l.Release()
	if ents, _ := os.ReadDir(q.locks); len(ents) != 0 {
		t.Fatalf("%d lock files left behind", len(ents))
	}
}

// TestCorruptDatabaseMovedAside: a file SQLite can't read is set aside and
// the queue starts empty instead of wedging every run.
func TestCorruptDatabaseMovedAside(t *testing.T) {
	o := testOpts(t)
	if err := os.WriteFile(o.Path, []byte(strings.Repeat("not a database ", 500)), 0o644); err != nil {
		t.Fatal(err)
	}
	q := open(t, o)
	_ = acquire(t, q, Request{Class: "go-test"}).Release()
	aside, _ := filepath.Glob(o.Path + ".corrupt-*")
	if len(aside) == 0 {
		t.Fatal("corrupt file not moved aside")
	}
}

func TestBypass(t *testing.T) {
	o := testOpts(t)
	q := open(t, o)
	h := acquire(t, q, Request{Class: "go-test"})
	defer h.Release()
	o.Getenv = func(k string) string {
		if k == EnvBypass {
			return "off"
		}
		return ""
	}
	l := acquire(t, open(t, o), Request{Class: "go-test"})
	if !l.Bypassed() {
		t.Fatal("not bypassed")
	}
	_ = l.Release()
}

// TestStatusLine: a waiter learns its position and the holder in one line.
func TestStatusLine(t *testing.T) {
	q := open(t, testOpts(t))
	h := acquire(t, q, Request{Class: "go-test", Label: "t83", Cmd: "make check"})
	lines := make(chan string, 10)
	done := make(chan struct{})
	go func() {
		defer close(done)
		l, err := q.Acquire(context.Background(), Request{Class: "go-test", Label: "t90",
			OnWait: func(w Wait) { lines <- w.String() }})
		if err != nil {
			t.Error(err)
			return
		}
		_ = l.Release()
	}()
	if l := <-lines; !strings.HasPrefix(l, "queued: position 1 of 1 for go-test, holder t83 (make check, ") {
		t.Fatalf("status line %q", l)
	}
	c := class(t, q, "go-test")
	if len(c.Holders) != 1 || c.Holders[0].Label != "t83" || len(c.Waiters) != 1 || c.Waiters[0].Position != 1 {
		t.Fatalf("status %+v", c)
	}
	_ = h.Release()
	<-done
}

// TestRunPrintsQueueAndPassesToken: Run prints the queued line, then the
// command runs with the lease token in its environment.
func TestRunPrintsQueueAndPassesToken(t *testing.T) {
	q := open(t, testOpts(t))
	h := acquire(t, q, Request{Class: "go-test", Label: "t83", Cmd: "make check"})
	var out strings.Builder
	var mu sync.Mutex
	w := writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return out.Write(p) })
	tokFile := filepath.Join(t.TempDir(), "tok")
	errc := make(chan error, 1)
	go func() {
		cmd := exec.Command("sh", "-c", `printf %s "$`+EnvLease+`" > `+tokFile)
		errc <- q.Run(context.Background(), "go-test", PrioWorker, cmd, w)
	}()
	waitFor(t, "queued line", func() bool { mu.Lock(); defer mu.Unlock(); return strings.Contains(out.String(), "queued:") })
	_ = h.Release()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	tok, _ := os.ReadFile(tokFile)
	if len(tok) != 24 {
		t.Fatalf("child saw token %q", tok)
	}
	if s := out.String(); !strings.Contains(s, "queued: position 1 of 1 for go-test, holder t83") || !strings.Contains(s, "go-test slot acquired after") {
		t.Fatalf("Run output %q", s)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestWaitDoesNotBusyPoll: a waiter queued for two seconds makes a bounded
// number of attempts (backoff to PollMax), not a tight loop.
func TestWaitDoesNotBusyPoll(t *testing.T) {
	o := testOpts(t)
	o.PollMax = 200 * time.Millisecond
	q := open(t, o)
	h := acquire(t, q, Request{Class: "go-test"})
	var waits atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := q.Acquire(ctx, Request{Class: "go-test", OnWait: func(Wait) { waits.Add(1) }})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	if n := q.attempts.Load(); n > 25 {
		t.Fatalf("%d attempts in 2s; want bounded backoff (<= ~15)", n)
	}
	if waits.Load() != 1 {
		t.Fatalf("OnWait called %d times for an unchanged queue", waits.Load())
	}
	_ = h.Release()
	if c := class(t, q, "go-test"); len(c.Waiters) != 0 {
		t.Fatalf("canceled request still queued: %+v", c)
	}
}

// TestReleaseWakesWaiter: a release wakes the waiter through the wake file
// well before its backoff timer would.
func TestReleaseWakesWaiter(t *testing.T) {
	o := testOpts(t)
	o.PollMax = 5 * time.Second
	o.Heartbeat = 10 * time.Second
	q := open(t, o)
	h := acquire(t, q, Request{Class: "go-test"})
	got := make(chan time.Time, 1)
	go func() {
		l, err := q.Acquire(context.Background(), Request{Class: "go-test"})
		if err != nil {
			t.Error(err)
		}
		got <- time.Now()
		_ = l.Release()
	}()
	waitFor(t, "queued", func() bool { return len(class(t, q, "go-test").Waiters) == 1 })
	time.Sleep(3 * time.Second) // backoff now well past a second
	released := time.Now()
	_ = h.Release()
	d := (<-got).Sub(released)
	t.Logf("handoff took %s", d)
	if d > time.Second {
		t.Fatalf("waiter woke %s after the release", d)
	}
}

func TestSlotsPerClass(t *testing.T) {
	o := testOpts(t)
	o.Slots = map[string]int{"go-test": 2}
	q := open(t, o)
	a := acquire(t, q, Request{Class: "go-test"})
	b := acquire(t, q, Request{Class: "go-test"})
	lint := acquire(t, q, Request{Class: "golangci-lint"})
	if c := class(t, q, "go-test"); c.Slots != 2 || len(c.Holders) != 2 {
		t.Fatalf("%+v", c)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := q.Acquire(ctx, Request{Class: "go-test"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("third go-test run: %v", err)
	}
	if err := q.SetSlots("go-test", 3); err != nil {
		t.Fatal(err)
	}
	_ = acquire(t, q, Request{Class: "go-test"}).Release()
	for _, l := range []*Lease{a, b, lint} {
		_ = l.Release()
	}
}

// TestKill: killing a waiter fails its Acquire; killing a holder closes Lost.
func TestKill(t *testing.T) {
	q := open(t, testOpts(t))
	h := acquire(t, q, Request{Class: "go-test", Label: "h"})
	errc := make(chan error, 1)
	go func() {
		_, err := q.Acquire(context.Background(), Request{Class: "go-test", Label: "w"})
		errc <- err
	}()
	waitFor(t, "queued", func() bool { return len(class(t, q, "go-test").Waiters) == 1 })
	if err := q.Kill(class(t, q, "go-test").Waiters[0].Token[:8]); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("killed waiter: %v", err)
	}
	if err := q.Kill(h.Token()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Lost():
	case <-time.After(5 * time.Second):
		t.Fatal("Lost not closed after kill")
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	if err := q.Kill("nope"); err == nil {
		t.Fatal("killing an unknown lease succeeded")
	}
}

type fakeLoad struct {
	mu sync.Mutex
	l  Load
}

func (f *fakeLoad) Sample() (Load, error) { f.mu.Lock(); defer f.mu.Unlock(); return f.l, nil }
func (f *fakeLoad) set(l Load)            { f.mu.Lock(); defer f.mu.Unlock(); f.l = l }

// TestLoadGateHoldsRunWhileSaturated: a free slot isn't taken while the box
// is saturated by other work; the run starts once pressure drops.
func TestLoadGateHoldsRunWhileSaturated(t *testing.T) {
	src := &fakeLoad{l: Load{Load1: 15, CPUs: 8}}
	o := testOpts(t)
	o.Gate = LoadGate{Source: src, MaxLoadPerCPU: 0.9, MaxPSISomeAvg10: 50}
	q := open(t, o)
	lines := make(chan string, 10)
	got := make(chan *Lease, 1)
	go func() {
		l, err := q.Acquire(context.Background(), Request{Class: "go-test", OnWait: func(w Wait) { lines <- w.String() }})
		if err != nil {
			t.Error(err)
		}
		got <- l
	}()
	if l := <-lines; !strings.Contains(l, "waiting on load: load 1.88/cpu > 0.90") {
		t.Fatalf("line %q", l)
	}
	src.set(Load{Load1: 2, CPUs: 8, PSI10: 80, HasPSI: true})
	if l := <-lines; !strings.Contains(l, "cpu pressure 80% > 50%") {
		t.Fatalf("line %q", l)
	}
	select {
	case <-got:
		t.Fatal("granted while the gate was closed")
	case <-time.After(200 * time.Millisecond):
	}
	src.set(Load{Load1: 2, CPUs: 8, PSI10: 5, HasPSI: true})
	_ = (<-got).Release()
}

// TestLoadGateEscapeHatch: a gate that never opens holds a run at most
// GateMaxWait, so a stuck probe or a busy box can't wedge the queue.
func TestLoadGateEscapeHatch(t *testing.T) {
	o := testOpts(t)
	o.Gate = LoadGate{Source: &fakeLoad{l: Load{Load1: 100, CPUs: 1}}, MaxLoadPerCPU: 1}
	o.GateMaxWait = 300 * time.Millisecond
	q := open(t, o)
	start := time.Now()
	_ = acquire(t, q, Request{Class: "go-test"}).Release()
	if d := time.Since(start); d < o.GateMaxWait {
		t.Fatalf("admitted after %s, before GateMaxWait", d)
	}
}

func TestProcLoad(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc", "pressure"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "proc", "loadavg"), []byte("3.50 2.00 1.00 2/300 1234\n"), 0o644)
	l, err := ProcLoad{Root: root}.Sample()
	if err != nil || l.Load1 != 3.5 || l.HasPSI {
		t.Fatalf("%+v %v", l, err)
	}
	_ = os.WriteFile(filepath.Join(root, "proc", "pressure", "cpu"),
		[]byte("some avg10=42.50 avg60=10.00 avg300=1.00 total=123\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"), 0o644)
	if l, _ = (ProcLoad{Root: root}).Sample(); !l.HasPSI || l.PSI10 != 42.5 {
		t.Fatalf("%+v", l)
	}
	if ok, _ := (LoadGate{Source: ProcLoad{Root: filepath.Join(root, "missing")}, MaxLoadPerCPU: 0.1}).Admit(); !ok {
		t.Fatal("an unreadable probe must fail open")
	}
}
