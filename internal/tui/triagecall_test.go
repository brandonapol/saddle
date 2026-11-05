package tui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/orch"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/triage"
)

// unauthorizedJev is a Jev that refuses every call, the way a missing or
// rejected key does.
func unauthorizedJev(t *testing.T) *triage.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid api key", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	return &triage.Client{Key: "k", BaseURL: srv.URL}
}

func chatCount(m *model, sub string) int {
	n := 0
	for _, c := range m.chat {
		if strings.Contains(c.text, sub) {
			n++
		}
	}
	return n
}

// A failed Jev call is stored and shown once. A repeat of the same failure
// is stored again but not added to the chat a second time (#512).
func TestFailedJevCallIsRecordedAndSurfacedOnce(t *testing.T) {
	const marker = "invalid api key"

	t.Run("needs_user", func(t *testing.T) {
		m := slashModel(t)
		m.eventTurn = true
		m.attachJev(unauthorizedJev(t))

		ask := func() {
			t.Helper()
			cmd := m.handleEvent(orch.Event{Kind: orch.Text, Text: "t3 wants to delete build/. Approve?"})
			if cmd == nil {
				t.Fatal("an event reply was not sent to Jev")
			}
			m.Update(cmd())
		}
		ask()
		ask()

		got, err := m.app.Store.TriageCalls()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("recorded %d calls, want 2: %+v", len(got), got)
		}
		for _, c := range got {
			if c.Purpose != triage.PurposeNeedsUser || c.Task != app.OrchestratorID || !strings.Contains(c.Error, marker) {
				t.Fatalf("call = %+v, want a needs_user failure for %s", c, app.OrchestratorID)
			}
		}
		if n := chatCount(m, marker); n != 1 {
			t.Fatalf("error surfaced %d times, want once", n)
		}
		for _, c := range m.chat {
			if c.role == store.ChatAssistant && c.attn != attnNormal {
				t.Fatalf("a failed judge marked the reply attn=%d; the error should stand on its own", c.attn)
			}
		}
	})

	t.Run("screen", func(t *testing.T) {
		m := slashModel(t)
		m.attachJev(unauthorizedJev(t))

		raise := func() {
			t.Helper()
			cmd := m.raise(attention{task: "t3", title: "cleanup", text: "t3 stopped without calling done.", screen: "Error: boom"})
			if cmd == nil {
				t.Fatal("a screen was not sent to Jev")
			}
			m.Update(cmd())
		}
		raise()
		raise()

		got, err := m.app.Store.TriageCalls()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("recorded %d calls, want 2: %+v", len(got), got)
		}
		for _, c := range got {
			if c.Purpose != triage.PurposeScreen || c.Task != "t3" || c.Route != triage.Orchestrator || !strings.Contains(c.Error, marker) {
				t.Fatalf("call = %+v, want a screen failure for t3 routed to the orchestrator", c)
			}
		}
		if n := chatCount(m, marker); n != 1 {
			t.Fatalf("error surfaced %d times, want once", n)
		}
		// The worker event still reaches the orchestrator, once per failure.
		if n := chatCount(m, "▲"); n != 2 {
			t.Fatalf("fallback escalations = %d, want 2\n%+v", n, m.chat)
		}
	})
}

// #780: triage runs only after a worker has stopped, so a "working" verdict
// must not write Running. The task stays Idle, and an action notice then
// reaches it with a wake line.
func TestWorkingVerdictKeepsStoppedWorkerIdleAndWakeable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"verdict":{"type":"choice","choice":"working","confidence":0.99}}}`))
	}))
	t.Cleanup(srv.Close)

	m := slashModel(t)
	m.app.Root = t.TempDir()
	tm := &typingTmux{}
	m.app.Tmux = tm
	m.attachJev(&triage.Client{Key: "k", BaseURL: srv.URL})
	if err := m.app.Store.CreateTask(store.Task{ID: "t1", Title: "view router", Role: store.RoleWorker, Status: store.Running, Window: "@1"}); err != nil {
		t.Fatal(err)
	}
	if err := m.app.Store.SetStatus("t1", store.Idle); err != nil {
		t.Fatal(err)
	}

	cmd := m.raise(attention{task: "t1", title: "view router", text: "t1 stopped.", screen: "✻ Thinking…"})
	if cmd == nil {
		t.Fatal("the screen was not sent to Jev")
	}
	m.Update(cmd())

	tk, err := m.app.Store.Task("t1")
	if err != nil {
		t.Fatal(err)
	}
	if tk.Status != store.Idle {
		t.Fatalf("status after a working verdict = %s, want idle", tk.Status)
	}
	if err := m.app.Notify("t1", store.NoticeAction, "answer to your question"); err != nil {
		t.Fatal(err)
	}
	var wakes int
	for _, s := range tm.all() {
		if s == agent.WakeLine {
			wakes++
		}
	}
	if wakes != 1 {
		t.Fatalf("wake lines sent = %d, want 1: %q", wakes, tm.all())
	}
}
