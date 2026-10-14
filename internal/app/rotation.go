package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// Adapter rotation (#321): when an adapter's agents hit its usage limit,
// the adapter is marked exhausted until its reset, and spawns that would
// have used it go to the next adapter in [adapters] order that has quota and
// can run here. Tasks already running on it park as before (#180).

const (
	EventAdapterExhausted = "adapter_exhausted"
	EventAdapterRestored  = "adapter_restored"
	EventAdapterRotated   = "adapter_rotated"
	// EventAdapterPick records the adapter a task runs on and why, for
	// status (#505).
	EventAdapterPick = "adapter_pick"
)

// ExhaustFor is how long an adapter stays exhausted when its banner names
// no reset saddle can read. A banner that clears sooner restores it.
var ExhaustFor = time.Hour

// Exhaustion is one adapter, or one of its models (#483), out of quota.
type Exhaustion struct {
	Adapter string    `json:"adapter"`
	Model   string    `json:"model,omitempty"` // set when only this model is out
	Since   time.Time `json:"since"`
	Until   time.Time `json:"until"`
	Resets  string    `json:"resets,omitempty"` // as the banner words it
}

// Label is "adapter", or "adapter:model" for a model out on its own. It is
// the entry's key in the exhausted file.
func (e Exhaustion) Label() string { return exhaustKey(e.Adapter, e.Model) }

func exhaustKey(adapter, model string) string {
	if model == "" {
		return adapter
	}
	return adapter + ":" + model
}

// when is the reset as the owner should read it.
func (e Exhaustion) when() string {
	if e.Resets != "" {
		return e.Resets
	}
	return e.Until.Local().Format("15:04")
}

// ErrAllExhausted refuses a spawn while every adapter is out of quota. It
// is a paused launch, so autopilot sleeps until the reset.
var ErrAllExhausted = fmt.Errorf("%w: every adapter is out of quota", ErrLaunchesPaused)

var exhaustMu sync.Mutex // guards the exhausted file within a process

func (a *App) exhaustedPath() string { return a.stateDir("adapters-exhausted.json") }

