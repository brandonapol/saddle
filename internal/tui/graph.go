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

// Provider adapters, stacked bottom-up in this order.
const (
	providerClaude = iota
	providerCodex
	providerGrok
	providerOther
	nProviders
)

var (
	providerNames  = [nProviders]string{usage.Claude, usage.Codex, usage.Grok, "other"}
	providerOwners = [nProviders]string{"Anthropic", "OpenAI", "xAI", "unknown"}
	providerColors = [nProviders]lipgloss.Color{cOpus, cSonnet, cHaiku, cDim}
)

// Model families remain useful for the merge-waste counter.
const (
	famOpus = iota
	famSonnet
	famHaiku
	famOther
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

func provider(adapter, model string) int {
	switch adapter {
	case usage.Claude:
		return providerClaude
	case usage.Codex:
		return providerCodex
	case usage.Grok:
		return providerGrok
	}
	// Buckets written before adapters were persisted get a best-effort
	// classification from their model.
	switch m := strings.ToLower(model); {
	case strings.HasPrefix(m, "claude"):
		return providerClaude
	case strings.HasPrefix(m, "grok"):
		return providerGrok
	case strings.HasPrefix(m, "gpt"), strings.Contains(m, "codex"):
		return providerCodex
	}
	return providerOther
}

// graphMinutes is how far back the usage graph reaches.
const graphMinutes = 60

// graphH is the usage graph's height: four bar rows, three providers and totals.
const graphH = 8

// usageGraph is the last hour of usage by provider, one slot per minute,
// oldest first, provider totals/costs, and Opus tokens spent resolving merges.
type usageGraph struct {
	Minutes     [graphMinutes][nProviders]int64
	Totals      [nProviders]int64
	AllTokens   [nProviders]int64
	Costs       [nProviders]float64
	QuotaResets [nProviders]string
	MergeOpus   int64
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
	g := buildGraphPrices(bs, es, now, a.Cfg.Usage.CountCacheReads, a.Cfg.Limits.Prices)
	if a.Root != "" {
		for name, ex := range a.ExhaustedAdapters(now) {
			p := provider(name, "")
			if ex.Resets != "" {
				g.QuotaResets[p] = ex.Resets
			} else if ex.Until.After(now) {
				g.QuotaResets[p] = shortDur(ex.Until.Sub(now))
			}
		}
	}
	return &e, g
}

func buildGraph(bs []usage.Bucket, es []store.Event, now time.Time, cacheReads bool) *usageGraph {
	return buildGraphPrices(bs, es, now, cacheReads, nil)
}

func buildGraphPrices(bs []usage.Bucket, es []store.Event, now time.Time, cacheReads bool, prices map[string]usage.Price) *usageGraph {
	g := &usageGraph{MergeOpus: mergeOpus(es, bs, now, cacheReads)}
	last := now.UTC().Truncate(time.Minute)
	for _, b := range bs {
		i := graphMinutes - 1 - int(last.Sub(b.Minute)/time.Minute)
		if i < 0 || i >= graphMinutes {
			continue
		}
		n := countTokens(b.Tokens, cacheReads)
		p := provider(b.Adapter, b.Model)
		g.Minutes[i][p] += n
		g.Totals[p] += n
		g.AllTokens[p] += b.Total()
		if b.CostUSD > 0 {
			g.Costs[p] += b.CostUSD
		} else {
			g.Costs[p] += usage.CostUSD(b.Model, b.Tokens, prices)
		}
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

// column assigns the cells of one bar, bottom-up, to providers in proportion
// to v, scaled so peak fills h cells. Any usage gets at least one cell.
func column(v [nProviders]int64, peak int64, h int) []int {
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

// render draws four rows of stacked provider bars, one row per known
// provider, then peak and merge totals.
func (g *usageGraph) render(w, h int) string {
	n := min(graphMinutes, w)
	cols := make([][nProviders]int64, n)
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
	barH := max(h-4, 1)
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
				b.WriteString(lipgloss.NewStyle().Foreground(providerColors[s[r]]).Render("█"))
			case r == 0:
				b.WriteString(sFaint.Render("·"))
			default:
				b.WriteByte(' ')
			}
		}
		rows = append(rows, b.String())
	}
	for p := providerClaude; p <= providerGrok; p++ {
		quota := "quota ok"
		if g.QuotaResets[p] != "" {
			quota = "quota resets " + g.QuotaResets[p]
		}
		row := lipgloss.NewStyle().Foreground(providerColors[p]).Render("■") + " " +
			sDim.Render(fmt.Sprintf("%s · %s  %s tok · $%.2f · %s",
				providerNames[p], providerOwners[p], humanTokens(g.AllTokens[p]), g.Costs[p], quota))
		rows = append(rows, lipgloss.NewStyle().MaxWidth(w).Render(row))
	}
	legend := []string{sDim.Render("peak " + humanTokens(peakMin) + "/m")}
	merges := sDim.Render(fmt.Sprintf("merges %s opus", humanTokens(g.MergeOpus)))
	if g.MergeOpus > 0 {
		merges = lipgloss.NewStyle().Foreground(cAlert).Render(fmt.Sprintf("merges %s opus", humanTokens(g.MergeOpus)))
	}
	legend = append(legend, merges)
	rows = append(rows, lipgloss.NewStyle().MaxWidth(w).Render(strings.Join(legend, "  ")))
	return strings.Join(rows, "\n")
}
