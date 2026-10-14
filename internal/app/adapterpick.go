package app

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/claims"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// Adapter selection (#481, #505): a spawn without adapter= takes the best
// adapter that has quota and can run here. One chooser serves the
// orchestrator's spawns, autopilot's and fallback rerouting. It ranks by
// the task's needs (capabilities it must have), then its difficulty (the
// adapters whose capability table suits it), then its kind ([adapters.<name>]
// kinds), then hooks for a task whose claims need enforcing, and then
// [adapters] policy decides: "balanced" (the default: the most unused
// quota, then the fewest running workers, then the lowest cost), "order"
// (the session default, then [adapters] order), "cost" (the lowest cost) or
// "quota" (the most of its five-hour budget left).

// AdapterChoice is one adapter's standing for the next pick.
type AdapterChoice struct {
	Name string
	Rank int    // 1 is picked first; 0 when it can't be picked now
	Why  string // what put it there, or why it can't be picked
}

// pickReq is what a pick knows about the task.
type pickReq struct {
	def        string   // the adapter "order" starts from: the session default
	kind       string   // spawn's kind=, lower case; "" for none
	difficulty string   // hard, standard or mechanical; "" for unknown (#505)
	needs      []string // capabilities the task needs (#505)
	claims     bool     // the task claims paths
	// crowded is set when the task's claims share a directory with another
	// live task's, so claim enforcement on every write matters most. Under
	// balanced only a crowded task prefers hooked adapters (#505).
	crowded bool
}

// newPickReq builds a pick for a spawn's kind, difficulty and needs.
func newPickReq(def, kind, difficulty string, needs []string, claims, crowded bool) pickReq {
	r := pickReq{def: def, kind: strings.ToLower(strings.TrimSpace(kind)), difficulty: config.NormalizeDifficulty(difficulty), claims: claims, crowded: crowded}
	for _, n := range needs {
		if n = config.NormalizeNeed(n); n != "" && !slices.Contains(r.needs, n) {
			r.needs = append(r.needs, n)
		}
	}
	return r
}

// adapterCost is [adapters.<name>] cost; unset is 1.
func (a *App) adapterCost(name string) float64 {
	if c := a.Cfg.Adapters[name].Cost; c > 0 {
		return c
	}
	return 1
}

// adapterHeadroom is the fraction of name's five-hour budget left at now,
// and whether it has a budget. Without one it has no known limit: 1.
func (a *App) adapterHeadroom(name string, used map[string]int64) (float64, bool) {
	b := a.Cfg.Adapters[name].Budget
	if b <= 0 {
		return 1, false
	}
	return max(0, 1-float64(used[name])/float64(b)), true
}

// adapterUse sums each adapter's tokens over the last five hours, cache
// reads left out as the plan windows leave them.
func (a *App) adapterUse(now time.Time) map[string]int64 {
	out := map[string]int64{}
	bs, err := a.Store.UsageBuckets(now.Add(-usage.FiveHours))
	if err != nil {
		return out
	}
	for _, b := range bs {
		out[b.Adapter] += b.Total() - b.CacheRead
	}
	return out
}

