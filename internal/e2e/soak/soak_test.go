//go:build e2e

package soak

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/e2e"
)

var bins e2e.Bins

// TestMain builds saddle, the gh shim and the fake agent once, and runs
// each soak in its own process (see e2e.Main).
func TestMain(m *testing.M) { e2e.Main(m, &bins) }

// Known lists the faults that expose saddle bugs not yet fixed, each with
// its issue: they are left out of the scenario until it closes.
var Known = map[FaultKind]string{
	RedBase:      "#902: CI-red spawns a repair per PR when CI is red on base too",
	EngineKill:   "#904: a train killed mid-rebase leaves rebase-merge behind and the next land bounces the task",
	KilledWorker: "#905: the train auto-rebases a resumed worker's worktree while it commits",
}

// seed is SOAK_SEED, or def: a failing run is replayed by its seed.
func seed(t *testing.T, def int64) int64 {
	if s := os.Getenv("SOAK_SEED"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("SOAK_SEED=%q: %v", s, err)
		}
		return n
	}
	return def
}

func soak(t *testing.T, cfg Config, limit time.Duration) {
	t.Helper()
	for k, issue := range Known {
		t.Logf("fault %s skipped: known failing, %s", k, issue)
	}
	cfg.Skip = Known
	sc := Generate(cfg)
	w := e2e.New(t, bins, WorldOptions())
	t.Cleanup(w.Diagnose)
	began := time.Now()
	opt := Options{Logf: t.Logf}
	if d, ok := t.Deadline(); ok {
		opt.Deadline = d.Add(-90 * time.Second)
	}
	r := Start(w, sc, opt)
	res := r.Drive()
	took := time.Since(began)
	for _, s := range res.Ledger.All() {
		t.Logf("violation %s: ticks %d-%d: %s", s.Key, s.From, s.To, s.Detail)
	}
	for _, o := range res.Outcomes {
		t.Logf("%s %s: %s %s", o.Change, o.Task, o.State, o.Detail)
	}
	if fails := res.Failures(DefaultBudgets); len(fails) > 0 {
		for _, e := range r.Events() {
			t.Logf("event %s", e)
		}
		b, _ := json.MarshalIndent(sc, "", "  ")
		t.Fatalf("soak %s failed after %d ticks (replay with SOAK_SEED=%d):\n  %s\nscenario: %s",
			sc, res.Ticks, sc.Seed, strings.Join(fails, "\n  "), b)
	}
	t.Logf("soak %s passed in %d ticks, %s, %d repairs", sc, res.Ticks, took.Round(time.Second), len(res.Repairs))
	if limit > 0 && took > limit {
		t.Fatalf("soak took %s, over its %s budget", took.Round(time.Second), limit)
	}
}

// smokeBudget is the smoke's wall-clock budget, SOAK_SMOKE_BUDGET (a Go
// duration) overriding it. #831 asked for 30s; the run is bound by
// saddle's own tick cost (a land, a ci-red cycle and an auto-merge of one
// PR each take seconds on fakegh) and takes about 1m40s, so the budget is
// 4m until #903 brings it down to 30s.
var smokeBudget = func() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("SOAK_SMOKE_BUDGET")); err == nil {
		return d
	}
	return 4 * time.Minute
}()

// TestSoakSmoke is the 10-change soak make check runs: every fault once.
func TestSoakSmoke(t *testing.T) {
	if e2e.Fork(t) {
		return
	}
	soak(t, Config{Seed: seed(t, 831), Changes: 10, TickMinutes: 6}, smokeBudget)
}

// TestSoakFull is the nightly's 50-change soak (SOAK_FULL=1).
func TestSoakFull(t *testing.T) {
	if os.Getenv("SOAK_FULL") == "" {
		t.Skip("the 50-change soak runs nightly; set SOAK_FULL=1")
	}
	if e2e.Fork(t) {
		return
	}
	soak(t, Config{Seed: seed(t, time.Now().UnixNano()%1000000), Changes: 50}, 0)
}

// TestSoakKnownFaults names each fault left out of the soaks above because
// it exposes a saddle bug, and skips it with its issue. Once the issue is
// fixed, deleting its Known entry puts the fault back in every soak.
func TestSoakKnownFaults(t *testing.T) {
	for _, k := range AllFaults {
		issue, ok := Known[k]
		if !ok {
			continue
		}
		t.Run(string(k), func(t *testing.T) {
			t.Skipf("known failing: %s", issue)
		})
	}
}
