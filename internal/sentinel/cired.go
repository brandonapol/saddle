package sentinel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/ciwatch"
	"github.com/brandonapol/saddle/internal/store"
)

// The ci-red watcher (#213): see app/cired.go for what a red layer holds.
// Each cycle costs one `gh pr view` per stacked PR; a log is fetched only
// for a head that newly went red. While nothing is red or changing, the
// wait between cycles doubles up to MaxInterval.

// Event kinds of the ci-red watcher.
const (
	EventCIRed      = "ci_red"
	EventCIRedClear = "ci_red_clear"
	EventCIRedError = "ci_red_error"
)

// CIRed watches the CI of every stacked PR.
type CIRed struct {
	App *app.App
	GH  ciwatch.Runner
	// Interval is the wait after a cycle that saw something red or changing;
	// MaxInterval caps the backoff while all is quiet.
	Interval, MaxInterval time.Duration
	// Logs fetches the failing checks' log tails of a PR; nil uses gh.
	Logs    func(ctx context.Context, pr string) []ciwatch.Failed
	labeled bool // the ci-red label exists
	lastErr string
	// fetched holds this cycle's failing-check logs by PR, so screening and
	// the repair share one fetch.
	fetched map[string][]ciwatch.Failed
}

// NewCIRed returns the ci-red watcher for a, talking to GitHub through gh.
func NewCIRed(a *app.App) *CIRed {
	return &CIRed{App: a, GH: ciwatch.ExecRunner(a.Root), Interval: a.Cfg.CI.RedInterval, MaxInterval: a.Cfg.CI.RedMaxInterval}
}

// CIRedReport is what one ci-red cycle found.
type CIRedReport struct {
	Red     []app.CIRedLayer `json:"red,omitempty"`     // every red layer now
	New     []string         `json:"new,omitempty"`     // tasks that went red this cycle
	Cleared []string         `json:"cleared,omitempty"` // tasks that went green
	Notes   []string         `json:"notes,omitempty"`   // what was done about new reds
}

// Run checks now and then on the backoff schedule until ctx ends.
func (c *CIRed) Run(ctx context.Context) error {
	iv, maxIv := c.Interval, c.MaxInterval
	if iv <= 0 {
		iv = DefaultInterval
	}
	maxIv = max(maxIv, iv)
	wait := iv
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		rep, err := c.Check(ctx)
		if err != nil {
			if msg := err.Error(); msg != c.lastErr {
				c.lastErr = msg
				c.App.Store.Event("", EventCIRedError, msg)
			}
		} else {
			c.lastErr = ""
		}
		wait = nextWait(wait, iv, maxIv, err == nil && len(rep.Red) == 0 && len(rep.Cleared) == 0)
		t.Reset(wait)
	}
}

// nextWait doubles the wait while quiet, up to maxIv, and drops it back to
// iv as soon as something is red or changed.
func nextWait(wait, iv, maxIv time.Duration, quiet bool) time.Duration {
	if !quiet {
		return iv
	}
	return min(wait*2, maxIv)
}

