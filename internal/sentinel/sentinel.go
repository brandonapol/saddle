// Package sentinel watches the PR stack for the things that break it: a PR
// GitHub can't merge, a bottom PR merged into base, base moving past
// integration, and branches that drifted or no longer show only their task's
// work. On a hit it labels the broken PR and every PR above it needs-human,
// records one stack_at_risk event, tells the orchestrator (and so the TUI) to
// run restack, and flags the stack so prs and land refuse to build on it. It
// never restacks itself. Once the stack checks clean it lifts the labels and
// the flag. Each labeled PR also gets one comment saying why, kept current as
// the cause changes and turned into a resolution note when the label lifts.
package sentinel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// Label marks PRs a human must look at before they merge.
const Label = "needs-human"

// Event kinds in the events table.
const (
	EventAtRisk = "stack_at_risk"
	EventClear  = "stack_clear"
	EventError  = "sentinel_error"
)

// DefaultInterval is how often Run checks the stack.
const DefaultInterval = 2 * time.Minute

// PR is what GitHub says about a pull request.
type PR struct {
	State     string `json:"state"`     // OPEN, CLOSED or MERGED
	Mergeable string `json:"mergeable"` // MERGEABLE, CONFLICTING or UNKNOWN
}

// GitHub is the part of GitHub the sentinel uses.
type GitHub interface {
	PR(url string) (PR, error)
	AddLabel(url, label string) error
	RemoveLabel(url, label string) error
	Comments(url string) ([]Comment, error)
	AddComment(url, body string) error
	EditComment(id, body string) error
}

// Comment is a PR comment; ID is its GraphQL node ID.
type Comment struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

// GH talks to GitHub through the gh CLI, run in Dir.
type GH struct {
	Dir   string
	ready bool // the label exists
}

