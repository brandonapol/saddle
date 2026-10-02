// Package forecast predicts which live or planned tasks will collide, with no
// LLM. It scores every pair of tasks from their claims, the files and symbols
// they have changed so far, in-progress renames, imports and landed history.
//
// Everything is a pure function of its Input: the same input always yields
// the same Result, in the same order, with the same reason strings, so the
// reactor can log a forecast and replay it in tests.
package forecast

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/brandonapol/saddle/internal/claims"
)

// Task is what the forecaster knows about one task. A planned task has only
// Claims; an in-flight task also has the changes in its worktree.
type Task struct {
	ID      string
	Claims  []string
	Files   []string // changed since the base: dirty or committed on the branch
	Symbols []Symbol // touched symbols, as gitx.State.Symbols reports them
	Renames []Rename // in-progress renames and moves
	// Imports maps a changed file to the repo-relative package directories it
	// imports. Files not listed import nothing the forecaster knows about.
	Imports map[string][]string
}

// Symbol is a touched function, method or type.
type Symbol struct{ Path, Name, Kind string }

// Rename is a file rename or move.
type Rename struct{ Old, New string }

// Commit is one landed commit, for co-change history.
type Commit struct{ Files []string }

// Input is everything one forecast run looks at.
type Input struct {
	Tasks   []Task
	History []Commit // landed commits, any order
	Serial  []string // globs only the merge train may write
}

// Kind is the rule that produced a piece of evidence.
type Kind string

const (
	KindSymbol      Kind = "symbol"       // both tasks touch the same symbol
	KindRename      Kind = "rename"       // one task moves a path the other references
	KindFile        Kind = "file"         // both tasks changed the same file
	KindClaimedFile Kind = "claimed-file" // one task edits a file under the other's claim
	KindImport      Kind = "import"       // one task imports a package the other changes
	KindClaims      Kind = "claims"       // the tasks' claims overlap
	KindCoChange    Kind = "co-change"    // the tasks' files often land together
)

// DefaultWeights is the score each kind of evidence carries.
var DefaultWeights = map[Kind]int{
	KindSymbol:      100,
	KindRename:      100,
	KindFile:        70,
	KindClaimedFile: 50,
	KindImport:      40,
	KindClaims:      30,
	KindCoChange:    20,
}

// Level buckets a score.
type Level int

const (
	None Level = iota
	Low
	Medium
	High
	Critical
)

func (l Level) String() string {
	return [...]string{"none", "low", "medium", "high", "critical"}[l]
}

// Thresholds are the minimum scores for each level. Pairs scoring below Low
// are dropped.
type Thresholds struct{ Low, Medium, High, Critical int }

// DefaultThresholds puts each default weight in the level the issue names.
var DefaultThresholds = Thresholds{Low: 20, Medium: 40, High: 70, Critical: 90}

// Config tunes a run. The zero value uses the defaults.
type Config struct {
	Thresholds  Thresholds   // zero means DefaultThresholds
	Weights     map[Kind]int // overrides DefaultWeights per kind
	CoChangeMin int          // landed commits a file pair needs to count; zero means 2
}

func (c Config) weight(k Kind) int {
	if w, ok := c.Weights[k]; ok {
		return w
	}
	return DefaultWeights[k]
}

func (c Config) level(score int) Level {
	t := c.Thresholds
	if t == (Thresholds{}) {
		t = DefaultThresholds
	}
	switch {
	case score >= t.Critical:
		return Critical
	case score >= t.High:
		return High
	case score >= t.Medium:
		return Medium
	case score >= t.Low:
		return Low
	}
	return None
}

// Evidence is one reason two tasks may collide.
type Evidence struct {
	Kind  Kind
	Score int
	Text  string
}

// Forecast is the predicted collision between tasks A and B, with A < B.
// Reason is the strongest evidence's text; Evidence lists all of it,
// strongest first.
type Forecast struct {
	A, B     string
	Score    int
	Level    Level
	Reason   string
	Evidence []Evidence
}

// Route is a write to a serial path, which only the merge train may make.
type Route struct{ Task, Path, Glob string }

// Result is a forecast run's output. Pairs are sorted by score, highest
// first, then by task ids; Serial by task then path.
type Result struct {
	Pairs  []Forecast
	Serial []Route
}

// Run forecasts collisions between every pair of tasks in in.
func Run(in Input, cfg Config) Result {
	tasks := make([]task, len(in.Tasks))
	for i, t := range in.Tasks {
		tasks[i] = normalize(t)
	}
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].id < tasks[j].id })
	co := coChanges(in.History)
	min := cfg.CoChangeMin
	if min <= 0 {
		min = 2
	}

	var res Result
	for i := range tasks {
		for j := i + 1; j < len(tasks); j++ {
			if f, ok := score(&tasks[i], &tasks[j], co, min, cfg); ok {
				res.Pairs = append(res.Pairs, f)
			}
		}
	}
	sort.SliceStable(res.Pairs, func(i, j int) bool {
		a, b := res.Pairs[i], res.Pairs[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.A != b.A {
			return a.A < b.A
		}
		return a.B < b.B
	})
	res.Serial = serial(tasks, in.Serial)
	return res
}

// task is a Task with sorted, cleaned, deduplicated fields.
type task struct {
	id      string
	claims  []string
	files   []string
	fileSet map[string]bool
	dirs    map[string][]string // directory -> changed files directly in it, sorted
	symbols map[Symbol]bool
	renames []Rename
	imports []imp
}

type imp struct{ file, pkg string }

