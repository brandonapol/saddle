package app

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// Stack collapse (#196). Auto-merge merges a stack bottom-up and only on
// green, so a red bottom PR whose fix landed as a PR above it waits forever.
// Collapse does the hand fix: it retargets an upper PR, which already holds
// every commit below it, to base, waits for its CI, squash-merges it, marks
// the tasks it covered merged, closes their now-empty PRs and restacks.

// How long collapse waits for CI on the retargeted PR, and how often it asks.
var (
	CollapseWait = 30 * time.Minute
	collapsePoll = 15 * time.Second
	sleep        = time.Sleep
)

// CollapseResult is what a collapse did.
type CollapseResult struct {
	Stack   string        `json:"stack"`   // the stack's bottom task
	Top     string        `json:"top"`     // the task whose PR was merged
	PR      string        `json:"pr"`      // that PR
	Covered []string      `json:"covered"` // tasks its merge carried, bottom first
	Closed  []string      `json:"closed,omitempty"`
	Restack RestackResult `json:"restack"`
}

// collapseLayer is one PR of the stack being collapsed.
type collapseLayer struct {
	Task store.Task
	PR   automerge.PR
}

// CollapseStack collapses ref's stack (stack name, task id, PR URL or
// number) into its top PR; nil gh means the gh CLI.
func (a *App) CollapseStack(ref string, hub automerge.GitHub) (CollapseResult, error) {
	layers, err := a.collapseLayers(ref, a.ghOr(hub))
	if err != nil {
		return CollapseResult{}, err
	}
	return a.collapse(layers, len(layers)-1, a.ghOr(hub))
}

// AutoCollapse is auto-merge's way out of a red bottom (#196): when stack's
// bottom PR is red and a PR above it is green and contains the bottom's
// commits, it collapses the stack up to the lowest such PR and returns the
// PR it merged. It returns "" and no error when the stack doesn't qualify.
func (a *App) AutoCollapse(stack string, hub automerge.GitHub) (string, error) {
	hub = a.ghOr(hub)
	layers, err := a.collapseLayers(stack, hub)
	if err != nil || len(layers) < 2 || layers[0].PR.Checks != automerge.ChecksFail {
		return "", nil
	}
	for k := 1; k < len(layers); k++ {
		if layers[k].PR.Checks != automerge.ChecksPass {
			continue
		}
		if a.contains(layers[k].PR.HeadSHA, layers[0].PR.HeadSHA) != nil {
			continue
		}
		res, err := a.collapse(layers, k, hub)
		return res.PR, err
	}
	return "", nil
}

func (a *App) ghOr(hub automerge.GitHub) automerge.GitHub {
	if hub == nil {
		return &automerge.GH{Run: automerge.ExecRunner(a.Root)}
	}
	return hub
}

// collapseLayers reads ref's published stack from GitHub, bottom first.
func (a *App) collapseLayers(ref string, hub automerge.GitHub) ([]collapseLayer, error) {
	t, err := a.taskByRef(ref)
	if err != nil {
		return nil, err
	}
	members, err := a.stackOf(t.ID)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("%s isn't in the PR stack", t.ID)
	}
	var out []collapseLayer
	for _, id := range members {
		mt, err := a.Store.Task(id)
		if err != nil {
			return nil, err
		}
		if mt.PR == "" {
			return nil, fmt.Errorf("%s has no PR yet; run prs first", id)
		}
		pr, err := hub.PR(mt.PR)
		if err != nil {
			return nil, fmt.Errorf("asking GitHub about %s's PR %s: %w", id, mt.PR, err)
		}
		if pr.State != "" && pr.State != "OPEN" {
			return nil, fmt.Errorf("%s's PR %s is %s; run restack first", id, mt.PR, strings.ToLower(pr.State))
		}
		out = append(out, collapseLayer{mt, pr})
	}
	return out, nil
}

// contains reports, as an error, whether head lacks commit; it fetches the
// stack's branches first if it has to.
func (a *App) contains(head, commit string) error {
	if head == "" || commit == "" {
		return errors.New("GitHub didn't say which commit a PR is at")
	}
	if _, err := gitx.Run(a.Root, "cat-file", "-e", head+"^{commit}"); err != nil {
		_, _ = trainGit(a.Root, "fetch", "--quiet", "origin", head)
	}
	_, err := gitx.Run(a.Root, "merge-base", "--is-ancestor", commit, head)
	return err
}

