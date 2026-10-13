package app

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/usage"
)

// Per-model fallback (#483): a model with a [fallback] chain, e.g.
// "codex:sol" = ["claude:opus", "grok"], is marked out of quota on its own
// when its workers hit the limit, so the adapter's other models stay
// usable. New spawns that would run it, and its running workers (handed off
// as in #481), go to the first chain entry that has quota, can run here and
// isn't excluded (#482). An adapter key ("grok" = ["claude"]) does the same
// for the whole adapter ahead of [adapters] policy. Without a chain, or
// when every entry is skipped, [adapters] policy picks among the others.

// fallbackPick is where work goes while its adapter or model is out.
type fallbackPick struct {
	Adapter string
	Model   string // the model to run; "" is the CLI's own
	Why     string
}

// Label is "adapter:model", or "adapter" when the model is the CLI's own.
func (p fallbackPick) Label() string { return exhaustKey(p.Adapter, p.Model) }

// modelOut is the entry saying model on adapter is out on its own.
func modelOut(exm map[string]Exhaustion, adapter, model string) (Exhaustion, bool) {
	for _, e := range exm {
		if e.Adapter == adapter && usage.ModelIs(model, e.Model) {
			return e, true
		}
	}
	return Exhaustion{}, false
}

// quotaOut is the entry that keeps model on adapter from running: the
// adapter's, else the model's own.
func quotaOut(ex, exm map[string]Exhaustion, adapter, model string) (Exhaustion, bool) {
	if e, out := ex[adapter]; out {
		return e, true
	}
	return modelOut(exm, adapter, model)
}

// chainPick walks the [fallback] chain for model on adapter and returns the
// first entry that has quota, can run here and isn't excluded. skipped says
// why each earlier entry was passed over; ok is false when there is no chain
// or nothing in it can take the work.
func (a *App) chainPick(adapter, model string, ex, exm map[string]Exhaustion) (p fallbackPick, skipped []string, ok bool) {
	key, chain, has := a.Cfg.FallbackFor(adapter, model)
	if !has {
		return p, nil, false
	}
	usable := agent.Usable(a.adapterStatus())
	for _, entry := range chain {
		ea, em, err := config.ParseModelRef(entry)
		if err != nil {
			continue
		}
		if ea != a.Cfg.Harness && !slices.Contains(usable, ea) {
			skipped = append(skipped, entry+" can't run here")
			continue
		}
		m := em
		if m == "" {
			m = a.modelFor(ea, "")
		} else if x := a.Cfg.ExcludedModel(ea, m); x != "" {
			skipped = append(skipped, entry+" is excluded")
			continue
		}
		if e, out := quotaOut(ex, exm, ea, m); out {
			skipped = append(skipped, fmt.Sprintf("%s is out until %s", entry, e.when()))
			continue
		}
		return fallbackPick{Adapter: ea, Model: m, Why: "[fallback] for " + exhaustKey(adapter, key)}, skipped, true
	}
	return p, skipped, false
}

// reroute finds where work for requested model on adapter goes at now when
// adapter, or the model it runs there, is out of quota: the [fallback]
// chain, else the best other adapter by [adapters] policy whose model has
// quota. out is false, and nothing else set, while both have quota.
func (a *App) reroute(adapter, requested string, r pickReq, now time.Time) (p fallbackPick, from Exhaustion, out bool, err error) {
	ex, exm := a.ExhaustedAdapters(now), a.ExhaustedModels(now)
	model := a.modelFor(adapter, requested)
	if from, out = quotaOut(ex, exm, adapter, model); !out {
		return p, from, false, nil
	}
	if p, _, ok := a.chainPick(adapter, model, ex, exm); ok {
		return p, from, true, nil
	}
	// The policy ranks adapters, so one whose model is out counts as out.
	ex = maps.Clone(ex)
	ex[adapter] = from
	r.def = adapter
	for range len(a.Cfg.AdapterOrder) + 1 {
		c, err := a.bestAdapter(r, ex, now)
		if err != nil {
			return p, from, true, err
		}
		m := a.modelFor(c.Name, requested)
		if e, gone := modelOut(exm, c.Name, m); gone {
			ex[c.Name] = e
			continue
		}
		return fallbackPick{Adapter: c.Name, Model: m, Why: c.Why}, from, true, nil
	}
	return p, from, true, ErrAllExhausted
}

// markModelOut marks model on adapter out of quota on its own until b's
// reset, or ExhaustFor from now when b names none.
func (a *App) markModelOut(adapter, model string, b usage.LimitBanner) error {
	now := time.Now()
	until, ok := parseReset(b.Resets, now)
	if !ok {
		until = now.Add(ExhaustFor)
	}
	resets := b.Resets
	if resetInRe.MatchString(resets) {
		resets = ""
	}
	return a.MarkModelExhausted(adapter, model, until, resets)
}

