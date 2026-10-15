package app

import (
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/forecast"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/gitx/symbols"
	"github.com/brandonapol/saddle/internal/store"
)

// ForecastBudget bounds how long Forecast waits for worktree snapshots, all
// of them together. A worktree that misses it is scored on its claims alone.
var ForecastBudget = 2 * time.Second

// forecastHistory is how many landed commits feed co-change history.
const forecastHistory = 200

// forecastTop caps the pairs status lists.
const forecastTop = 10

// forecastSnapshot reads one worktree; tests stub it to fail or stall.
var forecastSnapshot = gitx.Snapshot

// Forecast is a collision forecast over the live workers.
type Forecast struct {
	Tasks  int // live workers scored
	Result forecast.Result
}

// ForecastPair is one predicted collision, as status and brief show it.
type ForecastPair struct {
	A      string `json:"a"`
	B      string `json:"b"`
	Level  string `json:"level" jsonschema:"low, medium, high or critical"`
	Reason string `json:"reason"`
}

// ForecastView is what status shows of a forecast.
type ForecastView struct {
	Pairs []ForecastPair `json:"pairs" jsonschema:"the riskiest pairs of live tasks, medium and above, at most 10, riskiest first"`
	More  int            `json:"more,omitempty" jsonschema:"medium-and-above pairs left out past the first 10"`
	Safe  int            `json:"safe" jsonschema:"pairs of live tasks scored low or none: safe side by side"`
}

// Forecast scores every pair of live workers for collisions from their
// claims, the files and symbols their worktrees changed, renames, landed
// history and the serial globs. It never fails on a worktree: one whose
// snapshot errors or misses ForecastBudget contributes its claims alone.
func (a *App) Forecast() (Forecast, error) {
	ts, err := a.Store.Tasks()
	if err != nil {
		return Forecast{}, err
	}
	cl, err := a.Store.Claims()
	if err != nil {
		return Forecast{}, err
	}
	var in forecast.Input
	var dirs []string // worktree per task in in.Tasks; empty for none
	for _, t := range ts {
		if forecastLive(t) {
			in.Tasks = append(in.Tasks, forecast.Task{ID: t.ID, Claims: cl[t.ID]})
			dirs = append(dirs, t.Worktree)
		}
	}
	type snap struct {
		i  int
		ok bool
		st gitx.State
		ch []string // committed on the branch since its base
	}
	// Buffered so a snapshot that finishes past the budget never blocks.
	results := make(chan snap, len(dirs))
	pending := 0
	x, snapshot := symbols.Default(), forecastSnapshot
	for i, dir := range dirs {
		if dir == "" {
			continue
		}
		pending++
		go func() {
			s := snap{i: i}
			st, err := snapshot(dir, gitx.SnapshotOptions{Integration: a.Cfg.Integration, Symbols: x})
			if err == nil {
				s.ok, s.st = true, st
				if st.Base != "" && st.Head != "" && st.Base != st.Head {
					s.ch, _ = gitx.ChangedFiles(dir, st.Base, st.Head)
				}
			}
			results <- s
		}()
	}
	deadline := time.After(ForecastBudget)
wait:
	for ; pending > 0; pending-- {
		select {
		case s := <-results:
			if s.ok {
				fillForecastTask(&in.Tasks[s.i], s.st, s.ch)
			}
		case <-deadline:
			break wait
		}
	}
	in.History = a.forecastHistory()
	in.Serial = a.Cfg.Serial
	return Forecast{Tasks: len(in.Tasks), Result: forecast.Run(in, forecast.Config{})}, nil
}

// forecastLive reports whether t is a worker whose changes have not landed.
func forecastLive(t store.Task) bool {
	if t.Role == store.RoleOrchestrator {
		return false
	}
	switch t.Status {
	case store.Landed, store.Killed, store.NoChange, StatusFailed:
		return false
	}
	return true
}

func fillForecastTask(t *forecast.Task, st gitx.State, committed []string) {
	for _, f := range st.Files {
		t.Files = append(t.Files, f.Path)
	}
	t.Files = append(t.Files, committed...)
	for _, s := range st.Symbols {
		t.Symbols = append(t.Symbols, forecast.Symbol{Path: s.Path, Name: s.Name, Kind: s.Kind})
	}
	for _, r := range st.Renames {
		t.Renames = append(t.Renames, forecast.Rename{Old: r.Old, New: r.New})
	}
}

// forecastHistory reads the files of the last landed commits on integration.
func (a *App) forecastHistory() []forecast.Commit {
	out, err := gitx.Run(a.Root, "log", "-n", strconv.Itoa(forecastHistory), "--name-only", "--format=%x00", a.Cfg.Integration, "--")
	if err != nil {
		return nil
	}
	var hist []forecast.Commit
	for _, block := range strings.Split(out, "\x00") {
		var c forecast.Commit
		for _, l := range strings.Split(block, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				c.Files = append(c.Files, l)
			}
		}
		if len(c.Files) > 0 {
			hist = append(hist, c)
		}
	}
	return hist
}

// View is what status shows: medium-and-above pairs, the riskiest
// forecastTop of them, and how many live pairs are safe side by side. It is
// nil when no pair reaches medium.
func (f Forecast) View() *ForecastView {
	v := ForecastView{Safe: f.Tasks * (f.Tasks - 1) / 2}
	for _, p := range f.Result.Pairs {
		if p.Level < forecast.Medium {
			continue
		}
		v.Safe--
		if len(v.Pairs) == forecastTop {
			v.More++
			continue
		}
		v.Pairs = append(v.Pairs, pairView(p))
	}
	if len(v.Pairs) == 0 {
		return nil
	}
	return &v
}

// Involving lists the scored pairs that include task, riskiest first.
func (f Forecast) Involving(task string) []ForecastPair {
	var out []ForecastPair
	for _, p := range f.Result.Pairs {
		if p.A == task || p.B == task {
			out = append(out, pairView(p))
		}
	}
	return out
}

func pairView(p forecast.Forecast) ForecastPair {
	return ForecastPair{A: p.A, B: p.B, Level: p.Level.String(), Reason: p.Reason}
}
