package autopilot

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/claims"
)

// Defaults for a Driver's zero fields.
const (
	DefaultInterval = 30 * time.Second
	// DefaultStallAfter is how long ready work may sit unstartable below the
	// cap, or notices sit unread, before the orchestrator is nudged.
	DefaultStallAfter = 2 * time.Minute
	// DefaultNudgeEvery is how soon the same stall is nudged again.
	DefaultNudgeEvery = 15 * time.Minute
	// DefaultPauseRetry is how long a usage pause sleeps when the reset
	// time isn't known.
	DefaultPauseRetry = 15 * time.Minute
)

// Event kinds, logged through Env.Event.
const (
	EventOn      = "autopilot_on"
	EventPause   = "autopilot_pause"
	EventResume  = "autopilot_resume"
	EventTick    = "autopilot_tick" // the tick's decision changed
	EventSpawn   = "autopilot_spawn"
	EventSkip    = "autopilot_skip" // a spawn failed; the issue waits
	EventSleep   = "autopilot_sleep"
	EventWake    = "autopilot_wake"
	EventNudge   = "autopilot_nudge"
	EventStopped = "autopilot_stopped" // the final summary
	EventError   = "autopilot_error"
)

// Errors Env.Spawn wraps so the driver knows to stop topping up.
var (
	ErrPaused = errors.New("launches are paused for plan limits")
	ErrAtCap  = errors.New("at the concurrency cap")
)

// Task is a worker as the driver sees it. Killed tasks are left out.
type Task struct {
	ID     string
	Issue  int      // the issue it implements, 0 for none
	Claims []string // nil means it declared none: it may touch anything
	Live   bool     // an agent works on it (running, idle, needs you, conflict, paused)
	Queued bool     // done, waiting in the merge train
}

// Usage is plan-limit pressure now.
type Usage struct {
	Percent  float64   // the fullest window, 1 at its cap; 0 with no caps
	Pause    bool      // limits say hold new launches
	ResetsAt time.Time // when the pressure lifts, if known
}

// SpawnReq is a task the driver starts for an issue.
type SpawnReq struct {
	Title, Prompt, Model string
	Issue                int
	Claims               []string
	After                []string // tasks whose work this one builds on
}

// Env is everything the driver touches.
type Env interface {
	Tasks() ([]Task, error)
	// Ready lists open issues with label, oldest first.
	Ready(label string) ([]Issue, error)
	// Capacity is how many workers count against the cap, and the cap.
	Capacity() (running, limit int, err error)
	Usage(now time.Time) (Usage, error)
	// Reconcile resumes or rescues tasks that lost their agent; it returns
	// the tasks it touched.
	Reconcile() ([]string, error)
	// Land lands queued tasks and publishes their PRs; it returns how many landed.
	Land() (int, error)
	Spawn(SpawnReq) (task string, err error)
	// Nudge types text into an idle orchestrator without interrupting it or
	// the owner; false means it wasn't idle and nothing was sent.
	Nudge(text string) (bool, error)
	// Backlog is when the oldest unread action notice for the orchestrator came in.
	Backlog() (since time.Time, ok bool)
	Notify(interrupt bool, text string)
	Event(kind, data string)
}

// Driver runs autopilot ticks against Env. Tests call Tick with an injected clock.
type Driver struct {
	Path       string // the state file
	Env        Env
	Now        func() time.Time
	Interval   time.Duration
	StallAfter time.Duration
	NudgeEvery time.Duration
	Rules      string // the repo's agent rules, put in every prompt
	DoneWhen   string // e.g. the check command that must pass
}

