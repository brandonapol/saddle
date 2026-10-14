package app

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/brandonapol/saddle/internal/store"
)

// The decision queue (#464). Owner-gated actions (a new dependency, a
// release tag, a gate tolerance) used to stall a lane until the owner was at
// the keyboard. Now the orchestrator, or a worker, files each one as a card
// with options, a recommended default and whether that default can be
// undone, and every unrelated lane keeps running:
//
//   - a [[preauth]] rule in config allows a class of action up front (say
//     dependency "github.com/aws/*", or release-tag "v*"): a card it matches
//     closes at once, or as soon as the rule is added, and the blocked task
//     is told to go ahead;
//   - a reversible card runs its recommended option once its stall budget
//     ([stall.kinds] owner) runs out;
//   - an irreversible one (tag pushes, publishes) never runs without the
//     owner or a rule;
//   - the owner reads one digest a day ([decisions] digest_at) of what waits
//     on them and what saddle decided meanwhile, instead of a message per
//     cycle.

// DecisionReq files an owner decision.
type DecisionReq struct {
	By           string // who files it: OrchestratorID or a worker
	Task         string // the task it blocks; By when empty
	Class        string // the kind of action, e.g. "dependency", "release-tag"
	Subject      string // what it acts on, e.g. a module path or a tag
	Question     string // the headline: what the owner is asked
	What         string // optional context line
	Blocking     string // optional; says it blocks only Task when empty
	Options      []DecisionOption
	Recommended  int // 1-based; 0 takes the option marked Recommended, else 1
	Irreversible bool
	Timeout      time.Duration // 0 is the stall budget for owner cards
}

// card builds the card req files.
func (req DecisionReq) card() (DecisionCard, error) {
	task := cmp.Or(strings.TrimSpace(req.Task), req.By)
	q := oneLine(req.Question)
	class := strings.ToLower(strings.TrimSpace(req.Class))
	subject := strings.TrimSpace(req.Subject)
	if q == "" || task == "" {
		return DecisionCard{}, errors.New("decision: give a question and the task it blocks")
	}
	opts := slices.Clone(req.Options)
	if len(opts) == 1 {
		opts = append(opts, DecisionOption{Label: "Not now", Cost: "the task carries on without it",
			Action: "saddle message " + task + " \"Not approved: carry on without it and say so in your done summary.\""})
	}
	rec := req.Recommended
	if rec == 0 {
		rec = 1
		for i, o := range opts {
			if o.Recommended {
				rec = i + 1
				break
			}
		}
	}
	if rec < 1 || rec > len(opts) {
		return DecisionCard{}, fmt.Errorf("decision: recommended %d is not an option", rec)
	}
	for i := range opts {
		opts[i].Recommended = i == rec-1
	}
	what := oneLine(req.What)
	if what == "" {
		what = q
		if class != "" {
			what = strings.TrimSpace(class + " " + subject)
		}
	}
	blocking := oneLine(req.Blocking)
	if blocking == "" {
		blocking = "Only " + task + ", which waits on it. Everything else keeps running."
	}
	key := subject
	if key == "" {
		key = q
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultDecisionTimeout
	}
	return DecisionCard{
		ID:   DecisionOwner + ":" + task + "|" + class + "|" + key,
		Kind: DecisionOwner, To: OrchestratorID, Task: task,
		Headline: q, What: what, Blocking: blocking,
		Options: opts, Default: rec, Timeout: timeout,
		Class: class, Subject: subject, Irreversible: req.Irreversible,
	}, nil
}

// FileDecision queues an owner decision and returns its card. A [[preauth]]
// rule that allows it closes it at once and tells the blocked task to go
// ahead. Filing the same decision again while it is open returns the open
// card and sends nothing. A card a worker files reaches the orchestrator as
// an action notice; the orchestrator's own come back to it as the result.
func (a *App) FileDecision(req DecisionReq, now time.Time) (DecisionCard, error) {
	c, err := req.card()
	if err != nil {
		return c, err
	}
	c.Opened = now.UTC()
	if req.Timeout <= 0 {
		c.Timeout = a.StallConfig().For(DecisionOwner)
	}
	if r, ok := a.preauthFor(c); ok {
		if err := a.applyOwnerDecision(c, r.option(c), DecidedPreauth, r.String()); err != nil {
			return c, err
		}
		c.Answer, c.By, c.Closed = r.option(c), DecidedPreauth, now.UTC()
		if err := a.withDecisions(func(cs *[]DecisionCard) error {
			*cs = append(slices.DeleteFunc(*cs, func(o DecisionCard) bool { return o.ID == c.ID && o.Open() }), c)
			return nil
		}); err != nil {
			return c, err
		}
		a.Store.Event(c.Task, EventDecisionPreauth, fmt.Sprintf("%s: option %d by %s", c.ID, c.Answer, r))
		return c, nil
	}
	fresh, err := a.openDecision(c, req.By != OrchestratorID)
	if err != nil || fresh {
		return c, err
	}
	open, err := a.OpenDecisions()
	if i := slices.IndexFunc(open, func(o DecisionCard) bool { return o.ID == c.ID }); i >= 0 {
		return open[i], err
	}
	return c, err
}

