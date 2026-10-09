// Package narrator turns saddle's event log into a short plain-English thread.
//
// A Narrator polls an event Source, batches events for BatchInterval (or
// flushes at once on a salient event: landed, conflict, needs-you, spawn),
// compacts each batch into per-task deltas and asks a cheap model (Claude
// Haiku 4.5 by default) for one line per salient change. Only compact
// structured deltas are sent, never raw pane text. The system prompt and the
// task roster are prompt-cache breakpoints, and a daily cost cap stops API
// calls once reached; past the cap, salient changes still get a plain local
// line.
//
// Ask answers the user's questions from the same roster and recent lines,
// plus an agent's screen when the user opts in, under the same daily cap.
//
// The package is a library: the source, roster, clock, model (or HTTP
// client) and output sink are injected, so nothing here touches SQLite, tmux
// or the TUI.
package narrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// Source yields events that arrived since the previous Poll.
type Source interface {
	Poll(ctx context.Context) ([]store.Event, error)
}

// Roster lists the tasks the narrator may mention. *store.Store satisfies it.
type Roster interface {
	Tasks() ([]store.Task, error)
}

// Clock tells the time. It drives batching and the daily cap's day boundary.
type Clock interface {
	Now() time.Time
}

// Doer sends HTTP requests. *http.Client satisfies it.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Ledger persists the daily spend so a restart does not reset the cap.
// *store.Store satisfies it.
type Ledger interface {
	NarratorSpend(day string) (float64, error)
	SetNarratorSpend(day string, usd float64) error
}

// Sink receives narrator lines.
type Sink interface {
	Emit(Line)
}

// Line is one line of narration.
type Line struct {
	Time     time.Time
	Task     string // empty when the line is not about one task
	Text     string
	NeedsYou bool // the task is waiting on the user; render it highlighted
}

// String renders the line, marking needs-you lines with a leading "‼ ".
func (l Line) String() string {
	s := l.Text
	if l.Task != "" {
		s = l.Task + ": " + s
	}
	if l.NeedsYou {
		s = "‼ " + s
	}
	return s
}

// Config tunes a Narrator. Zero fields take the defaults below.
type Config struct {
	APIKey        string
	Endpoint      string        // default https://api.anthropic.com/v1/messages
	Model         string        // default claude-haiku-4-5
	MaxTokens     int           // default 400
	BatchInterval time.Duration // default 20s; also the retry backoff after an API error
	PollInterval  time.Duration // default 2s; Run only
	DailyCapUSD   float64       // default 1.00
	MaxPending    int           // default 500 events held while the API is down
	// Prices overrides usage.DefaultPrices when costing responses.
	Prices map[string]usage.Price
}

const (
	DefaultEndpoint = "https://api.anthropic.com/v1/messages"
	DefaultModel    = "claude-haiku-4-5"
	apiVersion      = "2023-06-01"
	maxNote         = 120
	maxLines        = 20
	maxRecent       = 30
)