func (g *GH) gh(args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	cmd.Dir = g.Dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh %s: %w: %s", strings.Join(args[:2], " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func (g *GH) PR(url string) (PR, error) {
	var pr PR
	out, err := g.gh("pr", "view", url, "--json", "state,mergeable")
	if err != nil {
		return pr, err
	}
	if err := json.Unmarshal([]byte(out), &pr); err != nil {
		return pr, fmt.Errorf("gh pr view %s: %w", url, err)
	}
	return pr, nil
}

func (g *GH) AddLabel(url, label string) error {
	if !g.ready {
		// gh pr edit fails on a label the repo doesn't have yet.
		if _, err := g.gh("label", "create", label, "--color", "D93F0B", "--force",
			"--description", "Saddle flagged this PR's stack; run restack before merging"); err != nil {
			return err
		}
		g.ready = true
	}
	_, err := g.gh("pr", "edit", url, "--add-label", label)
	return err
}

func (g *GH) RemoveLabel(url, label string) error {
	_, err := g.gh("pr", "edit", url, "--remove-label", label)
	return err
}

func (g *GH) Comments(url string) ([]Comment, error) {
	out, err := g.gh("pr", "view", url, "--json", "comments")
	if err != nil {
		return nil, err
	}
	var v struct {
		Comments []Comment `json:"comments"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, fmt.Errorf("gh pr view %s: %w", url, err)
	}
	return v.Comments, nil
}

func (g *GH) AddComment(url, body string) error {
	_, err := g.gh("pr", "comment", url, "--body", body)
	return err
}

func (g *GH) EditComment(id, body string) error {
	_, err := g.gh("api", "graphql",
		"-f", "query=mutation($id: ID!, $body: String!) { updateIssueComment(input: {id: $id, body: $body}) { issueComment { id } } }",
		"-f", "id="+id, "-f", "body="+body)
	return err
}

// Sentinel checks one repo's PR stack.
type Sentinel struct {
	App      *app.App
	GH       GitHub
	Interval time.Duration
	lastErr  string
	// posted is the sentinel comment last seen or written on each PR, so a
	// quiet tick costs no GitHub calls.
	posted map[string]Comment
}

// New returns a sentinel for a's stack that talks to GitHub through gh.
func New(a *app.App) *Sentinel {
	return &Sentinel{App: a, GH: &GH{Dir: a.Root}, Interval: DefaultInterval}
}

// Report is the outcome of one check.
type Report struct {
	Busy   bool     `json:"busy,omitempty"` // the train held its lock, so nothing was checked
	AtRisk bool     `json:"at_risk"`
	Task   string   `json:"task,omitempty"`  // the first broken task
	Cause  string   `json:"cause,omitempty"` // why it broke
	Hits   []string `json:"hits,omitempty"`  // every problem found, "task: problem"
	PRs    []string `json:"prs,omitempty"`   // PRs labeled needs-human
}

// Run checks the stack now and then every Interval until ctx ends. A failed
// check is recorded as an event (once per distinct error) and retried on the
// next tick.
func (s *Sentinel) Run(ctx context.Context) error {
	iv := s.Interval
	if iv <= 0 {
		iv = DefaultInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		if _, err := s.Check(); err != nil {
			if msg := err.Error(); msg != s.lastErr {
				s.lastErr = msg
				s.App.Store.Event("", EventError, msg)
			}
		} else {
			s.lastErr = ""
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// hit is one problem, at index at of the open PRs; the PRs from from up are
// at risk.
type hit struct {
	at, from int
	cause    string
}

// Check runs one cycle: it checks every open PR in the stack, then flags or
// clears the stack. It skips the cycle when the train is busy, since a land or
// restack in progress moves the refs it reads.
func (s *Sentinel) Check() (Report, error) {
	a := s.App
	unlock, ok, err := a.TryLockTrain()
	if err != nil {
		return Report{}, err
	}
	if !ok {
		return Report{Busy: true}, nil
	}
	defer unlock()

	layers, err := a.StackLayers()
	if err != nil {
		return Report{}, err
	}
	var open []app.StackLayer
	for _, l := range layers {
		if l.Task.PR != "" && !l.Merged {
			open = append(open, l)
		}
	}
	flag, flagged, err := a.Flag()
	if err != nil {
		return Report{}, err
	}

	var hits []hit
	merged := map[int]bool{}
	for i, l := range open {
		pr, err := s.GH.PR(l.Task.PR)
		if err != nil {
			return Report{}, err
		}
		switch {
		case pr.State == "MERGED":
			merged[i] = true
			hits = append(hits, hit{i, i + 1, fmt.Sprintf("its PR %s was merged into %s, so the PRs above it need restacking", l.Task.PR, a.Cfg.Base)})
		case pr.State == "OPEN" && pr.Mergeable == "CONFLICTING":
			hits = append(hits, hit{i, i, fmt.Sprintf("GitHub reports its PR %s conflicts with its base", l.Task.PR)})
		}
		if l.Problem != "" && !merged[i] { // a merged PR's branch no longer matters
			hits = append(hits, hit{i, i, l.Problem})
		}
	}
	if len(open) > 0 {
		if n := s.baseAhead(); n > 0 {
			// The bottom PR GitHub hasn't merged is the first one base moved under.
			at := 0
			for at < len(open)-1 && merged[at] {
				at++
			}
			commits := "1 commit"
			if n > 1 {
				commits = fmt.Sprintf("%d commits", n)
			}
			hits = append(hits, hit{at, at, fmt.Sprintf("origin/%s has %s %s lacks", a.Cfg.Base, commits, a.Cfg.Integration)})
		}
	}

	if len(hits) == 0 {
		if flagged {
			return Report{}, s.clear(flag)
		}
		return Report{}, nil
	}

	first := hits[0]
	for _, h := range hits[1:] {
		if h.from < first.from {
			first = h
		}
	}
	firstAt := min(first.from, len(open)-1)
	rep := Report{AtRisk: true, Task: open[firstAt].Task.ID, Cause: first.cause}
	if firstAt != first.at {
		rep.Cause = open[first.at].Task.ID + ": " + first.cause
	}
	for _, h := range hits {
		rep.Hits = append(rep.Hits, open[h.at].Task.ID+": "+h.cause)
	}
	var want []string
	for i := first.from; i < len(open); i++ {
		if !merged[i] {
			want = append(want, open[i].Task.PR)
		}
	}

	next := app.StackFlag{Task: rep.Task, Cause: rep.Cause, PRs: append([]string(nil), flag.PRs...)}
	var errs []error
	for _, pr := range want {
		if slices.Contains(next.PRs, pr) {
			continue
		}
		if err := s.GH.AddLabel(pr, Label); err != nil {
			errs = append(errs, err)
			continue
		}
		next.PRs = append(next.PRs, pr)
	}
	rep.PRs = next.PRs
	for _, pr := range next.PRs {
		if i := slices.IndexFunc(open, func(l app.StackLayer) bool { return l.Task.PR == pr }); i >= 0 {
			if err := s.comment(pr, flagComment(open, hits, i, rep, a.Cfg.Base)); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := a.SetFlag(next); err != nil {
		return rep, errors.Join(append(errs, err)...)
	}
	if !flagged || flag.Task != next.Task || flag.Cause != next.Cause {
		a.Store.Event(rep.Task, EventAtRisk, rep.Cause)
		errs = append(errs, a.Notify(app.OrchestratorID, store.NoticeAction, notice(rep, a.Cfg.Base)))
	}
	return rep, errors.Join(errs...)
}

func notice(rep Report, base string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Stack at risk from %s up: %s.", rep.Task, rep.Cause)
	if len(rep.Hits) > 1 {
		b.WriteString("\nEverything the sentinel found:")
		for _, h := range rep.Hits {
			b.WriteString("\n  " + h)
		}
	}
	if len(rep.PRs) > 0 {
		fmt.Fprintf(&b, "\nLabeled %s: %s.", Label, strings.Join(rep.PRs, ", "))
	}
	fmt.Fprintf(&b, "\nprs and land refuse to build on the stack until it checks clean. "+
		"Run restack to rebuild it on origin/%s; don't fix it with git or a worker. The flag and labels clear by themselves once it is sound.", base)
	return b.String()
}

// clear lifts the flag and the labels it put on.
func (s *Sentinel) clear(flag app.StackFlag) error {
	a := s.App
	var errs, left []string
	for _, pr := range flag.PRs {
		if err := s.GH.RemoveLabel(pr, Label); err != nil {
			errs = append(errs, err.Error())
			left = append(left, pr)
			continue
		}
		if err := s.comment(pr, clearComment(flag)); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(left) > 0 {
		// Keep the flag so the next cycle retries the labels it couldn't remove.
		flag.PRs = left
		if err := a.SetFlag(flag); err != nil {
			errs = append(errs, err.Error())
		}
		return errors.New(strings.Join(errs, "; "))
	}
	if err := a.ClearFlag(); err != nil {
		return err
	}
	a.Store.Event(flag.Task, EventClear, flag.Cause)
	err := a.Notify(app.OrchestratorID, store.NoticeInfo,
		fmt.Sprintf("The PR stack checks clean again; the %s flag on %s is lifted and prs and land work again.", Label, flag.Task))
	if len(errs) > 0 {
		// Only resolution notes failed. The labels are off, so the flag goes anyway.
		err = errors.Join(errors.New(strings.Join(errs, "; ")), err)
	}
	return err
}

// baseAhead fetches origin/<base> and counts the commits it has that
// integration lacks. It is 0 when there is no origin or the fetch fails.
func (s *Sentinel) baseAhead() int {
	a := s.App
	remote := "refs/remotes/origin/" + a.Cfg.Base
	if _, err := gitx.Run(a.Root, "fetch", "--quiet", "origin", "+refs/heads/"+a.Cfg.Base+":"+remote); err != nil {
		return 0
	}
	n, err := gitx.CommitsBetween(a.Root, a.Cfg.Integration, remote)
	if err != nil {
		return 0
	}
	return n
}

// commentMarker tags the sentinel's comment on a PR, so it finds the comment
// again after a restart instead of posting another.
const commentMarker = "<!-- saddle-sentinel:needs-human -->"

// comment makes the sentinel's comment on pr read body: it posts one if the PR
// has none and edits it if it says something else.
func (s *Sentinel) comment(pr, body string) error {
	body = commentMarker + "\n" + body
	c, ok := s.posted[pr]
	if !ok {
		cs, err := s.GH.Comments(pr)
		if err != nil {
			return err
		}
		for _, x := range cs {
			if strings.Contains(x.Body, commentMarker) {
				c, ok = x, true
			}
		}
	}
	switch {
	case ok && c.Body == body:
	case ok:
		if err := s.GH.EditComment(c.ID, body); err != nil {
			return err
		}
		c.Body = body
	default:
		if err := s.GH.AddComment(pr, body); err != nil {
			return err
		}
		// Look the new comment up next time; gh pr comment doesn't print its ID.
		return nil
	}
	if s.posted == nil {
		s.posted = map[string]Comment{}
	}
	s.posted[pr] = c
	return nil
}

// flagComment explains why open[i]'s PR is labeled: its own findings, and
// those below it that put it at risk.
func flagComment(open []app.StackLayer, hits []hit, i int, rep Report, base string) string {
	var why []string
	for _, h := range hits {
		if h.at == i {
			why = append(why, fmt.Sprintf("This PR's branch `%s` (%s): %s.", open[i].Task.Branch, open[i].Task.ID, h.cause))
		}
	}
	for _, h := range hits {
		if h.at < i && h.from <= i {
			below := open[h.at].Task
			why = append(why, fmt.Sprintf("It is stacked on %s (`%s`, %s), where: %s.", below.ID, below.Branch, below.PR, h.cause))
		}
	}
	if len(why) == 0 {
		// Labeled on an earlier tick, below what is wrong now.
		why = append(why, fmt.Sprintf("Saddle flagged the stack from %s up: %s.", rep.Task, rep.Cause))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**Saddle labeled this PR `%s`.** Don't merge it until the label is gone.\n\n**Why:**\n", Label)
	for _, w := range why {
		b.WriteString("- " + w + "\n")
	}
	fmt.Fprintf(&b, "\n**What's needed:** run `saddle restack` (or have the orchestrator run it) to rebuild the stack on origin/%s. "+
		"Don't fix the branch by hand with git or a worker. If restack stops on a conflict, a human has to decide how to resolve it. "+
		"Until then `saddle prs` and `saddle land` refuse to build on the stack.\n\n"+
		"**What clears it:** nothing to do on this PR. Once the stack checks clean after the restack, the sentinel removes the label and updates this comment.\n", base)
	return b.String()
}

// clearComment replaces a flag comment once the label is lifted.
func clearComment(flag app.StackFlag) string {
	return fmt.Sprintf("**Cleared:** the PR stack checks clean again, so Saddle removed the `%s` label. "+
		"It had been flagged from %s up: %s.\n", Label, flag.Task, flag.Cause)
}
