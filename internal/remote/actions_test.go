package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
)

// fakeActor is a Source and an Actor whose state lives in memory. Call
// applies the orchestrator tool's effect to that state and records it, so
// a test sees exactly which calls reached the repo.
type fakeActor struct {
	liveSource
	amu     sync.Mutex
	st      State
	screens map[string]string
	calls   []string
}

func newFakeActor() *fakeActor {
	f := &fakeActor{screens: map[string]string{}}
	f.st = State{
		Queue:       []QueueEntry{{Task: "t4"}, {Task: "t5"}},
		Train:       map[string]string{"t1": "ok", "t4": "queued", "t5": "queued"},
		AutoMerge:   AutoMergeState{On: false},
		Concurrency: ConcurrencyState{Limit: 4},
		Stack:       StackState{Layers: []StackLayer{{Task: "t1", PR: "https://github.com/o/r/pull/11"}}},
	}
	f.items = []NeedsYouItem{{ID: "p-t3-aaaa", Task: "t3", Kind: KindPrompt, Prompt: "ask",
		Question: "Do you want to proceed?", Options: []Option{{N: 1, Label: "Yes"}, {N: 2, Label: "No"}}}}
	f.screens["t3"] = "Do you want to proceed?\n❯ 1. Yes\n  2. No\n"
	f.screens["t6"] = "╭──────╮\n│ > \x1b[0mhalf a sentence│\n╰──────╯\n"
	f.screens["t7"] = "╭──────╮\n│ > │\n╰──────╯\n"
	return f
}

func (f *fakeActor) State() (State, error) {
	f.amu.Lock()
	defer f.amu.Unlock()
	st := f.st
	st.Queue = slices.Clone(f.st.Queue)
	st.AutoMerge.Holds = slices.Clone(f.st.AutoMerge.Holds)
	return st, nil
}

func (f *fakeActor) Screen(task string) (string, error) {
	f.amu.Lock()
	defer f.amu.Unlock()
	s, ok := f.screens[task]
	if !ok {
		return "", fmt.Errorf("%s has no live window", task)
	}
	return s, nil
}

func (f *fakeActor) Call(_ context.Context, tool string, args map[string]any) (string, error) {
	f.amu.Lock()
	defer f.amu.Unlock()
	b, _ := json.Marshal(args)
	f.calls = append(f.calls, tool+" "+string(b))
	task, _ := args["task"].(string)
	switch tool {
	case "queue_hold", "queue_release":
		for i := range f.st.Queue {
			if f.st.Queue[i].Task == task {
				f.st.Queue[i].Held = tool == "queue_hold"
			}
		}
	case "automerge":
		switch args["action"] {
		case "on", "off":
			f.st.AutoMerge.On = args["action"] == "on"
		case "hold":
			f.st.AutoMerge.Holds = append(f.st.AutoMerge.Holds, args["stack"].(string))
		case "release":
			f.st.AutoMerge.Holds = nil
		}
	case "concurrency":
		if args["reset"] == true {
			f.st.Concurrency = ConcurrencyState{Limit: 4}
		} else {
			f.st.Concurrency = ConcurrencyState{Limit: args["limit"].(int), Runtime: true}
		}
	case "send_keys":
		f.mu.Lock()
		f.items = nil // the prompt is answered
		f.mu.Unlock()
	}
	return "ok", nil
}

// set changes the state as another controller (the local TUI) would.
func (f *fakeActor) set(fn func(*State)) {
	f.amu.Lock()
	defer f.amu.Unlock()
	fn(&f.st)
}

func (f *fakeActor) got() []string {
	f.amu.Lock()
	defer f.amu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeActor) version(key string) string {
	st, _ := f.State()
	return st.Versions()[key]
}

// confirmed calls tool, checks it was held, then confirms it and returns
// the confirm call's result.
func confirmed(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()
	ctx := context.Background()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if callErr(res, err) {
		t.Fatalf("%s: %v %s", tool, err, resText(res))
	}
	code := confirmCode(t, res)
	return cs.CallTool(ctx, &mcp.CallToolParams{Name: ConfirmTool, Arguments: map[string]any{"code": code}})
}