// collapse merges layer k's PR, carrying layers 0..k, into base.
func (a *App) collapse(layers []collapseLayer, k int, hub automerge.GitHub) (CollapseResult, error) {
	res := CollapseResult{Stack: layers[0].Task.ID}
	if k < 1 {
		return res, fmt.Errorf("stack %s has one PR; there is nothing to collapse", res.Stack)
	}
	top := layers[k]
	res.Top, res.PR = top.Task.ID, top.Task.PR
	var refuse []string
	for _, l := range layers[:k+1] {
		res.Covered = append(res.Covered, l.Task.ID)
		switch {
		case slices.Contains(l.PR.Labels, automerge.NeedsHuman):
			refuse = append(refuse, fmt.Sprintf("%s's PR %s is labeled %s", l.Task.ID, l.Task.PR, automerge.NeedsHuman))
		case l.PR.Mergeable == "CONFLICTING":
			refuse = append(refuse, fmt.Sprintf("%s's PR %s conflicts with its base", l.Task.ID, l.Task.PR))
		}
	}
	for _, l := range layers[:k] {
		if err := a.contains(top.PR.HeadSHA, l.PR.HeadSHA); err != nil {
			refuse = append(refuse, fmt.Sprintf("%s's PR %s doesn't contain %s's commits", top.Task.ID, top.Task.PR, l.Task.ID))
		}
	}
	if len(refuse) > 0 {
		return res, fmt.Errorf("won't collapse stack %s: %s. Settle that first (restack, or resolve the conflict)", res.Stack, strings.Join(refuse, "; "))
	}

	was := top.PR.Base
	if was != a.Cfg.Base {
		if _, err := gh(a.Root, "pr", "edit", top.Task.PR, "--base", a.Cfg.Base); err != nil {
			return res, err
		}
		a.Store.Event(top.Task.ID, "collapse_retarget", top.Task.PR+" → "+a.Cfg.Base)
	}
	restore := func(why string) error {
		if was != a.Cfg.Base {
			_, _ = gh(a.Root, "pr", "edit", top.Task.PR, "--base", was)
		}
		return fmt.Errorf("collapse of stack %s stopped: %s; %s targets %s again and nothing merged", res.Stack, why, top.Task.PR, was)
	}
	pr, err := a.waitChecks(top.Task.PR, hub)
	if err != nil {
		return res, restore(err.Error())
	}
	if err := hub.Merge(top.Task.PR, "squash", pr.HeadSHA); err != nil {
		return res, restore("merging it failed: " + err.Error())
	}
	a.Store.Event(top.Task.ID, "collapse_merged", fmt.Sprintf("%s squash-merged into %s, carrying %s", top.Task.PR, a.Cfg.Base, strings.Join(res.Covered, ", ")))

	if err := a.markCollapsed(res); err != nil {
		return res, err
	}
	for _, l := range layers[:k] {
		body := fmt.Sprintf("Merged into %s as part of %s (saddle stack collapse): its CI was red and the fix lived in a PR above it, so the stack merged as one.",
			a.Cfg.Base, top.Task.PR)
		_, _ = gh(a.Root, "pr", "comment", l.Task.PR, "--body", body)
		if _, err := gh(a.Root, "pr", "close", l.Task.PR); err == nil {
			res.Closed = append(res.Closed, l.Task.PR)
		}
	}
	res.Restack, err = a.Restack()
	msg := fmt.Sprintf("Collapsed stack %s: %s's PR %s carried %s into %s (squash). Closed %s.",
		res.Stack, top.Task.ID, top.Task.PR, strings.Join(res.Covered, ", "), a.Cfg.Base, strings.Join(res.Closed, ", "))
	if err != nil {
		msg += " The restack after it failed: " + err.Error()
	}
	_ = a.Notify(OrchestratorID, store.NoticeInfo, msg)
	return res, err
}

// waitChecks waits for CI on url to finish; green or none passes.
func (a *App) waitChecks(url string, hub automerge.GitHub) (automerge.PR, error) {
	deadline := time.Now().Add(CollapseWait)
	for {
		pr, err := hub.PR(url)
		if err != nil {
			return pr, err
		}
		switch pr.Checks {
		case automerge.ChecksPass, automerge.ChecksNone:
			return pr, nil
		case automerge.ChecksFail:
			return pr, fmt.Errorf("CI on the combined head of %s is red", url)
		}
		if time.Now().After(deadline) {
			return pr, fmt.Errorf("CI on %s didn't finish within %s", url, CollapseWait)
		}
		sleep(collapsePoll)
	}
}

// markCollapsed takes the covered tasks out of the stack as merged.
func (a *App) markCollapsed(res CollapseResult) error {
	unlock, err := a.lockTrain()
	if err != nil {
		return err
	}
	defer unlock()
	all, err := a.landedAll()
	if err != nil {
		return err
	}
	var errs []error
	for _, l := range all {
		if !slices.Contains(res.Covered, l.ID) || !l.stacked() {
			continue
		}
		errs = append(errs, a.Store.SetTrain(l.ID, TrainMerged, l.rangeNote(), false))
		a.Store.Event(l.ID, "unstacked", fmt.Sprintf("%s: %s carried it into %s (stack collapse)", TrainMerged, res.PR, a.Cfg.Base))
	}
	return errors.Join(errs...)
}