func normalize(t Task) task {
	n := task{id: t.ID, fileSet: map[string]bool{}, dirs: map[string][]string{}, symbols: map[Symbol]bool{}}
	n.claims = uniq(t.Claims)
	n.files = uniq(t.Files)
	for _, f := range n.files {
		n.fileSet[f] = true
		d := dir(f)
		n.dirs[d] = append(n.dirs[d], f)
	}
	for _, s := range t.Symbols {
		s.Path = claims.Clean(s.Path)
		n.symbols[s] = true
	}
	seen := map[Rename]bool{}
	for _, r := range t.Renames {
		r = Rename{claims.Clean(r.Old), claims.Clean(r.New)}
		if !seen[r] {
			seen[r] = true
			n.renames = append(n.renames, r)
		}
	}
	sort.Slice(n.renames, func(i, j int) bool {
		if n.renames[i].Old != n.renames[j].Old {
			return n.renames[i].Old < n.renames[j].Old
		}
		return n.renames[i].New < n.renames[j].New
	})
	for f, pkgs := range t.Imports {
		f = claims.Clean(f)
		for _, p := range uniq(pkgs) {
			n.imports = append(n.imports, imp{f, p})
		}
	}
	sort.Slice(n.imports, func(i, j int) bool {
		if n.imports[i].file != n.imports[j].file {
			return n.imports[i].file < n.imports[j].file
		}
		return n.imports[i].pkg < n.imports[j].pkg
	})
	return n
}

func uniq(ss []string) []string {
	m := map[string]bool{}
	var out []string
	for _, s := range ss {
		s = claims.Clean(s)
		if s != "" && s != "." && !m[s] {
			m[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func dir(p string) string {
	if d := path.Dir(p); d != "." {
		return d
	}
	return ""
}

func score(a, b *task, co map[[2]string]int, coMin int, cfg Config) (Forecast, bool) {
	var ev []Evidence
	add := func(k Kind, format string, args ...any) {
		ev = append(ev, Evidence{Kind: k, Score: cfg.weight(k), Text: fmt.Sprintf(format, args...)})
	}

	for s := range a.symbols {
		if b.symbols[s] {
			add(KindSymbol, "same symbol: %s %s (%s)", s.Path, s.Name, s.Kind)
		}
	}
	for _, f := range a.files {
		if b.fileSet[f] {
			add(KindFile, "same file: %s", f)
		}
	}
	for _, x := range [][2]*task{{a, b}, {b, a}} {
		mover, other := x[0], x[1]
		for _, r := range mover.renames {
			if t, ok := references(other, r); ok {
				add(KindRename, "%s moves %s -> %s, which %s %s", mover.id, r.Old, r.New, other.id, t)
			}
		}
		editor, owner := x[0], x[1]
		for _, f := range editor.files {
			if owner.fileSet[f] {
				continue // already same-file evidence
			}
			for _, g := range owner.claims {
				if claims.Match(g, f) {
					add(KindClaimedFile, "%s edits %s under %s's claim %s", editor.id, f, owner.id, g)
					break
				}
			}
		}
		for _, im := range editor.imports {
			if !editor.fileSet[im.file] {
				continue
			}
			if fs := owner.dirs[im.pkg]; len(fs) > 0 {
				add(KindImport, "%s's %s imports %s, which %s changes (%s)", editor.id, im.file, im.pkg, owner.id, fs[0])
			}
		}
	}
	for _, ga := range a.claims {
		for _, gb := range b.claims {
			if claims.Overlap(ga, gb) {
				add(KindClaims, "claims overlap: %s %s and %s %s", a.id, ga, b.id, gb)
			}
		}
	}
	for _, fa := range a.files {
		for _, fb := range b.files {
			if fa == fb {
				continue
			}
			k := [2]string{fa, fb}
			if fb < fa {
				k = [2]string{fb, fa}
			}
			if n := co[k]; n >= coMin {
				add(KindCoChange, "landed together %d times: %s and %s", n, k[0], k[1])
			}
		}
	}

	if len(ev) == 0 {
		return Forecast{}, false
	}
	sort.Slice(ev, func(i, j int) bool {
		if ev[i].Score != ev[j].Score {
			return ev[i].Score > ev[j].Score
		}
		return ev[i].Text < ev[j].Text
	})
	f := Forecast{A: a.id, B: b.id, Score: ev[0].Score, Evidence: ev}
	f.Level = cfg.level(f.Score)
	if f.Level == None {
		return Forecast{}, false
	}
	f.Reason = ev[0].Text
	if len(ev) > 1 {
		f.Reason += fmt.Sprintf(" (+%d more)", len(ev)-1)
	}
	return f, true
}

// references reports how t refers to the source of rename r, if it does.
func references(t *task, r Rename) (string, bool) {
	if t.fileSet[r.Old] {
		return "edits", true
	}
	for _, g := range t.claims {
		if claims.Match(g, r.Old) {
			return "claims (" + g + ")", true
		}
	}
	if from := dir(r.Old); from != dir(r.New) {
		for _, im := range t.imports {
			if im.pkg == from && t.fileSet[im.file] {
				return "imports from " + im.file, true
			}
		}
	}
	return "", false
}

// coChanges counts, for each pair of files, the landed commits that changed both.
func coChanges(hist []Commit) map[[2]string]int {
	co := map[[2]string]int{}
	for _, c := range hist {
		fs := uniq(c.Files)
		for i := range fs {
			for j := i + 1; j < len(fs); j++ {
				co[[2]string{fs[i], fs[j]}]++
			}
		}
	}
	return co
}

func serial(tasks []task, globs []string) []Route {
	var out []Route
	for _, t := range tasks {
		for _, f := range t.files {
			for _, g := range globs {
				if claims.Match(g, f) {
					out = append(out, Route{Task: t.id, Path: f, Glob: strings.TrimSpace(g)})
					break
				}
			}
		}
	}
	return out
}