func (a *App) readExhausted() map[string]Exhaustion {
	out := map[string]Exhaustion{}
	b, err := os.ReadFile(a.exhaustedPath())
	if err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func (a *App) writeExhausted(m map[string]Exhaustion) error {
	p := a.exhaustedPath()
	if len(m) == 0 {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ExhaustedAdapters lists the adapters out of quota at now, by name. Ones
// whose reset has passed are dropped, and the owner hears they are back.
// Models out on their own are ExhaustedModels.
func (a *App) ExhaustedAdapters(now time.Time) map[string]Exhaustion {
	m := a.exhausted(now)
	maps.DeleteFunc(m, func(_ string, e Exhaustion) bool { return e.Model != "" })
	return m
}

// ExhaustedModels lists the models out of quota on their own at now (#483),
// keyed "adapter:model".
func (a *App) ExhaustedModels(now time.Time) map[string]Exhaustion {
	m := a.exhausted(now)
	maps.DeleteFunc(m, func(_ string, e Exhaustion) bool { return e.Model == "" })
	return m
}

// exhausted lists every adapter and model out of quota at now, by Label.
func (a *App) exhausted(now time.Time) map[string]Exhaustion {
	exhaustMu.Lock()
	m := a.readExhausted()
	var back []Exhaustion
	for n, e := range m {
		if !now.Before(e.Until) {
			back = append(back, e)
			delete(m, n)
		}
	}
	if len(back) > 0 {
		_ = a.writeExhausted(m)
	}
	exhaustMu.Unlock()
	for _, e := range back {
		a.restored(e.Label(), m)
	}
	return m
}

// MarkExhausted records that adapter is out of quota until until (resets is
// the banner's wording, if any), and tells the owner which adapter takes
// over, or that none can. Marking an adapter already out says nothing.
func (a *App) MarkExhausted(adapter string, until time.Time, resets string) error {
	now := time.Now()
	ex := a.ExhaustedAdapters(now)
	if _, out := ex[adapter]; out {
		return nil
	}
	exhaustMu.Lock()
	m := a.readExhausted()
	if _, out := m[adapter]; out {
		exhaustMu.Unlock()
		return nil
	}
	e := Exhaustion{Adapter: adapter, Since: now, Until: until, Resets: resets}
	m[adapter] = e
	err := a.writeExhausted(m)
	exhaustMu.Unlock()
	if err != nil {
		return err
	}
	a.Store.Event(OrchestratorID, EventAdapterExhausted, fmt.Sprintf("%s until %s", adapter, e.when()))
	ex = a.ExhaustedAdapters(now)
	next, nerr := a.nextAdapter(adapter, ex)
	if p, _, ok := a.chainPick(adapter, a.modelFor(adapter, ""), ex, a.ExhaustedModels(now)); ok {
		next, nerr = p.Label(), nil
	}
	var text string
	if nerr != nil {
		first := firstReset(ex)
		text = fmt.Sprintf("All adapters are out of quota (%s ran out last). New spawns are refused until the first reset: %s at %s. "+
			"Running tasks stay parked and continue after their reset; nothing is killed.", adapter, first.Adapter, first.when())
	} else {
		text = fmt.Sprintf("Adapter %s is out of quota until %s. New spawns that would use %s go to %s instead until then. ", adapter, e.when(), adapter, next)
		if p := a.Cfg.AdapterPolicy; p.Failover == config.FailoverHandoff && (p.HandoffWait <= 0 || until.Sub(now) > p.HandoffWait) {
			text += fmt.Sprintf("Tasks already on %s are handed to the next adapter with quota (a hooked one when they claim paths) to finish in their worktrees.", adapter)
		} else {
			text += fmt.Sprintf("Tasks already on %s stay parked and %s after the reset.", adapter, resumeHow(adapter))
		}
		if ad, err := agent.ByName(next); err == nil && !ad.Hooks() {
			text += fmt.Sprintf(" %s has no hooks: claims of tasks it runs are checked only when they commit and call done.", next)
		}
	}
	return a.tellOwner(text)
}

// restored tells the orchestrator an adapter or model (by Label) has quota
// again: routine news, so no desktop or webhook notification.
func (a *App) restored(label string, still map[string]Exhaustion) {
	a.Store.Event(OrchestratorID, EventAdapterRestored, label)
	what := "Adapter"
	if strings.Contains(label, ":") {
		what = "Model"
	}
	text := fmt.Sprintf("%s %s has quota again; spawns that would use it go back to it.", what, label)
	if len(still) > 0 {
		var names []string
		for n := range still {
			names = append(names, n)
		}
		sort.Strings(names)
		text += " Still out: " + strings.Join(names, ", ") + "."
	}
	_ = a.Notify(OrchestratorID, store.NoticeInfo, text)
}

// ClearExhausted restores an adapter, or a model by its Label, early, e.g.
// when a parked agent's limit banner cleared.
func (a *App) ClearExhausted(adapter string) error {
	exhaustMu.Lock()
	m := a.readExhausted()
	if _, out := m[adapter]; !out {
		exhaustMu.Unlock()
		return nil
	}
	delete(m, adapter)
	err := a.writeExhausted(m)
	exhaustMu.Unlock()
	if err != nil {
		return err
	}
	a.restored(adapter, m)
	return nil
}

// resumeHow says how a task parked on adapter's limit gets going again.
func resumeHow(adapter string) string {
	if agent.AutoResumes(adapter) {
		return "continue on their own"
	}
	return "saddle types them a continue line"
}

// AdapterLimitHit marks the adapter task runs on exhausted: its screen shows
// a usage-limit banner. The reset comes from the banner when saddle can read
// it, otherwise ExhaustFor from now.
// A model [fallback] names is marked out on its own (#483).
func (a *App) AdapterLimitHit(task string, b usage.LimitBanner) error {
	t, err := a.Store.Task(task)
	if err != nil {
		return err
	}
	ad := a.taskAdapter(t).Name()
	if m, ok := a.Cfg.TracksModel(ad, t.Model); ok {
		return a.markModelOut(ad, m, b)
	}
	return a.markOut(ad, b)
}

// AdapterLimitCleared restores task's adapter: its usage-limit banner cleared.
func (a *App) AdapterLimitCleared(task string) error {
	t, err := a.Store.Task(task)
	if err != nil {
		return err
	}
	ad := a.taskAdapter(t).Name()
	if m, ok := a.Cfg.TracksModel(ad, t.Model); ok {
		return a.ClearExhausted(exhaustKey(ad, m))
	}
	return a.ClearExhausted(ad)
}

// RotationBanner is the TUI's line while any adapter is out of quota, or "".
func (a *App) RotationBanner(now time.Time) string {
	all := a.exhausted(now)
	if len(all) == 0 {
		return ""
	}
	var outs []string
	for _, e := range sortedExhaustion(all) {
		outs = append(outs, fmt.Sprintf("%s until %s", e.Label(), e.when()))
	}
	line := "out of quota: " + strings.Join(outs, ", ")
	if p, _, out, err := a.reroute(a.Cfg.Harness, "", pickReq{}, now); err != nil {
		line += "; all adapters out, spawns paused"
	} else if out {
		line += "; new spawns go to " + p.Label()
	}
	return line
}

// nextAdapter is from itself when it is not out of quota, else the best
// adapter with quota that can run here, as [adapters] policy ranks them
// starting from from (#481).
func (a *App) nextAdapter(from string, ex map[string]Exhaustion) (string, error) {
	if _, out := ex[from]; !out {
		return from, nil
	}
	c, err := a.bestAdapter(pickReq{def: from}, ex, time.Now())
	return c.Name, err
}

func sortedExhaustion(m map[string]Exhaustion) []Exhaustion {
	var out []Exhaustion
	for _, e := range m {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Until.Equal(out[j].Until) {
			return out[i].Until.Before(out[j].Until)
		}
		return out[i].Label() < out[j].Label()
	})
	return out
}

func firstReset(m map[string]Exhaustion) Exhaustion {
	if s := sortedExhaustion(m); len(s) > 0 {
		return s[0]
	}
	return Exhaustion{}
}

// spawnPick is the adapter, and maybe the model, a spawn runs on.
type spawnPick struct {
	Name string
	// Model, when ModelSet, is the model a [fallback] or rotation chose in
	// place of the requested one (#483).
	Model    string
	ModelSet bool
	// Rotated names the adapter or adapter:model it replaced because that
	// one is out of quota.
	Rotated string
	Why     string // what picked it
}

// spawnAdapter picks the adapter for a spawn: the requested one, else the
// best one by [adapters] policy for the task's kind and claims, rotated past
// adapters out of quota (#481). A model out on its own, or an adapter out
// with a [fallback] chain, goes to the chain's first entry with quota (#483).
func (a *App) spawnAdapter(requested, model string, r pickReq, force bool) (spawnPick, error) {
	now := time.Now()
	ex := a.ExhaustedAdapters(now)
	if requested != "" {
		if force {
			return spawnPick{Name: requested, Why: "requested"}, nil
		}
		m := a.modelFor(requested, model)
		if e, out := quotaOut(ex, a.ExhaustedModels(now), requested, m); out {
			hint := "no other adapter has quota"
			if p, _, _, err := a.reroute(requested, model, r, now); err == nil {
				hint = "spawn without adapter (or adapter=" + p.Adapter + ") to use " + p.Label()
			}
			what := "adapter " + requested
			if e.Model != "" {
				what = "model " + e.Label()
			}
			return spawnPick{}, fmt.Errorf("%s is out of quota until %s: %s, or pass force to launch it anyway", what, e.when(), hint)
		}
		return spawnPick{Name: requested, Why: "requested"}, nil
	}
	def := a.Cfg.Harness
	r.def = def
	c, err := a.bestAdapter(r, ex, now)
	if err != nil {
		first := firstReset(ex)
		return spawnPick{}, fmt.Errorf("%w; the first reset is %s at %s. Running agents stay parked", ErrAllExhausted, first.Adapter, first.when())
	}
	pick := spawnPick{Name: c.Name, Why: c.Why}
	if _, out := ex[def]; out && c.Name != def {
		pick.Rotated = def
	}
	// A chain for the out default overrides the policy's pick; a model out
	// on the picked adapter moves on down its chain.
	from := c.Name
	if _, out := ex[def]; out {
		if _, _, chained := a.Cfg.FallbackFor(def, a.modelFor(def, model)); chained {
			from = def
		}
	}
	p, e, out, err := a.reroute(from, model, r, now)
	if err != nil {
		return spawnPick{}, fmt.Errorf("%w; %s is out until %s and nothing in its [fallback] chain or [adapters] has quota. Running agents stay parked", ErrAllExhausted, e.Label(), e.when())
	}
	if out {
		pick = spawnPick{Name: p.Adapter, Model: p.Model, ModelSet: true, Rotated: e.Label(), Why: p.Why}
	}
	return pick, nil
}

// rotatedTask records that t runs on ad because from is out of quota, and
// warns the owner when that leaves its claims unenforced.
func (a *App) rotatedTask(t store.Task, from string, ad agent.Adapter, claims []string) error {
	a.Store.Event(t.ID, EventAdapterRotated, from+" -> "+ad.Name())
	if len(claims) == 0 || ad.Hooks() {
		return nil
	}
	return a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
		"%s %q runs on %s because %s is out of quota. %s has no hooks, so saddle checks its claims (%s) only when it commits and calls done, not on each write. Kill it and respawn on a hooked adapter if that matters.",
		t.ID, t.Title, ad.Name(), from, ad.Name(), strings.Join(claims, ", ")))
}