func (c Config) withDefaults() Config {
	if c.Endpoint == "" {
		c.Endpoint = DefaultEndpoint
	}
	if c.Model == "" {
		c.Model = DefaultModel
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = 400
	}
	if c.BatchInterval <= 0 {
		c.BatchInterval = 20 * time.Second
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.DailyCapUSD <= 0 {
		c.DailyCapUSD = 1
	}
	if c.MaxPending <= 0 {
		c.MaxPending = 500
	}
	return c
}

// Deps are the Narrator's collaborators. Clock and HTTP default to the real
// clock and an http.Client with a timeout. Model defaults to the Messages API
// over HTTP.
type Deps struct {
	Source Source
	Roster Roster
	Clock  Clock
	HTTP   Doer
	Model  Model
	Sink   Sink
	Ledger Ledger      // optional; without it spend is kept in memory only
	Heavy  HeavySource // optional; the heavy-run queue, for long waits and overdue runs
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Narrator batches events and narrates them. Drive it from one goroutine
// with Run or Step; Ask may be called from any other.
type Narrator struct {
	cfg  Config
	deps Deps

	pending    []store.Event
	batchStart time.Time
	retryAt    time.Time
	hv         heavy

	mu     sync.Mutex // guards the fields below, shared with Ask
	day    string
	spent  float64
	recent []Line // the latest lines emitted, for answering questions
}

// New returns a Narrator.
func New(cfg Config, deps Deps) *Narrator {
	if deps.Clock == nil {
		deps.Clock = realClock{}
	}
	if deps.HTTP == nil {
		deps.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	cfg = cfg.withDefaults()
	if deps.Model == nil {
		deps.Model = apiModel{endpoint: cfg.Endpoint, key: cfg.APIKey, http: deps.HTTP}
	}
	return &Narrator{cfg: cfg, deps: deps}
}

// Run calls Step every PollInterval until ctx is done. Step errors go to
// onErr (which may be nil); events from a failed call are kept and retried.
func (n *Narrator) Run(ctx context.Context, onErr func(error)) error {
	t := time.NewTicker(n.cfg.PollInterval)
	defer t.Stop()
	for {
		if err := n.Step(ctx); err != nil && onErr != nil {
			onErr(err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Pending is the number of events waiting to be narrated.
func (n *Narrator) Pending() int { return len(n.pending) }

// SpentToday is the estimated API spend in USD for the current day.
func (n *Narrator) SpentToday() float64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.rollDay()
	return n.spent
}

// CapReached reports whether today's spend has hit DailyCapUSD.
func (n *Narrator) CapReached() bool { return n.SpentToday() >= n.cfg.DailyCapUSD }

// rollDay resets spend at a day boundary. Hold mu.
func (n *Narrator) rollDay() {
	if d := n.deps.Clock.Now().Format(time.DateOnly); d != n.day {
		n.day, n.spent = d, 0
		if n.deps.Ledger != nil {
			// On a read error, assume nothing spent: the cap is a soft budget.
			n.spent, _ = n.deps.Ledger.NarratorSpend(d)
		}
	}
}

// Step polls the source once and flushes the batch if it is due: a salient
// event is pending, or the oldest pending event is BatchInterval old. After
// an API error nothing is flushed until BatchInterval has passed.
func (n *Narrator) Step(ctx context.Context) (err error) {
	now := n.deps.Clock.Now()
	es, err := n.deps.Source.Poll(ctx)
	if err != nil {
		return fmt.Errorf("narrator: poll events: %w", err)
	}
	es = n.takeBackpressure(now, es)
	// A queue that can't be read is reported, but this poll's events
	// still go on.
	if herr := n.checkHeavy(now); herr != nil {
		defer func() {
			if err == nil {
				err = herr
			}
		}()
	}
	if len(es) > 0 {
		if len(n.pending) == 0 {
			n.batchStart = now
		}
		n.pending = append(n.pending, es...)
		n.trim()
	}
	if len(n.pending) == 0 || now.Before(n.retryAt) {
		return nil
	}
	if !n.hasSalient() && now.Sub(n.batchStart) < n.cfg.BatchInterval {
		return nil
	}
	return n.flush(ctx, now)
}

func (n *Narrator) hasSalient() bool {
	for _, e := range n.pending {
		if Salient(e.Kind) != "" {
			return true
		}
	}
	return false
}

// trim keeps pending within MaxPending, dropping the oldest routine events
// first so salient ones survive a long API outage.
func (n *Narrator) trim() {
	over := len(n.pending) - n.cfg.MaxPending
	if over <= 0 {
		return
	}
	kept := n.pending[:0]
	for _, e := range n.pending {
		if over > 0 && Salient(e.Kind) == "" {
			over--
			continue
		}
		kept = append(kept, e)
	}
	n.pending = kept[over:]
}

func (n *Narrator) flush(ctx context.Context, now time.Time) error {
	tasks, err := n.deps.Roster.Tasks()
	if err != nil {
		return fmt.Errorf("narrator: roster: %w", err)
	}
	deltas := Compact(n.pending, tasks)

	if n.CapReached() {
		for _, d := range deltas {
			for _, c := range d.Changes {
				n.out(Line{Time: now, Task: d.Task, Text: c.String(), NeedsYou: c.Kind == ChangeNeedsYou})
			}
		}
		n.pending = nil
		return nil
	}

	text, err := n.call(ctx, tasks, deltas)
	if err != nil {
		n.retryAt = now.Add(n.cfg.BatchInterval)
		return err
	}
	n.pending = nil
	n.emit(now, text, deltas)
	return nil
}

// out emits a line and keeps it for answering questions.
func (n *Narrator) out(l Line) {
	n.mu.Lock()
	n.recent = append(n.recent, l)
	if len(n.recent) > maxRecent {
		n.recent = n.recent[len(n.recent)-maxRecent:]
	}
	n.mu.Unlock()
	n.deps.Sink.Emit(l)
}

// emit writes the model's lines, then a local line for any needs-you task the
// model left out: those must never be dropped.
func (n *Narrator) emit(now time.Time, text string, deltas []TaskDelta) {
	inBatch := map[string]bool{}
	needs := map[string]Change{}
	for _, d := range deltas {
		inBatch[d.Task] = true
		for _, c := range d.Changes {
			if c.Kind == ChangeNeedsYou {
				needs[d.Task] = c
			}
		}
	}
	covered := map[string]bool{}
	count := 0
	for _, raw := range strings.Split(text, "\n") {
		s := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(raw), "-*‼!• "))
		if s == "" || count >= maxLines {
			continue
		}
		var task string
		if id, rest, ok := strings.Cut(s, ":"); ok && inBatch[strings.TrimSpace(id)] {
			task, s = strings.TrimSpace(id), strings.TrimSpace(rest)
		}
		_, nu := needs[task]
		covered[task] = true
		count++
		n.out(Line{Time: now, Task: task, Text: s, NeedsYou: nu})
	}
	var missing []string
	for task := range needs {
		if !covered[task] {
			missing = append(missing, task)
		}
	}
	sort.Strings(missing)
	for _, task := range missing {
		n.out(Line{Time: now, Task: task, Text: needs[task].String(), NeedsYou: true})
	}
}

// Change kinds: the salient events that warrant their own line.
const (
	ChangeLanded   = "landed"
	ChangeConflict = "conflict"
	ChangeNeedsYou = "needs_you"
	ChangeSpawn    = "spawn"
)

// Salient maps an event kind to its change kind, or "" for a routine event.
func Salient(kind string) string {
	switch kind {
	case "landed":
		return ChangeLanded
	case "train_conflict", "train_test_failed", "restack_conflict":
		return ChangeConflict
	case "notification":
		return ChangeNeedsYou
	case "spawn":
		return ChangeSpawn
	}
	return ""
}

// Change is one salient event in a delta.
type Change struct {
	Kind string `json:"kind"`
	Note string `json:"note,omitempty"` // truncated event data
}

// String is the local, model-free rendering of a change.
func (c Change) String() string {
	s := map[string]string{
		ChangeLanded:   "landed",
		ChangeConflict: "hit a conflict",
		ChangeNeedsYou: "needs you",
		ChangeSpawn:    "spawned",
	}[c.Kind]
	if s == "" {
		s = c.Kind
	}
	if c.Note != "" {
		s += ": " + c.Note
	}
	return s
}

// TaskDelta is what changed for one task in a batch. Routine events are only
// counted by kind; their data is never sent.
type TaskDelta struct {
	Task    string         `json:"task"`
	Title   string         `json:"title,omitempty"`
	Status  string         `json:"status,omitempty"` // status implied by the latest salient change
	Events  map[string]int `json:"events"`
	Changes []Change       `json:"changes,omitempty"`
}

// Compact folds events into one delta per task, sorted by task id.
func Compact(es []store.Event, tasks []store.Task) []TaskDelta {
	titles := map[string]string{}
	for _, t := range tasks {
		titles[t.ID] = t.Title
	}
	by := map[string]*TaskDelta{}
	for _, e := range es {
		d := by[e.Task]
		if d == nil {
			d = &TaskDelta{Task: e.Task, Title: titles[e.Task], Events: map[string]int{}}
			by[e.Task] = d
		}
		d.Events[e.Kind]++
		kind := Salient(e.Kind)
		if kind == "" {
			continue
		}
		d.Changes = append(d.Changes, Change{Kind: kind, Note: truncate(e.Data, maxNote)})
		d.Status = map[string]string{
			ChangeLanded:   store.Landed,
			ChangeConflict: store.Conflict,
			ChangeNeedsYou: store.NeedsYou,
			ChangeSpawn:    store.Running,
		}[kind]
	}
	out := make([]TaskDelta, 0, len(by))
	for _, d := range by {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Task < out[j].Task })
	return out
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// rosterText lists active tasks by id. It leaves out status and other
// volatile fields so the block stays byte-stable and cacheable between calls.
func rosterText(tasks []store.Task) string {
	ts := append([]store.Task(nil), tasks...)
	sort.Slice(ts, func(i, j int) bool { return ts[i].ID < ts[j].ID })
	var b strings.Builder
	b.WriteString("Task roster (id | role | parent | title):\n")
	for _, t := range ts {
		if !t.Active() {
			continue
		}
		fmt.Fprintf(&b, "%s | %s | %s | %s\n", t.ID, t.Role, t.Parent, t.Title)
	}
	return b.String()
}

const systemPrompt = `You narrate a team of parallel coding agents for the human supervising them.

Each request gives you the task roster, then a JSON array of per-task deltas from the last few seconds: event counts by kind and the salient changes (landed, conflict, needs_you, spawn) with short notes.

Write one line per salient change, and at most one line per task otherwise. Format each line exactly as "<task id>: <sentence>". Keep each sentence under 15 words, plain English, present tense, no markdown. Lead with needs_you items: say what the agent is waiting on so the human can act. Skip tasks whose only activity is routine tool use unless nothing else happened. Never invent changes that are not in the deltas.`

func (n *Narrator) call(ctx context.Context, tasks []store.Task, deltas []TaskDelta) (string, error) {
	delta, err := json.Marshal(deltas)
	if err != nil {
		return "", err
	}
	return n.complete(ctx, Request{
		MaxTokens: n.cfg.MaxTokens,
		System:    systemPrompt,
		Blocks:    []Block{{Text: rosterText(tasks), Cache: true}, {Text: string(delta)}},
	})
}

// complete sends r to the model and charges its tokens to today's spend.
func (n *Narrator) complete(ctx context.Context, r Request) (string, error) {
	r.Model = n.cfg.Model
	resp, err := n.deps.Model.Complete(ctx, r)
	if err != nil {
		return "", err
	}
	n.mu.Lock()
	n.rollDay()
	n.spent += usage.CostUSD(n.cfg.Model, resp.Tokens, n.cfg.Prices)
	if n.deps.Ledger != nil {
		_ = n.deps.Ledger.SetNarratorSpend(n.day, n.spent)
	}
	n.mu.Unlock()
	return resp.Text, nil
}
