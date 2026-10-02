package forecast

import (
	"reflect"
	"testing"
)

func pair(t *testing.T, fs []Forecast, a, b string) Forecast {
	t.Helper()
	for _, f := range fs {
		if f.A == a && f.B == b {
			return f
		}
	}
	t.Fatalf("no forecast for %s/%s in %+v", a, b, fs)
	return Forecast{}
}

func TestEmptyInput(t *testing.T) {
	r := Run(Input{}, Config{})
	if len(r.Pairs) != 0 || len(r.Serial) != 0 {
		t.Fatalf("Run(empty) = %+v, want nothing", r)
	}
	r = Run(Input{Tasks: []Task{{ID: "t1", Files: []string{"a.go"}}}}, Config{})
	if len(r.Pairs) != 0 {
		t.Fatalf("one task produced pairs: %+v", r.Pairs)
	}
}

func TestDisjointTasks(t *testing.T) {
	r := Run(Input{Tasks: []Task{
		{ID: "t1", Claims: []string{"internal/a/**"}, Files: []string{"internal/a/x.go"}},
		{ID: "t2", Claims: []string{"internal/b/**"}, Files: []string{"internal/b/y.go"}},
	}}, Config{})
	if len(r.Pairs) != 0 {
		t.Fatalf("disjoint tasks collided: %+v", r.Pairs)
	}
}

func TestOverlappingClaims(t *testing.T) {
	r := Run(Input{Tasks: []Task{
		{ID: "t2", Claims: []string{"pkg/billing/invoice/**"}},
		{ID: "t1", Claims: []string{"pkg/billing/**"}},
	}}, Config{})
	f := pair(t, r.Pairs, "t1", "t2")
	if f.Level != Low {
		t.Errorf("level = %v, want low", f.Level)
	}
	if want := "claims overlap: t1 pkg/billing/** and t2 pkg/billing/invoice/**"; f.Reason != want {
		t.Errorf("reason = %q, want %q", f.Reason, want)
	}
}

func TestSameFileDirty(t *testing.T) {
	r := Run(Input{Tasks: []Task{
		{ID: "t1", Files: []string{"invoice/period.go", "a.go"}},
		{ID: "t2", Files: []string{"./invoice/period.go"}},
	}}, Config{})
	f := pair(t, r.Pairs, "t1", "t2")
	if f.Level != High {
		t.Errorf("level = %v, want high", f.Level)
	}
	if want := "same file: invoice/period.go"; f.Reason != want {
		t.Errorf("reason = %q, want %q", f.Reason, want)
	}
}

func TestFileUnderOtherClaim(t *testing.T) {
	// t2 is planned: it has claims but no edits yet.
	r := Run(Input{Tasks: []Task{
		{ID: "t1", Files: []string{"pkg/billing/x.go"}},
		{ID: "t2", Claims: []string{"pkg/billing/**"}},
	}}, Config{})
	f := pair(t, r.Pairs, "t1", "t2")
	if f.Level != Medium {
		t.Errorf("level = %v, want medium", f.Level)
	}
	if want := "t1 edits pkg/billing/x.go under t2's claim pkg/billing/**"; f.Reason != want {
		t.Errorf("reason = %q, want %q", f.Reason, want)
	}
}

func TestSameSymbol(t *testing.T) {
	r := Run(Input{Tasks: []Task{
		{ID: "t1", Files: []string{"inv.go"}, Symbols: []Symbol{{Path: "inv.go", Name: "Period", Kind: "func"}}},
		{ID: "t2", Files: []string{"inv.go"}, Symbols: []Symbol{{Path: "inv.go", Name: "Period", Kind: "func"}, {Path: "inv.go", Name: "Other", Kind: "func"}}},
	}}, Config{})
	f := pair(t, r.Pairs, "t1", "t2")
	if f.Level != Critical {
		t.Errorf("level = %v, want critical", f.Level)
	}
	if want := "same symbol: inv.go Period (func) (+1 more)"; f.Reason != want {
		t.Errorf("reason = %q, want %q", f.Reason, want)
	}
	if len(f.Evidence) != 2 || f.Evidence[1].Kind != KindFile {
		t.Errorf("evidence = %+v, want symbol then file", f.Evidence)
	}
}

