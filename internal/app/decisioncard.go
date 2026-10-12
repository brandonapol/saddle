package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// Decision cards (#404). Every interrupt-class ask (a stuck stack, a CI
// repair cap, ask_owner, a risky prompt) is a card with fixed fields, so an
// owner who walked away can decide in 30 seconds: what happened, what it
// blocks, numbered options with one recommended, and what saddle does if
// nobody answers. The default and its timeout are carried on the card, so
// the owner can always walk away (#119); the stall budget (#406) runs them
// through ExpireDecisions. A bare "1", "2" or "3" answers the oldest open
// card addressed to the owner (AnswerDecision).

// Kinds of decision card.
const (
	DecisionStuckStack  = "stuck-stack"
	DecisionCIRepairCap = "ci-repair-cap"
	DecisionAskOwner    = "ask-owner"
	DecisionRiskyPrompt = "risky-prompt"
)

// Who closed a card.
const (
	DecidedByOwner   = "owner"
	DecidedByTimeout = "timeout"
	DecidedResolved  = "resolved" // the situation cleared on its own
)

// Events logging a card's life. Their data starts with the card's ID.
const (
	EventDecisionOpen          = "decision_open"
	EventDecisionAnswered      = "decision_answered"
	EventDecisionDefault       = "decision_default"
	EventDecisionDefaultFailed = "decision_default_failed"
	EventDecisionResolved      = "decision_resolved"
)

// DefaultDecisionTimeout is how long a card waits for an answer before
// saddle runs its default.
const DefaultDecisionTimeout = 30 * time.Minute

// DecisionOption is one numbered choice on a card.
type DecisionOption struct {
	Label       string `json:"label"`          // what it does, in plain words
	Cost        string `json:"cost,omitempty"` // its consequence: who does it, how long, what it leaves
	Action      string `json:"action"`         // what saddle or the orchestrator runs for it
	Recommended bool   `json:"recommended,omitempty"`
}

// DecisionCard is one needs-you ask.
type DecisionCard struct {
	ID       string           `json:"id"` // stable per situation: a re-ask while open is the same card
	Kind     string           `json:"kind"`
	To       string           `json:"to"`   // who must answer: OrchestratorID for the owner, or a task
	Task     string           `json:"task"` // the task it is about
	Headline string           `json:"headline"`
	What     string           `json:"what"`     // one plain line
	Blocking string           `json:"blocking"` // one plain line
	Options  []DecisionOption `json:"options"`
	Default  int              `json:"default"` // 1-based option run if ignored
	Timeout  time.Duration    `json:"timeout"`
	Opened   time.Time        `json:"opened"`
	Closed   time.Time        `json:"closed,omitzero"`
	Answer   int              `json:"answer,omitempty"` // the option taken
	By       string           `json:"by,omitempty"`     // DecidedByOwner or DecidedByTimeout
}

// Open reports whether nobody has answered the card and it hasn't timed out.
func (c DecisionCard) Open() bool { return c.Closed.IsZero() }

// Due reports whether an open card's timeout has run out at now.
func (c DecisionCard) Due(now time.Time) bool {
	return c.Open() && !now.Before(c.Opened.Add(c.Timeout))
}

// DefaultOption is the option run if nobody answers.
func (c DecisionCard) DefaultOption() DecisionOption { return c.Options[c.Default-1] }

