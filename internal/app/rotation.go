package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
)

// ExhaustFor is how long an adapter stays exhausted when its banner names
// no reset saddle can read. A banner that clears sooner restores it.
var ExhaustFor = time.Hour

// Exhaustion is one adapter out of quota.
type Exhaustion struct {
	Adapter string    `json:"adapter"`
	Since   time.Time `json:"since"`
	Until   time.Time `json:"until"`
	Resets  string    `json:"resets,omitempty"` // as the banner words it
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

// ExhaustedAdapters lists the adapters out of quota at now. Ones whose reset
// has passed are dropped, and the owner hears they are back.
func (a *App) ExhaustedAdapters(now time.Time) map[string]Exhaustion {
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
		a.restored(e.Adapter, m)
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
	next, nerr := a.nextAdapter(adapter, m)
	var text string
	if nerr != nil {
		first := firstReset(m)
		text = fmt.Sprintf("All adapters are out of quota (%s ran out last). New spawns are refused until the first reset: %s at %s. "+
			"Running tasks stay parked and continue on their own; nothing is killed.", adapter, first.Adapter, first.when())
	} else {
		text = fmt.Sprintf("Adapter %s is out of quota until %s. New spawns that would use %s go to %s instead until then. "+
			"Tasks already on %s stay parked and continue on their own after the reset.", adapter, e.when(), adapter, next, adapter)
		if ad, err := agent.ByName(next); err == nil && !ad.Hooks() {
			text += fmt.Sprintf(" %s has no hooks: claims of tasks it runs are advisory.", next)
		}
	}
	return a.tellOwner(text)
}

// restored tells the owner an adapter has quota again.
func (a *App) restored(adapter string, still map[string]Exhaustion) {
	a.Store.Event(OrchestratorID, EventAdapterRestored, adapter)
	text := fmt.Sprintf("Adapter %s has quota again; spawns that would use it go back to it.", adapter)
	if len(still) > 0 {
		var names []string
		for n := range still {
			names = append(names, n)
		}
		sort.Strings(names)
		text += " Still out: " + strings.Join(names, ", ") + "."
	}
	_ = a.tellOwner(text)
}

// ClearExhausted restores adapter early, e.g. when a parked agent's limit
// banner cleared.
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

// AdapterLimitHit marks the adapter task runs on exhausted: its screen shows
// a usage-limit banner. The reset comes from the banner when saddle can read
// it, otherwise ExhaustFor from now.
func (a *App) AdapterLimitHit(task string, b usage.LimitBanner) error {
	t, err := a.Store.Task(task)
	if err != nil {
		return err
	}
	now := time.Now()
	until, ok := parseReset(b.Resets, now)
	if !ok {
		until = now.Add(ExhaustFor)
	}
	return a.MarkExhausted(a.taskAdapter(t).Name(), until, b.Resets)
}

// AdapterLimitCleared restores task's adapter: its usage-limit banner cleared.
func (a *App) AdapterLimitCleared(task string) error {
	t, err := a.Store.Task(task)
	if err != nil {
		return err
	}
	return a.ClearExhausted(a.taskAdapter(t).Name())
}

// RotationBanner is the TUI's line while any adapter is out of quota, or "".
func (a *App) RotationBanner(now time.Time) string {
	ex := a.ExhaustedAdapters(now)
	if len(ex) == 0 {
		return ""
	}
	var outs []string
	for _, e := range sortedExhaustion(ex) {
		outs = append(outs, fmt.Sprintf("%s until %s", e.Adapter, e.when()))
	}
	line := "out of quota: " + strings.Join(outs, ", ")
	if next, err := a.nextAdapter(a.Cfg.Harness, ex); err != nil {
		line += "; all adapters out, spawns paused"
	} else if next != a.Cfg.Harness {
		line += "; new spawns go to " + next
	}
	return line
}

// nextAdapter is from itself when it is not out of quota, else the next adapter after
// it in [adapters] order that has quota and can run here.
func (a *App) nextAdapter(from string, ex map[string]Exhaustion) (string, error) {
	if _, out := ex[from]; !out {
		return from, nil
	}
	usable := agent.Usable(a.adapterStatus())
	ok := func(n string) bool {
		_, out := ex[n]
		return !out && slices.Contains(usable, n)
	}
	order := a.Cfg.AdapterOrder
	start := slices.Index(order, from) + 1 // 0 when from is not listed
	for i := range order {
		if n := order[(start+i)%len(order)]; ok(n) {
			return n, nil
		}
	}
	return "", ErrAllExhausted
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
		return out[i].Adapter < out[j].Adapter
	})
	return out
}

func firstReset(m map[string]Exhaustion) Exhaustion {
	if s := sortedExhaustion(m); len(s) > 0 {
		return s[0]
	}
	return Exhaustion{}
}

// spawnAdapter picks the adapter for a spawn: the requested one, else the
// session default, rotated past adapters out of quota. rotated names the
// adapter it replaced.
func (a *App) spawnAdapter(requested string, force bool) (name, rotated string, err error) {
	ex := a.ExhaustedAdapters(time.Now())
	if requested != "" {
		if e, out := ex[requested]; out && !force {
			hint := "no other adapter has quota"
			if next, err := a.nextAdapter(requested, ex); err == nil {
				hint = "spawn without adapter (or adapter=" + next + ") to use " + next
			}
			return "", "", fmt.Errorf("adapter %s is out of quota until %s: %s, or pass force to launch it anyway", requested, e.when(), hint)
		}
		return requested, "", nil
	}
	def := a.Cfg.Harness
	next, err := a.nextAdapter(def, ex)
	if err != nil {
		first := firstReset(ex)
		return "", "", fmt.Errorf("%w; the first reset is %s at %s. Running agents stay parked", ErrAllExhausted, first.Adapter, first.when())
	}
	if next != def {
		rotated = def
	}
	return next, rotated, nil
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
// names another adapter's family (a ticket's "opus" on grok), then the
// adapter's configured default.
func (a *App) modelFor(adapter, requested string) string {
	if requested != "" {
		if fam := modelFamily(requested); fam == "" || fam == adapter {
			return requested
		}
	}
	switch adapter {
	case usage.Grok:
		return a.Cfg.Grok.Model
	case usage.Claude:
		return a.Cfg.Claude.Model
	}
	return ""
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
	}
	return ""
}

var resetClockRe = regexp.MustCompile(`(?i)(?:\b([a-z]{3})[a-z]*\s+(\d{1,2}),?\s+)?\b(\d{1,2})(?::(\d{2}))?\s*(am|pm)\b(?:\s*\(([^)]+)\))?`)

// parseReset reads a banner's reset wording ("3pm (America/New_York)",
// "12:10pm", "Oct 12, 3pm") as the next such time after now.
func parseReset(s string, now time.Time) (time.Time, bool) {
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
