package tui

import (
	"cmp"
	"fmt"
	"hash/fnv"
	"math"
	"slices"
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

// graphBars is the usage graph's bar height. Below the bars are one row per
// series and the totals.
const graphBars = 4

// A series is one coloured row of the usage graph: a provider, or for codex
// one model (#482).
type series struct {
	Provider int
	Model    string // codex's model name; "" when the rollout didn't say
}

func seriesOf(adapter, model string) series {
	p := provider(adapter, model)
	if p == providerCodex {
		return series{p, usage.CodexModelName(model)}
	}
	return series{Provider: p}
}

// codexColors tells the codex models apart; unknown models take one of
// codexSpare by name, so they are never gray.
var (
	codexColors = map[string]lipgloss.Color{
		"luna": "#A9B8FF", "terra": "#7FB069", "sol": "#F6D365", "astra": "#C77DFF",
	}
	codexSpare = []lipgloss.Color{"#5DADE2", "#F1948A", "#76D7C4", "#D7BDE2"}
)

func (s series) color() lipgloss.Color {
	if s.Provider != providerCodex || s.Model == "" {
		return providerColors[s.Provider]
	}
	if c, ok := codexColors[s.Model]; ok {
		return c
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(s.Model))
	return codexSpare[h.Sum32()%uint32(len(codexSpare))]
}

func (s series) label() string {
	if s.Model != "" {
		return providerNames[s.Provider] + " " + s.Model
	}
	return providerNames[s.Provider]
}

// usageGraph is the last hour of usage by series, one slot per minute,
// oldest first, series totals/costs, and Opus tokens spent resolving merges.
type usageGraph struct {
	Minutes     [graphMinutes]map[series]int64
	Totals      map[series]int64
	AllTokens   map[series]int64
	Costs       map[series]float64
	QuotaResets [nProviders]string
	// CodexExcluded are [adapters.codex] exclude_models; their rows say so.
	CodexExcluded []string
	MergeOpus     int64
}

// add counts n tokens (all with cache reads) costing cost in minute i.
func (g *usageGraph) add(i int, s series, n, all int64, cost float64) {
	if g.Minutes[i] == nil {
		g.Minutes[i] = map[series]int64{}
	}
	if g.Totals == nil {
		g.Totals, g.AllTokens, g.Costs = map[series]int64{}, map[series]int64{}, map[series]float64{}
	}
	g.Minutes[i][s] += n
	g.Totals[s] += n
	g.AllTokens[s] += all
	g.Costs[s] += cost
}

// rows lists the series the graph shows, bottom-up: claude, each codex
// model with usage (luna, terra, sol, astra, then others by name), grok,
// and other when anything landed there. Claude, codex and grok always have
// a row so their quota shows.
func (g *usageGraph) rows() []series {
	var codex []series
	for s, n := range g.AllTokens {
		if s.Provider == providerCodex && n > 0 {
			codex = append(codex, s)
		}
	}
	rank := func(s series) int {
		if i := slices.Index(usage.CodexModels, s.Model); i >= 0 {
			return i
		}
		if s.Model == "" {
			return len(usage.CodexModels) + 1
		}
		return len(usage.CodexModels)
	}
	slices.SortFunc(codex, func(x, y series) int {
		if c := cmp.Compare(rank(x), rank(y)); c != 0 {
			return c
		}
		return strings.Compare(x.Model, y.Model)
	})
	if len(codex) == 0 {
		codex = []series{{Provider: providerCodex}}
	}
	out := append([]series{{Provider: providerClaude}}, codex...)
	out = append(out, series{Provider: providerGrok})
	if g.AllTokens[series{Provider: providerOther}] > 0 {
		out = append(out, series{Provider: providerOther})
	}
	return out
}

// height is the graph's height: the bars, a row per series and the totals.
func (g *usageGraph) height() int { return graphBars + len(g.rows()) + 1 }

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
	g.CodexExcluded = a.Cfg.ExcludedModels(usage.Codex)
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
		cost := b.CostUSD
		if cost <= 0 {
			cost = usage.CostUSD(b.Model, b.Tokens, prices)
		}
		g.add(i, seriesOf(b.Adapter, b.Model), countTokens(b.Tokens, cacheReads), b.Total(), cost)
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

// column assigns the cells of one bar, bottom-up, to series (indexes into
// v) in proportion to v, scaled so peak fills h cells. Any usage gets at
// least one cell.
func column(v []int64, peak int64, h int) []int {
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

// render draws stacked bars by series, one labelled row per series, then
// peak and merge totals, in h lines.
func (g *usageGraph) render(w, h int) string {
	ss := g.rows()
	n := min(graphMinutes, w)
	cols := make([][]int64, n)
	for j := range cols {
		cols[j] = make([]int64, len(ss))
	}
	var peakMin int64
	for i, m := range g.Minutes {
		j := i * n / graphMinutes
		var t int64
		for f, s := range ss {
			cols[j][f] += m[s]
			t += m[s]
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
	barH := max(h-len(ss)-1, 1)
	stacks := make([][]int, n)
	for j, c := range cols {
		stacks[j] = column(c, peak, barH)
	}
	rows := make([]string, 0, h)
	for r := barH - 1; r >= 0; r-- {
		var b strings.Builder
		for _, st := range stacks {
			switch {
			case r < len(st):
				b.WriteString(lipgloss.NewStyle().Foreground(ss[st[r]].color()).Render("█"))
			case r == 0:
				b.WriteString(sFaint.Render("·"))
			default:
				b.WriteByte(' ')
			}
		}
		rows = append(rows, b.String())
	}
	for _, s := range ss {
		quota := "quota ok"
		if g.QuotaResets[s.Provider] != "" {
			quota = "quota resets " + g.QuotaResets[s.Provider]
		}
		if s.Provider == providerCodex && slices.ContainsFunc(g.CodexExcluded, func(x string) bool { return usage.ModelIs(s.Model, x) }) {
			quota = "excluded"
		}
		row := lipgloss.NewStyle().Foreground(s.color()).Render("■") + " " +
			sDim.Render(fmt.Sprintf("%s · %s  %s tok · $%.2f · %s",
				s.label(), providerOwners[s.Provider], humanTokens(g.AllTokens[s]), g.Costs[s], quota))
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