func resText(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	return text(res)
}

// actOut(t)(call) checks call succeeded and decodes its ActOut.
func actOut(t *testing.T) func(*mcp.CallToolResult, error) ActOut {
	t.Helper()
	return func(res *mcp.CallToolResult, err error) ActOut {
		t.Helper()
		if callErr(res, err) {
			t.Fatalf("call failed: %v %s", err, resText(res))
		}
		var out ActOut
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("result %s: %v", b, err)
		}
		return out
	}
}

func adminSession(t *testing.T, f *fakeActor) (*rig, *mcp.ClientSession) {
	t.Helper()
	r := newRigSrc(t, f, Options{})
	cs, err := r.connect(t, r.token(t, "owner", ScopeAdmin))
	if err != nil {
		t.Fatal(err)
	}
	return r, cs
}

// TestStatusHasVersions: status carries a version per mutable object, and
// a version moves when its object does.
func TestStatusHasVersions(t *testing.T) {
	f := newFakeActor()
	r := newRigSrc(t, f, Options{})
	cs, err := r.connect(t, r.token(t, "phone", ScopeRead))
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := callJSON[Snapshot](t, cs, "status", nil)
	for _, k := range []string{"queue", "automerge", "concurrency", "stack"} {
		if snap.Versions[k] == "" {
			t.Errorf("status has no version for %s: %v", k, snap.Versions)
		}
	}
	f.set(func(s *State) { s.Queue[0].Held = true })
	snap2, _ := callJSON[Snapshot](t, cs, "status", nil)
	if snap2.Versions["queue"] == snap.Versions["queue"] {
		t.Fatal("queue version didn't move when the queue changed")
	}
	if snap2.Versions["automerge"] != snap.Versions["automerge"] {
		t.Fatal("automerge version moved though automerge didn't change")
	}
}

// TestActionsNeedAdminAndConfirm: every mutating tool is hidden from and
// refused to a token without admin, even one scoped act and land; with
// admin it runs nothing until confirmed, and every step is audited.
func TestActionsNeedAdminAndConfirm(t *testing.T) {
	f := newFakeActor()
	r := newRigSrc(t, f, Options{})
	cs, err := r.connect(t, r.token(t, "laptop", ScopeAct, ScopeLand))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range lt.Tools {
		if Mutating(tl.Name) {
			t.Errorf("act+land token sees mutating tool %q", tl.Name)
		}
	}
	for _, tool := range []string{"queue_hold", "land", "answer", "send_keys"} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{"task": "t4"}})
		if !callErr(res, err) {
			t.Fatalf("%s with an act+land token succeeded", tool)
		}
	}

	admin, err := r.connect(t, r.token(t, "owner", ScopeAdmin))
	if err != nil {
		t.Fatal(err)
	}
	lt, err = admin.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range lt.Tools {
		names = append(names, tl.Name)
	}
	for _, want := range []string{"answer", "send_keys", "message", "queue_move", "queue_hold", "queue_release", "automerge",
		"concurrency", "unstack", "requeue", "publish", "land", "prs", "restack", "sentinel_ack", "confirm"} {
		if !slices.Contains(names, want) {
			t.Errorf("admin token doesn't see %s: %v", want, names)
		}
	}
	res, err := admin.CallTool(ctx, &mcp.CallToolParams{Name: "queue_hold",
		Arguments: map[string]any{"task": "t4", "if_version": f.version("queue")}})
	if callErr(res, err) {
		t.Fatal(err)
	}
	if len(f.got()) != 0 {
		t.Fatalf("queue_hold ran before it was confirmed: %v", f.got())
	}
	code := confirmCode(t, res)
	out := actOut(t)(admin.CallTool(ctx, &mcp.CallToolParams{Name: ConfirmTool, Arguments: map[string]any{"code": code}}))
	if got := f.got(); len(got) != 1 || !strings.HasPrefix(got[0], "queue_hold ") || out.Noop {
		t.Fatalf("calls = %v, out = %+v", got, out)
	}
	if out.Version != f.version("queue") || out.Object != "queue" {
		t.Fatalf("result carries version %q of %q, want the new queue version %q", out.Version, out.Object, f.version("queue"))
	}
	trail := auditTrail(r)
	if !slices.Contains(trail, "queue_hold:"+DecisionPending) || !slices.Contains(trail, "queue_hold:"+DecisionAllowed) {
		t.Fatalf("audit = %v", trail)
	}
	for _, e := range r.events {
		if e.Tool == "queue_hold" && e.Decision == DecisionAllowed && !strings.Contains(e.Args, "t4") {
			t.Fatalf("audit entry doesn't record the arguments: %+v", e)
		}
	}
}

