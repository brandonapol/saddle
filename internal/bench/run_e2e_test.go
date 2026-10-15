//go:build e2e

package bench

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/e2e"
)

// TestBenchRunsControlAndACollision runs two scenarios end to end with the
// fake agent: control lands both tickets with nothing in the way;
// narrow-claim has its out-of-claim write denied, so its ticket can't build
// and needs help while the other lands.
func TestBenchRunsControlAndACollision(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("needs tmux")
	}
	bins, err := e2e.Build(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	run := func(name string) *Result {
		t.Helper()
		var log strings.Builder
		res, err := Run(Options{BenchDir: benchDir, Scenario: name, Out: out, Bins: &bins,
			Stall: 20 * time.Second, Timeout: 4 * time.Minute, Progress: &log})
		if err != nil {
			t.Fatalf("bench run %s: %v\n%s", name, err, log.String())
		}
		t.Logf("%s:\n%s\n%s", name, log.String(), Markdown(res.Report))
		for _, f := range []string{res.JSON, res.MD} {
			if _, err := os.Stat(f); err != nil {
				t.Errorf("%s: no result file: %v", name, err)
			}
		}
		return res
	}

	r := run("control").Report
	if !r.AllLanded || !r.IntegrationGreen || r.EndedEarly != "" || r.DeniedWrites != 0 || r.TrainBounces != 0 ||
		r.ClaimWaits != 0 || len(r.NeededHelp) != 0 || r.PeakParallelism != 2 {
		t.Errorf("control: %+v", r)
	}

	r = run("narrow-claim").Report
	if r.Landed != 1 || !slices.Equal(r.NeededHelp, []string{"#2"}) || r.DeniedWrites < 1 || !r.IntegrationGreen {
		t.Errorf("narrow-claim: %+v", r)
	}
	if !slices.Contains(r.Forecast.Hits, Pair{A: "#1", B: "#2"}) {
		t.Errorf("narrow-claim: the forecaster's medium pair should be a hit: %+v", r.Forecast)
	}
}

// TestBenchSymbolGranularity runs the symbol-claim scenarios in both modes
// (#626). same-file's tickets touch different functions of one file: file
// claims run them one at a time, symbol claims run both and land both.
// same-function and signature must not collide silently under symbol
// claims: the second writer of Rect.Area is denied, and the caller of the
// old units.Scale is warned.
//
// The symbol rules have time budgets (250ms, 300ms) and fail open past
// them, which a loaded machine (make check runs suites in parallel) can
// hit. A symbol-mode run whose check fails is retried, up to three times in
// all, only when it logged a "budget exceeded" fallback; any other miss
// fails at once. #630 would let the World raise the budgets instead.
func TestBenchSymbolGranularity(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("needs tmux")
	}
	bins, err := e2e.Build(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	once := func(name, granularity string) (Report, bool) {
		t.Helper()
		var log strings.Builder
		res, err := Run(Options{BenchDir: benchDir, Scenario: name, Granularity: granularity, Bins: &bins,
			Stall: 20 * time.Second, Timeout: 4 * time.Minute, Progress: &log})
		if err != nil {
			t.Fatalf("bench run %s --granularity %s: %v\n%s", name, granularity, err, log.String())
		}
		t.Logf("%s (%s):\n%s\n%s", name, granularity, log.String(), Markdown(res.Report))
		overBudget := false
		for _, e := range res.Log.Events {
			if strings.HasPrefix(e.Kind, "symbol_") || e.Kind == "deny" {
				t.Logf("  %s %s: %s", e.Task, e.Kind, e.Data)
			}
			overBudget = overBudget || e.Kind == "symbol_fallback" && strings.HasSuffix(e.Data, ": budget exceeded")
		}
		if res.Report.Granularity != granularity {
			t.Errorf("%s: report granularity %q, want %q", name, res.Report.Granularity, granularity)
		}
		return res.Report, overBudget
	}
	// check runs name and reports what bad says is wrong with it, retrying
	// a run that went over a symbol rule's budget.
	check := func(name, granularity string, bad func(Report) string) {
		t.Helper()
		for attempt := 1; ; attempt++ {
			r, overBudget := once(name, granularity)
			why := bad(r)
			if why == "" {
				return
			}
			if !overBudget || attempt == 3 {
				t.Errorf("%s, %s claims: %s: %+v", name, granularity, why, r)
				return
			}
			t.Logf("%s, %s claims: %s, after a symbol rule went over its budget; retrying", name, granularity, why)
		}
	}
	collided := func(r Report) bool { return slices.Contains(r.Forecast.ActualPairs, Pair{A: "#1", B: "#2"}) }

	check("same-file", "file", func(r Report) string {
		if r.PeakParallelism != 1 {
			return "want peak parallelism 1"
		}
		return ""
	})
	check("same-file", "symbol", func(r Report) string {
		if r.PeakParallelism != 2 || !r.AllLanded || !r.IntegrationGreen || r.DeniedWrites != 0 || r.SymbolShares < 1 {
			return "want peak 2, both landed, green, no denies, a share"
		}
		return ""
	})
	// Rule 1 denies the second writer of Rect.Area; past its budget it falls
	// back to the path-claim deny. Either way the write is refused.
	check("same-function", "symbol", func(r Report) string {
		if r.DeniedWrites < 1 || !collided(r) || !r.IntegrationGreen {
			return "want a deny recorded as the #1–#2 collision, and green"
		}
		return ""
	})
	check("signature", "symbol", func(r Report) string {
		if r.SymbolNotes+r.SymbolDenies < 1 || !collided(r) {
			return "want a note or deny recorded as the #1–#2 collision"
		}
		return ""
	})
}