// rankAdapters ranks every adapter the policy knows: [adapters] order and
// the session default. Ones out of quota or unable to run here rank 0. The
// session default counts as able to run, as spawns always treated it.
func (a *App) rankAdapters(r pickReq, ex map[string]Exhaustion, now time.Time) []AdapterChoice {
	names := slices.Clone(a.Cfg.AdapterOrder)
	if r.def != "" && !slices.Contains(names, r.def) {
		names = append([]string{r.def}, names...)
	}
	usable := agent.Usable(a.adapterStatus())
	// orderRank is the rotation from the default through [adapters] order.
	start := slices.Index(a.Cfg.AdapterOrder, r.def)
	orderRank := func(n string) int {
		if n == r.def {
			return -1
		}
		i := slices.Index(a.Cfg.AdapterOrder, n)
		if start < 0 {
			return i
		}
		return (i - start + len(a.Cfg.AdapterOrder)) % len(a.Cfg.AdapterOrder)
	}
	pol := a.Cfg.AdapterPolicy.Pick
	var used map[string]int64
	if pol == config.PickQuota || pol == config.PickBalanced {
		used = a.adapterUse(now)
	}
	var running map[string]int
	if pol == config.PickBalanced {
		running = a.runningByAdapter()
	}
	kindMatch := func(n string) bool {
		return r.kind != "" && slices.Contains(a.Cfg.Adapters[n].Kinds, r.kind)
	}
	hooks := func(n string) bool {
		ad, err := agent.ByName(n)
		return err == nil && ad.Hooks()
	}
	// missing is the needs n lacks.
	missing := func(n string) []string {
		var out []string
		for _, need := range r.needs {
			has := slices.Contains(a.Cfg.AdapterCapabilities(n), need)
			if need == config.CapHooks {
				has = has || hooks(n)
			}
			if !has {
				out = append(out, need)
			}
		}
		return out
	}
	// Under balanced, hooks win only when the claims are crowded: the other
	// policies keep #481's preference for any task with claims.
	wantHooks := r.claims && (pol != config.PickBalanced || r.crowded)
	type cand struct {
		AdapterChoice
		kind, hooks, suits bool
		lacks              []string
		cost, room         float64
		order, running     int
	}
	byOrder := pol == config.PickOrder
	var ok []cand
	var out []AdapterChoice
	for _, n := range names {
		if e, gone := ex[n]; gone {
			out = append(out, AdapterChoice{Name: n, Why: "out of quota until " + e.when()})
			continue
		}
		if n != r.def && !slices.Contains(usable, n) {
			out = append(out, AdapterChoice{Name: n, Why: "can't run here"})
			continue
		}
		c := cand{AdapterChoice: AdapterChoice{Name: n}, kind: kindMatch(n), hooks: hooks(n), cost: a.adapterCost(n), order: orderRank(n),
			suits: a.Cfg.SuitsDifficulty(n, r.difficulty), lacks: missing(n), running: running[n]}
		c.room, _ = a.adapterHeadroom(n, used)
		ok = append(ok, c)
	}
	sort.SliceStable(ok, func(i, j int) bool {
		x, y := ok[i], ok[j]
		if len(x.lacks) != len(y.lacks) {
			return len(x.lacks) < len(y.lacks)
		}
		if x.suits != y.suits {
			return x.suits
		}
		if x.kind != y.kind {
			return x.kind
		}
		// "order" keeps the session default ahead of the hooks preference:
		// saddle up codex means codex.
		if byOrder && (x.Name == r.def) != (y.Name == r.def) {
			return x.Name == r.def
		}
		if wantHooks && x.hooks != y.hooks {
			return x.hooks
		}
		switch pol {
		case config.PickCost:
			if x.cost != y.cost {
				return x.cost < y.cost
			}
		case config.PickQuota:
			if x.room != y.room {
				return x.room > y.room
			}
		case config.PickBalanced:
			if x.room != y.room {
				return x.room > y.room
			}
			if x.running != y.running {
				return x.running < y.running
			}
			if x.cost != y.cost {
				return x.cost < y.cost
			}
		}
		return x.order < y.order
	})
	ranked := make([]AdapterChoice, 0, len(ok)+len(out))
	for i, c := range ok {
		var why []string
		if len(c.lacks) > 0 {
			why = append(why, "lacks "+strings.Join(c.lacks, ", "))
		} else if len(r.needs) > 0 {
			why = append(why, "has "+strings.Join(r.needs, ", "))
		}
		if r.difficulty != "" {
			if c.suits {
				why = append(why, "suits "+r.difficulty)
			} else {
				why = append(why, "not suited to "+r.difficulty)
			}
		}
		if c.kind {
			why = append(why, "kind "+r.kind)
		}
		if wantHooks && c.hooks {
			why = append(why, "hooks enforce its claims")
		}
		switch pol {
		case config.PickCost:
			why = append(why, fmt.Sprintf("cost %g", c.cost))
		case config.PickQuota:
			why = append(why, a.budgetWhy(c.Name, used))
		case config.PickBalanced:
			why = append(why, a.budgetWhy(c.Name, used), fmt.Sprintf("%d running", c.running))
		default:
			if c.Name == r.def {
				why = append(why, "default")
			} else {
				why = append(why, "next in order")
			}
		}
		c.Rank, c.Why = i+1, strings.Join(why, ", ")
		ranked = append(ranked, c.AdapterChoice)
	}
	return append(ranked, out...)
}

// budgetWhy says how much of name's budget is left.
func (a *App) budgetWhy(name string, used map[string]int64) string {
	if room, has := a.adapterHeadroom(name, used); has {
		return fmt.Sprintf("%.0f%% of budget left", room*100)
	}
	return "no budget set"
}

