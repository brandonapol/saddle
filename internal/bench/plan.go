package bench

import (
	"go/parser"
	"go/token"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/brandonapol/saddle/internal/forecast"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/gitx/symbols"
)

// PlannedTicket is a ticket as the forecaster sees it: its claims and what
// its edits touch when applied to the fixture.
type PlannedTicket struct {
	Number  int                 `json:"number"`
	Key     string              `json:"key"`
	Title   string              `json:"title"`
	Claims  []string            `json:"claims"`
	Files   []string            `json:"files"`
	Symbols []forecast.Symbol   `json:"symbols,omitempty"`
	Renames []forecast.Rename   `json:"renames,omitempty"`
	Imports map[string][]string `json:"imports,omitempty"`
	Stale   []string            `json:"stale,omitempty"`
}

// Plan is a scenario's forecast before anything runs.
type Plan struct {
	Tickets []PlannedTicket `json:"tickets"`
	Pairs   []Pair          `json:"pairs"`
	// Overlap is the worst pair's level, "none" with no pairs.
	Overlap string `json:"overlap"`
}

// Pair is a predicted collision between two tickets, A < B.
type Pair struct {
	A, B   string
	Level  string `json:",omitempty"`
	Reason string `json:",omitempty"`
}

// MakePlan applies each ticket's edits to the fixture and forecasts every
// pair, the way internal/forecast scores live tasks.
func MakePlan(fixture Tree, sc *Scenario) Plan {
	mod := modulePath(fixture)
	x := symbols.Default()
	var p Plan
	var in forecast.Input
	for _, t := range sc.Tickets {
		c := Apply(fixture, t.Edits)
		pt := PlannedTicket{Number: t.Number, Key: t.Key(), Title: t.Title, Claims: t.Claims,
			Files: c.Files(), Renames: c.Renames, Stale: c.Stale, Imports: map[string][]string{}}
		from := map[string]string{} // a moved file's source
		for _, r := range c.Renames {
			from[r.New] = r.Old
		}
		for _, f := range pt.Files {
			if isSource(c, f) {
				continue // the rename evidence covers a moved file's source
			}
			src := f
			if s, ok := from[f]; ok {
				src = s // a moved file touches only what changed in the move
			}
			old, _ := fixture.Read(src)
			nw, written := c.Writes[f]
			pt.Symbols = append(pt.Symbols, touched(x, f, old, nw)...)
			if written {
				if imps := goImports(mod, f, nw); len(imps) > 0 {
					pt.Imports[f] = imps
				}
			}
		}
		p.Tickets = append(p.Tickets, pt)
		in.Tasks = append(in.Tasks, forecast.Task{ID: pt.Key, Claims: pt.Claims, Files: pt.Files,
			Symbols: pt.Symbols, Renames: pt.Renames, Imports: pt.Imports})
	}
	res := forecast.Run(in, forecast.Config{})
	worst := forecast.None
	for _, f := range res.Pairs {
		p.Pairs = append(p.Pairs, Pair{A: f.A, B: f.B, Level: f.Level.String(), Reason: f.Reason})
		worst = max(worst, f.Level)
	}
	p.Overlap = worst.String()
	return p
}

func isSource(c Change, f string) bool {
	for _, r := range c.Renames {
		if r.Old == f {
			return true
		}
	}
	return false
}

// touched lists the symbols whose source differs between old and new.
func touched(x *symbols.Extractor, file, old, nw string) []forecast.Symbol {
	if !x.Supports(file) {
		return nil
	}
	a, b := bodies(x, file, old), bodies(x, file, nw)
	var out []forecast.Symbol
	for k, v := range a {
		if b[k] != v {
			out = append(out, forecast.Symbol{Path: file, Name: k[0], Kind: k[1]})
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, forecast.Symbol{Path: file, Name: k[0], Kind: k[1]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// bodies maps each symbol (name, kind) in src to its source lines.
func bodies(x *symbols.Extractor, file, src string) map[[2]string]string {
	out := map[[2]string]string{}
	if src == "" {
		return out
	}
	syms, err := x.Symbols(file, []byte(src))
	if err != nil {
		return out
	}
	lines := strings.Split(src, "\n")
	for _, s := range syms {
		out[[2]string{s.Name, s.Kind}] = span(lines, s)
	}
	return out
}

func span(lines []string, s gitx.Symbol) string {
	lo, hi := max(s.StartLine-1, 0), min(s.EndLine, len(lines))
	if lo >= hi {
		return ""
	}
	return strings.Join(lines[lo:hi], "\n")
}

// modulePath is the fixture's module path from its go.mod.
func modulePath(t Tree) string {
	gomod, _ := t.Read("go.mod")
	for _, l := range strings.Split(gomod, "\n") {
		if m, ok := strings.CutPrefix(strings.TrimSpace(l), "module "); ok {
			return strings.TrimSpace(m)
		}
	}
	return ""
}

// goImports lists the repo-relative package dirs a Go file imports from
// module mod.
func goImports(mod, file, src string) []string {
	if mod == "" || path.Ext(file) != ".go" {
		return nil
	}
	f, err := parser.ParseFile(token.NewFileSet(), file, src, parser.ImportsOnly)
	if err != nil {
		return nil
	}
	var out []string
	for _, im := range f.Imports {
		p, err := strconv.Unquote(im.Path.Value)
		if err != nil {
			continue
		}
		if rel, ok := strings.CutPrefix(p, mod+"/"); ok {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}
