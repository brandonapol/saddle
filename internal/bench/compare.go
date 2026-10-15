package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/brandonapol/saddle/internal/e2e"
)

// Comparison is one scenario run under file claims and under symbol claims.
type Comparison struct {
	Scenario     string
	File, Symbol *Report
	// Err is why a run failed; the other report may still be set.
	Err string
}

// Compare runs each scenario twice, once per granularity, with o's other
// options, and returns the pairs in order. A failed run is recorded in its
// Comparison and the rest go on. The binaries are built once.
func Compare(o Options, scenarios []string) ([]Comparison, error) {
	o.Granularity = ""
	if err := o.Check(); err != nil {
		return nil, err
	}
	if o.Bins == nil {
		root, err := isolatedTemp()
		if err != nil {
			return nil, err
		}
		defer func() { _ = os.RemoveAll(root) }()
		if o.Progress != nil {
			fmt.Fprintln(o.Progress, "building saddle, the fake gh and the fake agent…")
		}
		b, err := e2e.Build(filepath.Join(root, "bin"))
		if err != nil {
			return nil, err
		}
		o.Bins = &b
	}
	var out []Comparison
	for _, name := range scenarios {
		c := Comparison{Scenario: name}
		var errs []string
		for _, g := range []string{"file", "symbol"} {
			ro := o
			ro.Scenario, ro.Granularity = name, g
			if ro.Progress != nil {
				fmt.Fprintf(ro.Progress, "== %s, %s claims\n", name, g)
			}
			res, err := Run(ro)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", g, err))
				continue
			}
			rep := res.Report
			if g == "file" {
				c.File = &rep
			} else {
				c.Symbol = &rep
			}
		}
		c.Err = strings.Join(errs, "; ")
		out = append(out, c)
	}
	return out, nil
}

// CompareMarkdown is the file-vs-symbol table: one row per scenario, each
// cell "file → symbol".
func CompareMarkdown(cs []Comparison) string {
	var b strings.Builder
	b.WriteString("# Bench: file vs symbol claims\n\nEach cell is file → symbol.\n\n")
	b.WriteString("| Scenario | Landed | Peak (safe) | Denied writes | Claim waits | Train bounces | Needed help | Integration | Symbol shares / denies / notes |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, c := range cs {
		if c.File == nil || c.Symbol == nil {
			fmt.Fprintf(&b, "| %s | error: %s |%s\n", c.Scenario, oneLine(c.Err), strings.Repeat(" – |", 7))
			continue
		}
		f, s := c.File, c.Symbol
		cell := func(get func(Report) string) string { return get(*f) + " → " + get(*s) }
		num := func(get func(Report) int) string {
			return cell(func(r Report) string { return fmt.Sprint(get(r)) })
		}
		fmt.Fprintf(&b, "| %s | %s | %s (%d) | %s | %s | %s | %s | %s | %d / %d / %d |\n", c.Scenario,
			cell(func(r Report) string { return fmt.Sprintf("%d/%d", r.Landed, r.Tickets) }),
			num(func(r Report) int { return r.PeakParallelism }), f.SafeParallelism,
			num(func(r Report) int { return r.DeniedWrites }),
			num(func(r Report) int { return r.ClaimWaits }),
			num(func(r Report) int { return r.TrainBounces }),
			cell(func(r Report) string { return orDash(strings.Join(r.NeededHelp, " ")) }),
			cell(func(r Report) string { return map[bool]string{true: "pass", false: "FAIL"}[r.IntegrationGreen] }),
			s.SymbolShares, s.SymbolDenies, s.SymbolNotes)
	}
	return b.String()
}

func oneLine(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(s), "\n", " "), "|", `\|`)
}
