package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/tmux"
	"github.com/brandonapol/saddle/internal/usage"
)

// Orchestrator auto-compaction (#179): the watcher reads how full the
// orchestrator's context is, wakes it once per crossing of
// orchestrator.compact_at, and types the harness's compact command when the
// orchestrator is idle and the owner isn't typing.

// Event kinds the compact watcher records against the orchestrator.
const (
	EventCompactThreshold = "compact-threshold" // context use crossed compact_at
	EventCompact          = "compact"           // the compact command was sent
	EventCompactSkip      = "compact-skip"      // no command for the harness, or gave up waiting
)

const (
	// DefaultCompactAt is orchestrator.compact_at when unset.
	DefaultCompactAt = 0.7
	// DefaultContextWindow is the window of a model the table doesn't know.
	DefaultContextWindow int64 = 200_000
	// DefaultCompactInterval is how often the watcher reads the transcript.
	DefaultCompactInterval = 30 * time.Second
	// DefaultCompactPendingFor is how long a crossing waits for an idle
	// orchestrator before the watcher gives up on it.
	DefaultCompactPendingFor = 30 * time.Minute
)

const longContext int64 = 1_000_000

// contextWindows maps model name prefixes to context windows, longest match
// first. Config-free on purpose: a wrong guess only moves when the notice fires.
var contextWindows = []struct {
	prefix string
	tokens int64
}{
	{"grok-code-fast", 256_000},
	{"grok-4", 256_000},
	{"grok-3", 131_072},
	{"gpt-5", 272_000},
	{"codex", 272_000},
	{"claude-", 200_000},
	{"opus", 200_000},
	{"sonnet", 200_000},
	{"haiku", 200_000},
	{"fable", 200_000},
}

// ContextWindow is model's context window in tokens. A "[1m]" suffix (Claude
// Code's long-context alias) means one million.
func ContextWindow(model string) int64 {
	m := strings.ToLower(model)
	if strings.HasSuffix(m, "[1m]") {
		return longContext
	}
	for _, w := range contextWindows {
		if strings.HasPrefix(m, w.prefix) {
			return w.tokens
		}
	}
	return DefaultContextWindow
}

// ContextUse is how full a session's context is: the prompt of its latest
// response against its model's window.
type ContextUse struct {
	Model  string
	Prompt int64 // input + cache read + cache creation of the latest response
	Window int64
}

// NewContextUse sizes tok against model's window. configured is the model
// saddle launched with, which may carry "[1m]" where the transcript's name
// doesn't. A Claude prompt larger than its table window means the session
// has the 1M window.
func NewContextUse(model, configured string, tok usage.Tokens) ContextUse {
	u := ContextUse{Model: model, Prompt: tok.Input + tok.CacheRead + tok.CacheCreation, Window: ContextWindow(model)}
	if ContextWindow(configured) == longContext {
		u.Window = longContext
	}
	if u.Prompt > u.Window && u.Window < longContext && strings.HasPrefix(strings.ToLower(model), "claude-") {
		u.Window = longContext
	}
	return u
}

// Fraction is Prompt/Window, 0 when nothing has been read.
func (u ContextUse) Fraction() float64 {
	if u.Window <= 0 {
		return 0
	}
	return float64(u.Prompt) / float64(u.Window)
}

// ContextReader tails one transcript and keeps its latest usage record, so
// repeated reads cost only the new lines.
type ContextReader struct {
	Agent      string // usage.Claude, usage.Codex or usage.Grok
	Configured string // the launch model, see NewContextUse
	path       string
	cur        usage.Cursor
	last       *usage.Record
}

// Read returns the context use at the end of the transcript at path. A new
// path starts over; a missing transcript reads as zero.
func (r *ContextReader) Read(path string) (ContextUse, error) {
	if path != r.path {
		r.path, r.cur, r.last = path, usage.Cursor{}, nil
	}
	parse := usage.ParserFor(r.Agent)
	if parse == nil {
		return ContextUse{}, fmt.Errorf("no transcript parser for %q", r.Agent)
	}
	if path != "" {
		recs, err := usage.Tail(path, &r.cur, parse)
		if err != nil {
			return ContextUse{}, err
		}
		if n := len(recs); n > 0 {
			r.last = &recs[n-1]
		}
	}
	if r.last == nil {
		return ContextUse{}, nil
	}
	return NewContextUse(r.last.Model, r.Configured, r.last.Tokens), nil
}

// orchestratorAgent is the usage agent kind the orchestrator runs as.
func (a *App) orchestratorAgent() string {
	if a.Cfg.Harness == config.HarnessGrok {
		return usage.Grok
	}
	return usage.Claude
}