// applyOwnerDecision tells whoever must act that option n of c was taken,
// and how: the blocked worker, or the orchestrator for its own cards.
func (a *App) applyOwnerDecision(c DecisionCard, n int, by, why string) error {
	o := c.Options[n-1]
	how := map[string]string{
		DecidedByOwner:   "the owner chose it",
		DecidedByTimeout: "nobody answered in " + fmtDuration(c.Timeout) + " and it can be undone",
		DecidedPreauth:   "pre-authorized by " + why,
	}[by]
	msg := fmt.Sprintf("Decision %q: option %d, %s (%s). Go ahead: %s", c.Headline, n, o.Label, how, o.Action)
	to := c.Task
	if to == "" || to == OrchestratorID {
		to = OrchestratorID
	} else if _, err := a.Store.Task(to); err != nil {
		to = OrchestratorID
	}
	return a.Notify(to, store.NoticeAction, msg)
}

// AnswerDecisionID closes the open card id with option n, as the owner's
// answer, and tells whoever must act. It returns the card closed.
func (a *App) AnswerDecisionID(id string, n int, now time.Time) (DecisionCard, error) {
	var card DecisionCard
	err := a.withDecisions(func(cs *[]DecisionCard) error {
		i := slices.IndexFunc(*cs, func(c DecisionCard) bool { return c.ID == id && c.Open() })
		if i < 0 {
			return fmt.Errorf("no open decision %s; saddle decisions lists them", id)
		}
		c := (*cs)[i]
		if n < 1 || n > len(c.Options) {
			return fmt.Errorf("decision %s has options 1 to %d", id, len(c.Options))
		}
		c.Answer, c.By, c.Closed = n, DecidedByOwner, now.UTC()
		(*cs)[i], card = c, c
		return nil
	})
	if err != nil {
		return card, err
	}
	a.Store.Event(card.Task, EventDecisionAnswered, fmt.Sprintf("%s: option %d (%s)", card.ID, n, card.Options[n-1].Action))
	if card.Kind == DecisionOwner {
		return card, a.applyOwnerDecision(card, n, DecidedByOwner, "")
	}
	return card, nil
}

// PreauthRule allows a class of owner decision without the owner: a
// [[preauth]] table in .saddle/config.toml or the user's config.
type PreauthRule struct {
	Class string `toml:"class"`
	// Match is a glob over the decision's subject, as in "v*" or
	// "github.com/aws/*"; empty or "*" allows every subject of the class.
	Match string `toml:"match"`
	// Option is the 1-based option it takes; 0 is the recommended one.
	Option int    `toml:"option"`
	Note   string `toml:"note"` // why the owner allows it, e.g. "listed in ADR 7"
}

func (r PreauthRule) String() string {
	s := "[[preauth]] " + r.Class
	if r.Match != "" {
		s += " " + strconv.Quote(r.Match)
	}
	if r.Note != "" {
		s += " (" + r.Note + ")"
	}
	return s
}

// Allows reports whether r allows c.
func (r PreauthRule) Allows(c DecisionCard) bool {
	if c.Class == "" || !strings.EqualFold(strings.TrimSpace(r.Class), c.Class) {
		return false
	}
	if r.Option > len(c.Options) {
		return false
	}
	m := strings.TrimSpace(r.Match)
	if m == "" || m == "*" || m == c.Subject {
		return true
	}
	ok, err := path.Match(m, c.Subject)
	return err == nil && ok
}

// option is the option r takes on c.
func (r PreauthRule) option(c DecisionCard) int {
	if r.Option >= 1 {
		return r.Option
	}
	return c.Default
}

