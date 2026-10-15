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
func TestBenchSymbolGranularity(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("needs tmux")
	}
	bins, err := e2e.Build(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := func(name, granularity string) Report {
		t.Helper()
		var log strings.Builder
		res, err := Run(Options{BenchDir: benchDir, Scenario: name, Granularity: granularity, Bins: &bins,
			Stall: 20 * time.Second, Timeout: 4 * time.Minute, Progress: &log})
		if err != nil {
			t.Fatalf("bench run %s --granularity %s: %v\n%s", name, granularity, err, log.String())
		}
		t.Logf("%s (%s):\n%s\n%s", name, granularity, log.String(), Markdown(res.Report))
		for _, e := range res.Log.Events {
			if strings.HasPrefix(e.Kind, "symbol_") || e.Kind == "deny" {
				t.Logf("  %s %s: %s", e.Task, e.Kind, e.Data)
			}
		}
		if res.Report.Granularity != granularity {
			t.Errorf("%s: report granularity %q, want %q", name, res.Report.Granularity, granularity)
		}
		return res.Report
	}

	f := run("same-file", "file")
	if f.PeakParallelism != 1 {
		t.Errorf("same-file, file claims: peak parallelism %d, want 1", f.PeakParallelism)
	}
	s := run("same-file", "symbol")
	if s.PeakParallelism != 2 || !s.AllLanded || !s.IntegrationGreen || s.DeniedWrites != 0 || s.SymbolShares < 1 {
		t.Errorf("same-file, symbol claims: want peak 2, both landed, green, no denies, a share: %+v", s)
	}

	s = run("same-function", "symbol")
	// Rule 1 denies the second writer of Rect.Area; past its time budget it
	// falls back to the path-claim deny. Either way the write is refused.
	if s.DeniedWrites < 1 || !slices.Contains(s.Forecast.ActualPairs, Pair{A: "#1", B: "#2"}) || !s.IntegrationGreen {
		t.Errorf("same-function, symbol claims: want a deny recorded as the #1–#2 collision and green: %+v", s)
	}

	s = run("signature", "symbol")
	if s.SymbolNotes+s.SymbolDenies < 1 || !slices.Contains(s.Forecast.ActualPairs, Pair{A: "#1", B: "#2"}) {
		t.Errorf("signature, symbol claims: want a note or deny recorded as the #1–#2 collision: %+v", s)
	}
}
