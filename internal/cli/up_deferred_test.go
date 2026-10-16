package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/doctor"
)

func deferredFor(a *app.App) upDeferred {
	return upDeferred{
		a:          a,
		timeout:    5 * time.Second,
		doctor:     fixed(doctor.Result{Name: doctor.CheckTmux, Status: doctor.OK}),
		merge:      func() error { return nil },
		baseBehind: func() string { return "" },
		// The real sweep would reach the machine's scratch dir.
		gates: func() app.GateSweep { return app.GateSweep{} },
	}
}

// #663: saddle up sweeps stale gate dirs after its first frame, without
// waiting for it.
func TestUpDeferredSweepsStaleGates(t *testing.T) {
	a := predictApp(t)
	d := deferredFor(a)
	release, swept := make(chan struct{}), make(chan struct{})
	d.gates = func() app.GateSweep {
		<-release
		close(swept)
		return app.GateSweep{}
	}
	start := time.Now()
	wait := d.run(t.Context())
	if took := time.Since(start); took > time.Second {
		t.Fatalf("run blocked for %s", took)
	}
	close(release)
	wait()
	select {
	case <-swept:
	default:
		t.Fatal("saddle up didn't sweep stale gate dirs")
	}
}

// #635: the slow doctor checks run after the first frame. They never block
// it, and a failing one opens a card instead of being dropped.
func TestUpDeferredDoctorFailureOpensACardWithoutBlocking(t *testing.T) {
	a := predictApp(t)
	d := deferredFor(a)
	release := make(chan struct{})
	d.doctor = func() []doctor.Result {
		<-release
		return []doctor.Result{
			{Name: doctor.CheckGH, Status: doctor.Fail, Detail: "not logged in", Fix: "gh auth login"},
			{Name: doctor.CheckPush, Status: doctor.Warn},
		}
	}
	start := time.Now()
	wait := d.run(t.Context())
	if took := time.Since(start); took > time.Second {
		t.Fatalf("run blocked for %s", took)
	}
	close(release)
	wait()
	cs, err := a.OpenDecisions()
	if err != nil || len(cs) != 1 {
		t.Fatalf("cards = %+v, %v", cs, err)
	}
	c := cs[0]
	if c.Kind != decisionDoctorFailed || !strings.Contains(c.Headline, doctor.CheckGH) || !strings.Contains(c.What, "gh auth login") {
		t.Fatalf("card = %+v", c)
	}
}

func TestUpDeferredHealthyDoctorOpensNoCard(t *testing.T) {
	a := predictApp(t)
	d := deferredFor(a)
	d.doctor = fixed(doctor.Result{Name: doctor.CheckPush, Status: doctor.Warn})
	d.run(t.Context())()
	if cs, _ := a.OpenDecisions(); len(cs) != 0 {
		t.Fatalf("cards = %+v", cs)
	}
}

// The doctor covers merge settings. With --skip-doctor, saddle up still
// checks them, after the first frame, and a refusal opens a card.
func TestUpDeferredMergeSettingsCheckedOnceAfterPaint(t *testing.T) {
	a := predictApp(t)
	d := deferredFor(a)
	calls := 0
	d.merge = func() error { calls++; return errors.New("merge commits are allowed") }
	d.run(t.Context())()
	if calls != 0 {
		t.Fatalf("merge settings checked %d times on top of the doctor", calls)
	}
	d.skipDoctor = true
	d.doctor = func() []doctor.Result { t.Error("--skip-doctor ran the doctor"); return nil }
	d.run(t.Context())()
	if calls != 1 {
		t.Fatalf("merge settings checked %d times with --skip-doctor", calls)
	}
	cs, _ := a.OpenDecisions()
	if len(cs) != 1 || !strings.Contains(cs[0].What, "merge commits are allowed") {
		t.Fatalf("cards = %+v", cs)
	}
}

// The base fetch moved after the first frame; its warning goes to the
// orchestrator through the notice policy, since stderr is under the TUI by
// then.
func TestUpDeferredBaseBehindNotifiesTheOrchestrator(t *testing.T) {
	a := predictApp(t)
	d := deferredFor(a)
	d.baseBehind = func() string { return "local main is 3 commit(s) behind origin/main" }
	d.run(t.Context())()
	ns, _ := a.Store.PeekNotices(app.OrchestratorID, false)
	for _, n := range ns {
		if strings.Contains(n.Text, "3 commit(s) behind") {
			return
		}
	}
	evs, _ := a.Store.Events(20)
	for _, e := range evs {
		if e.Task == app.OrchestratorID && strings.Contains(e.Data, "3 commit(s) behind") {
			return
		}
	}
	t.Fatalf("no notice: notices %+v, events %+v", ns, evs)
}

// A slow check that outlives its timeout is recorded, not waited on.
func TestUpDeferredDoctorTimeoutIsAnEvent(t *testing.T) {
	a := predictApp(t)
	d := deferredFor(a)
	d.timeout = 50 * time.Millisecond
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	d.doctor = func() []doctor.Result { <-hang; return nil }
	d.run(t.Context())()
	evs, _ := a.Store.Events(20)
	for _, e := range evs {
		if e.Kind == eventDoctorTimeout {
			return
		}
	}
	t.Fatalf("no %s event in %+v", eventDoctorTimeout, evs)
}

// Watchers start on the first frame. If saddle up quits first, they never
// start, and stop waits only for what did.
func TestLazyStartRunsOnceAndNotAfterStop(t *testing.T) {
	var l lazyStart
	starts, stops := 0, 0
	start := func() func() { starts++; return func() { stops++ } }
	l.Start(start)
	l.Start(start)
	l.Stop()
	l.Start(start)
	if starts != 1 || stops != 1 {
		t.Fatalf("starts=%d stops=%d", starts, stops)
	}
	var never lazyStart
	never.Stop()
	never.Start(start)
	if starts != 1 {
		t.Fatalf("started after stop")
	}
}