// tellOwner sends text to the orchestrator (and so the TUI chat) and to the
// owner's configured notifier.
func (a *App) tellOwner(text string) error {
	err := a.Notify(OrchestratorID, store.NoticeAction, text)
	if a.OwnerNotify != nil {
		a.OwnerNotify(text)
	} else {
		go a.notifyExternal(text)
	}
	return err
}

// notifyExternal pops a desktop notification and posts the webhook, when
// [notify] sets them. Failures are events; nothing waits on them.
func (a *App) notifyExternal(text string) {
	n := a.Cfg.Notify
	if n.Desktop {
		var cmd *exec.Cmd
		if runtime.GOOS == "darwin" {
			cmd = exec.Command("osascript", "-e", "display notification "+strconv.Quote(text)+" with title \"saddle\"")
		} else {
			cmd = exec.Command("notify-send", "saddle", text)
		}
		if err := cmd.Run(); err != nil {
			a.Store.Event(OrchestratorID, "notify_failed", "desktop: "+err.Error())
		}
	}
	if n.Webhook != "" {
		body, _ := json.Marshal(map[string]string{"text": text})
		c := http.Client{Timeout: 10 * time.Second}
		resp, err := c.Post(n.Webhook, "application/json", bytes.NewReader(body))
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 300 {
				err = fmt.Errorf("status %s", resp.Status)
			}
		}
		if err != nil {
			a.Store.Event(OrchestratorID, "notify_failed", "webhook: "+err.Error())
		}
	}
}

