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
// root cause and posts a card naming it. While the same stall lasts it posts
// again with backoff (1h, 2h, then every 4h), and a cause saddle can clear by
// itself (a zombie, a lost window, a task the hung watch gave up on) has that
// fix as the card's default (#783). When work moves, the card closes by
// itself.

// Root causes, most fundamental first: Diagnose names the first that holds.
const (
	CauseGHAuth   = "gh-auth"   // gh can't reach GitHub: nothing opens or merges
	CauseQuota    = "quota"     // every adapter is out of quota: nothing starts
	CauseZombie   = "zombie"    // a live task's worktree is gone (#765)
	CauseNoWindow = "no-window" // a live task has no window of its own
	CausePrompts  = "prompts"   // every live worker sits on a prompt
	CauseGaveUp   = "gave-up"   // the hung watch gave up on a task holding its slot
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
	Zombies       []string // live tasks whose worktree is gone
	Windowless    []string // live tasks with no window of their own
	GaveUp        []string // live tasks the hung watch gave up on or was told to keep
}

// RootCause is the watchdog's diagnosis.
type RootCause struct {
	Key  string
	What string // one plain line
	Fix  string // what clears it
	// Run is the registered action that clears it without the owner, if
	// there is one; the card runs it when nobody answers.
	Run string
}

// Diagnose names the single root cause of a stall.
func Diagnose(s WatchStatus) RootCause {
	if s.GHAuth != nil {
		return RootCause{Key: CauseGHAuth, What: "gh auth has expired or failed (" + oneLine(s.GHAuth.Error()) + "), so saddle can't open, check or merge PRs.", Fix: "gh auth login"}
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
		return RootCause{Key: CauseQuota, What: "every adapter is out of quota, so no agent can start or continue.", Fix: "wait for the reset, or add an adapter under [adapters]"}
	}
	if len(s.Zombies) > 0 {
		return RootCause{CauseZombie, "live tasks whose worktree is gone (" + strings.Join(s.Zombies, ", ") + ") do nothing but hold their claims and slots.",
			"saddle rescue " + strings.Join(s.Zombies, " "), RunRescueStalled}
	}
	if len(s.Windowless) > 0 {
		return RootCause{CauseNoWindow, "live tasks with no window of their own (" + strings.Join(s.Windowless, ", ") + ") hold their slots with no agent working on them.",
			"saddle resume " + strings.Join(s.Windowless, " "), RunResumeLost}
	}
	if s.Live > 0 && s.PromptBlocked >= s.Live {
		return RootCause{Key: CausePrompts, What: fmt.Sprintf("all %d live workers sit on a prompt nobody answered.", s.Live), Fix: "answer the prompts in the needs-you band"}
	}
	if len(s.GaveUp) > 0 {
		return RootCause{CauseGaveUp, "the hung-worker watch gave up on " + strings.Join(s.GaveUp, ", ") + ", which still hold their slots.",
			"saddle rescue " + strings.Join(s.GaveUp, " "), RunRescueStalled}
	}
	// A wedged gate loads the machine too, so it outranks overload (#432).
	if len(s.Hung) > 0 {
		return hungGateCause(s.Hung)
	}
	if s.CPUs > 0 && s.Load1 > 2*float64(s.CPUs) {
		return RootCause{Key: CauseOverload, What: fmt.Sprintf("the machine is overloaded (load %.0f on %d CPUs), so gates and agents crawl.", s.Load1, s.CPUs), Fix: "lower [concurrency] or stop other heavy jobs"}
	}
	return RootCause{Key: CauseUnknown, What: "work waits but nothing moved, and saddle sees no quota, auth, prompt, zombie, window, hung gate or load problem.", Fix: "saddle status, then peek at the oldest open task"}
}

// watchdogState is what the watchdog last posted, saved under .saddle.
type watchdogState struct {
	Cause    string    `json:"cause,omitempty"`
	Progress time.Time `json:"progress,omitzero"` // the stall it was posted for
	Card     string    `json:"card,omitempty"`
	Posts    int       `json:"posts,omitempty"`    // cards posted for this stall and cause
	PostedAt time.Time `json:"posted_at,omitzero"` // when the last one was
}

