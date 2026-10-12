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
func (a *App) LandRecorded() ([]LandResult, error) {
	queued := a.queuedTasks()
	rs, err := a.Land()
	return a.accountLandings(queued, rs), err
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