// decisionsConfig is the [decisions] table and the [[preauth]] rules.
type decisionsConfig struct {
	Decisions struct {
		// DigestAt is the local clock time the daily digest goes out, "08:00".
		DigestAt string `toml:"digest_at"`
	} `toml:"decisions"`
	Preauth []PreauthRule `toml:"preauth"`
}

// DefaultDigestAt is when the decisions digest goes out by default.
const DefaultDigestAt = "08:00"

// configPaths are the user's and the repo's config files, the repo's last.
func (a *App) configPaths() []string {
	paths := []string{a.stateDir("config.toml")}
	if home, err := os.UserConfigDir(); err == nil {
		paths = append([]string{filepath.Join(home, "saddle", "config.toml")}, paths...)
	}
	return paths
}

// decisionsConfig reads [decisions] and every [[preauth]] rule from the
// user's and the repo's config, the repo's digest_at winning.
func (a *App) decisionsConfig() decisionsConfig {
	var out decisionsConfig
	for _, p := range a.configPaths() {
		var f decisionsConfig
		if _, err := toml.DecodeFile(p, &f); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				a.Store.Event("", "decisions_config_error", err.Error())
			}
			continue
		}
		if f.Decisions.DigestAt != "" {
			out.Decisions.DigestAt = f.Decisions.DigestAt
		}
		out.Preauth = append(out.Preauth, f.Preauth...)
	}
	if out.Decisions.DigestAt == "" {
		out.Decisions.DigestAt = DefaultDigestAt
	}
	return out
}

// PreauthRules lists the [[preauth]] rules in force.
func (a *App) PreauthRules() []PreauthRule { return a.decisionsConfig().Preauth }

func (a *App) preauthFor(c DecisionCard) (PreauthRule, bool) {
	for _, r := range a.PreauthRules() {
		if r.Allows(c) {
			return r, true
		}
	}
	return PreauthRule{}, false
}

// ApplyPreauth closes every open card a [[preauth]] rule now allows and
// tells the blocked task to go ahead. The sentinel runs it every cycle, so a
// rule the owner adds overnight unblocks what already waits on it. It
// returns the cards it closed.
func (a *App) ApplyPreauth(now time.Time) ([]DecisionCard, error) {
	rules := a.PreauthRules()
	if len(rules) == 0 {
		return nil, nil
	}
	open, err := a.OpenDecisions()
	if err != nil {
		return nil, err
	}
	var done []DecisionCard
	var errs []error
	for _, c := range open {
		i := slices.IndexFunc(rules, func(r PreauthRule) bool { return r.Allows(c) })
		if i < 0 {
			continue
		}
		r := rules[i]
		n := r.option(c)
		closed := false
		err := a.withDecisions(func(cs *[]DecisionCard) error {
			j := slices.IndexFunc(*cs, func(o DecisionCard) bool { return o.ID == c.ID && o.Open() })
			if j >= 0 {
				o := (*cs)[j]
				o.Answer, o.By, o.Closed = n, DecidedPreauth, now.UTC()
				(*cs)[j], c, closed = o, o, true
			}
			return nil
		})
		if err != nil || !closed {
			errs = append(errs, err)
			continue
		}
		a.Store.Event(c.Task, EventDecisionPreauth, fmt.Sprintf("%s: option %d by %s", c.ID, n, r))
		if c.Kind == DecisionOwner {
			errs = append(errs, a.applyOwnerDecision(c, n, DecidedPreauth, r.String()))
		} else {
			errs = append(errs, a.Notify(OrchestratorID, store.NoticeAction, c.Line()+" Do it now: "+c.Options[n-1].Action))
		}
		done = append(done, c)
	}
	return done, errors.Join(errs...)
}

