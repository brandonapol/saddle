package remote

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
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

	items, err := NeedsYouFrom(st)
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
