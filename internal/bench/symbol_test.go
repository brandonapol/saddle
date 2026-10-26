package bench

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/forecast"
)

func TestCheckAcceptsSymbolGranularity(t *testing.T) {
	for _, g := range []string{"", "file", "symbol"} {
		if err := (Options{Granularity: g}).Check(); err != nil {
			t.Errorf("granularity %q: %v", g, err)
		}
	}
	if err := (Options{Granularity: "line"}).Check(); err == nil {
		t.Error("granularity line was accepted")
	}
}

func TestClaimsTableSetsGranularity(t *testing.T) {
	if got := claimsTable("file"); got != "" {
		t.Errorf("file mode wrote %q; want the default left alone", got)
	}
	if got := claimsTable("symbol"); !strings.Contains(got, "[claims]") || !strings.Contains(got, `granularity = "symbol"`) {
		t.Errorf("symbol mode wrote %q", got)
	}
}

// symbolLog is same-file and signature in one symbol-mode run: #2 writes
// #1's claimed file and rule 1 lets it (a deny turned allow); #3 writes
// Rect.Area that #1 already changed and rule 1 denies it; #4 calls a
// signature #1 is changing and gets a note.
func symbolLog() Log {
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	ev := func(id int64, task, kind, data string) Event {
		return Event{ID: id, TS: t0.Add(time.Duration(id) * time.Second), Task: task, Kind: kind, Data: data}
	}
	return Log{
		Scenario: "sym", Adapter: "fake", Granularity: "symbol",
		Started: t0, Ended: t0.Add(30 * time.Second),
		Expected: Expected{Overlap: "low", SafeParallelism: 2},
		Plan: Plan{Tickets: []PlannedTicket{
			{Number: 1, Key: "#1", Claims: []string{"shapes/shapes.go"}, Files: []string{"shapes/shapes.go"}},
			{Number: 2, Key: "#2", Claims: []string{"shapes/shapes.go"}, Files: []string{"shapes/shapes.go"}},
			{Number: 3, Key: "#3", Claims: []string{"shapes/shapes.go"}, Files: []string{"shapes/shapes.go"}},
			{Number: 4, Key: "#4", Claims: []string{"extra/**"}, Files: []string{"extra/twice.go"}},
		}},
		Tasks: []TaskRecord{
			{Ticket: 1, Task: "t1", Status: "landed"},
			{Ticket: 2, Task: "t2", Status: "landed"},
			{Ticket: 3, Task: "t3", Status: "running"},
			{Ticket: 4, Task: "t4", Status: "landed"},
		},
		Events: []Event{
			ev(1, "t1", "spawn", ""),
			ev(2, "t2", "spawn", ""),
			ev(3, "t2", "deny", "shapes/shapes.go"),
			ev(4, "t2", "symbol_fallback", "stale-overwrite shapes/shapes.go: same blob"),
			ev(5, "t2", "symbol_allow", "symbol-share shapes/shapes.go: t1"),
			ev(6, "t3", "spawn", ""),
			ev(7, "t3", "deny", "shapes/shapes.go"),
			ev(8, "t3", "symbol_deny", "symbol-share shapes/shapes.go: Rect.Area with t1"),
			ev(9, "t4", "spawn", ""),
			ev(10, "t4", "symbol_note", "t1 func Scale units/scale.go abc"),
			ev(11, "t4", "symbol_fallback", "stale-assumption extra/twice_test.go: parse error in a changed range"),
			ev(12, "t1", "landed", ""),
			ev(13, "t2", "landed", ""),
			ev(14, "t4", "landed", ""),
		},
		IntegrationGreen: true,
	}
}

func TestScoreSymbolEvents(t *testing.T) {
	r := Score(symbolLog())
	if r.Granularity != "symbol" {
		t.Errorf("granularity = %q", r.Granularity)
	}
	// t2's deny was overridden by rule 1; t3's deny and its symbol_deny are
	// one refused write.
	if r.DeniedWrites != 1 || r.SymbolShares != 1 || r.SymbolDenies != 1 || r.SymbolNotes != 1 || r.SymbolFallbacks != 2 {
		t.Errorf("denied %d, shares %d, symbol denies %d, notes %d, fallbacks %d; want 1, 1, 1, 1, 2",
			r.DeniedWrites, r.SymbolShares, r.SymbolDenies, r.SymbolNotes, r.SymbolFallbacks)
	}
	want := []Pair{{A: "#1", B: "#3"}, {A: "#1", B: "#4"}}
	if !reflect.DeepEqual(r.Forecast.ActualPairs, want) {
		t.Errorf("actual pairs = %v, want %v (a share is not a collision)", r.Forecast.ActualPairs, want)
	}
	if r.PeakParallelism != 4 || !slices.Equal(r.NeededHelp, []string{"#3"}) {
		t.Errorf("peak %d, needed help %v", r.PeakParallelism, r.NeededHelp)
	}
	md := Markdown(r)
	for _, s := range []string{"symbol claims", "| Symbol shares | 1 |", "| Symbol denies | 1 |", "| Symbol notes | 1 |"} {
		if !strings.Contains(md, s) {
			t.Errorf("markdown lacks %q:\n%s", s, md)
		}
	}
	if strings.Contains(Markdown(Score(fixedLog())), "Symbol shares") {
		t.Error("a file-mode summary shows symbol rows")
	}
}

