package runq

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBackpressureThreshold: spawns hold off when a class has more than
// 2 × slots waiters or its oldest waiter waited past the limit; at or under
// both, or with the class drained, they don't.
func TestBackpressureThreshold(t *testing.T) {
	const limit = 10 * time.Minute
	for _, tc := range []struct {
		name string
		bs   []Backlog
		want string // the class under pressure; "" for none
		why  string
	}{
		{"empty", nil, "", ""},
		{"at 2x slots", []Backlog{{Class: "go-test", Slots: 2, Waiting: 4, Oldest: time.Minute}}, "", ""},
		{"over 2x slots", []Backlog{{Class: "go-test", Slots: 2, Waiting: 5, Oldest: time.Minute}}, "go-test", "more than 2× its 2 slots"},
		{"one slot", []Backlog{{Class: "e2e", Slots: 1, Waiting: 3}}, "e2e", "more than 2× its 1 slot"},
		{"oldest at the limit", []Backlog{{Class: "go-test", Slots: 2, Waiting: 1, Oldest: limit}}, "", ""},
		{"oldest past the limit", []Backlog{{Class: "go-test", Slots: 2, Waiting: 1, Oldest: limit + time.Second}}, "go-test", "oldest waiter has waited 10m (limit 10m)"},
		{"drained class is the owner's doing", []Backlog{{Class: "go-test", Slots: 0, Waiting: 9, Oldest: time.Hour}}, "", ""},
		{"no waiters", []Backlog{{Class: "go-test", Slots: 1, Waiting: 0}}, "", ""},
		{"worst class wins", []Backlog{
			{Class: "a", Slots: 1, Waiting: 3, Oldest: time.Minute},
			{Class: "b", Slots: 1, Waiting: 3, Oldest: 20 * time.Minute},
		}, "b", "more than 2× its 1 slot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Backpressure(tc.bs, limit)
			if tc.want == "" {
				if p != nil {
					t.Fatalf("pressure %+v, want none", *p)
				}
				return
			}
			if p == nil || p.Class != tc.want || !strings.Contains(p.Why, tc.why) {
				t.Fatalf("pressure %+v, want %s because %q", p, tc.want, tc.why)
			}
		})
	}
}

// TestBackpressureAgeCheckOff: a limit of 0 or less turns the age check off,
// not the length check.
func TestBackpressureAgeCheckOff(t *testing.T) {
	old := []Backlog{{Class: "go-test", Slots: 2, Waiting: 1, Oldest: 24 * time.Hour}}
	if p := Backpressure(old, -1); p != nil {
		t.Fatalf("age check off still found %+v", *p)
	}
	long := []Backlog{{Class: "go-test", Slots: 1, Waiting: 3}}
	if p := Backpressure(long, -1); p == nil {
		t.Fatal("length check went off with the age check")
	}
}

// TestPressureString names the class, the queue length and the ETA.
func TestPressureString(t *testing.T) {
	p := Pressure{Backlog: Backlog{Class: "go-test", Slots: 2, Waiting: 5, Oldest: 3 * time.Minute, Median: 2 * time.Minute}, Why: "more than 2× its 2 slots"}
	want := "go-test has 5 runs waiting for 2 slots (oldest 3m, clears in ~6m): more than 2× its 2 slots"
	if got := p.String(); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	p.Median = 0
	if got := p.String(); !strings.Contains(got, "no run-time history for an ETA") {
		t.Fatalf("no history: %q", got)
	}
}

// TestBacklogsSkipGateRuns: gate waiters (the merge train, pre-publish
// checks) never count toward backpressure; worker and background waiters
// do, with their oldest wait and the class's median run time.
func TestBacklogsSkipGateRuns(t *testing.T) {
	now := time.UnixMilli(time.Now().UnixMilli()) // leases store milliseconds
	o := testOpts(t)
	o.Now = func() time.Time { return now }
	o.StaleAfter = time.Hour // the fake clock jumps; flock says who is alive
	q := open(t, o)
	holder := acquire(t, q, Request{Class: "go-test", Label: "h"})
	if err := holder.Release(); err != nil { // history: one 0s run
		t.Fatal(err)
	}
	acquire(t, q, Request{Class: "go-test", Label: "h2"})
	for i, prio := range []int{PrioGate, PrioWorker, PrioBackground, PrioGate + 5} {
		if _, err := q.enqueue(Request{Class: "go-test", Label: "w", Prio: prio}, ModeEnforce); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			now = now.Add(3 * time.Minute) // the gate run is the oldest; it mustn't set Oldest
		}
	}
	now = now.Add(2 * time.Minute)
	bs, err := q.Backlogs()
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 1 {
		t.Fatalf("backlogs %+v, want go-test only", bs)
	}
	b := bs[0]
	if b.Class != "go-test" || b.Slots != 1 || b.Waiting != 2 || b.Oldest != 2*time.Minute {
		t.Fatalf("backlog %+v, want 2 non-gate waiters, oldest 2m, 1 slot", b)
	}
}

// TestBacklogsDropDeadWaiters: a waiter whose process died doesn't hold
// spawns back.
func TestBacklogsDropDeadWaiters(t *testing.T) {
	o := testOpts(t)
	q := open(t, o)
	acquire(t, q, Request{Class: "go-test", Label: "h"})
	p := startHelper(t, "hold", helperEnv(o, "RUNQ_CLASS=go-test", "RUNQ_LABEL=dead", "RUNQ_HOLD_MS=forever")...)
	waitFor(t, "the helper waiting", func() bool {
		bs, err := q.Backlogs()
		return err == nil && len(bs) == 1 && bs[0].Waiting == 1
	})
	p.kill(t)
	waitFor(t, "the dead waiter dropped", func() bool {
		bs, err := q.Backlogs()
		return err == nil && (len(bs) == 0 || bs[0].Waiting == 0)
	})
}

// TestConfigBackpressureWait: [runq] backpressure_wait parses and the user
// file overrides the repo's.
func TestConfigBackpressureWait(t *testing.T) {
	dir := t.TempDir()
	repo, user := filepath.Join(dir, "repo.toml"), filepath.Join(dir, "user.toml")
	if err := os.WriteFile(repo, []byte("backpressure_wait = \"5m\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(repo)
	if err != nil || c.BackpressureWait.D != 5*time.Minute {
		t.Fatalf("repo only: %v %v", c.BackpressureWait, err)
	}
	if err := os.WriteFile(user, []byte("backpressure_wait = \"20m\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, err = LoadConfig(repo, user); err != nil || c.BackpressureWait.D != 20*time.Minute {
		t.Fatalf("user wins: %v %v", c.BackpressureWait, err)
	}
	if got := (Config{}).BackpressureLimit(); got != DefaultBackpressureWait {
		t.Fatalf("default limit %s", got)
	}
}
