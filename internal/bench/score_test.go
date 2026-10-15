package bench

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fixedLog is a three-ticket run with one of every kind of trouble:
// #1 and #2 share shapes/shapes.go, #3 writes outside its claim.
func fixedLog() Log {
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	ev := func(id int64, task, kind, data string) Event {
		return Event{ID: id, TS: t0.Add(time.Duration(id) * time.Second), Task: task, Kind: kind, Data: data}
	}
	return Log{
		Scenario: "fixed", Adapter: "fake", Granularity: "file",
		Started: t0, Ended: t0.Add(90 * time.Second),
		Expected: Expected{Overlap: "critical", SafeParallelism: 2},
		Plan: Plan{
			Tickets: []PlannedTicket{
				{Number: 1, Key: "#1", Claims: []string{"shapes/shapes.go"}, Files: []string{"shapes/shapes.go"}},
				{Number: 2, Key: "#2", Claims: []string{"shapes/**"}, Files: []string{"shapes/shapes.go", "shapes/extra.go"}},
				{Number: 3, Key: "#3", Claims: []string{"report/**"}, Files: []string{"report/r.go", "shapes/helper.go"},
					Imports: map[string][]string{"report/r.go": {"shapes"}}},
			},
			Pairs: []Pair{
				{A: "#1", B: "#2", Level: "critical"},
				{A: "#2", B: "#3", Level: "medium"},
				{A: "#1", B: "#3", Level: "low"},
			},
			Overlap: "critical",
		},
		Tasks: []TaskRecord{
			{Ticket: 1, Task: "t1", Status: "landed"},
			{Ticket: 2, Task: "t2", Status: "landed"},
			{Ticket: 3, Task: "t3", Status: "conflict"},
		},
		Events: []Event{
			ev(1, "t1", "spawn", "parent= adapter=claude"),
			ev(2, "t3", "spawn", "parent= adapter=claude"),
			ev(3, "t3", "deny", "shapes/helper.go"),
			ev(4, "t1", "done_called", ""),
			ev(5, "t1", "landed", "abc"),
			ev(6, "t2", "spawn", "parent= adapter=claude"),
			ev(7, "t2", "train_conflict", "shapes/shapes.go"),
			ev(8, "t2", "auto_rebase", "conflicts="),
			ev(9, "t2", "restack", "saddle/t2 abc → def (abort=false)"),
			ev(10, "t2", "restack_conflict", "abc123 shapes/shapes.go"),
			ev(11, "t2", "landed", "def"),
			ev(12, "t3", "train_test_failed", "go test failed"),
			ev(13, "t3", "train_escalated", "3 attempts"),
			ev(14, "t3", "bench_stale", "shapes/extra.go"),
			ev(15, "", "autopilot_tick", "noise"),
		},
		Waits: []Wait{
			{Issue: 2, Why: "its claim shapes/** overlaps t1's shapes/shapes.go"},
			{Issue: 2, Why: "its claim shapes/** overlaps t1's shapes/shapes.go"},
			{Issue: 3, Why: "at the cap (2 running)"},
		},
		IntegrationGreen: true,
	}
}

func TestScoreFixedLog(t *testing.T) {
	got := Score(fixedLog())
	want := Report{
		Scenario: "fixed", Adapter: "fake", Granularity: "file",
		WallSeconds: 90,
		Tickets:     3, Landed: 2, AllLanded: false,
		IntegrationGreen: true,
		DeniedWrites:     1,
		ClaimWaits:       1,
		TrainBounces:     2, TrainConflicts: 1, TestFailures: 1,
		Restacks: 2, RestackConflicts: 1,
		StaleEdits:      1,
		NeededHelp:      []string{"#3"},
		PeakParallelism: 2, SafeParallelism: 2,
		Forecast: ForecastScore{
			Expected: "critical", Predicted: "critical",
			PredictedPairs: []Pair{{A: "#1", B: "#2"}, {A: "#2", B: "#3"}},
			ActualPairs:    []Pair{{A: "#1", B: "#2"}, {A: "#1", B: "#3"}, {A: "#2", B: "#3"}},
			Hits:           []Pair{{A: "#1", B: "#2"}, {A: "#2", B: "#3"}},
			Misses:         []Pair{{A: "#1", B: "#3"}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", "  ")
		w, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("Score =\n%s\nwant\n%s", g, w)
	}
}

func TestScoreCleanRun(t *testing.T) {
	l := fixedLog()
	l.Tasks[2].Status = "landed"
	l.Events = []Event{
		{ID: 1, Task: "t1", Kind: "spawn"}, {ID: 2, Task: "t2", Kind: "spawn"}, {ID: 3, Task: "t3", Kind: "spawn"},
		{ID: 4, Task: "t1", Kind: "landed"}, {ID: 5, Task: "t2", Kind: "landed"}, {ID: 6, Task: "t3", Kind: "landed"},
	}
	l.Waits = nil
	r := Score(l)
	if !r.AllLanded || r.Landed != 3 || len(r.NeededHelp) != 0 || r.PeakParallelism != 3 || r.TrainBounces != 0 {
		t.Fatalf("clean run scored %+v", r)
	}
	if len(r.Forecast.ActualPairs) != 0 || len(r.Forecast.Unseen) != 2 {
		t.Fatalf("forecast = %+v, want both predicted pairs unseen", r.Forecast)
	}
}

func TestMarkdownNamesTheNumbers(t *testing.T) {
	md := Markdown(Score(fixedLog()))
	for _, want := range []string{"# Bench: fixed", "| Denied writes | 1 |", "| Tickets landed | 2/3 |", "| Needed help | #3 |",
		"| Integration tests | pass |", "| Peak parallelism | 2 (safe: 2) |", "#1–#3"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
}
