package remote

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

func TestCompactSnapshot(t *testing.T) {
	st := mcpserver.StatusOut{Integration: "saddle/integration", Warnings: []string{"w"}, Tasks: []mcpserver.TaskView{
		{ID: app.OrchestratorID, Status: store.Running},
		{ID: "t1", Title: "a", Status: store.Running, Worktree: "/home/me/secret/path", Window: "@3"},
		{ID: "t2", Title: "b", Status: store.NeedsYou},
		{ID: "t3", Title: "c", Status: store.Done, Train: "queued"},
		{ID: "t4", Title: "d", Status: store.Landed, PR: "https://github.com/o/r/pull/4"},
		{ID: "t5", Title: "e", Status: store.Killed},
	}}
	s := Compact(st, 1, "on")
	want := Counts{Running: 1, NeedsYou: 1, Queued: 1, Landed: 1}
	if s.Counts != want {
		t.Fatalf("counts = %+v, want %+v", s.Counts, want)
	}
	var ids []string
	for _, l := range s.Tasks {
		ids = append(ids, l.ID)
	}
	if strings.Join(ids, ",") != "t2,t1,t3" {
		t.Fatalf("task lines %v, want live tasks with needs-you first and no t0, landed or killed", ids)
	}
	if s.AutoMerge != "on" || len(s.Warnings) != 1 {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestNeedsYouFromStore(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for _, tk := range []store.Task{
		{ID: app.OrchestratorID, Title: "orchestrator", Role: store.RoleOrchestrator, Status: store.Running},
		{ID: "t1", Title: "meter", Role: store.RoleWorker, Status: store.Running},
		{ID: "t2", Title: "stripe", Role: store.RoleWorker, Status: store.Running},
	} {
		if err := st.CreateTask(tk); err != nil {
			t.Fatal(err)
		}
	}
	st.Event("t1", "notification", "Claude needs your permission to use Bash")
	st.Event("t1", "notification", "Claude needs your permission to use Write")
	if err := st.SetStatus("t1", store.NeedsYou); err != nil {
		t.Fatal(err)
	}
	if err := st.Notify(app.OrchestratorID, store.NoticeAction, "t2 escalated: tests keep failing"); err != nil {
		t.Fatal(err)
	}
	if err := st.Notify(app.OrchestratorID, store.NoticeInfo, "t3 landed"); err != nil {
		t.Fatal(err)
	}

	items, err := NeedsYouFrom(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %+v, want the prompt and the action notice", items)
	}
	if items[0].Task != "t1" || items[0].Kind != KindPrompt || !strings.Contains(items[0].Text, "Write") {
		t.Fatalf("prompt item = %+v, want t1's latest notification", items[0])
	}
	if items[1].Task != app.OrchestratorID || items[1].Kind != KindNotice || !strings.Contains(items[1].Text, "escalated") {
		t.Fatalf("notice item = %+v", items[1])
	}
	// Reading needs-you must not consume the orchestrator's notices.
	if n, _ := st.PendingActionNotices(app.OrchestratorID); n != 1 {
		t.Fatalf("pending action notices = %d after a read, want 1", n)
	}
}

// TestStackGraph: status draws the PR stack bottom up from the base, the
// merge train queue in landing order, and named stacks, one short line
// each, marking the layer at risk and CI-red PRs.
func TestStackGraph(t *testing.T) {
	tasks := []mcpserver.TaskView{
		{ID: "t1", PR: "https://github.com/o/r/pull/11"},
		{ID: "t2", PR: "https://github.com/o/r/pull/12"},
		{ID: "t3", PR: "https://github.com/o/r/pull/13"},
		{ID: "t4"}, {ID: "t5"},
		{ID: "t8", Status: store.Killed, PR: "https://github.com/o/r/pull/18"},
	}
	train := []store.TrainEntry{
		{Task: "t1", State: store.TrainOK},
		{Task: "t2", State: app.TrainMerged},
		{Task: "t8", State: store.TrainOK},
		{Task: "t3", State: store.TrainOK},
		{Task: "t4", State: store.Queued},
		{Task: "t5", State: store.OnHold},
	}
	custom := []app.CustomStack{{Name: "api", Tasks: []string{"t6", "t7"}}}
	cired := []app.CIRedHold{{Task: "t1", PR: "https://github.com/o/r/pull/11"}}
	got := StackGraph("main", train, tasks, custom, "t3", cired)
	want := []string{
		"stack: main ← t1 #11 (ci red) ← t3 #13 (at risk)",
		"queue: t4, t5 (held)",
		"stack api: t6 ← t7",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("graph:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if g := StackGraph("main", nil, nil, nil, "", nil); len(g) != 0 {
		t.Fatalf("empty state drew %v", g)
	}
}

// TestLimitsLine: status carries plan-limit usage per window, compactly.
func TestLimitsLine(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	e := usage.LimitEstimate{Now: now, State: usage.Warn,
		FiveHour: usage.WindowEstimate{Name: "5h", Percent: 0.856, State: usage.Warn, USD: 12.5,
			Tokens: usage.Tokens{Input: 10, Output: 20}, ResetsAt: now.Add(90 * time.Minute), ResetIn: 90 * time.Minute, Cap: usage.Cap{USD: 15}},
		Weekly: usage.WindowEstimate{Name: "weekly", Unlimited: true, Tokens: usage.Tokens{Input: 5}},
	}
	l := LimitsFrom(e)
	if l.State != "warn" || l.LaunchesPaused {
		t.Fatalf("limits = %+v", l)
	}
	if l.FiveHour.Percent != 86 || l.FiveHour.State != "warn" || l.FiveHour.Tokens != 30 || l.FiveHour.ResetIn != "1h30m" || l.FiveHour.USD != 12.5 {
		t.Fatalf("5h = %+v", l.FiveHour)
	}
	if !l.Weekly.Unlimited || l.Weekly.Tokens != 5 {
		t.Fatalf("weekly = %+v", l.Weekly)
	}
}