func (d *Driver) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func orDefault(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// Report is what one tick did.
type Report struct {
	Decision string
	Resumed  []string
	Landed   int
	Spawned  []Spawned
	Skipped  map[int]string // why each ready issue didn't start
	Sleeping bool
	Nudged   string // the nudge sent, if any
	Stopped  string // why the run ended this tick
	Dropped  bool   // the owner changed the controls mid-tick; this tick's state wasn't saved
}

// Options are what `autopilot on` sets.
type Options struct {
	Stop       Stop
	ReadyLabel string
}

// Status reads the state file.
func (d *Driver) Status() (State, error) { return Load(d.Path) }

// Enable starts a run. It resets the previous run's record.
func (d *Driver) Enable(o Options) (State, error) {
	old, err := Load(d.Path)
	if err != nil {
		return old, err
	}
	st := State{Gen: old.Gen + 1, On: true, Stop: o.Stop, ReadyLabel: o.ReadyLabel, Started: d.now()}
	if err := st.Save(d.Path); err != nil {
		return st, err
	}
	d.Env.Event(EventOn, fmt.Sprintf("ready label %s, %s", st.Label(), st.Stop))
	return st, nil
}

// Disable ends the run with a summary. The owner asked, so nobody is interrupted.
func (d *Driver) Disable() (State, error) {
	st, err := Load(d.Path)
	if err != nil || !st.On {
		return st, err
	}
	st.Gen++
	d.finish(&st, "turned off", d.now(), false)
	return st, st.Save(d.Path)
}

// Pause holds the driver: ticks do nothing until Resume. Running work goes on.
func (d *Driver) Pause() (State, error) { return d.setPaused(true) }

// Resume undoes Pause.
func (d *Driver) Resume() (State, error) { return d.setPaused(false) }

func (d *Driver) setPaused(p bool) (State, error) {
	st, err := Load(d.Path)
	if err != nil {
		return st, err
	}
	if !st.On {
		return st, errors.New("autopilot is off; `saddle autopilot on` starts it")
	}
	st.Gen++
	st.Paused = p
	kind := EventResume
	if p {
		kind = EventPause
	}
	d.Env.Event(kind, "")
	return st, st.Save(d.Path)
}

// Run ticks now and every Interval until ctx ends. Failed ticks are
// events, once per distinct error.
func (d *Driver) Run(ctx context.Context) error {
	t := time.NewTicker(orDefault(d.Interval, DefaultInterval))
	defer t.Stop()
	last := ""
	for {
		if _, err := d.Tick(); err != nil {
			if msg := err.Error(); msg != last {
				last = msg
				d.Env.Event(EventError, msg)
			}
		} else {
			last = ""
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// occupant is a task holding claims the next spawn must stay clear of.
type occupant struct {
	id     string
	claims []string // nil: everything
}

func (o occupant) overlap(want []string) string {
	if o.claims == nil {
		return o.id + " declares no claims, so it runs alone"
	}
	if want == nil {
		return "it declares no claims, so it runs alone once nothing else runs (" + o.id + " does)"
	}
	for _, a := range want {
		for _, b := range o.claims {
			if claims.Overlap(a, b) {
				return fmt.Sprintf("its claim %s overlaps %s's %s", a, o.id, b)
			}
		}
	}
	return ""
}

// Tick runs one step: reconcile, land, check the stop condition and usage,
// top up to the cap, and nudge the orchestrator on a stall.
func (d *Driver) Tick() (Report, error) {
	rep := Report{Skipped: map[int]string{}}
	st, err := Load(d.Path)
	if err != nil {
		return rep, err
	}
	if !st.On {
		rep.Decision = "off"
		return rep, nil
	}
	gen, now, env := st.Gen, d.now(), d.Env
	st.LastTick = now
	if st.Paused {
		rep.Decision = "paused by the owner"
		return rep, d.save(&st, gen, &rep)
	}

	if res, err := env.Reconcile(); err != nil {
		env.Event(EventError, "reconcile: "+err.Error())
	} else {
		rep.Resumed = res
	}
	if n, err := env.Land(); err != nil {
		env.Event(EventError, "land: "+err.Error())
	} else {
		rep.Landed = n
	}
	tasks, err := env.Tasks()
	if err != nil {
		return rep, err
	}
	running, limit, err := env.Capacity()
	if err != nil {
		return rep, err
	}
	use, err := env.Usage(now)
	if err != nil {
		env.Event(EventError, "usage: "+err.Error())
	}
	issues, err := env.Ready(st.Label())
	if err != nil {
		rep.Decision = "can't read the ready queue: " + err.Error()
		_ = d.save(&st, gen, &rep)
		return rep, err
	}
	slices.SortStableFunc(issues, func(a, b Issue) int { return a.Number - b.Number })

	if st.Draining == "" {
		st.Draining = stopReason(st, now, use)
	}
	mine := map[string]bool{}
	for _, s := range st.Spawned {
		mine[s.Task] = true
	}
	byIssue := map[int]Task{}
	var occupied []occupant
	busy := false // a task of this run still runs or waits to land
	for _, t := range tasks {
		if t.Issue > 0 {
			byIssue[t.Issue] = t
		}
		if t.Live || t.Queued {
			occupied = append(occupied, occupant{t.ID, t.Claims})
			busy = busy || mine[t.ID]
		}
	}

	sleeping := d.sleep(&st, now, use)
	inQueue := map[int]bool{}
	for _, is := range issues {
		inQueue[is.Number] = true
	}
	slots := limit - running
	ready := 0      // issues that could start but for claims, deps, cap, sleep or a drain
	exclusive := "" // a claimless task picked this tick
	var whyStuck string
	for _, is := range issues {
		why, after := d.blocked(is, byIssue, inQueue)
		if why == "" && exclusive != "" {
			why = exclusive + " declares no claims, so it runs alone"
		}
		want := ParseClaims(is.Body)
		if why == "" {
			for _, o := range occupied {
				if why = o.overlap(want); why != "" {
					break
				}
			}
			if why == "" && want == nil && len(occupied) > 0 {
				why = "it declares no claims, so it runs alone once nothing else runs"
			}
		}
		if is.HasPR || byIssue[is.Number].ID != "" {
			rep.Skipped[is.Number] = why
			continue
		}
		ready++
		switch {
		case why != "":
		case st.Draining != "":
			why = "draining: " + st.Draining
		case sleeping:
			why = "sleeping until " + clock(st.SleepUntil)
		case slots <= 0:
			why = fmt.Sprintf("at the cap (%d running)", limit)
		}
		if why != "" {
			rep.Skipped[is.Number] = why
			if whyStuck == "" {
				whyStuck = fmt.Sprintf("#%d waits: %s", is.Number, why)
			}
			continue
		}
		req := SpawnReq{Title: is.Title, Prompt: Prompt(is, d.Rules, d.DoneWhen), Model: ModelScope(is.Body),
			Issue: is.Number, Claims: want, After: after}
		id, err := env.Spawn(req)
		switch {
		case errors.Is(err, ErrPaused):
			st.SleepUntil = resetOr(use.ResetsAt, now)
			sleeping = true
			env.Event(EventSleep, "spawn refused: "+err.Error()+"; sleeping until "+clock(st.SleepUntil))
			rep.Skipped[is.Number] = err.Error()
			continue
		case errors.Is(err, ErrAtCap):
			slots = 0
			rep.Skipped[is.Number] = err.Error()
			continue
		case err != nil:
			rep.Skipped[is.Number] = err.Error()
			env.Event(EventSkip, fmt.Sprintf("#%d: %v", is.Number, err))
			continue
		}
		ready--
		sp := Spawned{Issue: is.Number, Task: id, At: now}
		st.Spawned = append(st.Spawned, sp)
		rep.Spawned = append(rep.Spawned, sp)
		mine[id], busy = true, true
		occupied = append(occupied, occupant{id, want})
		if want == nil {
			exclusive = id
		}
		slots--
		env.Event(EventSpawn, fmt.Sprintf("%s for #%d %q (%s, claims %s)", id, is.Number, is.Title, req.Model, orNone(want)))
		if st.Draining == "" {
			st.Draining = stopReason(st, now, use)
		}
	}
	running += len(rep.Spawned)

	switch {
	case st.Draining != "" && !busy:
		d.finish(&st, st.Draining, now, true)
	case st.Draining == "" && ready == 0 && !busy:
		d.finish(&st, "queue empty", now, true)
	}
	if !st.On {
		rep.Stopped, rep.Decision = st.Stopped, "stopped: "+st.Stopped
		return rep, d.save(&st, gen, &rep)
	}

	rep.Sleeping = sleeping
	digest := fmt.Sprintf("%d/%d running, %d ready, %d spawned this run", running, limit, ready, len(st.Spawned))
	switch {
	case st.Draining != "":
		rep.Decision = "draining (" + st.Draining + "): " + digest
	case sleeping:
		rep.Decision = "sleeping until " + clock(st.SleepUntil) + " for plan limits: " + digest
	case len(rep.Spawned) > 0:
		var ids []string
		for _, s := range rep.Spawned {
			ids = append(ids, fmt.Sprintf("%s (#%d)", s.Task, s.Issue))
		}
		rep.Decision = "spawned " + strings.Join(ids, ", ") + ": " + digest
	default:
		rep.Decision = digest
	}

	stalled := !sleeping && st.Draining == "" && ready > 0 && len(rep.Spawned) == 0 && running < limit
	if !stalled {
		st.StallSince = time.Time{}
	} else if st.StallSince.IsZero() {
		st.StallSince = now
	}
	reason, why := "", ""
	switch {
	case stalled && now.Sub(st.StallSince) >= orDefault(d.StallAfter, DefaultStallAfter):
		reason, why = "stalled", fmt.Sprintf("%d ready but none can start (%s)", ready, whyStuck)
	default:
		if since, ok := env.Backlog(); ok && now.Sub(since) >= orDefault(d.StallAfter, DefaultStallAfter) {
			reason, why = "silent", "action notices waiting "+now.Sub(since).Round(time.Second).String()+" for you"
		}
	}
	if reason == "" {
		st.Nudged = ""
	} else if reason != st.Nudged || now.Sub(st.NudgedAt) >= orDefault(d.NudgeEvery, DefaultNudgeEvery) {
		text := "[saddle autopilot] continue: " + why + ". State: " + digest +
			". Don't end your turn to wait: check status, land, review or sequence the blocked work, pick other work; never ask the owner whether to continue."
		sent, err := env.Nudge(text)
		switch {
		case err != nil:
			env.Event(EventError, "nudge: "+err.Error())
		case sent:
			st.Nudged, st.NudgedAt, rep.Nudged = reason, now, text
			env.Event(EventNudge, why)
		default:
			rep.Decision += "; nudge waits for an idle orchestrator"
		}
	}
	return rep, d.save(&st, gen, &rep)
}

// blocked says why is can't start yet for reasons of its own: an open PR,
// a task already on it, or an after: issue not done. It returns the tasks
// it builds on.
func (d *Driver) blocked(is Issue, byIssue map[int]Task, inQueue map[int]bool) (string, []string) {
	if is.HasPR {
		return "an open PR already closes it", nil
	}
	if t, ok := byIssue[is.Number]; ok {
		return "task " + t.ID + " already works on it", nil
	}
	var after []string
	for _, dep := range ParseAfter(is.Body) {
		t, ok := byIssue[dep]
		switch {
		case ok && t.Live:
			return fmt.Sprintf("after #%d, which %s still works on", dep, t.ID), nil
		case ok:
			after = append(after, t.ID)
		case inQueue[dep]:
			return fmt.Sprintf("after #%d, which hasn't started", dep), nil
		}
	}
	return "", after
}

// sleep decides whether a usage pause holds spawns this tick.
func (d *Driver) sleep(st *State, now time.Time, use Usage) bool {
	switch {
	case now.Before(st.SleepUntil):
		return true
	case use.Pause:
		st.SleepUntil = resetOr(use.ResetsAt, now)
		msg := fmt.Sprintf("plan limit at %.0f%%: no new tasks until %s; running work goes on", use.Percent*100, clock(st.SleepUntil))
		d.Env.Event(EventSleep, msg)
		d.Env.Notify(false, "Autopilot is sleeping: "+msg+".")
		return true
	case !st.SleepUntil.IsZero():
		st.SleepUntil = time.Time{}
		d.Env.Event(EventWake, "plan limit reset; topping up again")
	}
	return false
}

func resetOr(at, now time.Time) time.Time {
	if at.After(now) {
		return at
	}
	return now.Add(DefaultPauseRetry)
}

// stopReason is why the run should stop spawning now, or "".
func stopReason(st State, now time.Time, use Usage) string {
	s := st.Stop
	switch {
	case !s.Until.IsZero() && !now.Before(s.Until):
		return "time reached (" + clock(s.Until) + ")"
	case s.UntilUsage > 0 && use.Percent >= s.UntilUsage:
		return fmt.Sprintf("usage at %.0f%% (stop at %.0f%%)", use.Percent*100, s.UntilUsage*100)
	case s.MaxTasks > 0 && len(st.Spawned) >= s.MaxTasks:
		return fmt.Sprintf("max tasks reached (%d)", s.MaxTasks)
	}
	return ""
}

// finish ends the run and writes its summary; notify interrupts the
// orchestrator with it.
func (d *Driver) finish(st *State, reason string, now time.Time, notify bool) {
	st.On, st.Paused, st.Stopped, st.Draining = false, false, reason, ""
	st.SleepUntil, st.StallSince = time.Time{}, time.Time{}
	var ids []string
	for _, s := range st.Spawned {
		ids = append(ids, fmt.Sprintf("%s (#%d)", s.Task, s.Issue))
	}
	sum := fmt.Sprintf("autopilot stopped: %s. Ran %s to %s and spawned %d %s",
		reason, clock(st.Started), clock(now), len(ids), plural(len(ids), "task"))
	if len(ids) > 0 {
		sum += ": " + strings.Join(ids, ", ")
	}
	st.Summary = sum + "."
	d.Env.Event(EventStopped, st.Summary)
	if notify {
		d.Env.Notify(true, st.Summary)
	}
}

// save writes st unless the owner changed the controls since the tick read them.
func (d *Driver) save(st *State, gen int, rep *Report) error {
	cur, err := Load(d.Path)
	if err != nil {
		return err
	}
	if cur.Gen != gen {
		rep.Dropped = true
		return nil
	}
	if rep.Decision != st.LastDecision {
		d.Env.Event(EventTick, rep.Decision)
	}
	st.LastDecision = rep.Decision
	return st.Save(d.Path)
}

func clock(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	return t.Local().Format("15:04")
}

func plural(n int, noun string) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

func orNone(c []string) string {
	if c == nil {
		return "none (runs alone)"
	}
	return strings.Join(c, " ")
}
