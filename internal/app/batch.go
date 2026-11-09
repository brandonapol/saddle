package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/store"
)

// Batch mode (#913, ADR 0011). With [train] output = "batch" every landed
// task is one layer of one linear stack in train order, published at the
// commit the train landed and tested. The batch merges once: auto-merge
// fast-forwards base to the highest layer whose CI is green, which carries
// every layer below it. Nothing is rewritten, so the forge sees each of
// those PRs merged; the layers above retarget to base and wait for the next
// green layer.
//
// Because each layer holds everything below it, red CI on a layer holds
// nothing back: a layer above it that goes green carries it. Only a red
// layer with nothing green or running above it needs a repair.

// OutputBatch is the [train] output value for batch mode.
const OutputBatch = "batch"

// Events batch mode logs.
const (
	EventBatchMerged  = "batch_merged"
	EventBatchWaiting = "batch_waiting"
)

// linearOutput reports whether prs lays out one linear stack in train order.
func (a *App) linearOutput() bool {
	return a.Cfg.Train.Output == "single" || a.Cfg.Train.Output == OutputBatch
}

// BatchMode reports whether the repo's PRs form one batch, merged once.
func (a *App) BatchMode() bool { return a.Cfg.Train.Output == OutputBatch }

// batchCarrier names the layer above task in the batch whose CI is green
// (green true) or still running (green false) as auto-merge last read it:
// that layer carries task's work to base, or may. "" outside batch mode, or
// when every layer above task is red or task is the top.
func (a *App) batchCarrier(task string) (carrier string, green bool) {
	if !a.BatchMode() {
		return "", false
	}
	st, err := a.AutomergeState()
	if err != nil {
		return "", false
	}
	for _, s := range st.Stacks {
		at := slices.IndexFunc(s.Nodes, func(n automerge.Node) bool { return n.Task == task })
		if at < 0 {
			continue
		}
		pending := ""
		for _, n := range s.Nodes[at+1:] {
			switch n.Checks {
			case automerge.ChecksPass:
				return n.Task, true
			case automerge.ChecksPending:
				if pending == "" {
					pending = n.Task
				}
			}
		}
		return pending, false
	}
	return "", false
}

// BatchMergeResult is what one batch merge did, or why it did nothing.
type BatchMergeResult struct {
	PR      string        `json:"pr,omitempty"`      // the green layer base was fast-forwarded to
	Top     string        `json:"top,omitempty"`     // its task
	Head    string        `json:"head,omitempty"`    // the commit base is at now
	Covered []string      `json:"covered,omitempty"` // tasks it carried, bottom first
	Why     string        `json:"why,omitempty"`     // why nothing merged
	Restack RestackResult `json:"restack"`
}

// batchLayer is one published layer of the batch as the forge reports it.
type batchLayer struct {
	l  landedTask
	pr automerge.PR
}

