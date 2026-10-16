package app

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/release"
	"github.com/brandonapol/saddle/internal/store"
)

// Restack spawns a repair for every repairing task with none live (#172).
// When the owner kills that repair, the work is usually superseded by
// newer work, and respawning it on every restack wastes tokens and makes
// the owner kill it again (#337). A killed repair is a "do not repair"
// mark: restack leaves the task alone and says so once, and `saddle
// repair` spawns one by hand. Repairs per task are also capped per day.

// eventRepairDeclined is recorded on the original when restack skips its
// repair; data: the killed repair's id, or the cap.
const eventRepairDeclined = "repair_declined"

// repairDailyCap is how many repairs restack spawns for one task in a day.
const repairDailyCap = 2

// repairDeclined reports the repair of orig the owner killed: the last
// repair spawned for it, when that one is killed.
func (a *App) repairDeclined(orig string) (string, bool) {
	r, ok := a.lastEvent(orig, eventRepairSpawned)
	if !ok {
		return "", false
	}
	t, err := a.Store.Task(r)
	return r, err == nil && t.Status == store.Killed
}

// repairsToday counts the repairs spawned for task in the day before now.
func repairsToday(es []store.Event, task string, now time.Time) int {
	n := 0
	for _, e := range es {
		if e.Task == task && e.Kind == eventRepairSpawned && now.Sub(e.TS) < 24*time.Hour {
			n++
		}
	}
	return n
}

// skipAutoRepair reports whether restack must not spawn orig's repair: its
// last one was killed, or the day's cap is spent. The orchestrator hears
// once per reason.
func (a *App) skipAutoRepair(orig string) bool {
	why := ""
	if r, ok := a.repairDeclined(orig); ok {
		why = fmt.Sprintf("%s's repair %s was killed, so restack won't spawn another: its work looks superseded. "+
			"%s stays out of the stack and its PR is left alone. Run `saddle repair %s` if it still needs re-landing, or `saddle unstack %s` to drop it.",
			orig, r, orig, orig, orig)
	} else if es, err := a.Store.Events(-1); err == nil && repairsToday(es, orig, time.Now()) >= repairDailyCap {
		why = fmt.Sprintf("%s already had %d repairs in the last day, so restack won't spawn another until tomorrow. "+
			"Run `saddle repair %s` to spawn one now.", orig, repairDailyCap, orig)
	}
	if why == "" {
		return false
	}
	if last, ok := a.lastEvent(orig, eventRepairDeclined); !ok || last != why {
		a.Store.Event(orig, eventRepairDeclined, why)
		_ = a.Notify(OrchestratorID, store.NoticeInfo, why)
	}
	return true
}

// eventCIRepairSkipped is recorded on a red task when its CI failure gets
// no repair task; data: why.
const eventCIRepairSkipped = "ci_repair_skipped"

// Not every red check is something a repair task can fix with a commit
// (#338). A canceled run failed nothing. A check on the PR's body or title
// (the attribution footer) is fixed on GitHub, not in a branch, so saddle
// strips the footer and reruns the job. A PR head that lacks the layers
// below it in its PR stack is red because of its base: a repair would end
// with no commits. Each gets a notice instead of a worker.

// prMetaRe matches a failure about the PR's body or title, not its code.
var prMetaRe = regexp.MustCompile(`(?i)\battribution\b|generated with \[?claude|\b(pr|pull request) (body|description|title)\b`)

// ciRepairScreen reports why task's red checks get no repair task, or ""
// when they need one. pr is its PR, head the commit the checks failed on
// ("" if unknown). Each reason is recorded once on task.
func (a *App) ciRepairScreen(task, pr, head string, fails []CIRedFailure) string {
	why := a.ciRepairWhy(task, pr, head, fails)
	if why == "" {
		return ""
	}
	if last, ok := a.lastEvent(task, eventCIRepairSkipped); !ok || last != why {
		a.Store.Event(task, eventCIRepairSkipped, why)
	}
	return why
}

