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
var Known = map[FaultKind]string{}

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
	r := Start(w, sc, Options{Logf: t.Logf})
	res := r.Drive()
	took := time.Since(began)
	for _, s := range res.Ledger.All() {
		t.Logf("violation %s: ticks %d-%d: %s", s.Key, s.From, s.To, s.Detail)
	}
	for _, o := range res.Outcomes {
		t.Logf("%s %s: %s %s", o.Change, o.Task, o.State, o.Detail)
	}
	if fails := res.Failures(DefaultBudgets); len(fails) > 0 {
		b, _ := json.MarshalIndent(sc, "", "  ")
		t.Fatalf("soak %s failed after %d ticks (replay with SOAK_SEED=%d):\n  %s\nscenario: %s",
			sc, res.Ticks, sc.Seed, strings.Join(fails, "\n  "), b)
	}
	t.Logf("soak %s passed in %d ticks, %s, %d repairs", sc, res.Ticks, took.Round(time.Second), len(res.Repairs))
	if limit > 0 && took > limit {
		t.Fatalf("soak took %s, over its %s budget", took.Round(time.Second), limit)
	}
}

// TestSoakSmoke is the 10-change soak make check runs: every fault once.
func TestSoakSmoke(t *testing.T) {
	if e2e.Fork(t) {
		return
	}
	soak(t, Config{Seed: seed(t, 831), Changes: 10}, 30*time.Second)
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
