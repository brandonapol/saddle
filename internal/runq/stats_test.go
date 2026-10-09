package runq

import (
	"path/filepath"
	"testing"
	"time"
)

var statsNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

type seed struct {
	class                  string
	ago                    time.Duration // when the run ended, before statsNow
	waited, held, cpu, rss int64         // ms, ms, ms, KB
}

func seeded(t *testing.T, rows []seed) *Queue {
	t.Helper()
	q := open(t, Options{Path: filepath.Join(t.TempDir(), "q.db"), Now: func() time.Time { return statsNow }})
	for _, r := range rows {
		_, err := q.db.Exec(`INSERT INTO history(class,label,cmd,waited_ms,held_ms,ended,how,cpu_ms,max_rss_kb) VALUES(?,?,?,?,?,?,?,?,?)`,
			r.class, "t", "c", r.waited, r.held, statsNow.Add(-r.ago).UnixMilli(), "ok", r.cpu, r.rss)
		if err != nil {
			t.Fatal(err)
		}
	}
	return q
}

func TestStats(t *testing.T) {
	h := time.Hour
	tests := []struct {
		name  string
		rows  []seed
		since time.Duration
		want  map[string]ClassStats
	}{
		{"empty", nil, 7 * 24 * h, map[string]ClassStats{}},
		{
			"percentiles, cores, rss",
			[]seed{
				{"go-test", 10 * h, 0, 1000, 2000, 100},
				{"go-test", 20 * h, 100, 2000, 6000, 200},
				{"go-test", 30 * h, 200, 3000, 0, 0}, // no cpu/rss reading
				{"go-test", 40 * h, 300, 4000, 8000, 400},
			},
			7 * 24 * h,
			map[string]ClassStats{"go-test": {Class: "go-test", Runs: 4, HeldP50MS: 2000, HeldP95MS: 4000,
				WaitedP50MS: 100, WaitedP95MS: 300, AvgCores: 16000.0 / 7000, RSSP95KB: 400}},
		},
		{
			"since excludes old rows",
			[]seed{{"a", 10 * h, 0, 1000, 1000, 10}, {"a", 10 * 24 * h, 0, 9000, 9000, 90}},
			24 * h,
			map[string]ClassStats{"a": {Class: "a", Runs: 1, HeldP50MS: 1000, HeldP95MS: 1000, AvgCores: 1, RSSP95KB: 10}},
		},
		{
			"overlap across classes",
			[]seed{
				// a ran [0,4s); b ran [2s,6s): a overlapped half, b half.
				{"a", h, 0, 4000, 0, 0},
				{"b", h - 2*time.Second, 0, 4000, 0, 0},
				// c ran alone a day earlier.
				{"c", 24 * h, 0, 5000, 0, 0},
			},
			7 * 24 * h,
			map[string]ClassStats{
				"a": {Class: "a", Runs: 1, HeldP50MS: 4000, HeldP95MS: 4000, Overlap: 0.5},
				"b": {Class: "b", Runs: 1, HeldP50MS: 4000, HeldP95MS: 4000, Overlap: 0.5},
				"c": {Class: "c", Runs: 1, HeldP50MS: 5000, HeldP95MS: 5000},
			},
		},
		{
			"overlap counts covered time once",
			[]seed{
				{"a", h, 0, 10000, 0, 0},
				{"b", h, 0, 6000, 0, 0},                 // [4s,10s)
				{"b", h - 2*time.Second, 0, 6000, 0, 0}, // [6s,12s) within a: [6,10)
			},
			7 * 24 * h,
			map[string]ClassStats{
				"a": {Class: "a", Runs: 1, HeldP50MS: 10000, HeldP95MS: 10000, Overlap: 0.6},
				"b": {Class: "b", Runs: 2, HeldP50MS: 6000, HeldP95MS: 6000, Overlap: 10.0 / 12},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := seeded(t, tt.rows).Stats(tt.since)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d classes %+v, want %d", len(got), got, len(tt.want))
			}
			for _, g := range got {
				w, ok := tt.want[g.Class]
				if !ok {
					t.Fatalf("unexpected class %q", g.Class)
				}
				if g.AvgCores-w.AvgCores > 1e-9 || w.AvgCores-g.AvgCores > 1e-9 || g.Overlap-w.Overlap > 1e-9 || w.Overlap-g.Overlap > 1e-9 {
					t.Errorf("%s: cores/overlap got %v/%v want %v/%v", g.Class, g.AvgCores, g.Overlap, w.AvgCores, w.Overlap)
				}
				g.AvgCores, w.AvgCores, g.Overlap, w.Overlap = 0, 0, 0, 0
				if g != w {
					t.Errorf("%s: got %+v want %+v", g.Class, g, w)
				}
			}
		})
	}
}

func TestOpenPrunesOldHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	now := func() time.Time { return statsNow }
	q := open(t, Options{Path: path, Now: now})
	for _, ago := range []time.Duration{time.Hour, 29 * 24 * time.Hour, 31 * 24 * time.Hour, 90 * 24 * time.Hour} {
		if _, err := q.db.Exec(`INSERT INTO history(class,label,cmd,waited_ms,held_ms,ended,how) VALUES('a','t','c',0,1,?, 'ok')`, statsNow.Add(-ago).UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	_ = q.Close()
	q2 := open(t, Options{Path: path, Now: now})
	var n int
	if err := q2.db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("history rows after open = %d, want 2", n)
	}
}
