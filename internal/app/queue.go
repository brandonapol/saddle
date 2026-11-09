package app

import (
	"fmt"
	"slices"

	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/store"
)

// Queue lists the train entries still waiting to land, in landing order:
// queued ones and the ones held back with Hold.
func (a *App) Queue() ([]store.TrainEntry, error) {
	es, err := a.Store.Train()
	if err != nil {
		return nil, err
	}
	var out []store.TrainEntry
	for _, e := range es {
		if waiting(e) {
			out = append(out, e)
		}
	}
	return out, nil
}

func waiting(e store.TrainEntry) bool { return e.State == store.Queued || e.State == store.OnHold }

// queueEntry finds task's entry in the waiting queue.
func (a *App) queueEntry(task string) ([]store.TrainEntry, int, error) {
	q, err := a.Queue()
	if err != nil {
		return nil, 0, err
	}
	i := slices.IndexFunc(q, func(e store.TrainEntry) bool { return e.Task == task })
	if i < 0 {
		return nil, 0, fmt.Errorf("%s isn't waiting in the merge train", task)
	}
	return q, i, nil
}

// MoveInQueue moves a waiting entry to position pos (1 is next to land) among
// the waiting entries; past the end means the back (#25).
func (a *App) MoveInQueue(task string, pos int) error {
	if a.inMergeQueue(task) {
		return a.QueueOrder(task, pos)
	}
	q, i, err := a.queueEntry(task)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(q))
	for _, e := range q {
		ids = append(ids, e.Task)
	}
	ids = slices.Delete(ids, i, i+1)
	pos = min(max(pos, 1), len(ids)+1)
	ids = slices.Insert(ids, pos-1, task)
	if err := a.Store.SetTrainOrder(ids); err != nil {
		return err
	}
	a.Store.Event(task, "train_move", fmt.Sprintf("to %d of %d", pos, len(ids)))
	return nil
}

// Hold keeps a queued entry from landing until Unhold, without losing its
// place. Calling done again doesn't release it (#25).
func (a *App) Hold(task, reason string) error {
	if a.inMergeQueue(task) {
		return a.holdPR(task, true, reason)
	}
	if _, _, err := a.queueEntry(task); err != nil {
		return err
	}
	if err := a.Store.SetTrain(task, store.OnHold, reason, false); err != nil {
		return err
	}
	a.Store.Event(task, "train_hold", reason)
	return nil
}

// Unhold puts a held entry back in line where it was.
func (a *App) Unhold(task string) error {
	if a.inMergeQueue(task) {
		return a.holdPR(task, false, "")
	}
	q, i, err := a.queueEntry(task)
	if err != nil {
		return err
	}
	if q[i].State != store.OnHold {
		return fmt.Errorf("%s isn't on hold", task)
	}
	if err := a.Store.SetTrain(task, store.Queued, "", false); err != nil {
		return err
	}
	a.Store.Event(task, "train_unhold", "")
	return nil
}

// inMergeQueue reports whether task is a published task waiting in the
// merge queue (#935) rather than in the train.
func (a *App) inMergeQueue(task string) bool {
	if _, _, err := a.queueEntry(task); err == nil {
		return false
	}
	t, err := a.Store.Task(task)
	return err == nil && t.Status == store.PROpen
}

// holdPR holds a published task's PR back from the merge queue, or lets it
// go: the same hold auto-merge keeps.
func (a *App) holdPR(task string, hold bool, reason string) error {
	_, err := automerge.Update(a.automergePath(), func(s *automerge.State) error {
		has := slices.Contains(s.Holds, task)
		switch {
		case hold && !has:
			s.Holds = append(s.Holds, task)
		case !hold && !has:
			return fmt.Errorf("%s isn't on hold", task)
		case !hold:
			s.Holds = slices.DeleteFunc(s.Holds, func(h string) bool { return h == task })
		}
		return nil
	})
	if err != nil {
		return err
	}
	if hold {
		a.Store.Event(task, "queue_hold", reason)
	} else {
		a.Store.Event(task, "queue_unhold", "")
	}
	return nil
}
