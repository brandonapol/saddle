package tui

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

var graphNow = time.Date(2026, 10, 2, 12, 0, 30, 0, time.UTC)

func ub(ago time.Duration, task, model string, tok usage.Tokens) usage.Bucket {
	return usage.Bucket{Key: usage.Key{Minute: graphNow.Truncate(time.Minute).Add(-ago), Task: task, Model: model}, Tokens: tok, Messages: 1}
}

// Each of the last 60 minutes sums tokens by provider; older usage and
// cache reads (unless counted) stay out.
func TestBuildUsageGraph(t *testing.T) {
	bs := []usage.Bucket{
		ub(90*time.Minute, "t1", "claude-opus-5-5", usage.Tokens{Input: 999}),
		ub(59*time.Minute, "t1", "claude-opus-5-5", usage.Tokens{Input: 10, Output: 5, CacheRead: 1000}),
		ub(0, "t1", "claude-opus-5-5", usage.Tokens{Input: 30}),
		ub(0, "t2", "claude-sonnet-5-5", usage.Tokens{Output: 10}),
		ub(0, "t3", "claude-haiku-4-5", usage.Tokens{Output: 4}),
		ub(0, "t4", "codex", usage.Tokens{Output: 1}),
	}
	g := buildGraph(bs, nil, graphNow, false)
	if got := g.Minutes[0]; !reflect.DeepEqual(got, [nProviders]int64{providerClaude: 15}) {
		t.Errorf("oldest minute = %v", got)
	}
	if got := g.Minutes[59]; !reflect.DeepEqual(got, [nProviders]int64{44, 1, 0, 0}) {
		t.Errorf("latest minute = %v", got)
	}
	if g.Totals != [nProviders]int64{59, 1, 0, 0} {
		t.Errorf("totals = %v", g.Totals)
	}
	if g := buildGraph(bs, nil, graphNow, true); g.Minutes[0][providerClaude] != 1015 {
		t.Errorf("with cache reads counted, oldest minute = %v", g.Minutes[0])
	}
}

// Opus tokens a task spends between a train conflict and its next done are
// merge tokens; other models, other tasks and other times are not.
func TestMergeOpusTokens(t *testing.T) {
	ev := func(ago time.Duration, task, kind string) store.Event {
		return store.Event{TS: graphNow.Add(-ago), Task: task, Kind: kind}
	}
	es := []store.Event{
		ev(50*time.Minute, "t1", "train_conflict"),
		ev(40*time.Minute, "t1", "done"),
		ev(10*time.Minute, "t2", "restack_conflict"), // still open
	}
	bs := []usage.Bucket{
		ub(55*time.Minute, "t1", "claude-opus-5-5", usage.Tokens{Input: 1}),    // before
		ub(45*time.Minute, "t1", "claude-opus-5-5", usage.Tokens{Input: 100}),  // during
		ub(45*time.Minute, "t1", "claude-sonnet-5", usage.Tokens{Input: 1000}), // not opus
		ub(30*time.Minute, "t1", "claude-opus-5-5", usage.Tokens{Input: 1}),    // after done
		ub(5*time.Minute, "t2", "claude-opus-5-5", usage.Tokens{Output: 20}),   // open window
		ub(5*time.Minute, "t3", "claude-opus-5-5", usage.Tokens{Output: 7}),    // no conflict
	}
	if got := mergeOpus(es, bs, graphNow, false); got != 120 {
		t.Errorf("merge opus = %d, want 120", got)
	}
}

// A column stacks providers bottom-up in proportion.
func TestGraphColumnStacks(t *testing.T) {
	got := column([nProviders]int64{providerClaude: 75, providerCodex: 25}, 100, 4)
	want := []int{providerClaude, providerClaude, providerClaude, providerCodex}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("column = %v, want %v", got, want)
	}
	if got := column([nProviders]int64{providerGrok: 1}, 100, 4); !reflect.DeepEqual(got, []int{providerGrok}) {
		t.Errorf("any usage shows at least one cell: %v", got)
	}
	if got := column([nProviders]int64{}, 100, 4); len(got) != 0 {
		t.Errorf("an idle minute is empty: %v", got)
	}
}