func TestScoreStaleSymbolDenyPairsWithTheChanger(t *testing.T) {
	l := symbolLog()
	l.Plan.Tickets[0].Files = []string{"units/scale.go"}
	l.Plan.Tickets[0].Symbols = []forecast.Symbol{{Path: "units/scale.go", Name: "Scale", Kind: "func"}}
	l.Events = []Event{
		{ID: 1, Task: "t1", Kind: "spawn"}, {ID: 2, Task: "t4", Kind: "spawn"},
		{ID: 3, Task: "t1", Kind: "landed"},
		{ID: 4, Task: "t4", Kind: "symbol_deny", Data: "stale-assumption extra/twice.go: Scale"},
		{ID: 5, Task: "t2", Kind: "symbol_deny", Data: "stale-overwrite units/scale.go: Scale"},
	}
	r := Score(l)
	if r.DeniedWrites != 2 || r.SymbolDenies != 2 {
		t.Errorf("denied %d, symbol denies %d; want 2, 2", r.DeniedWrites, r.SymbolDenies)
	}
	want := []Pair{{A: "#1", B: "#2"}, {A: "#1", B: "#4"}}
	if !reflect.DeepEqual(r.Forecast.ActualPairs, want) {
		t.Errorf("actual pairs = %v, want %v", r.Forecast.ActualPairs, want)
	}
}

func TestCompareTable(t *testing.T) {
	file := Score(fixedLog())
	file.Scenario = "same-file"
	sym := Score(symbolLog())
	sym.Scenario = "same-file"
	md := CompareMarkdown([]Comparison{{Scenario: "same-file", File: &file, Symbol: &sym}, {Scenario: "rename", Err: "boom"}})
	for _, s := range []string{
		"| Scenario |", "file → symbol",
		"| same-file | 2/3 → 3/4 | 2 → 4 (2) |",
		"| rename | error: boom |",
	} {
		if !strings.Contains(md, s) {
			t.Errorf("table lacks %q:\n%s", s, md)
		}
	}
}

// A wait_for hold on a ticket autopilot keeps back for overlapping claims
// would never release while the live task runs, so the hold stands.
func TestHeldBack(t *testing.T) {
	overlap := "its claim shapes/shapes.go overlaps t1's shapes/shapes.go"
	for _, c := range []struct {
		task, why string
		want      bool
	}{
		{"", overlap, true},
		{"t2", overlap, false},
		{"", "at the cap (2 running)", false},
		{"", "", false},
	} {
		if got := heldBack(c.task, c.why); got != c.want {
			t.Errorf("heldBack(%q, %q) = %v, want %v", c.task, c.why, got, c.want)
		}
	}
}

func TestLoadRejectsStartAfterCycle(t *testing.T) {
	dir := t.TempDir()
	sd := filepath.Join(ScenariosDir(dir), "cyc", "tickets")
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"01.md", "02.md"} {
		if err := os.WriteFile(filepath.Join(sd, f), []byte("# T\n\nclaims: a/**\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	toml := "overlap = \"none\"\nsafe_parallelism = 1\n" +
		"[[ticket]]\nfile = \"01.md\"\nstart_after = [2]\n[[ticket.edit]]\npath = \"a/x\"\nnew = \"x\"\n" +
		"[[ticket]]\nfile = \"02.md\"\nstart_after = [1, 3]\n[[ticket.edit]]\npath = \"a/y\"\nnew = \"y\"\n"
	if err := os.WriteFile(filepath.Join(sd, "..", "scenario.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir, "cyc")
	if err == nil {
		t.Fatal("loaded a start_after cycle")
	}
	for _, want := range []string{"starts after #3, which is not another ticket", "start_after cycle"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

func TestStepsHoldTheStart(t *testing.T) {
	tk := &Ticket{Number: 2, Title: "x", StartAfter: []int{1}}
	st := steps(tk, Change{Writes: map[string]string{"a.go": "package a\n"}})
	if st[0].Wait != start {
		t.Fatalf("first step = %+v, want a wait for %q", st[0], start)
	}
}