// BatchMerge merges the batch once: it fast-forwards base to the highest
// layer whose CI is green, after pointing every PR up to it at base so the
// forge marks each one merged when its head lands there. It stops below a
// layer that isn't open, is a draft, is labeled needs-human, conflicts or is
// held. A base that moved past the batch (a hand merge) is restacked first,
// and the merge waits for the next tick. hub nil is the repo's forge.
func (a *App) BatchMerge(hub automerge.GitHub) (BatchMergeResult, error) {
	var res BatchMergeResult
	if a.remote() == "" {
		res.Why = "the repo has no remote"
		return res, nil
	}
	hub = a.ghOr(hub)
	remote := "refs/remotes/origin/" + a.Cfg.Base
	if _, err := trainGit(a.Root, "fetch", "--quiet", "origin", "+refs/heads/"+a.Cfg.Base+":"+remote); err != nil {
		return res, err
	}
	stack, err := a.landedStack()
	if err != nil {
		return res, err
	}
	holds := a.batchHolds()
	var layers []batchLayer
	var waits []string
	top := -1
	for _, l := range stack {
		if l.PR == "" {
			waits = append(waits, l.ID+" has no PR yet")
			break
		}
		pr, err := hub.PR(l.PR)
		if err != nil {
			waits = append(waits, fmt.Sprintf("%s couldn't be read: %v", l.PR, err))
			break
		}
		if why := batchStop(l, pr, holds); why != "" {
			waits = append(waits, why)
			break
		}
		layers = append(layers, batchLayer{l, pr})
		switch pr.Checks {
		case automerge.ChecksPass:
			top = len(layers) - 1
		case automerge.ChecksFail:
			waits = append(waits, l.ID+" is red")
		default:
			waits = append(waits, l.ID+" is "+pr.Checks)
		}
	}
	if top < 0 {
		res.Why = "no layer of the batch is green yet"
		if len(waits) > 0 {
			res.Why += ": " + strings.Join(waits, "; ")
		}
		return res, nil
	}
	head := layers[top].pr.HeadSHA
	if head == "" {
		res.Why = a.forgeName() + " didn't say which commit " + layers[top].l.PR + " is at"
		return res, nil
	}
	if err := a.contains(head, remote); err != nil {
		// Base has work the batch lacks (a hand merge, a squash): restack the
		// batch onto it, which re-runs its CI, and merge on a later tick.
		res.Why = fmt.Sprintf("%s has commits the batch lacks, so it can't fast-forward; restacking the batch onto it first", a.Cfg.Base)
		a.Store.Event(layers[top].l.ID, EventBatchWaiting, res.Why)
		res.Restack, err = a.Restack()
		return res, err
	}

	ctx := context.Background()
	for _, x := range layers[:top+1] {
		if x.pr.Base == a.Cfg.Base {
			continue
		}
		if err := a.Forge().SetPRBase(ctx, x.l.PR, a.Cfg.Base); err != nil {
			a.restoreBatchBases(layers[:top+1])
			return res, fmt.Errorf("batch merge: pointing %s at %s: %w; nothing merged", x.l.PR, a.Cfg.Base, err)
		}
	}
	if _, err := trainGit(a.Root, "push", "origin", head+":refs/heads/"+a.Cfg.Base); err != nil {
		a.restoreBatchBases(layers[:top+1])
		return res, fmt.Errorf("batch merge: fast-forwarding %s to %s (%s): %w; nothing merged", a.Cfg.Base, short(head), layers[top].l.ID, err)
	}
	_, _ = trainGit(a.Root, "update-ref", remote, head)

	res.PR, res.Top, res.Head = layers[top].l.PR, layers[top].l.ID, head
	for _, x := range layers[:top+1] {
		res.Covered = append(res.Covered, x.l.ID)
	}
	a.Store.Event(res.Top, EventBatchMerged, fmt.Sprintf("%s fast-forwarded to %s (%s, %s), carrying %s",
		a.Cfg.Base, short(head), res.Top, res.PR, strings.Join(res.Covered, ", ")))
	if err := a.markCarried(res.Covered, fmt.Sprintf("the batch merge fast-forwarded %s to %s's %s", a.Cfg.Base, res.Top, res.PR)); err != nil {
		return res, err
	}
	res.Restack, err = a.Restack()
	msg := fmt.Sprintf("Batch merged: %s is now at %s's %s, carrying %s.", a.Cfg.Base, res.Top, res.PR, strings.Join(res.Covered, ", "))
	if err != nil {
		msg += " The restack after it failed: " + err.Error()
	}
	_ = a.Notify(OrchestratorID, store.NoticeInfo, msg)
	return res, err
}

// batchStop says why the batch can't merge at or above l, "" if it can.
func batchStop(l landedTask, pr automerge.PR, holds []string) string {
	switch {
	case pr.State != "" && pr.State != "OPEN":
		return fmt.Sprintf("%s's PR %s is %s; the restack takes it out", l.ID, l.PR, strings.ToLower(pr.State))
	case pr.Draft:
		return fmt.Sprintf("%s's PR %s is a draft", l.ID, l.PR)
	case slices.Contains(pr.Labels, automerge.NeedsHuman):
		return fmt.Sprintf("%s's PR %s is labeled %s", l.ID, l.PR, automerge.NeedsHuman)
	case pr.Mergeable == "CONFLICTING":
		return fmt.Sprintf("%s's PR %s conflicts with its base", l.ID, l.PR)
	case slices.Contains(holds, l.ID) || slices.Contains(holds, l.PR):
		return fmt.Sprintf("%s is held; `saddle automerge release %s` lets the batch past it", l.ID, l.ID)
	}
	return ""
}

// batchHolds is auto-merge's holds: in a batch, a hold stops the merge
// below the held layer.
func (a *App) batchHolds() []string {
	s, err := automerge.Load(a.automergePath())
	if err != nil {
		return nil
	}
	return s.Holds
}

// restoreBatchBases points the PRs a failed batch merge retargeted back at
// the bases they had.
func (a *App) restoreBatchBases(layers []batchLayer) {
	for _, x := range layers {
		if x.pr.Base != "" && x.pr.Base != a.Cfg.Base {
			_ = a.Forge().SetPRBase(context.Background(), x.l.PR, x.pr.Base)
		}
	}
}

// markCarried takes covered out of the stack as merged, with why in each
// task's unstacked event, and closes their issues.
func (a *App) markCarried(covered []string, why string) error {
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
		if !slices.Contains(covered, l.ID) || !l.stacked() {
			continue
		}
		errs = append(errs, a.Store.SetTrain(l.ID, TrainMerged, l.rangeNote(), false))
		a.Store.Event(l.ID, "unstacked", fmt.Sprintf("%s: %s", TrainMerged, why))
		a.closeMergedIssue(l.Task, why)
	}
	return errors.Join(errs...)
}