// TestStaleVersionIsRefused is the #209 race: an action names the version
// it saw, and fails with the current state when the object moved.
func TestStaleVersionIsRefused(t *testing.T) {
	f := newFakeActor()
	_, cs := adminSession(t, f)
	v := f.version("queue")
	f.set(func(s *State) { s.Queue = []QueueEntry{{Task: "t5"}, {Task: "t4"}} }) // the TUI reordered it
	res, err := confirmed(t, cs, "queue_hold", map[string]any{"task": "t4", "if_version": v})
	if !callErr(res, err) {
		t.Fatal("queue_hold with a stale version ran")
	}
	if msg := resText(res) + fmt.Sprint(err); !strings.Contains(msg, f.version("queue")) || !strings.Contains(msg, "t5") {
		t.Fatalf("refusal doesn't carry the current version and state: %s", msg)
	}
	if len(f.got()) != 0 {
		t.Fatalf("calls = %v", f.got())
	}
}

// TestStateMovedBeforeConfirm: the version is checked when the confirmed
// call runs, not only when it was held.
func TestStateMovedBeforeConfirm(t *testing.T) {
	f := newFakeActor()
	_, cs := adminSession(t, f)
	ctx := context.Background()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "automerge",
		Arguments: map[string]any{"action": "on", "if_version": f.version("automerge")}})
	if callErr(res, err) {
		t.Fatal(err)
	}
	code := confirmCode(t, res)
	f.set(func(s *State) { s.AutoMerge.Stopped = "merge failed" })
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: ConfirmTool, Arguments: map[string]any{"code": code}})
	if !callErr(res, err) || len(f.got()) != 0 {
		t.Fatalf("automerge on ran after its state moved: %v %v", f.got(), resText(res))
	}
}

func TestIfVersionIsRequired(t *testing.T) {
	f := newFakeActor()
	_, cs := adminSession(t, f)
	res, err := confirmed(t, cs, "concurrency", map[string]any{"limit": 6})
	if !callErr(res, err) || !strings.Contains(resText(res)+fmt.Sprint(err), "if_version") {
		t.Fatalf("concurrency without if_version: %v %s", err, resText(res))
	}
	if len(f.got()) != 0 {
		t.Fatalf("calls = %v", f.got())
	}
}

// TestIdempotentVerbs: asking for the state that already holds is a no-op
// success, not a toggle, even with a stale version, so a retry is safe.
func TestIdempotentVerbs(t *testing.T) {
	f := newFakeActor()
	f.set(func(s *State) {
		s.Queue[0].Held = true
		s.AutoMerge.On = true
		s.Concurrency = ConcurrencyState{Limit: 6, Runtime: true}
		s.Train["t2"] = "superseded"
	})
	_, cs := adminSession(t, f)
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"queue_hold", map[string]any{"task": "t4", "if_version": "stale"}},
		{"queue_release", map[string]any{"task": "t5", "if_version": "stale"}},
		{"queue_move", map[string]any{"task": "t5", "position": 2, "if_version": "stale"}},
		{"automerge", map[string]any{"action": "on", "if_version": "stale"}},
		{"automerge", map[string]any{"action": "release", "stack": "t1", "if_version": "stale"}},
		{"concurrency", map[string]any{"limit": 6, "if_version": "stale"}},
		{"unstack", map[string]any{"task": "t2", "if_version": "stale"}},
		{"requeue", map[string]any{"task": "t4", "if_version": "stale"}},
	} {
		out := actOut(t)(confirmed(t, cs, c.tool, c.args))
		if !out.Noop {
			t.Errorf("%s %v: not a no-op: %+v", c.tool, c.args, out)
		}
	}
	if len(f.got()) != 0 {
		t.Fatalf("no-ops reached the repo: %v", f.got())
	}
	// Holding twice is still one hold.
	f.set(func(s *State) { s.Queue[0].Held = false })
	v := f.version("queue")
	actOut(t)(confirmed(t, cs, "queue_hold", map[string]any{"task": "t4", "if_version": v}))
	out := actOut(t)(confirmed(t, cs, "queue_hold", map[string]any{"task": "t4", "if_version": v}))
	if !out.Noop || len(f.got()) != 1 {
		t.Fatalf("second hold: %+v, calls %v", out, f.got())
	}
}

