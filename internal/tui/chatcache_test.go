package tui

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/narrator"
	"github.com/brandonapol/saddle/internal/orch"
	"github.com/brandonapol/saddle/internal/store"
)

// storeChatModel is chatModel with a store, for paths that save chat lines.
func storeChatModel(t *testing.T) *model {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m := chatModel()
	m.app = &app.App{Store: st, Cfg: config.Default()}
	m.vp.Height = 100
	return m
}

// Jev flagging an already rendered reply must redraw it as urgent.
func TestChatCacheClearedWhenTriageFlagsUrgent(t *testing.T) {
	m := chatModel()
	m.vp.Height = 100
	m.chat = []chatLine{{role: store.ChatAssistant, text: "t3 is blocked."}}
	m.renderChat()
	m.chat[0].out = "CACHED"
	m.Update(salienceMsg{idx: 0, needs: true})
	if v := m.vp.View(); strings.Contains(v, "CACHED") || !strings.Contains(v, "needs you") {
		t.Fatalf("flagged line kept its stale rendering:\n%s", v)
	}
}

// setAttn is the one way attention changes; it must drop the cache.
func TestSetAttnClearsCache(t *testing.T) {
	m := chatModel()
	m.chat = []chatLine{{role: store.ChatAssistant, text: "x", out: "CACHED", outW: 60}}
	m.setAttn(0, attnUrgent)
	if m.chat[0].out != "" {
		t.Fatal("setAttn kept the cached rendering")
	}
}

// A reply the orchestrator marks urgent itself renders urgent at once.
func TestChatCacheOrchestratorMarkedReply(t *testing.T) {
	m := storeChatModel(t)
	m.renderChat()
	m.handleEvent(orch.Event{Kind: orch.Text, Text: "‼ t3 is blocked. Details."})
	m.renderChat()
	if v := m.vp.View(); !strings.Contains(v, "needs you") {
		t.Fatalf("marked reply not shown as urgent:\n%s", v)
	}
}

// New lines (user, narrator, events) and the streaming reply show up even
// though earlier lines come from the cache.
func TestChatCacheNewLinesAndStreaming(t *testing.T) {
	m := storeChatModel(t)
	m.chat = []chatLine{{role: store.ChatEvent, text: "hello"}}
	m.renderChat()
	m.chat[0].out = "CACHED"

	m.addChat(store.ChatUser, "work on t3")
	m.Update(narrMsg{line: narrator.Line{Text: "t3 started"}})
	m.handleEvent(orch.Event{Kind: orch.Delta, Text: "partial"})
	m.renderChat()
	v := m.vp.View()
	for _, want := range []string{"CACHED", "work on t3", "t3 started", "partial"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}

	m.handleEvent(orch.Event{Kind: orch.Delta, Text: " more"})
	m.renderChat()
	if v := m.vp.View(); !strings.Contains(v, "partial more") {
		t.Errorf("streaming reply not refreshed:\n%s", v)
	}
	m.handleEvent(orch.Event{Kind: orch.Text, Text: "final answer"})
	m.renderChat()
	if v := m.vp.View(); strings.Contains(v, "partial") || !strings.Contains(v, "final answer") {
		t.Errorf("finished reply should replace the stream:\n%s", v)
	}
}
