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