// orchestratorTranscript is where the orchestrator's transcript should be, or
// "" before its session is known.
func (a *App) orchestratorTranscript() (string, error) {
	t, err := a.Store.Task(OrchestratorID)
	if err != nil {
		return "", err
	}
	var src agent.UsageSource
	if a.orchestratorAgent() == usage.Grok {
		src = agent.Grok{}.Usage()
	} else {
		dir := os.Getenv("CLAUDE_CONFIG_DIR")
		src = agent.Claude{ConfigDir: dir}.Usage()
		if t.SessionID == "" {
			return "", nil
		}
	}
	return src.Transcript(t.Worktree, a.stateDir("run", t.ID), t.SessionID), nil
}

// OrchestratorContext reads how full the orchestrator's context is now. It
// reads the whole transcript; the watcher keeps a ContextReader instead.
func (a *App) OrchestratorContext() (ContextUse, error) {
	path, err := a.orchestratorTranscript()
	if err != nil {
		return ContextUse{}, err
	}
	r := &ContextReader{Agent: a.orchestratorAgent(), Configured: a.orchModel()}
	return r.Read(path)
}

// CompactAt is orchestrator.compact_at, the context fraction at which the
// orchestrator is told to compact. Unset or outside (0, 1) means
// DefaultCompactAt.
//
// It reads the key straight from the config files until config.Config grows
// an Orchestrator section; then this becomes a.Cfg.Orchestrator.CompactAt.
func (a *App) CompactAt() float64 {
	var f struct {
		Orchestrator struct {
			CompactAt *float64 `toml:"compact_at"`
		} `toml:"orchestrator"`
	}
	paths := []string{filepath.Join(a.Root, ".saddle", "config.toml")}
	if home, err := os.UserConfigDir(); err == nil {
		paths = append([]string{filepath.Join(home, "saddle", "config.toml")}, paths...)
	}
	for _, p := range paths {
		_, _ = toml.DecodeFile(p, &f) // a missing or bad file leaves the value; config.Load reports bad ones
	}
	if v := f.Orchestrator.CompactAt; v != nil && *v > 0 && *v < 1 {
		return *v
	}
	return DefaultCompactAt
}

// compactKeep is what a compaction must carry over.
const compactKeep = "running tasks and their status, open PRs, queued follow-ups, owner decisions pending, rules in force"

// CompactCommand is the text that compacts a session of the agent kind, and
// false when the harness has none. It is one line: a newline would submit early.
func CompactCommand(agentKind string) (string, bool) {
	switch agentKind {
	case usage.Claude:
		return "/compact Keep a short saddle state summary: " + compactKeep + ".", true
	case usage.Codex:
		return "/compact", true // takes no instruction
	}
	return "", false
}

// CompactTarget is the orchestrator's input box as the watcher sees it.
type CompactTarget interface {
	Busy() bool     // a turn is in progress
	Drafting() bool // the owner has text in the input
	Send(text string) error
}

// FuncTarget adapts plain funcs, e.g. the TUI's orch.Proc and its input box.
type FuncTarget struct {
	BusyFn, DraftingFn func() bool
	SendFn             func(string) error
}

func (f FuncTarget) Busy() bool          { return f.BusyFn != nil && f.BusyFn() }
func (f FuncTarget) Drafting() bool      { return f.DraftingFn != nil && f.DraftingFn() }
func (f FuncTarget) Send(s string) error { return f.SendFn(s) }

// PaneTarget is an orchestrator running interactively in a tmux pane. It
// sends through tmux.SendWhenIdle, which re-checks for a draft.
type PaneTarget struct {
	Tmux   tmux.Driver
	Window string
	BusyFn func() bool
}

func (p PaneTarget) Busy() bool {
	return !p.Tmux.Alive(p.Window) || (p.BusyFn != nil && p.BusyFn())
}

func (p PaneTarget) Drafting() bool {
	pane, err := p.Tmux.Capture(p.Window, 15)
	return err != nil || tmux.HasDraft(pane)
}

func (p PaneTarget) Send(s string) error {
	tmux.SendWhenIdle(p.Tmux, p.Window, s, nil)
	return nil
}

// CompactWatcher wakes the orchestrator when its context passes Threshold
// and compacts it once it is idle. Check runs one step; tests call it with
// an injected clock, usage and target.
type CompactWatcher struct {
	App        *App
	Threshold  float64
	Interval   time.Duration
	PendingFor time.Duration // give up on a crossing not compacted by then
	Harness    string        // usage agent kind, picks the command
	Now        func() time.Time
	Usage      func() (ContextUse, error)

	mu     sync.Mutex
	target CompactTarget

	armed   bool      // below the threshold since the last crossing
	pending bool      // crossed, not compacted yet
	since   time.Time // when pending began
	lastErr string
}

