package remote

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/autopilot"
	"github.com/brandonapol/saddle/internal/config"
)

// pauseActor is a fakeActor that is also a Pauser, recording stops and
// restores.
type pauseActor struct {
	*fakeActor
	root     string
	pmu      sync.Mutex
	stops    int
	restored []PausedRepo
}

func (p *pauseActor) StopAuto() (PausedRepo, error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	p.stops++
	return PausedRepo{Root: p.root, AutoMerge: true}, nil
}

func (p *pauseActor) RestoreAuto(r PausedRepo) error {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	p.restored = append(p.restored, r)
	return nil
}

func callPause(t *testing.T, cs *mcp.ClientSession) PauseOut {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: PauseTool, Arguments: map[string]any{"reason": "phone lost"}})
	if callErr(res, err) {
		t.Fatalf("pause: %v %s", err, resText(res))
	}
	var out PauseOut
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("pause result %s: %v", b, err)
	}
	return out
}

// TestPauseToolNeedsActOnly: pause is reachable with act alone and runs at
// once, with no confirm round trip; a read token can't see or call it.
func TestPauseToolNeedsActOnly(t *testing.T) {
	f := &pauseActor{fakeActor: newFakeActor(), root: "/src/a"}
	r := newRigSrc(t, f, Options{})
	reader, err := r.connect(t, r.token(t, "ro", ScopeRead))
	if err != nil {
		t.Fatal(err)
	}
	res, err := reader.CallTool(context.Background(), &mcp.CallToolParams{Name: PauseTool})
	if !callErr(res, err) {
		t.Fatal("a read token paused remote control")
	}
	act, err := r.connect(t, r.token(t, "laptop", ScopeAct))
	if err != nil {
		t.Fatal(err)
	}
	lt, err := act.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(lt.Tools, func(tl *mcp.Tool) bool { return tl.Name == PauseTool }) {
		t.Fatal("an act token doesn't see pause")
	}
	out := callPause(t, act)
	if out.Paused.By != "remote:laptop" || out.Paused.Reason != "phone lost" {
		t.Fatalf("pause = %+v", out)
	}
	if f.stops != 1 {
		t.Fatalf("pause stopped auto-merge and autopilot %d times, want 1", f.stops)
	}
	p, err := LoadPanic(r.dir)
	if err != nil || p == nil || len(p.Repos) != 1 || !p.Repos[0].AutoMerge {
		t.Fatalf("pause file = %+v, %v", p, err)
	}
	if !slices.Contains(auditTrail(r), PauseTool+":"+DecisionAllowed) {
		t.Fatalf("pause isn't audited: %v", auditTrail(r))
	}
}

// TestPauseRefusesActionsKeepsReads: while paused, every state-changing
// call is refused, including confirming a call held before the pause;
// status, needs_you and peek keep working, and status shows the pause.
func TestPauseRefusesActionsKeepsReads(t *testing.T) {
	f := &pauseActor{fakeActor: newFakeActor(), root: "/src/a"}
	r := newRigSrc(t, f, Options{})
	cs, err := r.connect(t, r.token(t, "owner", ScopeAdmin))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	held, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "queue_hold", Arguments: map[string]any{"task": "t4", "if_version": f.version(ObjQueue)}})
	if callErr(held, err) {
		t.Fatal(err)
	}
	code := confirmCode(t, held)

	callPause(t, cs)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: ConfirmTool, Arguments: map[string]any{"code": code}})
	if !callErr(res, err) || !strings.Contains(errText(res, err), "paused") {
		t.Fatalf("confirm ran while paused: %v %s", err, resText(res))
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "queue_release", Arguments: map[string]any{"task": "t4", "if_version": f.version(ObjQueue)}})
	if !callErr(res, err) || !strings.Contains(errText(res, err), "saddle remote resume") {
		t.Fatalf("queue_release was held instead of refused while paused: %v %s", err, resText(res))
	}
	if got := f.got(); len(got) != 0 {
		t.Fatalf("calls reached the repo while paused: %v", got)
	}
	// Pausing again is harmless.
	callPause(t, cs)

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "status"})
	if callErr(res, err) {
		t.Fatalf("status while paused: %v %s", err, resText(res))
	}
	var snap Snapshot
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &snap); err != nil || snap.Paused == nil || snap.Paused.By != "remote:owner" {
		t.Fatalf("status doesn't show the pause: %s %v", b, err)
	}
	for tool, args := range map[string]map[string]any{"needs_you": {}, "peek": {"task": "t3"}} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if callErr(res, err) {
			t.Fatalf("%s while paused: %v %s", tool, err, resText(res))
		}
	}

	if _, err := Resume(r.dir, func(string) (Pauser, func(), error) { return f, func() {}, nil }); err != nil {
		t.Fatal(err)
	}
	if len(f.restored) != 1 {
		t.Fatalf("resume restored %v", f.restored)
	}
	actOut(t)(confirmed(t, cs, "queue_hold", map[string]any{"task": "t4", "if_version": f.version(ObjQueue)}))
}

func errText(res *mcp.CallToolResult, err error) string {
	if err != nil {
		return err.Error()
	}
	return resText(res)
}