// Validate checks that every field a walk-away owner needs is filled.
func (c DecisionCard) Validate() error {
	var errs []error
	for name, v := range map[string]string{"id": c.ID, "kind": c.Kind, "to": c.To, "headline": c.Headline, "what": c.What, "blocking": c.Blocking} {
		if strings.TrimSpace(v) == "" {
			errs = append(errs, fmt.Errorf("no %s", name))
		}
	}
	for name, v := range map[string]string{"headline": c.Headline, "what": c.What, "blocking": c.Blocking} {
		if strings.Contains(v, "\n") {
			errs = append(errs, fmt.Errorf("%s is more than one line", name))
		}
	}
	if len(c.Options) < 2 {
		errs = append(errs, errors.New("fewer than two options"))
	}
	rec := 0
	for i, o := range c.Options {
		if strings.TrimSpace(o.Label) == "" || strings.TrimSpace(o.Action) == "" {
			errs = append(errs, fmt.Errorf("option %d lacks a label or an action", i+1))
		}
		if o.Recommended {
			rec++
		}
	}
	if rec != 1 {
		errs = append(errs, fmt.Errorf("%d options recommended, want 1", rec))
	}
	if c.Default < 1 || c.Default > len(c.Options) {
		errs = append(errs, fmt.Errorf("default %d is not an option", c.Default))
	}
	if c.Timeout <= 0 {
		errs = append(errs, errors.New("no timeout"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("decision card %s: %w", c.ID, err)
	}
	return nil
}

// Render is the card as the owner reads it. The format is fixed here, not
// left to a model.
func (c DecisionCard) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "‼ %s\n", c.Headline)
	fmt.Fprintf(&b, "What:     %s\n", c.What)
	fmt.Fprintf(&b, "Blocking: %s\n", c.Blocking)
	b.WriteString("Options:\n")
	for i, o := range c.Options {
		var notes []string
		if o.Recommended {
			notes = append(notes, "recommended")
		}
		if o.Cost != "" {
			notes = append(notes, o.Cost)
		}
		fmt.Fprintf(&b, "  %d. %s", i+1, o.Label)
		if len(notes) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(notes, "; "))
		}
		fmt.Fprintf(&b, ": `%s`\n", o.Action)
	}
	if c.Default >= 1 && c.Default <= len(c.Options) {
		fmt.Fprintf(&b, "If you ignore it: saddle runs option %d (%s) after %s.\n", c.Default, c.DefaultOption().Label, fmtDuration(c.Timeout))
	}
	fmt.Fprintf(&b, "Reply %s.", replyChoices(len(c.Options)))
	return b.String()
}

// Line is a closed card collapsed to one line.
func (c DecisionCard) Line() string {
	if c.Open() {
		return "‼ " + c.Headline + " (open)"
	}
	how := "you chose"
	switch c.By {
	case DecidedByTimeout:
		how = "nobody answered, so saddle ran"
	case DecidedResolved:
		return "✓ " + c.Headline + ": it cleared on its own."
	}
	label := ""
	if c.Answer >= 1 && c.Answer <= len(c.Options) {
		label = " (" + c.Options[c.Answer-1].Label + ")"
	}
	return fmt.Sprintf("✓ %s: %s option %d%s.", c.Headline, how, c.Answer, label)
}

func replyChoices(n int) string {
	var s []string
	for i := 1; i <= n; i++ {
		s = append(s, strconv.Itoa(i))
	}
	if len(s) < 2 {
		return strings.Join(s, "")
	}
	return strings.Join(s[:len(s)-1], ", ") + " or " + s[len(s)-1]
}

// fmtDuration prints 30m, 1h or 1h30m.
func fmtDuration(d time.Duration) string {
	s := strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// ParseDecisionReply reads a bare option number ("2", "2.") as an answer.
func ParseDecisionReply(text string) (int, bool) {
	s := strings.TrimSuffix(strings.TrimSpace(text), ".")
	if len(s) != 1 || s[0] < '1' || s[0] > '9' {
		return 0, false
	}
	return int(s[0] - '0'), true
}

// decisionsKeep is how long closed cards are kept for the needs-you band.
const decisionsKeep = 24 * time.Hour

func (a *App) decisionsPath() string { return a.stateDir("decisions.json") }

// withDecisions runs fn on the saved cards under a file lock and saves them
// if it returns nil. The sentinel, the TUI and MCP calls all reach it.
func (a *App) withDecisions(fn func(cs *[]DecisionCard) error) error {
	if err := os.MkdirAll(a.stateDir(), 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(a.decisionsPath()+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) }()
	cs, err := a.readDecisions()
	if err != nil {
		return err
	}
	if err := fn(&cs); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.decisionsPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.decisionsPath())
}