func (a *App) ciRepairWhy(task, pr, head string, fails []CIRedFailure) string {
	if len(fails) > 0 && !slices.ContainsFunc(fails, func(f CIRedFailure) bool {
		class, _ := ClassifyCIFailure(f, nil)
		return class != CIIgnore
	}) {
		return "the run was canceled, not failed, so no repair was spawned; the next run counts."
	}
	if len(fails) > 0 && !slices.ContainsFunc(fails, func(f CIRedFailure) bool {
		return !prMetaRe.MatchString(f.Check + "\n" + f.Step + "\n" + f.LogTail)
	}) {
		return a.fixPRMeta(task, pr, fails)
	}
	if missing := a.stackBaseMissing(task, head); len(missing) > 0 {
		return fmt.Sprintf("%s's PR head %s lacks %s below it in its PR stack, so the red comes from its base, not its work, and no repair was spawned. "+
			"Restack (`saddle restack`) so its PR is built on %s, or merge %s first.",
			task, short(head), strings.Join(missing, ", "), strings.Join(missing, ", "), strings.Join(missing, ", "))
	}
	return ""
}

// fixPRMeta handles a failure on the PR's body or title: it strips the
// attribution footer from the body and reruns the failed jobs.
func (a *App) fixPRMeta(task, pr string, fails []CIRedFailure) string {
	what := []string{}
	if pr != "" {
		if v, err := a.Forge().PR(context.Background(), pr, forge.PRBody); err != nil {
			if !isParseErr(err) {
				a.Store.Event(task, EventCIError, err.Error())
			}
		} else if body := stripPRFooter(v.Body); body != strings.TrimSpace(v.Body) {
			if err := a.Forge().SetPRBody(context.Background(), pr, body); err != nil {
				a.Store.Event(task, EventCIError, err.Error())
			} else {
				what = append(what, "stripped the attribution footer from its body")
			}
		}
	}
	var runs []string
	for _, f := range fails {
		if id := forge.RunID(f.RunURL); id != "" && !slices.Contains(runs, id) {
			runs = append(runs, id)
			if err := a.Forge().RerunFailed(context.Background(), id); err != nil {
				a.Store.Event(task, EventCIError, err.Error())
			} else {
				what = append(what, "reran "+ciRunName(f.RunURL, id))
			}
		}
	}
	if len(what) == 0 {
		what = append(what, "changed nothing; fix the PR's body or title by hand")
	}
	return fmt.Sprintf("%s's red check is about its PR's body or title, which no commit can fix, so no repair was spawned: saddle %s.",
		task, strings.Join(what, " and "))
}

// stripPRFooter drops the AI attribution lines from a PR body.
func stripPRFooter(body string) string { return release.StripAttribution(body) }

// stackBaseMissing lists the layers below task in its PR stack whose work
// head lacks and origin's base doesn't have yet. It lists none when head
// is unknown or not here.
func (a *App) stackBaseMissing(task, head string) []string {
	if head == "" {
		return nil
	}
	if _, err := gitx.Run(a.Root, "cat-file", "-e", head+"^{commit}"); err != nil {
		return nil
	}
	stack, err := a.landedStack()
	if err != nil {
		return nil
	}
	at := slices.IndexFunc(stack, func(l landedTask) bool { return l.ID == task })
	if at < 0 {
		return nil
	}
	layout, _, err := a.stackLayout(stack)
	if err != nil {
		return nil
	}
	base := "refs/remotes/origin/" + a.Cfg.Base
	var out []string
	for j, l := range stack {
		if j == at || !above(layout, at, j) || l.From == "" || l.To == "" || l.Lost != "" {
			continue
		}
		if lacks(a.Root, head, l.From, l.To) && lacks(a.Root, base, l.From, l.To) {
			out = append(out, l.ID)
		}
	}
	return out
}

// lacks reports whether some commit of from..to has no equivalent in ref.
func lacks(dir, ref, from, to string) bool {
	out, err := gitx.Run(dir, "cherry", ref, to, from)
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "+") {
			return true
		}
	}
	return false
}