func TestDifferentSymbolsSameFileIsOnlyHigh(t *testing.T) {
	r := Run(Input{Tasks: []Task{
		{ID: "t1", Files: []string{"inv.go"}, Symbols: []Symbol{{Path: "inv.go", Name: "A", Kind: "func"}}},
		{ID: "t2", Files: []string{"inv.go"}, Symbols: []Symbol{{Path: "inv.go", Name: "B", Kind: "func"}}},
	}}, Config{})
	if f := pair(t, r.Pairs, "t1", "t2"); f.Level != High {
		t.Errorf("level = %v, want high", f.Level)
	}
}

func TestImportOfChangedPackage(t *testing.T) {
	r := Run(Input{Tasks: []Task{
		{ID: "t1", Files: []string{"cmd/main.go"}, Imports: map[string][]string{"cmd/main.go": {"internal/store"}}},
		{ID: "t2", Files: []string{"internal/store/db.go", "internal/store/sub/x.go"}},
	}}, Config{})
	f := pair(t, r.Pairs, "t1", "t2")
	if f.Level != Medium {
		t.Errorf("level = %v, want medium", f.Level)
	}
	if want := "t1's cmd/main.go imports internal/store, which t2 changes (internal/store/db.go)"; f.Reason != want {
		t.Errorf("reason = %q, want %q", f.Reason, want)
	}
}

func TestRenameSourceReferenced(t *testing.T) {
	cases := []struct {
		name   string
		other  Task
		reason string
	}{
		{"edits source", Task{ID: "t1", Files: []string{"billing/meter.go"}},
			"t2 moves billing/meter.go -> pkg/billing/meter.go, which t1 edits"},
		{"claims source", Task{ID: "t1", Claims: []string{"billing/**"}},
			"t2 moves billing/meter.go -> pkg/billing/meter.go, which t1 claims (billing/**)"},
		{"imports source dir", Task{ID: "t1", Files: []string{"cmd/x.go"}, Imports: map[string][]string{"cmd/x.go": {"billing"}}},
			"t2 moves billing/meter.go -> pkg/billing/meter.go, which t1 imports from cmd/x.go"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Run(Input{Tasks: []Task{
				c.other,
				{ID: "t2", Renames: []Rename{{Old: "billing/meter.go", New: "pkg/billing/meter.go"}}},
			}}, Config{})
			f := pair(t, r.Pairs, "t1", "t2")
			if f.Level != Critical {
				t.Errorf("level = %v, want critical", f.Level)
			}
			if f.Reason != c.reason {
				t.Errorf("reason = %q, want %q", f.Reason, c.reason)
			}
		})
	}
}

func TestRenameWithinDirDoesNotHitImporters(t *testing.T) {
	r := Run(Input{Tasks: []Task{
		{ID: "t1", Files: []string{"cmd/x.go"}, Imports: map[string][]string{"cmd/x.go": {"billing"}}},
		{ID: "t2", Renames: []Rename{{Old: "billing/a.go", New: "billing/b.go"}}},
	}}, Config{})
	for _, f := range r.Pairs {
		if f.Level == Critical {
			t.Fatalf("same-dir rename flagged critical: %+v", f)
		}
	}
}

func TestLandedCoChange(t *testing.T) {
	in := Input{
		Tasks: []Task{
			{ID: "t1", Files: []string{"api/handler.go"}},
			{ID: "t2", Files: []string{"web/client.ts"}},
		},
		History: []Commit{
			{Files: []string{"api/handler.go", "web/client.ts"}},
			{Files: []string{"web/client.ts", "api/handler.go", "README.md"}},
			{Files: []string{"api/handler.go"}},
		},
	}
	f := pair(t, Run(in, Config{}).Pairs, "t1", "t2")
	if f.Level != Low {
		t.Errorf("level = %v, want low", f.Level)
	}
	if want := "landed together 2 times: api/handler.go and web/client.ts"; f.Reason != want {
		t.Errorf("reason = %q, want %q", f.Reason, want)
	}
	in.History = in.History[1:]
	if r := Run(in, Config{}); len(r.Pairs) != 0 {
		t.Errorf("one co-change below the minimum still forecast: %+v", r.Pairs)
	}
}

