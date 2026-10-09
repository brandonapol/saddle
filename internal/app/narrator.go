package app

import (
	"context"
	"os"
	"strings"
	"sync"

	"github.com/brandonapol/saddle/internal/narrator"
	"github.com/brandonapol/saddle/internal/store"
)

// EventNarratorError is the events-table kind for a failed narrator step.
// The narrator never reads these back, so a failing API can't feed itself.
const EventNarratorError = "narrator_error"

// NarratorFromEnv is NewNarrator with ANTHROPIC_API_KEY and the real HTTP
// client.
func (a *App) NarratorFromEnv(sink narrator.Sink) *narrator.Narrator {
	return a.NewNarrator(os.Getenv("ANTHROPIC_API_KEY"), sink, nil)
}

// NewNarrator returns a narrator over this repo's event log, or nil when it
// is off: no API key, or no narrator.daily_cap_usd. Its daily spend is kept
// in the store so a restart does not reset the cap. http may be nil.
func (a *App) NewNarrator(apiKey string, sink narrator.Sink, http narrator.Doer) *narrator.Narrator {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" || a.Cfg.Narrator.DailyCapUSD <= 0 {
		return nil
	}
	src, err := a.narratorSource()
	if err != nil {
		a.Store.Event("", EventNarratorError, "start: "+err.Error())
		return nil
	}
	return narrator.New(narrator.Config{
		APIKey:      apiKey,
		Model:       a.Cfg.Narrator.Model,
		DailyCapUSD: a.Cfg.Narrator.DailyCapUSD,
		Prices:      a.Cfg.Limits.Prices,
	}, narrator.Deps{Source: src, Roster: a.Store, HTTP: http, Sink: sink, Ledger: a.Store, Heavy: narratorHeavy{a}})
}

// RunNarrator drives n until ctx is done, recording each distinct error once.
func (a *App) RunNarrator(ctx context.Context, n *narrator.Narrator) {
	_ = n.Run(ctx, a.narratorErrors())
}

// narratorErrors records an error as an event only when it differs from the
// last one, so an API outage leaves one line, not one per poll.
func (a *App) narratorErrors() func(error) {
	var mu sync.Mutex
	last := ""
	return func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if err.Error() == last {
			return
		}
		last = err.Error()
		a.Store.Event("", EventNarratorError, last)
	}
}

// eventSource hands out events past a cursor. It starts at the newest event,
// so history from before the narrator started is not narrated again.
type eventSource struct {
	st    *store.Store
	after int64
}

func (a *App) narratorSource() (*eventSource, error) {
	id, err := a.Store.LastEventID()
	if err != nil {
		return nil, err
	}
	return &eventSource{st: a.Store, after: id}, nil
}

func (s *eventSource) Poll(context.Context) ([]store.Event, error) {
	es, err := s.st.EventsSince(s.after)
	if err != nil || len(es) == 0 {
		return nil, err
	}
	s.after = es[len(es)-1].ID
	out := es[:0]
	for _, e := range es {
		if e.Kind != EventNarratorError {
			out = append(out, e)
		}
	}
	return out, nil
}
