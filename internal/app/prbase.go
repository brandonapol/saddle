package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/brandonapol/saddle/internal/ghstack"
	"github.com/brandonapol/saddle/internal/store"
)

// Which branch a PR targets (#358, #359). The layout stacks a task on the
// earlier tasks it depends on (see clusterLayers); file overlap finds most of
// those, but not a task whose work only makes sense on top of another's in
// disjoint files. Before prs and restack lay the stack out, noteDependencies
// records the edges saddle can see beyond the explicit after ones:
//
//   - the task's title, prompt or issue names an earlier stacked task: its
//     id, issue, PR or branch ("follow-up to #3093");
//   - someone retargeted the task's PR onto the branch of a stacked task
//     that landed before it, outside saddle (gh pr edit --base). Saddle
//     remembers the base it last set on each PR, so a base it didn't set is
//     a person's.
//
// Saddle changes a PR's base only out loud: when the base it sets differs
// from the one GitHub has, it logs a pr_base event naming both, and tells the
// orchestrator when the old one was set by hand. And a layer whose
// dependency has an open PR outside its stack (in the merge queue, say)
// stays where it is until that PR merges, instead of moving onto base.

// EventPRBase is the events-table kind for a PR base saddle changed.
const EventPRBase = "pr_base"

// EventDependencyHold is the events-table kind for a layer prs or restack
// left alone because a task it depends on hasn't merged.
const EventDependencyHold = "dependency_hold"

func (a *App) prBasesPath() string { return a.stateDir("pr-bases.json") }

// savedBases maps each task to the base saddle last set on its PR.
func (a *App) savedBases() map[string]string {
	m := map[string]string{}
	if b, err := os.ReadFile(a.prBasesPath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func (a *App) saveBase(task, base string) error {
	m := a.savedBases()
	if m[task] == base {
		return nil
	}
	m[task] = base
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.prBasesPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.prBasesPath())
}

// issueText is issue n's title and body, as gh reports them. Tests replace it.
var issueText = func(a *App, n int) (string, error) {
	out, err := gh(a.Root, "issue", "view", strconv.Itoa(n), "--json", "title,body")
	if err != nil {
		return "", err
	}
	var is struct{ Title, Body string }
	if err := json.Unmarshal([]byte(out), &is); err != nil {
		return "", fmt.Errorf("gh issue view %d: %w", n, err)
	}
	return is.Title + "\n" + is.Body, nil
}

// cachedIssueText is issueText, kept under .saddle/issues so each issue is
// asked for once. "" when GitHub can't be asked.
func (a *App) cachedIssueText(n int) string {
	p := a.stateDir("issues", strconv.Itoa(n))
	if b, err := os.ReadFile(p); err == nil {
		return string(b)
	}
	s, err := issueText(a, n)
	if err != nil {
		return ""
	}
	if os.MkdirAll(a.stateDir("issues"), 0o755) == nil {
		_ = os.WriteFile(p, []byte(s), 0o644)
	}
	return s
}

// taskText is what a task says about its work: title, prompt and issue.
func (a *App) taskText(t store.Task) string {
	s := t.Title + "\n" + t.Prompt
	if t.Issue > 0 {
		s += "\n" + a.cachedIssueText(t.Issue)
	}
	return s
}

// names reports what of t's text names: its id, issue, PR or branch. Ids,
// issue and PR numbers must stand alone, so t1 doesn't match t12 and #31
// doesn't match #312.
func names(text string, t store.Task) (string, bool) {
	word := func(w string) bool {
		re := regexp.MustCompile(`(^|[^\w-])` + regexp.QuoteMeta(w) + `($|[^\w])`)
		return re.MatchString(text)
	}
	switch {
	case t.ID != "" && word(t.ID):
		return t.ID, true
	case t.Issue > 0 && word("#"+strconv.Itoa(t.Issue)):
		return "#" + strconv.Itoa(t.Issue), true
	case t.PR != "" && strings.Contains(text, t.PR):
		return t.PR, true
	case t.PR != "" && ghstack.PRNumber(t.PR) > 0 && word("#"+strconv.Itoa(ghstack.PRNumber(t.PR))):
		return "#" + strconv.Itoa(ghstack.PRNumber(t.PR)), true
	case t.Branch != "" && strings.Contains(text, t.Branch):
		return t.Branch, true
	}
	return "", false
}

// noteDependencies records the after edges of stack's layers that saddle can
// see but nobody declared: an earlier layer the task's text names (#358), and
// a stacked branch someone retargeted the task's PR onto outside saddle
// (#359). info is what GitHub said about each open PR (see ReconcileStack).
func (a *App) noteDependencies(stack []landedTask, info map[string]PRInfo) error {
	saved := a.savedBases()
	var errs []error
	for i, l := range stack {
		var text string
		for j := range i {
			e := stack[j]
			if slices.Contains(a.TaskAfter(l.ID), e.ID) {
				continue
			}
			if text == "" {
				text = a.taskText(l.Task)
			}
			if what, ok := names(text, e.Task); ok {
				_, err := a.addAfter(l.ID, e.ID, fmt.Sprintf("inferred: %s's task text names %s", l.ID, what))
				errs = append(errs, err)
			}
		}
		live := info[l.PR].BaseRefName
		if l.PR == "" || live == "" || live == saved[l.ID] || live == a.Cfg.Base {
			continue
		}
		// Only a layer that landed before it can sit under it; a retarget
		// anywhere else is changed back, out loud (see notePRBase).
		j := slices.IndexFunc(stack, func(o landedTask) bool { return o.Branch == live })
		if j < 0 || j >= i {
			continue
		}
		added, err := a.addAfter(l.ID, stack[j].ID, fmt.Sprintf("manual retarget: %s was moved onto %s outside saddle", l.PR, live))
		errs = append(errs, err)
		if added {
			_ = a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf(
				"%s's PR %s was retargeted onto %s's branch outside saddle. Saddle recorded that %s depends on %s and keeps it stacked there.",
				l.ID, l.PR, stack[j].ID, l.ID, stack[j].ID))
		}
	}
	return errors.Join(errs...)
}