// TestTwoControllersRace: two controllers act on the same version; the
// first wins and the second is refused instead of undoing it.
func TestTwoControllersRace(t *testing.T) {
	f := newFakeActor()
	r := newRigSrc(t, f, Options{})
	a, err := r.connect(t, r.token(t, "phone", ScopeAdmin))
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.connect(t, r.token(t, "laptop", ScopeAdmin))
	if err != nil {
		t.Fatal(err)
	}
	v := f.version("concurrency")
	actOut(t)(confirmed(t, a, "concurrency", map[string]any{"limit": 8, "if_version": v}))
	res, err := confirmed(t, b, "concurrency", map[string]any{"limit": 2, "if_version": v})
	if !callErr(res, err) {
		t.Fatal("the second controller overwrote the first")
	}
	if st, _ := f.State(); st.Concurrency.Limit != 8 {
		t.Fatalf("limit = %d, want 8", st.Concurrency.Limit)
	}
}

// TestActionsNeedExplicitTargets: no "current" anything.
func TestActionsNeedExplicitTargets(t *testing.T) {
	f := newFakeActor()
	_, cs := adminSession(t, f)
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"queue_hold", map[string]any{"if_version": f.version("queue")}},
		{"automerge", map[string]any{"action": "hold", "if_version": f.version("automerge")}},
		{"message", map[string]any{"message": "hi"}},
		{"publish", map[string]any{"if_version": f.version("stack")}},
		{"send_keys", map[string]any{"text": "yes"}},
	} {
		if res, err := confirmed(t, cs, c.tool, c.args); !callErr(res, err) {
			t.Errorf("%s without a target ran", c.tool)
		}
	}
	if len(f.got()) != 0 {
		t.Fatalf("calls = %v", f.got())
	}
}

// TestAnswerIsBoundToItem: answer presses an option only while the prompt
// the owner saw (its item id) still stands.
func TestAnswerIsBoundToItem(t *testing.T) {
	f := newFakeActor()
	_, cs := adminSession(t, f)
	for _, bad := range []map[string]any{
		{"item": "p-t3-bbbb", "option": 1}, // the prompt changed
		{"item": "p-t3-aaaa", "option": 3}, // not an option
	} {
		if res, err := confirmed(t, cs, "answer", bad); !callErr(res, err) {
			t.Fatalf("answer %v ran", bad)
		}
	}
	if len(f.got()) != 0 {
		t.Fatalf("calls = %v", f.got())
	}
	actOut(t)(confirmed(t, cs, "answer", map[string]any{"item": "p-t3-aaaa", "option": 2}))
	if got := f.got(); len(got) != 1 || got[0] != `send_keys {"keys":["2"],"task":"t3"}` {
		t.Fatalf("calls = %v", got)
	}
	// Answering again finds the item gone and presses nothing.
	res, err := confirmed(t, cs, "answer", map[string]any{"item": "p-t3-aaaa", "option": 2})
	if !callErr(res, err) || !strings.Contains(resText(res)+fmt.Sprint(err), "gone") || len(f.got()) != 1 {
		t.Fatalf("second answer: %v %s %v", err, resText(res), f.got())
	}
}

