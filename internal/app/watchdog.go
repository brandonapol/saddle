package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/store"
)

// The no-progress watchdog (#406). When nothing has landed, merged or
// started for [stall] idle_alarm while work waits, saddle works out the one
// root cause and posts a single card naming it. It doesn't post again until
// the cause changes or work moves and stalls anew; when work moves, the card
// closes by itself.

// Root causes, most fundamental first: Diagnose names the first that holds.
const (
	CauseGHAuth   = "gh-auth"   // gh can't reach GitHub: nothing opens or merges
	CauseQuota    = "quota"     // every adapter is out of quota: nothing starts
	CausePrompts  = "prompts"   // every live worker sits on a prompt
	CauseHungGate = "hung-gate" // a gate or test runs far past its own timeout
	CauseOverload = "overload"  // the machine is too loaded to make headway
	CauseUnknown  = "unknown"   // none of the above
)

// WatchStatus is what the watchdog looks at.
type WatchStatus struct {
	LastProgress  time.Time      // the last land, merge or spawn
	OpenTasks     int            // workers alive, paused or queued
	ReadyWork     int            // branches waiting in the train
	Live          int            // workers alive
	PromptBlocked int            // live workers sitting on a prompt
	Adapters      []agent.Status // which adapters run, and their quota
	GHAuth        error          // gh auth status; nil when fine
	Hung          []HungGate     // gate and test processes far past their timeout
	Load1         float64        // the 1-minute load average
	CPUs          int
}

// RootCause is the watchdog's diagnosis.
type RootCause struct {
	Key  string
	What string // one plain line
	Fix  string // what clears it
}

// Diagnose names the single root cause of a stall.
func Diagnose(s WatchStatus) RootCause {
	if s.GHAuth != nil {
		return RootCause{CauseGHAuth, "gh auth has expired or failed (" + oneLine(s.GHAuth.Error()) + "), so saddle can't open, check or merge PRs.", "gh auth login"}
	}
	usable, out := 0, 0
	for _, ad := range s.Adapters {
		if ad.OK {
			usable++
			if ad.OutOfQuotaUntil != "" {
				out++
			}
		}
	}
	if usable > 0 && out == usable {
		return RootCause{CauseQuota, "every adapter is out of quota, so no agent can start or continue.", "wait for the reset, or add an adapter under [adapters]"}
	}
	if s.Live > 0 && s.PromptBlocked >= s.Live {
		return RootCause{CausePrompts, fmt.Sprintf("all %d live workers sit on a prompt nobody answered.", s.Live), "answer the prompts in the needs-you band"}
	}
	// A wedged gate loads the machine too, so it outranks overload (#432).
	if len(s.Hung) > 0 {
		return hungGateCause(s.Hung)
	}
	if s.CPUs > 0 && s.Load1 > 2*float64(s.CPUs) {
		return RootCause{CauseOverload, fmt.Sprintf("the machine is overloaded (load %.0f on %d CPUs), so gates and agents crawl.", s.Load1, s.CPUs), "lower [concurrency] or stop other heavy jobs"}
	}
	return RootCause{CauseUnknown, "work waits but nothing moved, and saddle sees no quota, auth, prompt, hung gate or load problem.", "saddle status, then peek at the oldest open task"}
}

// watchdogState is what the watchdog last posted, saved under .saddle.
type watchdogState struct {
	Cause    string    `json:"cause,omitempty"`
	Progress time.Time `json:"progress,omitzero"` // the stall it was posted for
	Card     string    `json:"card,omitempty"`
}

func (a *App) watchdogPath() string { return a.stateDir("watchdog.json") }

func (a *App) watchdogState() watchdogState {
	var s watchdogState
	if b, err := os.ReadFile(a.watchdogPath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func (a *App) setWatchdogState(s watchdogState) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := a.watchdogPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.watchdogPath())
}

// stalled reports whether st is a stall at now: work waits and nothing
// moved for idle.
func stalled(st WatchStatus, now time.Time, idle time.Duration) bool {
	return st.OpenTasks+st.ReadyWork > 0 && !st.LastProgress.IsZero() && now.Sub(st.LastProgress) >= idle
}

