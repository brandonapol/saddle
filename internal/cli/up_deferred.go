package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/doctor"
	"github.com/brandonapol/saddle/internal/store"
)

// Kinds of what saddle up records about the checks it runs after its first
// frame (#635).
const (
	decisionDoctorFailed  = "doctor-failed"
	decisionMergeSettings = "merge-settings"
	eventDoctorTimeout    = "doctor_timeout"
	eventDoctorWarnings   = "doctor_warnings"
)

// upDeferredTimeout bounds each check saddle up runs after its first frame.
// Nothing waits on them, so it only stops a hung gh call from being
// reported forever.
const upDeferredTimeout = 2 * time.Minute

// upDeferred is the startup work saddle up runs once its first frame is on
// screen (#635). None of it is needed to draw the TUI, and under load it
// took tens of seconds: the doctor's GitHub calls, push dry-run and
// leftovers scan, the merge-settings check and the base fetch. The sweep of
// stale gate dirs runs here too. Nothing is
// dropped: what used to stop up or print a warning opens a card or a
// notice instead.
type upDeferred struct {
	a          *app.App
	trace      *startupTrace
	skipDoctor bool
	timeout    time.Duration
	doctor     func() []doctor.Result // the full doctor
	merge      func() error           // the merge-settings refusal; the doctor covers it
	baseBehind func() string          // fetches base, notes a stale integration; "" when the checkout is current
	// gates sweeps stale gate-* dirs and what still runs under them (#663);
	// nil is App.SweepStaleGates.
	gates func() app.GateSweep
}

// run starts the work and returns at once. wait blocks until each piece
// finished, timed out or ctx ended.
func (d upDeferred) run(ctx context.Context) (wait func()) {
	var wg sync.WaitGroup
	if d.skipDoctor {
		wg.Go(func() {
			err, ok := within(ctx, d.timeout, d.trace.Span("merge settings"), func() error { return d.merge() })
			if ok && err != nil {
				d.open(mergeSettingsCard(err))
			}
		})
	} else {
		wg.Go(func() {
			rs, ok := within(ctx, d.timeout, d.trace.Span("doctor (full)"), d.doctor)
			if !ok {
				if ctx.Err() == nil {
					d.a.Store.Event(app.OrchestratorID, eventDoctorTimeout, "saddle up's doctor checks timed out after "+d.timeout.String()+"; run saddle doctor")
				}
				return
			}
			d.report(rs)
		})
	}
	gates := d.gates
	if gates == nil {
		gates = func() app.GateSweep { return d.a.SweepStaleGates(app.GateSweepAge) }
	}
	wg.Go(func() { within(ctx, d.timeout, d.trace.Span("stale gate sweep"), gates) })
	wg.Go(func() {
		within(ctx, d.timeout, d.trace.Span("stale layout sweep"), func() app.LayoutSweep { return d.a.SweepStaleLayouts(app.GateSweepAge) })
	})
	wg.Go(func() {
		warn, ok := within(ctx, d.timeout, d.trace.Span("base fetch"), d.baseBehind)
		if ok && warn != "" {
			_ = d.a.Notify(app.OrchestratorID, store.NoticeInfo, "warning: "+warn)
		}
	})
	return wg.Wait
}

// report opens one card for failing checks and records warnings as an event.
func (d upDeferred) report(rs []doctor.Result) {
	if c, ok := doctorFailedCard(rs); ok {
		d.open(c)
	}
	warns := 0
	for _, r := range rs {
		if r.Status == doctor.Warn {
			warns++
		}
	}
	if warns > 0 {
		d.a.Store.Event(app.OrchestratorID, eventDoctorWarnings, fmt.Sprintf("doctor: %d warning(s) (run saddle doctor)", warns))
	}
}

func (d upDeferred) open(c app.DecisionCard) {
	if err := d.a.OpenDecision(c); err != nil {
		d.a.Store.Event(app.OrchestratorID, "doctor_error", err.Error())
	}
}

// within runs f, ending span when it returns. ok is false when timeout or
// ctx ran out first; f keeps running and its result is dropped.
func within[T any](ctx context.Context, timeout time.Duration, span func(), f func() T) (v T, ok bool) {
	got := make(chan T, 1)
	go func() {
		v := f()
		span()
		got <- v
	}()
	select {
	case v = <-got:
		return v, true
	case <-time.After(timeout):
	case <-ctx.Done():
	}
	return v, false
}

// doctorFailedCard is the card for the checks that would have stopped saddle
// up before #635, or false when none fails. Its ID hashes the failures, so a
// second saddle up with the same ones doesn't send it again.
func doctorFailedCard(rs []doctor.Result) (app.DecisionCard, bool) {
	var names, what []string
	fix := ""
	h := sha256.New()
	for _, r := range rs {
		if r.Status != doctor.Fail {
			continue
		}
		names = append(names, r.Name)
		what = append(what, r.Name+": "+r.Detail+" (fix: "+oneLineFix(r.Fix)+")")
		if fix == "" {
			fix = oneLineFix(r.Fix)
		}
		fmt.Fprintln(h, r.Name, r.Detail)
	}
	if len(names) == 0 {
		return app.DecisionCard{}, false
	}
	return startupCard(decisionDoctorFailed, h.Sum(nil),
		fmt.Sprintf("saddle doctor: %d check(s) failing: %s.", len(names), strings.Join(names, ", ")),
		strings.Join(what, "; ")+".", fix, "saddle doctor"), true
}

// mergeSettingsCard is the card for the merge-settings refusal saddle up
// gave before its first frame until #635. land and prs still refuse.
func mergeSettingsCard(err error) app.DecisionCard {
	h := sha256.Sum256([]byte(err.Error()))
	return startupCard(decisionMergeSettings, h[:],
		"The repo's merge settings break stacked PRs; saddle land and prs refuse until they change.",
		oneLineFix(err.Error()), "fix the merge settings named above on GitHub", "saddle doctor")
}

func startupCard(kind string, sum []byte, headline, what, fix, check string) app.DecisionCard {
	return app.DecisionCard{
		ID:       kind + ":" + hex.EncodeToString(sum)[:12],
		Kind:     kind,
		To:       app.OrchestratorID,
		Headline: headline,
		What:     what,
		Blocking: "saddle up started anyway; whatever needs the failing check fails on its own until it is fixed.",
		Options: []app.DecisionOption{
			{Label: "Fix it now", Cost: "you do it, a few minutes", Action: fix, Recommended: true},
			{Label: "Carry on", Cost: "saddle keeps running; the check stays red", Action: check},
		},
		Default: 2,
		Timeout: app.DefaultDecisionTimeout,
	}
}

// lazyStart starts something at most once, and never after Stop: saddle up
// starts its watchers on the first frame, which may never come.
type lazyStart struct {
	mu      sync.Mutex
	stop    func()
	stopped bool
}

// Start runs start unless it already ran or Stop was called.
func (l *lazyStart) Start(start func() (stop func())) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped || l.stop != nil {
		return
	}
	l.stop = start()
}

// Stop stops what Start started, if anything, and waits for it.
func (l *lazyStart) Stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stopped = true
	if l.stop != nil {
		l.stop()
	}
}