func (a *App) readDecisions() ([]DecisionCard, error) {
	b, err := os.ReadFile(a.decisionsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cs []DecisionCard
	if err := json.Unmarshal(b, &cs); err != nil {
		return nil, fmt.Errorf("%s: %w", a.decisionsPath(), err)
	}
	return cs, nil
}

// Decisions lists the saved cards, open and recently closed, oldest first.
func (a *App) Decisions() ([]DecisionCard, error) { return a.readDecisions() }

// OpenDecisions lists the open cards, oldest first.
func (a *App) OpenDecisions() ([]DecisionCard, error) {
	cs, err := a.readDecisions()
	return slices.DeleteFunc(cs, func(c DecisionCard) bool { return !c.Open() }), err
}

// OpenDecision saves c and sends it to c.To as an action notice. A card
// whose ID is already open is not saved or sent again: one card per
// blocker, never a re-ping.
func (a *App) OpenDecision(c DecisionCard) error {
	if b, ok := a.stallBudget(c.Kind); ok {
		c.Timeout = b // [stall] budget (#406)
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Opened.IsZero() {
		c.Opened = time.Now().UTC()
	}
	fresh := false
	err := a.withDecisions(func(cs *[]DecisionCard) error {
		if slices.ContainsFunc(*cs, func(o DecisionCard) bool { return o.ID == c.ID && o.Open() }) {
			return nil
		}
		*cs = slices.DeleteFunc(*cs, func(o DecisionCard) bool {
			return !o.Open() && c.Opened.Sub(o.Closed) > decisionsKeep
		})
		*cs = append(*cs, c)
		slices.SortStableFunc(*cs, func(x, y DecisionCard) int { return x.Opened.Compare(y.Opened) })
		fresh = true
		return nil
	})
	if err != nil || !fresh {
		return err
	}
	a.Store.Event(c.Task, EventDecisionOpen, c.ID)
	return a.Notify(c.To, store.NoticeAction, c.Render())
}

// AnswerDecision takes text as the owner's answer to the oldest open card
// addressed to the owner, if it is a bare option number that card has. It
// closes the card and returns it with the option chosen; the caller runs
// the option's action.
func (a *App) AnswerDecision(text string, now time.Time) (DecisionCard, DecisionOption, bool, error) {
	n, ok := ParseDecisionReply(text)
	if !ok {
		return DecisionCard{}, DecisionOption{}, false, nil
	}
	var card DecisionCard
	found := false
	err := a.withDecisions(func(cs *[]DecisionCard) error {
		for i, c := range *cs {
			if !c.Open() || c.To != OrchestratorID {
				continue
			}
			if n > len(c.Options) {
				return nil // not an option of the card it would answer
			}
			c.Answer, c.By, c.Closed = n, DecidedByOwner, now.UTC()
			(*cs)[i], card, found = c, c, true
			return nil
		}
		return nil
	})
	if err != nil || !found {
		return DecisionCard{}, DecisionOption{}, false, err
	}
	opt := card.Options[n-1]
	a.Store.Event(card.Task, EventDecisionAnswered, fmt.Sprintf("%s: option %d (%s)", card.ID, n, opt.Action))
	return card, opt, true, nil
}

// ResolveDecision closes the open card id because its situation cleared,
// so the same situation recurring opens a new card. No card is fine.
func (a *App) ResolveDecision(id string, now time.Time) error {
	var card DecisionCard
	err := a.withDecisions(func(cs *[]DecisionCard) error {
		for i, c := range *cs {
			if c.ID == id && c.Open() {
				c.By, c.Closed = DecidedResolved, now.UTC()
				(*cs)[i], card = c, c
			}
		}
		return nil
	})
	if err == nil && card.ID != "" {
		a.Store.Event(card.Task, EventDecisionResolved, id)
	}
	return err
}

// ExpireDecisions runs the default of every open card whose timeout has run
// out at now, through run, and closes it. A default that fails stays open
// for the next call. It returns the cards it closed.
func (a *App) ExpireDecisions(now time.Time, run func(DecisionCard, DecisionOption) error) ([]DecisionCard, error) {
	var due []DecisionCard
	open, err := a.OpenDecisions()
	if err != nil {
		return nil, err
	}
	for _, c := range open {
		if c.Due(now) {
			due = append(due, c)
		}
	}
	var done []DecisionCard
	var errs []error
	for _, c := range due {
		opt := c.DefaultOption()
		if err := run(c, opt); err != nil {
			a.Store.Event(c.Task, EventDecisionDefaultFailed, c.ID+": "+err.Error())
			errs = append(errs, fmt.Errorf("decision %s: default %q: %w", c.ID, opt.Action, err))
			continue
		}
		closed := false
		err := a.withDecisions(func(cs *[]DecisionCard) error {
			for i, o := range *cs {
				if o.ID == c.ID && o.Open() {
					o.Answer, o.By, o.Closed = c.Default, DecidedByTimeout, now.UTC()
					(*cs)[i], c, closed = o, o, true
					return nil
				}
			}
			return nil
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if closed {
			a.Store.Event(c.Task, EventDecisionDefault, fmt.Sprintf("%s: option %d (%s) after %s", c.ID, c.Default, opt.Action, fmtDuration(c.Timeout)))
			done = append(done, c)
		}
	}
	return done, errors.Join(errs...)
}

// taskLabel names a task by what it does: t150 ("Remote-control PR").
func (a *App) taskLabel(id string) string {
	if t, err := a.Store.Task(id); err == nil && t.Title != "" {
		return fmt.Sprintf("%s (%q)", id, t.Title)
	}
	return id
}

// oneLine squeezes text onto one line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