// Watchdog judges st at now. In a stall it posts one card naming the root
// cause, unless it already posted that cause for this stall; otherwise it
// closes the card it posted. posted says whether it sent a card now.
func (a *App) Watchdog(now time.Time, st WatchStatus) (rc RootCause, posted bool, err error) {
	s := a.watchdogState()
	if !stalled(st, now, a.StallConfig().IdleAlarm) {
		if s.Card != "" {
			if err := a.ResolveDecision(s.Card, now); err != nil {
				return rc, false, err
			}
		}
		if s != (watchdogState{}) {
			err = a.setWatchdogState(watchdogState{})
		}
		return rc, false, err
	}
	rc = Diagnose(st)
	if s.Cause == rc.Key && s.Progress.Equal(st.LastProgress) {
		return rc, false, nil
	}
	if s.Card != "" {
		if err := a.ResolveDecision(s.Card, now); err != nil {
			return rc, false, err
		}
	}
	card := noProgressCard(rc, now.Sub(st.LastProgress), st)
	card.Opened = now
	if err := a.OpenDecision(card); err != nil {
		return rc, false, err
	}
	a.Store.Event("", "watchdog_stall", rc.Key)
	return rc, true, a.setWatchdogState(watchdogState{Cause: rc.Key, Progress: st.LastProgress, Card: card.ID})
}

// noProgressCard is the watchdog's card for rc after a stall of idle.
func noProgressCard(rc RootCause, idle time.Duration, st WatchStatus) DecisionCard {
	return DecisionCard{
		ID:       DecisionNoProgress + ":" + rc.Key + "@" + strconv.FormatInt(st.LastProgress.Unix(), 10),
		Kind:     DecisionNoProgress,
		To:       OrchestratorID,
		Headline: fmt.Sprintf("No progress for %s: nothing landed, merged or started.", fmtDuration(idle)),
		What:     "Root cause: " + rc.What,
		Blocking: fmt.Sprintf("Everything: %d open tasks and %d queued branches wait.", st.OpenTasks, st.ReadyWork),
		Options: []DecisionOption{
			{Label: "Clear the cause", Cost: "you, a minute or two", Action: rc.Fix, Recommended: true},
			{Label: "Leave it", Cost: "saddle posts again only if the cause changes", Action: "nothing"},
		},
		Default: 2,
		Timeout: DefaultDecisionTimeout,
	}
}

// progressKinds are the events that count as work moving.
var progressKinds = map[string]bool{"landed": true, "spawn": true, automerge.EventMerged: true, "pr_merged": true}

// WatchdogTick runs the watchdog on this repo's status at now. The costly
// checks (gh auth, adapters, processes, load) run only in a stall.
func (a *App) WatchdogTick(now time.Time) (RootCause, bool, error) {
	st, err := a.watchStatus()
	if err != nil {
		return RootCause{}, false, err
	}
	if stalled(st, now, a.StallConfig().IdleAlarm) {
		st.Adapters = a.Adapters()
		if err := a.Forge().AuthStatus(context.Background()); err != nil {
			st.GHAuth = err
		}
		if ts, err := a.Store.Tasks(); err == nil {
			st.Hung = HungGates(psProcs, ts)
		}
		st.Load1, st.CPUs = loadAvg(), runtime.NumCPU()
	}
	return a.Watchdog(now, st)
}

// watchStatus reads the cheap part of WatchStatus from the store.
func (a *App) watchStatus() (WatchStatus, error) {
	var st WatchStatus
	ts, err := a.Store.Tasks()
	if err != nil {
		return st, err
	}
	for _, t := range ts {
		if t.Role != store.RoleWorker {
			continue
		}
		switch {
		case liveStatus(t.Status):
			st.Live++
			st.OpenTasks++
			if t.Status == store.NeedsYou {
				st.PromptBlocked++
			}
		case t.Status == StatusPaused:
			st.OpenTasks++
		}
	}
	es, err := a.Store.Train()
	if err != nil {
		return st, err
	}
	for _, e := range es {
		if e.State == store.Queued {
			st.ReadyWork++
		}
	}
	evs, err := a.Store.Events(2000)
	if err != nil {
		return st, err
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if progressKinds[evs[i].Kind] {
			st.LastProgress = evs[i].TS.UTC()
			break
		}
	}
	return st, nil
}

// loadAvg is the 1-minute load average; 0 where /proc doesn't have it.
func loadAvg() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	f, _ := strconv.ParseFloat(strings.Fields(string(b) + " 0")[0], 64)
	return f
}