// setPRBase points t's PR at base and remembers that saddle set it. live is
// the base GitHub reported before ("" when unknown). A change from a base
// saddle didn't set is someone's retarget being undone: the orchestrator
// hears which, and why.
func (a *App) setPRBase(t store.Task, live, base, source string) error {
	if _, err := gh(a.Root, "pr", "edit", t.PR, "--base", base); err != nil {
		return err
	}
	return a.notePRBase(t, live, base, source)
}

// notePRBase records that saddle set t's PR base to base, logging a change
// from live.
func (a *App) notePRBase(t store.Task, live, base, source string) error {
	set := a.savedBases()[t.ID]
	if err := a.saveBase(t.ID, base); err != nil {
		return err
	}
	if live == "" || live == base {
		return nil
	}
	a.Store.Event(t.ID, EventPRBase, fmt.Sprintf("%s: %s → %s (%s)", t.PR, live, base, source))
	if set == "" || live == set {
		return nil
	}
	return a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
		"%s changed %s's PR %s base from %s back to %s. Someone set %s outside saddle, but saddle stacks a PR only on a stacked task that landed before it. "+
			"To keep tasks together, group them with `saddle stack create <name> <tasks…>`, or open the PR on its own with `saddle publish`.",
		source, t.ID, t.PR, live, base, live))
}

// waitingOn names a task that layer l depends on, outside stack, whose PR is
// still open: l's work isn't complete on base until that PR merges, so
// prs and restack leave l's PR where it is (#358). lookup asks GitHub.
func (a *App) waitingOn(l landedTask, stack []landedTask, lookup PRLookup) (dep, pr string) {
	for _, d := range a.TaskAfter(l.ID) {
		if slices.ContainsFunc(stack, func(o landedTask) bool { return o.ID == d }) {
			continue // the layout stacks on it
		}
		t, err := a.Store.Task(d)
		if err != nil || t.PR == "" {
			continue
		}
		if info, err := lookup(t.PR); err == nil && info.State == "OPEN" {
			return d, t.PR
		}
	}
	return "", ""
}

// holdForDependency logs a layer left alone for its dependency and tells
// the orchestrator, once per hold.
func (a *App) holdForDependency(l landedTask, dep, pr, source string) {
	a.Store.Event(l.ID, EventDependencyHold, fmt.Sprintf("%s: %s depends on %s, whose PR %s is still open", source, l.ID, dep, pr))
	p := a.stateDir("deps", l.ID+".held")
	if b, err := os.ReadFile(p); err == nil && string(b) == pr {
		return
	}
	_ = os.WriteFile(p, []byte(pr), 0o644)
	_ = a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf(
		"%s left %s's PR as it was: %s depends on %s, whose PR %s hasn't merged and isn't below it in the stack. "+
			"Once %s merges, the next restack moves %s onto %s.",
		source, l.ID, l.ID, dep, pr, pr, l.ID, a.Cfg.Base))
}

// releaseDependencyHold forgets l's hold once it is published again.
func (a *App) releaseDependencyHold(id string) { _ = os.Remove(a.stateDir("deps", id+".held")) }