// Check runs one cycle: poll every stacked PR, update the red layers and
// their labels, and tell the orchestrator what changed.
func (c *CIRed) Check(ctx context.Context) (CIRedReport, error) {
	var rep CIRedReport
	a := c.App
	// A landed repair joins its red layer first, so the poll sees the new head.
	if _, err := a.FoldCIRepairs(); err != nil {
		a.Store.Event("", EventCIRedError, "fold: "+err.Error())
	}
	targets, err := a.CIRedTargets()
	if err != nil {
		return rep, err
	}
	var polls []app.CIPoll
	var errs []error
	for _, t := range targets {
		p, open, err := c.poll(ctx, t)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if open {
			polls = append(polls, p)
		}
	}
	if len(errs) > 0 && len(polls) == 0 && len(targets) > 0 {
		return rep, errors.Join(errs...)
	}
	// Canceled runs are not red, and infra or flaky failures get one rerun
	// first (#234). Only a head that newly failed costs a log fetch.
	c.fetched = map[string][]ciwatch.Failed{}
	if known, err := a.CIRed(); err == nil {
		for i, p := range polls {
			if l, ok := known.Layer(p.Task); len(p.Failed) == 0 || ok && l.Head == p.Head && slices.Equal(l.Checks, p.Failed) {
				continue
			}
			polls[i], err = a.ScreenCIPoll(p, ciFailures(c.logs(ctx, p.PR)), func(run string) error {
				_, err := c.GH(ctx, "run", "rerun", run, "--failed")
				return err
			})
			if err != nil {
				errs = append(errs, err)
			}
		}
	}
	// A PR whose poll failed keeps what was known about it.
	ch, err := a.ApplyCIRed(polls)
	if err != nil {
		return rep, errors.Join(append(errs, err)...)
	}
	rep.Red = ch.State.Red
	for _, l := range ch.Cleared {
		rep.Cleared = append(rep.Cleared, l.Task)
		a.Store.Event(l.Task, EventCIRedClear, l.Head)
		errs = append(errs, a.Notify(app.OrchestratorID, store.NoticeInfo, clearedNotice(l)))
	}
	for _, l := range ch.Red {
		rep.New = append(rep.New, l.Task)
		a.Store.Event(l.Task, EventCIRed, short(l.Head)+" "+strings.Join(l.Checks, ", "))
		red, _ := ch.State.Layer(l.Task)
		note := c.onRed(ctx, red)
		rep.Notes = append(rep.Notes, note)
		errs = append(errs, a.Notify(app.OrchestratorID, store.NoticeInfo, redNotice(red, note)))
	}
	if err := c.label(); err != nil {
		errs = append(errs, err)
	}
	if s, err := a.CIRed(); err == nil {
		rep.Red = s.Red
	}
	return rep, errors.Join(errs...)
}

// rollup is one entry of a PR's statusCheckRollup: a check run or a commit
// status.
type rollup struct {
	Typename   string `json:"__typename"`
	Name       string `json:"name"`
	Workflow   string `json:"workflowName"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Context    string `json:"context"`
	State      string `json:"state"`
	StartedAt  string `json:"startedAt"`
}

func (r rollup) label() string {
	if r.Typename == "StatusContext" {
		return r.Context
	}
	return ciwatch.Check{Name: r.Name, Workflow: r.Workflow}.Label()
}

// verdict sorts a rollup entry into failed, pending or passed.
func (r rollup) verdict() string {
	if r.Typename == "StatusContext" {
		switch r.State {
		case "FAILURE", "ERROR":
			return "fail"
		case "SUCCESS":
			return "pass"
		}
		return "pending"
	}
	if r.Status != "COMPLETED" {
		return "pending"
	}
	switch r.Conclusion {
	case "FAILURE", "TIMED_OUT", "STARTUP_FAILURE", "ACTION_REQUIRED":
		return "fail"
	}
	return "pass" // success, neutral, skipped, cancelled
}

// latest keeps the newest run of each check: a failed run that a newer run
// of the same check on the same head superseded is not red (#234).
func latest(rs []rollup) []rollup {
	var out []rollup
	at := map[string]int{}
	for _, r := range rs {
		k := r.Typename + "\x00" + r.label()
		if i, ok := at[k]; ok {
			if r.StartedAt >= out[i].StartedAt { // RFC 3339 sorts as text
				out[i] = r
			}
			continue
		}
		at[k] = len(out)
		out = append(out, r)
	}
	return out
}

// poll asks GitHub for one PR's head and checks: one gh call.
func (c *CIRed) poll(ctx context.Context, t app.CIRedTarget) (app.CIPoll, bool, error) {
	p := app.CIPoll{Task: t.Task, PR: t.PR}
	out, err := c.GH(ctx, "pr", "view", t.PR, "--json", "state,headRefOid,statusCheckRollup")
	if err != nil {
		return p, false, err
	}
	var v struct {
		State  string   `json:"state"`
		Head   string   `json:"headRefOid"`
		Checks []rollup `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return p, false, fmt.Errorf("gh pr view %s: %w", t.PR, err)
	}
	if v.State != "" && v.State != "OPEN" {
		return p, false, nil // the stack sentinel takes it out of the stack
	}
	p.Head, p.Green = v.Head, true
	for _, r := range latest(v.Checks) {
		switch r.verdict() {
		case "fail":
			p.Failed = append(p.Failed, r.label())
			p.Green = false
		case "pending":
			p.Green = false
		}
	}
	return p, true, nil
}

