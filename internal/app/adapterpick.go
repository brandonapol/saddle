package app

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/usage"
)

// Adapter selection (#481): a spawn without adapter= takes the best adapter
// that has quota and can run here. A task kind first narrows the choice to
// adapters whose [adapters.<name>] kinds list it, a task with claims prefers
// adapters that run hooks (they enforce claims), and then [adapters] policy
// decides: "order" (the session default, then [adapters] order), "cost" (the
// lowest cost) or "quota" (the most of its five-hour budget left).

// AdapterChoice is one adapter's standing for the next pick.
type AdapterChoice struct {
	Name string
	Rank int    // 1 is picked first; 0 when it can't be picked now
	Why  string // what put it there, or why it can't be picked
}

// pickReq is what a pick knows about the task.
type pickReq struct {
	def    string // the adapter "order" starts from: the session default
	kind   string // spawn's kind=, lower case; "" for none
	claims bool   // the task claims paths
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
	var used map[string]int64
	if a.Cfg.AdapterPolicy.Pick == config.PickQuota {
		used = a.adapterUse(now)
	}
	kindMatch := func(n string) bool {
		return r.kind != "" && slices.Contains(a.Cfg.Adapters[n].Kinds, r.kind)
	}
	hooks := func(n string) bool {
		ad, err := agent.ByName(n)
		return err == nil && ad.Hooks()
	}
	type cand struct {
		AdapterChoice
		kind, hooks bool
		cost, room  float64
		order       int
	}
	byOrder := a.Cfg.AdapterPolicy.Pick != config.PickCost && a.Cfg.AdapterPolicy.Pick != config.PickQuota
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
		c := cand{AdapterChoice: AdapterChoice{Name: n}, kind: kindMatch(n), hooks: hooks(n), cost: a.adapterCost(n), order: orderRank(n)}
		c.room, _ = a.adapterHeadroom(n, used)
		ok = append(ok, c)
	}
	sort.SliceStable(ok, func(i, j int) bool {
		x, y := ok[i], ok[j]
		if x.kind != y.kind {
			return x.kind
		}
		// "order" keeps the session default ahead of the hooks preference:
		// saddle up codex means codex.
		if byOrder && (x.Name == r.def) != (y.Name == r.def) {
			return x.Name == r.def
		}
		if r.claims && x.hooks != y.hooks {
			return x.hooks
		}
		switch a.Cfg.AdapterPolicy.Pick {
		case config.PickCost:
			if x.cost != y.cost {
				return x.cost < y.cost
			}
		case config.PickQuota:
			if x.room != y.room {
				return x.room > y.room
			}
		}
		return x.order < y.order
	})
	ranked := make([]AdapterChoice, 0, len(ok)+len(out))
	for i, c := range ok {
		var why []string
		if c.kind {
			why = append(why, "kind "+r.kind)
		}
		if r.claims && c.hooks {
			why = append(why, "hooks enforce its claims")
		}
		switch a.Cfg.AdapterPolicy.Pick {
		case config.PickCost:
			why = append(why, fmt.Sprintf("cost %g", c.cost))
		case config.PickQuota:
			if room, has := a.adapterHeadroom(c.Name, used); has {
				why = append(why, fmt.Sprintf("%.0f%% of budget left", room*100))
			} else {
				why = append(why, "no budget set")
			}
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