// watchdogRepost is how long the watchdog waits after its nth card for a
// stall before posting the next: 1h, 2h, then 4h each time (#783).
func watchdogRepost(n int) time.Duration {
	return time.Hour << min(max(n-1, 0), 2)
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

// Watchdog judges st at now. In a stall it posts a card naming the root
// cause, unless it already posted that cause for this stall within the
// backoff (watchdogRepost); otherwise it closes the card it posted. posted
// says whether it sent a card now.
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
	posts := 1
	if s.Cause == rc.Key && s.Progress.Equal(st.LastProgress) {
		if !s.PostedAt.IsZero() && now.Sub(s.PostedAt) < watchdogRepost(s.Posts) {
			return rc, false, nil
		}
		posts = s.Posts + 1
	}
	if s.Card != "" {
		if err := a.ResolveDecision(s.Card, now); err != nil {
			return rc, false, err
		}
	}
	card := noProgressCard(rc, now.Sub(st.LastProgress), st)
	if posts > 1 {
		card.ID += "#" + strconv.Itoa(posts)
	}
	card.Opened = now
	if err := a.OpenDecision(card); err != nil {
		return rc, false, err
	}
	a.Store.Event("", "watchdog_stall", rc.Key)
	return rc, true, a.setWatchdogState(watchdogState{Cause: rc.Key, Progress: st.LastProgress, Card: card.ID, Posts: posts, PostedAt: now.UTC()})
}

// noProgressCard is the watchdog's card for rc after a stall of idle.
func noProgressCard(rc RootCause, idle time.Duration, st WatchStatus) DecisionCard {
	leave := DecisionOption{Label: "Leave it", Cost: "saddle posts again after 1h, 2h, then every 4h while it lasts", Action: "nothing", Run: RunNothing}
	if rc.Run != "" {
		return DecisionCard{
			ID:       DecisionNoProgress + ":" + rc.Key + "@" + strconv.FormatInt(st.LastProgress.Unix(), 10),
			Kind:     DecisionNoProgress,
			To:       OrchestratorID,
			Headline: fmt.Sprintf("No progress for %s: nothing landed, merged or started.", fmtDuration(idle)),
			What:     "Root cause: " + rc.What,
			Blocking: fmt.Sprintf("Everything: %d open tasks and %d queued branches wait.", st.OpenTasks, st.ReadyWork),
			Options: []DecisionOption{
				{Label: "Clear the cause", Cost: "saddle does it; work so far is kept", Action: rc.Fix, Recommended: true, Run: rc.Run},
				leave,
			},
			Default: 1,
			Timeout: DefaultDecisionTimeout,
		}
	}
	return DecisionCard{
		ID:       DecisionNoProgress + ":" + rc.Key + "@" + strconv.FormatInt(st.LastProgress.Unix(), 10),
		Kind:     DecisionNoProgress,
		To:       OrchestratorID,
		Headline: fmt.Sprintf("No progress for %s: nothing landed, merged or started.", fmtDuration(idle)),
		What:     "Root cause: " + rc.What,
		Blocking: fmt.Sprintf("Everything: %d open tasks and %d queued branches wait.", st.OpenTasks, st.ReadyWork),
		Options: []DecisionOption{
			{Label: "Clear the cause", Cost: "you, a minute or two", Action: rc.Fix, Recommended: true},
			leave,
		},
		Default: 2,
		Timeout: DefaultDecisionTimeout,
	}
}

// progressKinds are the events that count as work moving.
var progressKinds = []string{"landed", "spawn", automerge.EventMerged, "pr_merged"}

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
	// The whole log, not its newest page: a flood of other events must not
	// hide the last progress and close the card on a live stall (#783).
	last, err := a.Store.LastEventTime(progressKinds...)
	if err != nil {
		return st, err
	}
	if !last.IsZero() {
		st.LastProgress = last.UTC()
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