// label puts the ci-red label on every red PR and every PR it holds, and
// takes it off the rest. Acked layers carry none.
func (c *CIRed) label() error {
	a := c.App
	s, err := a.CIRed()
	if err != nil {
		return err
	}
	tasks, err := a.Store.Tasks()
	if err != nil {
		return err
	}
	prOf := map[string]string{}
	for _, t := range tasks {
		prOf[t.ID] = t.PR
	}
	var want []string
	for _, r := range s.Red {
		if r.Acked {
			continue
		}
		for _, id := range append([]string{r.Task}, r.Held...) {
			if pr := prOf[id]; pr != "" && !slices.Contains(want, pr) {
				want = append(want, pr)
			}
		}
	}
	var errs []error
	var now []string
	for _, pr := range s.Labeled {
		if slices.Contains(want, pr) {
			now = append(now, pr)
			continue
		}
		if _, err := c.GH(context.Background(), "pr", "edit", pr, "--remove-label", app.LabelCIRed); err != nil {
			errs = append(errs, err)
			now = append(now, pr) // retried next cycle
		}
	}
	for _, pr := range want {
		if slices.Contains(now, pr) {
			continue
		}
		if err := c.addLabel(pr); err != nil {
			errs = append(errs, err)
			continue
		}
		now = append(now, pr)
	}
	if !slices.Equal(now, s.Labeled) {
		s.Labeled = now
		errs = append(errs, a.SetCIRed(s))
	}
	return errors.Join(errs...)
}

func (c *CIRed) addLabel(pr string) error {
	ctx := context.Background()
	if !c.labeled {
		// gh pr edit fails on a label the repo doesn't have yet.
		if _, err := c.GH(ctx, "label", "create", app.LabelCIRed, "--color", "FBCA04", "--force",
			"--description", "Saddle: CI is red on this PR or one below it; layers above wait for it"); err != nil {
			return err
		}
		c.labeled = true
	}
	_, err := c.GH(ctx, "pr", "edit", pr, "--add-label", app.LabelCIRed)
	return err
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func redNotice(l app.CIRedLayer, note string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CI is red on %s's PR %s at %s (%s).", l.Task, l.PR, short(l.Head), strings.Join(l.Checks, ", "))
	if len(l.Held) > 0 {
		fmt.Fprintf(&b, " Holding the layers above it: %s.", strings.Join(l.Held, ", "))
	}
	b.WriteString(" prs pushes nothing above it, land holds work that would stack on it, and auto-merge merges none of it; " +
		"the hold lifts by itself when its checks pass.")
	if note != "" {
		b.WriteString(" " + note)
	}
	b.WriteString(" No action needed unless that stalls; `saddle sentinel ack` acknowledges the hold and `saddle unstack " + l.Task + "` drops the task.")
	return b.String()
}

func clearedNotice(l app.CIRedLayer) string {
	msg := fmt.Sprintf("CI on %s's PR %s is green again (or it left the stack), so the ci-red hold is lifted", l.Task, l.PR)
	if len(l.Held) > 0 {
		msg += " from " + strings.Join(l.Held, ", ")
	}
	return msg + "; prs and land carry on."
}