// NewCompactWatcher returns a watcher for a's orchestrator with the configured
// threshold. It has no target until SetTarget, so it only notifies.
func (a *App) NewCompactWatcher() *CompactWatcher {
	r := &ContextReader{Agent: a.orchestratorAgent(), Configured: a.orchModel()}
	return &CompactWatcher{
		App: a, Threshold: a.CompactAt(), Interval: DefaultCompactInterval,
		PendingFor: DefaultCompactPendingFor, Harness: a.orchestratorAgent(), Now: time.Now,
		Usage: func() (ContextUse, error) {
			path, err := a.orchestratorTranscript()
			if err != nil {
				return ContextUse{}, err
			}
			return r.Read(path)
		},
		armed: true,
	}
}

// SetTarget sets where this watcher types the compact command, over the
// App-wide target; nil falls back to it. Safe to call while Run runs.
func (w *CompactWatcher) SetTarget(t CompactTarget) {
	w.mu.Lock()
	w.target = t
	w.mu.Unlock()
}

func (w *CompactWatcher) getTarget() CompactTarget {
	w.mu.Lock()
	t := w.target
	w.mu.Unlock()
	if t != nil {
		return t
	}
	if v, ok := compactTargets.Load(w.App); ok {
		return v.(CompactTarget)
	}
	return nil
}

// compactTargets holds each App's orchestrator input, set by whatever runs
// the orchestrator (the TUI). It lives here, not on App, until App grows a field.
var compactTargets sync.Map // *App -> CompactTarget

// SetCompactTarget registers the orchestrator's input for every compact
// watcher of a; nil unregisters it (the orchestrator stopped).
func (a *App) SetCompactTarget(t CompactTarget) {
	if t == nil {
		compactTargets.Delete(a)
		return
	}
	compactTargets.Store(a, t)
}

// CompactReport is the outcome of one Check.
type CompactReport struct {
	Fraction float64
	Crossed  bool   // this check crossed the threshold and notified
	Injected bool   // this check sent the compact command
	GaveUp   bool   // the crossing waited PendingFor and was dropped
	Waiting  string // why a pending compaction wasn't sent: busy, drafting, no target
}

// Check reads the context, notifies on a crossing and compacts when it can.
func (w *CompactWatcher) Check() (CompactReport, error) {
	use, err := w.Usage()
	if err != nil {
		return CompactReport{}, err
	}
	frac := use.Fraction()
	rep := CompactReport{Fraction: frac}
	if frac < w.Threshold {
		w.armed, w.pending = true, false
		return rep, nil
	}
	a := w.App
	pct := fmt.Sprintf("%.0f%%", frac*100)
	cmd, ok := CompactCommand(w.Harness)
	if w.armed {
		w.armed, rep.Crossed = false, true
		a.Store.Event(OrchestratorID, EventCompactThreshold, fmt.Sprintf("context at %s (threshold %.0f%%)", pct, w.Threshold*100))
		if !ok {
			a.Store.Event(OrchestratorID, EventCompactSkip, "harness "+w.Harness+" has no compact command")
			return rep, a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
				"Your context is at %s. Harness %s has no compact command saddle can send: write a short state summary (%s) and ask the owner to start a fresh session.", pct, w.Harness, compactKeep))
		}
		w.pending, w.since = true, w.Now()
		if err := a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
			"Your context is at %s. At the next natural breakpoint, compact: saddle sends `%s` once you are idle and the owner isn't typing. Run it yourself, or ask the owner to, if it doesn't arrive.", pct, cmd)); err != nil {
			return rep, err
		}
	}
	if !w.pending {
		return rep, nil
	}
	if w.Now().Sub(w.since) >= w.PendingFor {
		w.pending, rep.GaveUp = false, true
		a.Store.Event(OrchestratorID, EventCompactSkip, "orchestrator stayed busy for "+w.PendingFor.String()+"; not compacting this crossing")
		return rep, nil
	}
	t := w.getTarget()
	switch {
	case t == nil:
		rep.Waiting = "no target"
	case t.Busy():
		rep.Waiting = "busy"
	case t.Drafting():
		rep.Waiting = "drafting"
	}
	if rep.Waiting != "" {
		return rep, nil
	}
	if err := t.Send(cmd); err != nil {
		return rep, fmt.Errorf("compact orchestrator: %w", err)
	}
	w.pending, rep.Injected = false, true
	a.Store.Event(OrchestratorID, EventCompact, "sent "+strings.Fields(cmd)[0]+" at "+pct)
	return rep, nil
}

// Run checks now and then every Interval until ctx ends. Failed checks are
// recorded as events, once per distinct error.
func (w *CompactWatcher) Run(ctx context.Context) error {
	iv := w.Interval
	if iv <= 0 {
		iv = DefaultCompactInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		if _, err := w.Check(); err != nil {
			if msg := err.Error(); msg != w.lastErr {
				w.lastErr = msg
				w.App.Store.Event(OrchestratorID, "error", msg)
			}
		} else {
			w.lastErr = ""
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
