package app

import (
	"errors"
	"fmt"
	"time"

	"github.com/brandonapol/saddle/internal/usage"
)

// ErrLaunchesPaused means a spawn was refused because a plan-limit window is
// over its cap and limits.pause_launches is on.
var ErrLaunchesPaused = errors.New("new launches are paused")

// Limits estimates plan-limit usage as of now from the stored usage buckets
// and the configured caps.
func (a *App) Limits(now time.Time) (usage.LimitEstimate, error) {
	// Both windows start no earlier than a week ago.
	bs, err := a.Store.UsageBuckets(now.Add(-usage.Week))
	if err != nil {
		return usage.LimitEstimate{}, err
	}
	return usage.EstimateBuckets(bs, a.Cfg.Limits, now), nil
}

// checkLaunch returns ErrLaunchesPaused, with which window is over and when
// it resets, when new spawns should be held.
func (a *App) checkLaunch() error {
	if !a.Cfg.Limits.PauseLaunches {
		return nil
	}
	e, err := a.Limits(time.Now())
	if err != nil {
		return fmt.Errorf("check usage limits: %w", err)
	}
	if !e.ShouldPauseLaunches() {
		return nil
	}
	var over []string
	for _, w := range []usage.WindowEstimate{e.FiveHour, e.Weekly} {
		if w.State != usage.Over {
			continue
		}
		s := fmt.Sprintf("the %s window is at %.0f%% of its cap", w.Name, w.Percent*100)
		if w.ResetIn > 0 {
			s += fmt.Sprintf(" (resets in %s)", w.ResetIn.Round(time.Minute))
		}
		over = append(over, s)
	}
	return fmt.Errorf("%w: %s and limits.pause_launches is on. Running agents keep going. "+
		"Tell the user; wait for the reset, raise the cap in .saddle/config.toml, or spawn with force to launch anyway",
		ErrLaunchesPaused, joinAnd(over))
}

func joinAnd(ss []string) string {
	switch len(ss) {
	case 0:
		return "a usage window is over its cap"
	case 1:
		return ss[0]
	}
	return ss[0] + " and " + joinAnd(ss[1:])
}
