package app

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/forecast"
	"github.com/brandonapol/saddle/internal/gitx"
)

const forecastMeter = "package billing\n\nfunc Meter() int {\n\treturn 1\n}\n\nfunc Rate() int {\n\treturn 2\n}\n"

// forecastSetup spawns two workers on a repo whose billing/meter.go has two
// functions, and edits it in each worktree with edit.
func forecastSetup(t *testing.T, edit1, edit2 func(string) string) (*App, string, string) {
	t.Helper()
	a, _ := setup(t)
	write(t, a.Root, "billing/meter.go", forecastMeter)
	commitAll(t, a.Root, "two funcs")
	t1, err := a.Spawn(SpawnReq{ID: "t1", Title: "one"})
	must(t, err)
	t2, err := a.Spawn(SpawnReq{ID: "t2", Title: "two"})
	must(t, err)
	write(t, t1.Worktree, "billing/meter.go", edit1(forecastMeter))
	write(t, t2.Worktree, "billing/meter.go", edit2(forecastMeter))
	commitAll(t, t2.Worktree, "committed edit") // committed and dirty both count
	return a, t1.ID, t2.ID
}

func editMeter(s string) string { return strings.Replace(s, "return 1", "return 10", 1) }
func editRate(s string) string  { return strings.Replace(s, "return 2", "return 20", 1) }

func TestForecastDifferentFunctionsOfOneFileIsLow(t *testing.T) {
	t.Parallel()
	a, _, _ := forecastSetup(t, editMeter, editRate)
	f, err := a.Forecast()
	must(t, err)
	if f.Tasks != 2 || len(f.Result.Pairs) != 1 {
		t.Fatalf("forecast = %+v, want one pair of two tasks", f)
	}
	p := f.Result.Pairs[0]
	if p.Level.String() != "low" || !strings.Contains(p.Reason, "billing/meter.go") {
		t.Fatalf("pair = %+v, want low naming billing/meter.go", p)
	}
	if v := f.View(); v != nil {
		t.Fatalf("view = %+v, want nil with no medium+ pairs", v)
	}
	if got := f.Involving("t1"); len(got) != 1 || got[0].Level != "low" {
		t.Fatalf("involving t1 = %+v", got)
	}
}

func TestForecastSameFunctionIsCritical(t *testing.T) {
	t.Parallel()
	a, _, _ := forecastSetup(t, editMeter, func(s string) string { return strings.Replace(s, "return 1", "return 11", 1) })
	f, err := a.Forecast()
	must(t, err)
	if len(f.Result.Pairs) != 1 {
		t.Fatalf("pairs = %+v, want one", f.Result.Pairs)
	}
	p := f.Result.Pairs[0]
	if p.Level.String() != "critical" || !strings.Contains(p.Reason, "Meter") {
		t.Fatalf("pair = %+v, want critical naming Meter", p)
	}
	v := f.View()
	if v == nil || len(v.Pairs) != 1 || v.Pairs[0] != (ForecastPair{A: "t1", B: "t2", Level: "critical", Reason: p.Reason}) || v.Safe != 0 {
		t.Fatalf("view = %+v", v)
	}
	if got := f.Involving("t3"); len(got) != 0 {
		t.Fatalf("involving t3 = %+v, want none", got)
	}
}

// A worktree whose snapshot fails or stalls is scored on claims alone.
// Not parallel: it stubs package state.
func TestForecastSnapshotFailureOrTimeoutUsesClaims(t *testing.T) {
	for _, tc := range []struct {
		name string
		snap func(string, gitx.SnapshotOptions) (gitx.State, error)
	}{
		{"fails", func(string, gitx.SnapshotOptions) (gitx.State, error) {
			return gitx.State{}, errors.New("boom")
		}},
		{"times out", func(dir string, o gitx.SnapshotOptions) (gitx.State, error) {
			time.Sleep(time.Second)
			return gitx.Snapshot(dir, o)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old, oldBudget := forecastSnapshot, ForecastBudget
			forecastSnapshot, ForecastBudget = tc.snap, 100*time.Millisecond
			t.Cleanup(func() { forecastSnapshot, ForecastBudget = old, oldBudget })

			a, t1, t2 := forecastSetup(t, editMeter, editRate)
			_, err := a.Claim(t1, []string{"billing/**"})
			must(t, err)
			_, err = a.Claim(t2, []string{"README.md"})
			must(t, err)
			start := time.Now()
			f, err := a.Forecast()
			must(t, err)
			if d := time.Since(start); d > 900*time.Millisecond {
				t.Fatalf("forecast took %v past its budget", d)
			}
			if f.Tasks != 2 || len(f.Result.Pairs) != 0 {
				t.Fatalf("forecast = %+v, want two tasks scored on disjoint claims alone", f)
			}
		})
	}
}

func TestForecastViewCapsAndCountsSafe(t *testing.T) {
	t.Parallel()
	var r forecast.Result
	for i := range 12 {
		r.Pairs = append(r.Pairs, forecast.Forecast{A: "t1", B: fmt.Sprintf("t%d", i+2), Level: forecast.High, Reason: "r"})
	}
	r.Pairs = append(r.Pairs, forecast.Forecast{A: "t2", B: "t3", Level: forecast.Low, Reason: "low"})
	v := Forecast{Tasks: 14, Result: r}.View()
	// 14 tasks make 91 pairs: 12 high, so 79 are low or none.
	if v == nil || len(v.Pairs) != 10 || v.More != 2 || v.Safe != 79 {
		t.Fatalf("view = %+v, want 10 pairs, 2 more, 79 safe", v)
	}
}
