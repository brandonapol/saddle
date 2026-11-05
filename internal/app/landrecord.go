package app

import (
	"slices"
	"strings"

	"github.com/brandonapol/saddle/internal/store"
)

// LandRecorded runs the train and accounts for every task that landed (#392):
// the results are never nil, a task queued when it was called that another
// run landed while this one waited for the lock is reported too, and each
// landed task's train note keeps the from..to range restack relies on.
//
// Landing ends with an open PR (#819): once the train has run, every landed
// task without a PR gets one, the way prs opens them, so no orchestrator has
// to remember prs. That includes tasks an older saddle landed without one.
// A PR that can't open yet doesn't fail the landing: its result says why, the
// alarm goes to the orchestrator, and the sentinel retries.
func (a *App) LandRecorded() ([]LandResult, error) {
	queued := a.queuedTasks()
	rs, err := a.Land()
	out := a.accountLandings(queued, rs)
	if err != nil {
		return out, err
	}
	_, perr := a.PublishLanded("land")
	a.noteDelivery(out, perr)
	return out, nil
}

// noteDelivery adds to each landed result where its work stands on the way
// to base: its open PR, or why it has none yet.
func (a *App) noteDelivery(rs []LandResult, perr error) {
	for i, r := range rs {
		if r.State != store.TrainOK {
			continue
		}
		t, err := a.Store.Task(r.Task)
		if err != nil {
			continue
		}
		var d string
		switch {
		case t.PR != "":
			d = DeliveryPROpen + " " + t.PR + " (not on " + a.Cfg.Base + " until it merges)"
		case a.remote() == "":
			d = DeliveryNoPR + ": the repo has no remote to open it on"
		case perr != nil:
			d = DeliveryNoPR + ": " + firstLineOf(perr.Error()) + "; saddle retries"
		default:
			d = DeliveryNoPR + "; saddle retries"
		}
		if rs[i].Note == "" {
			rs[i].Note = d
		} else {
			rs[i].Note += "; " + d
		}
	}
}

// queuedTasks lists the tasks waiting in the train.
func (a *App) queuedTasks() []string {
	es, err := a.Store.Train()
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range es {
		if e.State == store.Queued {
			out = append(out, e.Task)
		}
	}
	return out
}

// accountLandings adds a result for each of queued that landed outside rs,
// and rebuilds the range of any landed task whose note lost it.
func (a *App) accountLandings(queued []string, rs []LandResult) []LandResult {
	out := slices.Clone(rs)
	if out == nil {
		out = []LandResult{}
	}
	es, err := a.Store.Train()
	if err != nil {
		return out
	}
	notes := map[string]string{}
	for _, e := range es {
		if e.State == store.TrainOK {
			notes[e.Task] = e.Note
		}
	}
	for _, id := range queued {
		note, landed := notes[id]
		if landed && !slices.ContainsFunc(out, func(r LandResult) bool { return r.Task == id }) {
			out = append(out, LandResult{Task: id, State: store.TrainOK, Note: "landed by another train run: " + note})
		}
	}
	var landings []store.Event
	for _, r := range out {
		note, ok := notes[r.Task]
		if r.State != store.TrainOK || !ok || strings.Contains(note, "..") {
			continue
		}
		if landings == nil {
			evs, _ := a.Store.Events(-1)
			landings = []store.Event{}
			for _, e := range evs {
				if e.Kind == "landed" {
					landings = append(landings, e)
				}
			}
		}
		if from, to, ok := a.landingRange(r.Task, landings); ok {
			_ = a.Store.SetTrain(r.Task, store.TrainOK, from+".."+to, false)
		}
	}
	return out
}