func TestSerialRoutes(t *testing.T) {
	r := Run(Input{
		Tasks: []Task{
			{ID: "t2", Files: []string{"go.sum", "x.go"}},
			{ID: "t1", Files: []string{"db/migrations/001.sql", "go.mod"}},
		},
		Serial: []string{"go.mod", "go.sum", "db/migrations/**"},
	}, Config{})
	want := []Route{
		{Task: "t1", Path: "db/migrations/001.sql", Glob: "db/migrations/**"},
		{Task: "t1", Path: "go.mod", Glob: "go.mod"},
		{Task: "t2", Path: "go.sum", Glob: "go.sum"},
	}
	if !reflect.DeepEqual(r.Serial, want) {
		t.Errorf("serial = %+v, want %+v", r.Serial, want)
	}
}

func TestConfigurableThresholds(t *testing.T) {
	in := Input{Tasks: []Task{
		{ID: "t1", Claims: []string{"a/**"}},
		{ID: "t2", Claims: []string{"a/b/**"}},
	}}
	cfg := Config{Thresholds: Thresholds{Low: 50, Medium: 60, High: 70, Critical: 90}}
	if r := Run(in, cfg); len(r.Pairs) != 0 {
		t.Errorf("claims overlap kept below raised Low threshold: %+v", r.Pairs)
	}
	cfg = Config{Weights: map[Kind]int{KindClaims: 95}}
	if f := pair(t, Run(in, cfg).Pairs, "t1", "t2"); f.Level != Critical || f.Score != 95 {
		t.Errorf("weighted claims = %v/%d, want critical/95", f.Level, f.Score)
	}
}

func TestDeterministicOrdering(t *testing.T) {
	tasks := []Task{
		{ID: "t3", Files: []string{"b.go", "a.go"}, Symbols: []Symbol{{Path: "a.go", Name: "F", Kind: "func"}}},
		{ID: "t1", Files: []string{"a.go", "b.go"}, Symbols: []Symbol{{Path: "a.go", Name: "F", Kind: "func"}}},
		{ID: "t2", Files: []string{"b.go"}, Claims: []string{"**/*.go"}},
	}
	first := Run(Input{Tasks: tasks}, Config{})
	// Reverse task order and per-task slices; the result must not change.
	rev := make([]Task, len(tasks))
	for i, tk := range tasks {
		files := append([]string(nil), tk.Files...)
		for l, r := 0, len(files)-1; l < r; l, r = l+1, r-1 {
			files[l], files[r] = files[r], files[l]
		}
		tk.Files = files
		rev[len(tasks)-1-i] = tk
	}
	for i := 0; i < 20; i++ {
		if got := Run(Input{Tasks: rev}, Config{}); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs:\n got %+v\nwant %+v", i, got, first)
		}
	}
	var order []string
	for _, f := range first.Pairs {
		order = append(order, f.A+"/"+f.B)
	}
	// Highest score first, then by task ids.
	if want := []string{"t1/t3", "t1/t2", "t2/t3"}; !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
	f := pair(t, first.Pairs, "t1", "t2")
	if want := "same file: b.go (+1 more)"; f.Reason != want {
		t.Errorf("reason = %q, want %q", f.Reason, want)
	}
}

func TestLevelString(t *testing.T) {
	for l, want := range map[Level]string{None: "none", Low: "low", Medium: "medium", High: "high", Critical: "critical"} {
		if l.String() != want {
			t.Errorf("%d.String() = %q, want %q", l, l.String(), want)
		}
	}
}