// modelFor is the model a task on adapter runs: the requested one unless it
// names another adapter's family (a ticket's "opus" on grok) or is in
// adapter's exclude_models, then the adapter's configured default, or ""
// (the CLI's own) when that is excluded too (#482).
func (a *App) modelFor(adapter, requested string) string {
	if requested != "" && a.Cfg.ExcludedModel(adapter, requested) == "" {
		if fam := modelFamily(requested); fam == "" || fam == adapter {
			return requested
		}
	}
	var def string
	switch adapter {
	case usage.Grok:
		def = a.Cfg.Grok.Model
	case usage.Claude:
		def = a.Cfg.Claude.Model
	default:
		def = a.Cfg.Adapters[adapter].Model
	}
	if a.Cfg.ExcludedModel(adapter, def) != "" {
		return ""
	}
	return def
}

// excludedModelErr refuses a spawn that names a model some adapter's
// exclude_models lists (#482), saying how to allow it.
func (a *App) excludedModelErr(model string) error {
	if model == "" {
		return nil
	}
	names := config.AdapterNames()
	for n := range a.Cfg.Adapters {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	for _, n := range names {
		if x := a.Cfg.ExcludedModel(n, model); x != "" {
			return fmt.Errorf("spawn: model %q is excluded: [adapters.%s] exclude_models lists %q (excluded: %s), so saddle never runs workers on it. "+
				"Pick another model, or leave model= empty for the adapter's default. The owner can allow it by editing exclude_models in .saddle/config.toml (exclude_models = [] excludes nothing)",
				model, n, x, strings.Join(a.Cfg.ExcludedModels(n), ", "))
		}
	}
	return nil
}

// modelFamily is the adapter a model name belongs to, or "" when unknown.
func modelFamily(model string) string {
	m := strings.ToLower(model)
	switch {
	case m == "opus" || m == "sonnet" || m == "haiku" || m == "fable" || strings.HasPrefix(m, "claude"):
		return usage.Claude
	case strings.HasPrefix(m, "grok"):
		return usage.Grok
	case strings.HasPrefix(m, "gpt") || strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4") || strings.Contains(m, "codex"):
		return usage.Codex
	case strings.HasPrefix(m, "gemini"):
		return agent.GeminiName
	case m == agent.OpenCodeName || strings.HasPrefix(m, agent.OpenCodeName+"/"):
		return agent.OpenCodeName
	}
	return ""
}

var (
	resetClockRe = regexp.MustCompile(`(?i)(?:\b([a-z]{3})[a-z]*\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(?:\d{4}\s+)?)?\b(\d{1,2})(?::(\d{2}))?\s*(am|pm)\b(?:\s*\(([^)]+)\))?`)
	resetInRe    = regexp.MustCompile(`(?i)^\s*in\s+\d`)
	resetPartRe  = regexp.MustCompile(`(?i)(\d+)\s*(days?|hours?|hrs?|minutes?|mins?|seconds?|secs?)\b`)
)

// resetUnits are the lengths of resetPartRe's units, by first letter.
var resetUnits = map[byte]time.Duration{'d': 24 * time.Hour, 'h': time.Hour, 'm': time.Minute, 's': time.Second}

// parseReset reads a banner's reset wording ("3pm (America/New_York)",
// "12:10pm", "Oct 12, 3pm", codex's "Oct 14th, 2026 3:04 PM" and
// "in 2 hours 5 minutes") as the next such time after now.
func parseReset(s string, now time.Time) (time.Time, bool) {
	if resetInRe.MatchString(s) {
		var d time.Duration
		for _, p := range resetPartRe.FindAllStringSubmatch(s, -1) {
			n, _ := strconv.Atoi(p[1])
			d += time.Duration(n) * resetUnits[strings.ToLower(p[2])[0]]
		}
		return now.Add(d), d > 0
	}
	m := resetClockRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, false
	}
	h, _ := strconv.Atoi(m[3])
	mins := 0
	if m[4] != "" {
		mins, _ = strconv.Atoi(m[4])
	}
	if h < 1 || h > 12 || mins > 59 {
		return time.Time{}, false
	}
	h %= 12
	if strings.EqualFold(m[5], "pm") {
		h += 12
	}
	loc := now.Location()
	if m[6] != "" {
		if l, err := time.LoadLocation(strings.TrimSpace(m[6])); err == nil {
			loc = l
		}
	}
	n := now.In(loc)
	if m[1] != "" {
		// A dated reset ("Oct 12, 3pm"): this year's, or next year's once past.
		mon, err := time.Parse("Jan", strings.ToUpper(m[1][:1])+strings.ToLower(m[1][1:]))
		day, _ := strconv.Atoi(m[2])
		if err == nil && day >= 1 && day <= 31 {
			t := time.Date(n.Year(), mon.Month(), day, h, mins, 0, 0, loc)
			if !t.After(n) {
				t = t.AddDate(1, 0, 0)
			}
			return t, true
		}
	}
	t := time.Date(n.Year(), n.Month(), n.Day(), h, mins, 0, 0, loc)
	if !t.After(n) {
		t = t.AddDate(0, 0, 1)
	}
	return t, true
}
