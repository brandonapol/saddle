package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// Model families, stacked bottom-up in this order.
const (
	famOpus = iota
	famSonnet
	famHaiku
	famOther
	nFamilies
)

var (
	famNames  = [nFamilies]string{"opus", "sonnet", "haiku", "other"}
	famColors = [nFamilies]lipgloss.Color{cOpus, cSonnet, cHaiku, cDim}
)

func family(model string) int {
	switch m := strings.ToLower(model); {
	case strings.Contains(m, "opus"):
		return famOpus
	case strings.Contains(m, "sonnet"):
		return famSonnet
	case strings.Contains(m, "haiku"):
		return famHaiku
	}
	return famOther
}

// graphMinutes is how far back the usage graph reaches.
const graphMinutes = 60

// graphH is the usage graph's height: bar rows plus the legend.
const graphH = 5

// usageGraph is the last hour of usage by model family, one slot per minute,
// oldest first, and the Opus tokens spent resolving merges.
type usageGraph struct {
	Minutes   [graphMinutes][nFamilies]int64
	Totals    [nFamilies]int64
	MergeOpus int64
}

func countTokens(t usage.Tokens, cacheReads bool) int64 {
	n := t.Total()
	if !cacheReads {
		n -= t.CacheRead
	}
	return n
}

// readUsage reads a week of usage once for both the limit estimate (as
// app.Limits computes it) and the graph.
func readUsage(a *app.App, now time.Time) (*usage.LimitEstimate, *usageGraph) {
	bs, err := a.Store.UsageBuckets(now.Add(-usage.Week))
	if err != nil {
		return nil, nil
	}
	e := usage.EstimateBuckets(bs, a.Cfg.Limits, now)
	es, _ := a.Store.Events(2000)
	return &e, buildGraph(bs, es, now, a.Cfg.Usage.CountCacheReads)
}

func buildGraph(bs []usage.Bucket, es []store.Event, now time.Time, cacheReads bool) *usageGraph {
	g := &usageGraph{MergeOpus: mergeOpus(es, bs, now, cacheReads)}
	last := now.UTC().Truncate(time.Minute)
	for _, b := range bs {
		i := graphMinutes - 1 - int(last.Sub(b.Minute)/time.Minute)
		if i < 0 || i >= graphMinutes {
			continue
		}
		n := countTokens(b.Tokens, cacheReads)
		g.Minutes[i][family(b.Model)] += n
		g.Totals[family(b.Model)] += n
	}
	return g
}

// mergeOpus sums the Opus tokens each task spent between a train or restack
// conflict and its next done, landing or kill: tokens spent on merging that
// the train exists to avoid.
func mergeOpus(es []store.Event, bs []usage.Bucket, now time.Time, cacheReads bool) int64 {
	type window struct{ from, to time.Time }
	open := map[string]time.Time{}
	wins := map[string][]window{}
	for _, e := range es {
		switch e.Kind {
		case "train_conflict", "train_test_failed", "restack_conflict":
			if _, ok := open[e.Task]; !ok {
				open[e.Task] = e.TS.UTC().Truncate(time.Minute)
			}
		case "done", "landed", "kill":
			if from, ok := open[e.Task]; ok {
				wins[e.Task] = append(wins[e.Task], window{from, e.TS})
				delete(open, e.Task)
			}
		}
	}
	for task, from := range open {
		wins[task] = append(wins[task], window{from, now.Add(time.Minute)})
	}
	var n int64
	for _, b := range bs {
		if family(b.Model) != famOpus {
			continue
		}
		for _, w := range wins[b.Task] {
			if !b.Minute.Before(w.from) && b.Minute.Before(w.to) {
				n += countTokens(b.Tokens, cacheReads)
				break
			}
		}
	}
	return n
}

// column assigns the cells of one bar, bottom-up, to families in proportion
// to v, scaled so peak fills h cells. Any usage gets at least one cell.
func column(v [nFamilies]int64, peak int64, h int) []int {
	var total int64
	for _, n := range v {
		total += n
	}
	if total <= 0 || peak <= 0 {
		return nil
	}
	cells := max(int(math.Round(float64(total)/float64(peak)*float64(h))), 1)
	out := make([]int, cells)
	for k := range out {
		pos := (float64(k) + 0.5) / float64(cells) * float64(total)
		var cum float64
		for f, n := range v {
			cum += float64(n)
			if pos < cum {
				out[k] = f
				break
			}
		}
	}
	return out
}

// render draws the graph in w columns and h lines: h-1 rows of stacked bars,
// newest minute on the right, then a legend.
func (g *usageGraph) render(w, h int) string {
	n := min(graphMinutes, w)
	cols := make([][nFamilies]int64, n)
	var peakMin int64
	for i, m := range g.Minutes {
		j := i * n / graphMinutes
		var t int64
		for f, v := range m {
			cols[j][f] += v
			t += v
		}
		peakMin = max(peakMin, t)
	}
	var peak int64
	for _, c := range cols {
		var t int64
		for _, v := range c {
			t += v
		}
		peak = max(peak, t)
	}
	barH := h - 1
	stacks := make([][]int, n)
	for j, c := range cols {
		stacks[j] = column(c, peak, barH)
	}
	rows := make([]string, 0, h)
	for r := barH - 1; r >= 0; r-- {
		var b strings.Builder
		for _, s := range stacks {
			switch {
			case r < len(s):
				b.WriteString(lipgloss.NewStyle().Foreground(famColors[s[r]]).Render("█"))
			case r == 0:
				b.WriteString(sFaint.Render("·"))
			default:
				b.WriteByte(' ')
			}
		}
		rows = append(rows, b.String())
	}
	var legend []string
	for f := range nFamilies {
		if f != famOpus && g.Totals[f] == 0 {
			continue
		}
		legend = append(legend, lipgloss.NewStyle().Foreground(famColors[f]).Render("■")+" "+
			sDim.Render(famNames[f]+" "+humanTokens(g.Totals[f])))
	}
	legend = append(legend, sDim.Render("peak "+humanTokens(peakMin)+"/m"))
	merges := sDim.Render(fmt.Sprintf("merges %s opus", humanTokens(g.MergeOpus)))
	if g.MergeOpus > 0 {
		merges = lipgloss.NewStyle().Foreground(cAlert).Render(fmt.Sprintf("merges %s opus", humanTokens(g.MergeOpus)))
	}
	legend = append(legend, merges)
	rows = append(rows, lipgloss.NewStyle().MaxWidth(w).Render(strings.Join(legend, "  ")))
	return strings.Join(rows, "\n")
}