func TestAnswerRefusesNotices(t *testing.T) {
	f := newFakeActor()
	f.add(NeedsYouItem{ID: "n-9", Task: "t0", Kind: KindNotice, Text: "t4 failed tests"})
	_, cs := adminSession(t, f)
	if res, err := confirmed(t, cs, "answer", map[string]any{"item": "n-9", "option": 1}); !callErr(res, err) {
		t.Fatal("answered a notice")
	}
}

// TestSendKeysIsSafeSend: never into a pane with the owner's draft, and
// never at a prompt (answer binds to the item instead).
func TestSendKeysIsSafeSend(t *testing.T) {
	f := newFakeActor()
	_, cs := adminSession(t, f)
	for _, task := range []string{"t6", "t3"} {
		res, err := confirmed(t, cs, "send_keys", map[string]any{"task": task, "text": "go on"})
		if !callErr(res, err) {
			t.Fatalf("send_keys into %s ran", task)
		}
	}
	if len(f.got()) != 0 {
		t.Fatalf("calls = %v", f.got())
	}
	actOut(t)(confirmed(t, cs, "send_keys", map[string]any{"task": "t7", "text": "go on"}))
	if got := f.got(); len(got) != 1 || !strings.Contains(got[0], `"go on"`) {
		t.Fatalf("calls = %v", got)
	}
}

func TestMessageNamesTheRemoteCaller(t *testing.T) {
	f := newFakeActor()
	_, cs := adminSession(t, f)
	actOut(t)(confirmed(t, cs, "message", map[string]any{"task": "t4", "message": "rebase please"}))
	if got := f.got(); len(got) != 1 || !strings.Contains(got[0], "owner") || !strings.Contains(got[0], "rebase please") {
		t.Fatalf("calls = %v", got)
	}
}

// TestReadOnlySourceServesNoActions: a Source that can't act serves only
// the read tools, whatever the token.
func TestReadOnlySourceServesNoActions(t *testing.T) {
	r := newRig(t)
	cs, err := r.connect(t, r.token(t, "owner", ScopeAdmin))
	if err != nil {
		t.Fatal(err)
	}
	if res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "land"}); !callErr(res, err) {
		t.Fatal("land ran on a read-only source")
	}
}

// TestAppSourceActsThroughTheOrchestratorTools: against a real repo
// state, State reads the queue and its version, and Call runs the
// orchestrator's own tool, so the version moves with the change.
func TestAppSourceActsThroughTheOrchestratorTools(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".saddle"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := pushStore(t)
	for _, id := range []string{"t4", "t5"} {
		if err := st.CreateTask(store.Task{ID: id, Title: id, Role: store.RoleWorker, Status: store.Idle}); err != nil {
			t.Fatal(err)
		}
		if err := st.Enqueue(id); err != nil {
			t.Fatal(err)
		}
	}
	src := AppSource{A: &app.App{Root: root, Cfg: config.Default(), Store: st}}
	before, err := src.State()
	if err != nil {
		t.Fatal(err)
	}
	if want := []QueueEntry{{Task: "t4"}, {Task: "t5"}}; !slices.Equal(before.Queue, want) {
		t.Fatalf("queue = %+v, want %+v", before.Queue, want)
	}
	if _, err := src.Call(context.Background(), "queue_hold", map[string]any{"task": "t5", "reason": "remote test"}); err != nil {
		t.Fatal(err)
	}
	after, err := src.State()
	if err != nil {
		t.Fatal(err)
	}
	if !after.Queue[1].Held || after.Versions()[ObjQueue] == before.Versions()[ObjQueue] {
		t.Fatalf("after queue_hold: %+v, versions %v -> %v", after.Queue, before.Versions(), after.Versions())
	}
	if _, err := src.Call(context.Background(), "queue_hold", map[string]any{"task": "t9"}); err == nil {
		t.Fatal("an orchestrator tool's error didn't come back")
	}
}