// MarkModelExhausted records that model on adapter is out of quota until
// until, leaving the adapter's other models usable, and tells the owner
// once who ran out, who takes over and when it resets.
func (a *App) MarkModelExhausted(adapter, model string, until time.Time, resets string) error {
	now := time.Now()
	key := exhaustKey(adapter, model)
	if _, out := a.exhausted(now)[key]; out {
		return nil
	}
	exhaustMu.Lock()
	m := a.readExhausted()
	if _, out := m[key]; out {
		exhaustMu.Unlock()
		return nil
	}
	e := Exhaustion{Adapter: adapter, Model: model, Since: now, Until: until, Resets: resets}
	m[key] = e
	err := a.writeExhausted(m)
	exhaustMu.Unlock()
	if err != nil {
		return err
	}
	a.Store.Event(OrchestratorID, EventAdapterExhausted, fmt.Sprintf("%s until %s", key, e.when()))
	p, _, _, rerr := a.reroute(adapter, model, pickReq{}, now)
	var text string
	if rerr != nil {
		text = fmt.Sprintf("Model %s is out of quota until %s, and nothing in its [fallback] chain or [adapters] has quota. New spawns that would use it are refused until then; "+
			"tasks on it stay parked and continue after the reset. Other %s models stay usable.", key, e.when(), adapter)
	} else {
		text = fmt.Sprintf("Model %s is out of quota until %s. New spawns that would use it go to %s instead (%s) until then; other %s models stay usable. ",
			key, e.when(), p.Label(), p.Why, adapter)
		if pol := a.Cfg.AdapterPolicy; pol.Failover == config.FailoverHandoff && (pol.HandoffWait <= 0 || until.Sub(now) > pol.HandoffWait) {
			text += fmt.Sprintf("Tasks already on %s are handed to %s to finish in their worktrees.", key, p.Label())
		} else {
			text += fmt.Sprintf("Tasks already on %s stay parked and %s after the reset.", key, resumeHow(adapter))
		}
		if ad, err := agent.ByName(p.Adapter); err == nil && !ad.Hooks() {
			text += fmt.Sprintf(" %s has no hooks: claims of tasks it runs are checked only when they commit and call done.", p.Adapter)
		}
	}
	return a.tellOwner(text)
}

// FallbackStatus is one [fallback] chain for status: what it covers, its
// entries, when the covered adapter or model is out until, and where work
// goes now.
type FallbackStatus struct {
	From            string   `json:"from"`
	Chain           []string `json:"chain"`
	OutOfQuotaUntil string   `json:"out_of_quota_until,omitempty"`
	// Now is the entry that takes new work while From is out.
	Now string `json:"now,omitempty"`
	// Skipped says why earlier entries are passed over.
	Skipped []string `json:"skipped,omitempty"`
}

// Fallbacks lists the [fallback] chains for status (#483).
func (a *App) Fallbacks(now time.Time) []FallbackStatus {
	if len(a.Cfg.Fallback) == 0 {
		return nil
	}
	ex, exm := a.ExhaustedAdapters(now), a.ExhaustedModels(now)
	var out []FallbackStatus
	for key, chain := range a.Cfg.Fallback {
		ad, model, err := config.ParseModelRef(key)
		if err != nil {
			continue
		}
		f := FallbackStatus{From: exhaustKey(ad, model), Chain: chain}
		var e Exhaustion
		var gone bool
		if model == "" {
			e, gone = ex[ad]
		} else {
			e, gone = quotaOut(ex, exm, ad, model)
		}
		if gone {
			f.OutOfQuotaUntil = e.when()
			if p, skipped, ok := a.chainPick(ad, model, ex, exm); ok {
				f.Now, f.Skipped = p.Label(), skipped
			} else {
				f.Skipped = skipped
			}
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].From < out[j].From })
	return out
}

// modelsOut is the models of adapter out on their own, with their resets.
func modelsOut(exm map[string]Exhaustion, adapter string) map[string]string {
	var out map[string]string
	for _, e := range exm {
		if e.Adapter != adapter {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[e.Model] = e.when()
	}
	return out
}

// fallbackLine is the status line for one chain.
func (f FallbackStatus) String() string {
	s := f.From + " -> " + strings.Join(f.Chain, ", ")
	if f.OutOfQuotaUntil != "" {
		s += "; out until " + f.OutOfQuotaUntil
		if f.Now != "" {
			s += ", now " + f.Now
		} else {
			s += ", nothing in the chain has quota"
		}
	}
	return s
}