// runningByAdapter counts each adapter's live workers.
func (a *App) runningByAdapter() map[string]int {
	out := map[string]int{}
	ts, err := a.Store.Tasks()
	if err != nil {
		return out
	}
	for _, t := range ts {
		if t.Role == store.RoleWorker && (t.Status == store.Running || t.Status == store.Idle || t.Status == store.NeedsYou) {
			out[a.AdapterOf(t)]++
		}
	}
	return out
}

// claimsCrowded reports whether want shares a top-level directory with a
// claim another live task holds.
func claimsCrowded(want []string, all map[string][]string, self string) bool {
	top := func(g string) string {
		g = claims.Clean(g)
		if i := strings.IndexAny(g, "*?[{"); i >= 0 {
			g = g[:i]
		}
		d, _, _ := strings.Cut(g, "/")
		return d
	}
	mine := map[string]bool{}
	for _, g := range want {
		if !claims.IsNot(g) {
			mine[top(g)] = true
		}
	}
	for task, gs := range all {
		if task == self {
			continue
		}
		for _, g := range gs {
			if claims.IsNot(g) {
				continue
			}
			if d := top(g); mine[d] || mine[""] || d == "" {
				return true
			}
		}
	}
	return false
}

// bestAdapter is the first adapter rankAdapters can pick, or
// ErrAllExhausted when every one is out of quota or can't run.
func (a *App) bestAdapter(r pickReq, ex map[string]Exhaustion, now time.Time) (AdapterChoice, error) {
	rs := a.rankAdapters(r, ex, now)
	if len(rs) > 0 && rs[0].Rank > 0 {
		return rs[0], nil
	}
	return AdapterChoice{}, ErrAllExhausted
}

// AdapterPolicy says how spawns pick and what failover does, for status.
func (a *App) AdapterPolicy() string {
	p := a.Cfg.AdapterPolicy
	s := "policy " + p.Pick + ", failover " + p.Failover
	if p.Failover == config.FailoverHandoff && p.HandoffWait > 0 {
		s += " (park when the reset is within " + p.HandoffWait.String() + ")"
	}
	return s
}

// spawnPickReq is the pick for spawn r: its kind, difficulty, needs and
// whether its claims are crowded by another live task's.
func (a *App) spawnPickReq(r SpawnReq) pickReq {
	crowded := false
	if len(r.Claims) > 0 {
		if all, err := a.Store.Claims(); err == nil {
			crowded = claimsCrowded(r.Claims, all, r.ID)
		}
	}
	return newPickReq(a.Cfg.Harness, r.Kind, r.Difficulty, r.Needs, len(r.Claims) > 0, crowded)
}

// pickLine is how status explains a task's adapter: "codex (luna): suits
// mechanical, 80% of budget left, 0 running".
func pickLine(adapter, model, why, rotated string) string {
	s := adapter
	if model != "" {
		s += " (" + model + ")"
	}
	if why != "" {
		s += ": " + why
	}
	if rotated != "" {
		s += "; " + rotated + " was out of quota"
	}
	return s
}

// TaskPicks maps each task to the adapter it was put on and why (#505).
func (a *App) TaskPicks() map[string]string {
	m, err := a.Store.LatestEventData(EventAdapterPick)
	if err != nil {
		return map[string]string{}
	}
	return m
}

// RoutePreview is where a spawn would go now, without spawning: the
// adapter, the model and why (#505). An adapter on r is the override.
func (a *App) RoutePreview(r SpawnReq) (adapter, model, why string, err error) {
	if r.Adapter != "" {
		if _, err := agent.ByName(r.Adapter); err != nil {
			return "", "", "", err
		}
	}
	pick, err := a.spawnAdapter(r.Adapter, r.Model, a.spawnPickReq(r), r.Force)
	if err != nil {
		return "", "", "", err
	}
	return pick.Name, a.spawnModel(pick, r), pick.Why, nil
}

// spawnModel is the model spawn r runs on pick's adapter. A model from
// another adapter's family (a ticket's "opus", a repair's "sonnet") is a
// hint: it falls back to the difficulty's model (#505), else the adapter's
// default. A [fallback] entry's model wins (#483).
func (a *App) spawnModel(pick spawnPick, r SpawnReq) string {
	if pick.ModelSet {
		return pick.Model
	}
	model := a.modelFor(pick.Name, r.Model)
	if m := a.Cfg.DifficultyModel(pick.Name, r.Difficulty); m != "" && (r.Model == "" || model != r.Model) {
		model = m
	}
	return model
}