func TestUsageGraphRender(t *testing.T) {
	withColor(t)
	bs := []usage.Bucket{
		ub(0, "t1", "claude-opus-5-5", usage.Tokens{Input: 75_000}),
		ub(0, "t2", "claude-sonnet-5-5", usage.Tokens{Input: 25_000}),
		ub(30*time.Minute, "t3", "claude-haiku-4-5", usage.Tokens{Input: 5_000}),
	}
	g := buildGraph(bs, nil, graphNow, false)
	g.MergeOpus = 1200
	for _, w := range []int{30, 40, 120} {
		out := g.render(w, graphH)
		if n := lipgloss.Height(out); n != graphH {
			t.Errorf("width %d: %d lines, want %d", w, n, graphH)
		}
		for _, l := range strings.Split(out, "\n") {
			if lipgloss.Width(l) > w {
				t.Errorf("width %d: %q too wide", w, l)
			}
		}
		if !strings.Contains(out, "claude") {
			t.Errorf("width %d: no legend:\n%s", w, out)
		}
	}
	out := g.render(120, graphH)
	for _, want := range []string{"claude", "Anthropic", "105.0k tok", "codex", "OpenAI", "grok", "xAI", "peak 100.0k/m", "merges 1.2k opus"} {
		if !strings.Contains(out, want) {
			t.Errorf("legend lacks %q:\n%s", want, out)
		}
	}
	// The newest minute is the rightmost column and belongs to Claude.
	rows := strings.Split(out, "\n")
	if bottom := rows[graphH-5]; !strings.HasSuffix(strings.TrimSuffix(bottom, "\x1b[0m"), "█") || !strings.Contains(bottom, fg(cOpus)) {
		t.Errorf("bottom row should end in an opus cell: %q", bottom)
	}
}

// #478: provider-backed agents have their own usage rows. A Codex-only run
// must not disappear into a model-family row named "other".
func TestUsageGraphRenderLabelsCodexProviderWithTokensAndCost(t *testing.T) {
	withColor(t)
	bs := []usage.Bucket{{
		Key:    usage.Key{Minute: graphNow.Truncate(time.Minute), Task: "t5", Adapter: usage.Codex, Model: "gpt-5-codex"},
		Tokens: usage.Tokens{Input: 1_000_000, CacheRead: 2_000_000, Output: 100_000}, Messages: 1,
	}}
	g := buildGraph(bs, nil, graphNow, false)
	out := g.render(120, graphH)
	for _, want := range []string{"codex", "OpenAI", "3.1M tok", "$2.50"} {
		if !strings.Contains(out, want) {
			t.Errorf("provider row lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "other") {
		t.Errorf("codex usage still labeled other:\n%s", out)
	}
}

func TestUsageGraphProviderRowShowsQuotaReset(t *testing.T) {
	g := &usageGraph{}
	g.QuotaResets[providerGrok] = "42m"
	out := g.render(120, graphH)
	if !strings.Contains(out, "grok · xAI") || !strings.Contains(out, "quota resets 42m") {
		t.Errorf("grok quota row missing:\n%s", out)
	}
}

// The control view shows the graph under the peek when there is room, and
// the merge view's title carries the merge counter.
func TestUsageGraphInViews(t *testing.T) {
	m := newViewModel(160, 50)
	m.graph = &usageGraph{MergeOpus: 0}
	m.graph.Minutes[59][providerClaude] = 10
	m.graph.Totals[providerClaude] = 10
	if out := m.viewLeft(100, 40); !strings.Contains(out, "USAGE · 60m") {
		t.Errorf("no usage panel:\n%s", out)
	}
	if out := m.viewLeft(100, 16); strings.Contains(out, "USAGE") {
		t.Errorf("usage panel squeezed into a short column:\n%s", out)
	}
	m.view = viewMerge
	if out := m.View(); !strings.Contains(out, "opus on merges 0") {
		t.Errorf("merge view lacks the counter:\n%s", out)
	}
}

// readUsage gives the same limit estimate as app.Limits and fills the graph.
func TestReadUsage(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Now()
	minute := now.UTC().Truncate(time.Minute)
	if err := st.AddUsage("s", false, []usage.Bucket{
		{Key: usage.Key{Minute: minute, Task: "t1", Model: "claude-opus-5-5"}, Tokens: usage.Tokens{Input: 500}, Messages: 1},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Limits.FiveHour.Tokens = 1000
	a := &app.App{Store: st, Cfg: cfg}
	lim, g := readUsage(a, now)
	want, _ := a.Limits(now)
	if lim == nil || lim.FiveHour.Percent != want.FiveHour.Percent || lim.FiveHour.Percent != 0.5 {
		t.Errorf("limits = %+v, want %+v", lim, want)
	}
	if g == nil || g.Minutes[59][providerClaude] != 500 {
		t.Errorf("graph = %+v", g)
	}
}