// AllowClass adds a [[preauth]] rule to .saddle/config.toml and applies it
// to the cards that wait now. It returns the cards it closed.
func (a *App) AllowClass(r PreauthRule, now time.Time) ([]DecisionCard, error) {
	r.Class = strings.ToLower(strings.TrimSpace(r.Class))
	if r.Class == "" {
		return nil, errors.New("allow: give a class, e.g. dependency or release-tag")
	}
	if r.Match != "" {
		if _, err := path.Match(r.Match, ""); err != nil {
			return nil, fmt.Errorf("allow: match %q: %w", r.Match, err)
		}
	}
	var b strings.Builder
	b.WriteString("\n[[preauth]]\n")
	fmt.Fprintf(&b, "class = %s\n", strconv.Quote(r.Class))
	if r.Match != "" {
		fmt.Fprintf(&b, "match = %s\n", strconv.Quote(r.Match))
	}
	if r.Option > 0 {
		fmt.Fprintf(&b, "option = %d\n", r.Option)
	}
	if r.Note != "" {
		fmt.Fprintf(&b, "note = %s\n", strconv.Quote(oneLine(r.Note)))
	}
	if err := os.MkdirAll(a.stateDir(), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(a.stateDir("config.toml"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	a.Store.Event(OrchestratorID, "preauth_added", r.String())
	return a.ApplyPreauth(now)
}

// digestState is when the last decisions digest went out.
type digestState struct {
	Last time.Time `json:"last"`
}

func (a *App) decisionDigestPath() string { return a.stateDir("decisions-digest.json") }

func (a *App) lastDecisionDigest() time.Time {
	var s digestState
	if b, err := os.ReadFile(a.decisionDigestPath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s.Last
}

// DecisionDigest is the decisions digest at now: every card that waits on
// the owner, once each, and the cards saddle closed without them since
// since. It is "" when there is neither.
func (a *App) DecisionDigest(since, now time.Time) (string, error) {
	cs, err := a.Decisions()
	if err != nil {
		return "", err
	}
	var waiting, decided []string
	for _, c := range cs {
		switch {
		case c.Open() && c.To == OrchestratorID:
			waiting = append(waiting, digestWaiting(c, now))
		case !c.Open() && !c.Closed.Before(since) && (c.By == DecidedByTimeout || c.By == DecidedPreauth):
			decided = append(decided, "  "+c.Line())
		}
	}
	if len(waiting) == 0 && len(decided) == 0 {
		return "", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Decisions digest (since %s):\n", since.Local().Format("Jan 2 15:04"))
	if len(waiting) > 0 {
		fmt.Fprintf(&b, "Waiting on you (%d):\n%s\n", len(waiting), strings.Join(waiting, "\n"))
	}
	if len(decided) > 0 {
		fmt.Fprintf(&b, "Decided without you (%d):\n%s\n", len(decided), strings.Join(decided, "\n"))
	}
	b.WriteString("Answer: saddle decisions answer <id> <option>. Allow a class for good: saddle decisions allow <class> [match].")
	return b.String(), nil
}

// digestWaiting is a waiting card's digest entry.
func digestWaiting(c DecisionCard, now time.Time) string {
	o := c.DefaultOption()
	then := "can't be undone, so it waits for you"
	if !c.Irreversible {
		at := c.Opened.Add(c.Timeout)
		then = "saddle runs it at " + at.Local().Format("15:04")
		if !now.Before(at) {
			then = "saddle runs it on its next cycle"
		}
	}
	return fmt.Sprintf("  ‼ %s [%s]: recommended %d (%s); %s.", c.Headline, c.ID, c.Default, o.Label, then)
}

// digestDue is the time today's digest is due at, in now's local clock.
func digestDue(at string, now time.Time) time.Time {
	t, err := time.Parse("15:04", strings.TrimSpace(at))
	if err != nil {
		t, _ = time.Parse("15:04", DefaultDigestAt)
	}
	l := now.Local()
	return time.Date(l.Year(), l.Month(), l.Day(), t.Hour(), t.Minute(), 0, 0, time.Local)
}

// DecisionDigestTick sends the orchestrator the day's decisions digest once
// now passes [decisions] digest_at and none went out since. A day with
// nothing to report sends nothing, and so does the first tick in a repo: it
// only starts the clock, so the first digest is the next morning's. It returns the digest sent, or "".
func (a *App) DecisionDigestTick(now time.Time) (string, error) {
	due := digestDue(a.decisionsConfig().Decisions.DigestAt, now)
	last := a.lastDecisionDigest()
	if !last.IsZero() && (now.Before(due) || !last.Before(due)) {
		return "", nil
	}
	text, err := a.DecisionDigest(last, now)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(digestState{Last: now.UTC()})
	if err := os.WriteFile(a.decisionDigestPath(), append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	if last.IsZero() || text == "" {
		return "", nil
	}
	a.Store.Event(OrchestratorID, "decision_digest_sent", strings.SplitN(text, "\n", 2)[0])
	return text, a.Notify(OrchestratorID, store.NoticeAction, text)
}