// TestNoRemoteResume: only the owner, on the host, can resume.
func TestNoRemoteResume(t *testing.T) {
	for name := range ToolScopes {
		if strings.Contains(name, "resume") || strings.Contains(name, "unpause") {
			t.Fatalf("%q would let a remote caller lift the pause", name)
		}
	}
	if ToolScopes[PauseTool] != ScopeAct || NeedsConfirm(PauseTool) || !RemoteAllows([]Scope{ScopeRead, ScopeAct}, PauseTool) {
		t.Fatal("pause must be act, without admin or confirm")
	}
}

// TestPausePausesEveryRepo: a pause on one repo's route refuses actions on
// every repo the listener serves and stops each one's automation.
func TestPausePausesEveryRepo(t *testing.T) {
	a := &pauseActor{fakeActor: newFakeActor(), root: "/src/a"}
	b := &pauseActor{fakeActor: newFakeActor(), root: "/src/b"}
	m := newMultiRig(t, ServedRepo{Name: "a", Root: "/src/a", Src: a}, ServedRepo{Name: "b", Root: "/src/b", Src: b})
	tok := m.token(t, "owner", ScopeAdmin)
	ca, err := m.connectPath(t, MCPPath+"/a", tok)
	if err != nil {
		t.Fatal(err)
	}
	callPause(t, ca)
	if a.stops != 1 || b.stops != 1 {
		t.Fatalf("stops a=%d b=%d, want both stopped", a.stops, b.stops)
	}
	cb, err := m.connectPath(t, MCPPath+"/b", tok)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cb.CallTool(context.Background(), &mcp.CallToolParams{Name: "queue_hold", Arguments: map[string]any{"task": "t4", "if_version": b.version(ObjQueue)}})
	if !callErr(res, err) {
		t.Fatal("repo b accepted an action while paused from repo a")
	}
}

// TestUnreadablePauseFileFailsClosed: a pause file that can't be parsed
// still refuses actions, and resume still removes it.
func TestUnreadablePauseFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, panicFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p := pausedState(dir); p == nil {
		t.Fatal("an unreadable pause file didn't pause")
	}
	if _, err := Resume(dir, nil); err == nil {
		t.Fatal("resume didn't report the unreadable file")
	}
	if p := pausedState(dir); p != nil {
		t.Fatalf("still paused after resume: %+v", p)
	}
}

// TestResumeUnblocksWhenRestoreFails: resume removes the pause even when a
// repo can't be opened, and says which.
func TestResumeUnblocksWhenRestoreFails(t *testing.T) {
	dir := t.TempDir()
	f := &pauseActor{fakeActor: newFakeActor(), root: "/src/gone"}
	if _, err := Pause(dir, "local", "", time.Now(), []Pauser{f}); err != nil {
		t.Fatal(err)
	}
	_, err := Resume(dir, func(string) (Pauser, func(), error) { return nil, nil, errors.New("no such repo") })
	if err == nil || !strings.Contains(err.Error(), "/src/gone") {
		t.Fatalf("resume error = %v", err)
	}
	if p, _ := LoadPanic(dir); p != nil {
		t.Fatal("pause file survived resume")
	}
	if p, err := Resume(dir, nil); p != nil || err != nil {
		t.Fatalf("resume when not paused = %+v, %v", p, err)
	}
}

// TestAppSourcePauseStopsAndRestoresAutomation: on a real repo, pause
// turns auto-merge off and pauses autopilot, and resume restarts both.
func TestAppSourcePauseStopsAndRestoresAutomation(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".saddle"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app.App{Root: root, Cfg: config.Default(), Store: pushStore(t)}
	if err := a.NewAutomerge(nil).SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.NewAutopilot(nil).Enable(autopilot.Options{}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p, err := Pause(dir, "local", "", time.Now(), []Pauser{AppSource{A: a}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Repos) != 1 || !p.Repos[0].AutoMerge || !p.Repos[0].Autopilot {
		t.Fatalf("pause recorded %+v", p.Repos)
	}
	if m, _ := a.AutomergeState(); m.Enabled {
		t.Fatal("auto-merge still on while paused")
	}
	if ap, _ := a.AutopilotState(); !ap.Paused {
		t.Fatal("autopilot not paused")
	}
	if _, err := Resume(dir, func(string) (Pauser, func(), error) { return AppSource{A: a}, func() {}, nil }); err != nil {
		t.Fatal(err)
	}
	if m, _ := a.AutomergeState(); !m.Enabled {
		t.Fatal("resume didn't turn auto-merge back on")
	}
	if ap, _ := a.AutopilotState(); ap.Paused || !ap.On {
		t.Fatalf("resume didn't resume autopilot: %+v", ap)
	}
}

// TestPauseLeavesOffAutomationOff: what wasn't running isn't started by
// resume.
func TestPauseLeavesOffAutomationOff(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".saddle"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app.App{Root: root, Cfg: config.Default(), Store: pushStore(t)}
	dir := t.TempDir()
	p, err := Pause(dir, "local", "", time.Now(), []Pauser{AppSource{A: a}})
	if err != nil || p.Repos[0].AutoMerge || p.Repos[0].Autopilot {
		t.Fatalf("pause = %+v, %v", p, err)
	}
	if _, err := Resume(dir, func(string) (Pauser, func(), error) { return AppSource{A: a}, func() {}, nil }); err != nil {
		t.Fatal(err)
	}
	if m, _ := a.AutomergeState(); m.Enabled {
		t.Fatal("resume turned on auto-merge that was off")
	}
	if ap, _ := a.AutopilotState(); ap.On {
		t.Fatal("resume started autopilot")
	}
}
