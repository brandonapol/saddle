package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

func withStats(m *model) *model {
	m.stats = map[string]agentStats{
		"t1": {Tokens: 1_260_000, Ctx: 84_000, CtxMax: 200_000, Activity: "Edit internal/tui/control.go"},
		"t3": {Tokens: 3_400, Ctx: 190_000, CtxMax: 200_000, Activity: "Bash go test ./..."},
	}
	return m
}

// On a wide screen each agent row carries its window, model, status, a
// context bar, tokens and what it is doing now.
func TestAgentRowsShowCtxTokensActivity(t *testing.T) {
	m := withStats(newViewModel(160, 40))
	out := m.viewLeft(120, 30)
	row := linesWith(out, "t1")[0]
	for _, want := range []string{"@1", "opus", "running", "42%", "1.3M", "Edit internal/tui/control.go"} {
		if !strings.Contains(row, want) {
			t.Errorf("t1 row lacks %q: %q", want, row)
		}
	}
	if row := linesWith(out, "t3 ")[0]; !strings.Contains(row, "95%") || !strings.Contains(row, "3.4k") {
		t.Errorf("t3 row: %q", row)
	}
	// Narrow: fewer columns, never too wide, and the title survives.
	for _, w := range []int{30, 36, 40, 60} {
		out := m.viewLeft(w, 30)
		for _, l := range strings.Split(out, "\n") {
			if lw := lipgloss.Width(l); lw > w {
				t.Errorf("width %d: %q is %d wide", w, l, lw)
			}
		}
		if row := linesWith(out, "t1")[0]; !strings.Contains(row, "view") {
			t.Errorf("width %d: the title was squeezed out: %q", w, row)
		}
	}
}

func TestCtxBarColors(t *testing.T) {
	withColor(t)
	if s := ctxCell(agentStats{Ctx: 190_000, CtxMax: 200_000}); !strings.Contains(s, fg(cAlert)) {
		t.Errorf("a nearly full context should be red: %q", s)
	}
	if s := ctxCell(agentStats{Ctx: 20_000, CtxMax: 200_000}); strings.Contains(s, fg(cAlert)) {
		t.Errorf("a light context should not be red: %q", s)
	}
	if s := ctxCell(agentStats{}); lipgloss.Width(s) != ctxW {
		t.Errorf("an unknown context still takes its column: %q", s)
	}
}

// The claims panel lists each live task's globs, the selected task first.
func TestClaimsPanel(t *testing.T) {
	m := newViewModel(160, 40)
	m.tasks[2].Claims = []string{"go.mod", "internal/app/**"}
	m.sel = 2
	out := m.viewLeft(100, 30)
	if !strings.Contains(out, "CLAIMS") {
		t.Fatalf("no claims panel:\n%s", out)
	}
	ls := linesWith(out, "internal/app/**")
	if len(ls) != 1 || !strings.Contains(ls[0], "t3") {
		t.Errorf("t3's claims: %q", ls)
	}
	if i, j := strings.Index(out, "internal/app/**"), strings.Index(out, "internal/tui/**"); i < 0 || j < 0 || i > j {
		t.Errorf("the selected task's claims should come first:\n%s", out)
	}
	for _, w := range []int{30, 40} {
		for _, l := range strings.Split(m.viewLeft(w, 30), "\n") {
			if lipgloss.Width(l) > w {
				t.Errorf("width %d: %q too wide", w, l)
			}
		}
	}
	// No claims, no panel; a short screen leaves it out too.
	m.tasks[0].Claims, m.tasks[2].Claims = nil, nil
	if out := m.viewLeft(100, 30); strings.Contains(out, "CLAIMS") {
		t.Errorf("panel without claims:\n%s", out)
	}
}

// readStats sums each task's tokens, estimates its context from its latest
// minute, and takes its activity from its latest tool event.
func TestReadStats(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Now().UTC().Truncate(time.Minute)
	b := func(ago time.Duration, task string, tok usage.Tokens, msgs int64) usage.Bucket {
		return usage.Bucket{Key: usage.Key{Minute: now.Add(-ago), Task: task, Model: "claude-opus-5-5"}, Tokens: tok, Messages: msgs}
	}
	if err := st.AddUsage("s1", false, []usage.Bucket{
		b(10*time.Minute, "t1", usage.Tokens{Input: 100, CacheRead: 9_000, Output: 50}, 1),
		b(2*time.Minute, "t1", usage.Tokens{Input: 200, CacheRead: 40_000, CacheCreation: 1_800, Output: 300}, 2),
	}); err != nil {
		t.Fatal(err)
	}
	st.Event("t1", "tool", "Read go.mod")
	st.Event("t1", "tool", "Edit internal/tui/control.go")
	st.Event("t2", "notification", "Claude needs your permission")
	s := readStats(&app.App{Store: st, Cfg: config.Default()}, now)
	t1 := s["t1"]
	if t1.Tokens != 100+9_000+50+200+40_000+1_800+300 {
		t.Errorf("tokens = %d", t1.Tokens)
	}
	if t1.Ctx != (200+40_000+1_800)/2 || t1.CtxMax != 200_000 {
		t.Errorf("ctx = %d/%d", t1.Ctx, t1.CtxMax)
	}
	if t1.Activity != "Edit internal/tui/control.go" {
		t.Errorf("activity = %q", t1.Activity)
	}
	if s["t2"].Activity != "waiting: Claude needs your permission" {
		t.Errorf("t2 activity = %q", s["t2"].Activity)
	}
}

// A grok worker's context cell uses app.WorkerContext: 196K of grok's 256K
// window renders about 77%, and a prompt past the window renders "?" (#522).
func TestCtxCellUsesWorkerContext(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Now().UTC().Truncate(time.Minute)
	b := func(task string, input int64) usage.Bucket {
		return usage.Bucket{
			Key:      usage.Key{Minute: now, Task: task, Model: "grok-4.7-build"},
			Tokens:   usage.Tokens{Input: input},
			Messages: 1,
		}
	}
	if err := st.AddUsage("s1", false, []usage.Bucket{
		b("t9", 196_000),
		b("t8", 10_700_000),
	}); err != nil {
		t.Fatal(err)
	}
	s := readStats(&app.App{Store: st, Cfg: config.Default()}, now)
	if got := ctxCell(s["t9"]); !strings.Contains(got, "77%") {
		t.Errorf("grok 196K renders %q, want about 77%%", got)
	}
	over := ctxCell(s["t8"])
	if !strings.Contains(over, "?") || strings.Contains(over, "999") {
		t.Errorf("over-window reading renders %q, want ?", over)
	}
	if w, ow := lipgloss.Width(ctxCell(s["t9"])), lipgloss.Width(over); w != ctxW || ow != ctxW {
		t.Errorf("context cells are %d and %d wide, want %d", w, ow, ctxW)
	}
}

// s opens the spawn prompt; p interrupts the selected agent.
func TestSpawnAndPauseKeys(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	m := newViewModel(120, 40)
	m.app.Store = st
	m.focus = focusTasks
	m.input.Blur()
	m.Update(runeKey('s'))
	if m.focus != focusChat || m.target != spawnTarget {
		t.Fatalf("s: focus=%d target=%q", m.focus, m.target)
	}
	m.input.Reset()
	m.unaim()
	m.focus = focusTasks
	_, c := m.Update(runeKey('p'))
	if c == nil {
		t.Fatal("p did nothing")
	}
	msg := c()
	if f, ok := msg.(flashMsg); !ok || !strings.Contains(string(f), "t1") {
		t.Errorf("p on a task saddle has no window for: %#v", msg)
	}
	if m.input.Value() != "" {
		t.Error("task keys leaked into the chat")
	}
}

// A row plus its selection marker never exceeds the list's inner width; the
// box would wrap it onto a second line.
func TestAgentRowNeverWraps(t *testing.T) {
	m := withStats(newViewModel(160, 40))
	m.tasks[0].Title = strings.Repeat("long title ", 10)
	for iw := 20; iw <= 200; iw++ {
		c := layoutCols(iw)
		for _, task := range m.tasks {
			if w := 1 + lipgloss.Width(m.agentRow(task, c)); w > iw {
				t.Fatalf("inner width %d: %s row is %d wide", iw, task.ID, w)
			}
		}
	}
}

func TestAgentLabel(t *testing.T) {
	for _, c := range []struct{ adapter, model, want string }{
		{"grok", "", "grok"},
		{"codex", "sol", "codex sol"},
		{"claude", "opus", "claude opus"},
		{"", "opus", "opus"}, // a row from before adapters were recorded
		{"", "", ""},
	} {
		if got := agentLabel(c.adapter, c.model); got != c.want {
			t.Errorf("agentLabel(%q, %q) = %q, want %q", c.adapter, c.model, got, c.want)
		}
	}
}

func TestAgentRowShowsAdapter(t *testing.T) {
	m := withStats(newViewModel(160, 40))
	c := layoutCols(160)
	for _, v := range []struct {
		task mcpserver.TaskView
		want string
	}{
		{mcpserver.TaskView{ID: "t1", Status: store.Running, Adapter: "grok"}, "grok"},
		{mcpserver.TaskView{ID: "t2", Status: store.Running, Adapter: "claude", Model: "opus"}, "claude opus"},
		{mcpserver.TaskView{ID: "t3", Status: store.Running, Model: "sonnet"}, "sonnet"}, // old row
		{mcpserver.TaskView{ID: "t4", Status: store.Running}, ""},                        // old row, no model
	} {
		if row := m.agentRow(v.task, c); !strings.Contains(row, v.want) {
			t.Errorf("%s row %q lacks %q", v.task.ID, row, v.want)
		}
	}
}

// A running grok worker whose usage buckets hold its turns' cumulative
// totals used to render "?"; the control view now shows its live reading
// against grok's 256K window, and a codex worker's against the window its
// rollout reports (#598).
func TestCtxCellShowsLiveGrokAndCodexContext(t *testing.T) {
	tmp := t.TempDir()
	grokHome, codexHome := filepath.Join(tmp, "grok"), filepath.Join(tmp, "codex")
	t.Setenv("GROK_HOME", grokHome)
	t.Setenv("CODEX_HOME", codexHome)
	st, err := store.Open(filepath.Join(tmp, "state.db"))
	must(t, err)
	defer func() { _ = st.Close() }()
	gw, cw := filepath.Join(tmp, "wt", "t1"), filepath.Join(tmp, "wt", "t2")
	must(t, st.CreateTask(store.Task{ID: "t1", Role: store.RoleWorker, Status: store.Running, Adapter: usage.Grok, Worktree: gw, SessionID: "01a1grok"}))
	must(t, st.CreateTask(store.Task{ID: "t2", Role: store.RoleWorker, Status: store.Running, Adapter: usage.Codex, Worktree: cw}))
	now := time.Now().UTC().Truncate(time.Minute)
	must(t, st.AddUsage("s1", false, []usage.Bucket{{
		Key:      usage.Key{Minute: now, Task: "t1", Model: "grok-4.7-build"},
		Tokens:   usage.Tokens{Input: 378_758, CacheRead: 8_753_664, Output: 124_474},
		Messages: 1,
	}}))
	sess := filepath.Join(grokHome, "sessions", "%2Fwt%2Ft1", "01a1grok")
	must(t, os.MkdirAll(sess, 0o755))
	must(t, os.WriteFile(filepath.Join(sess, "usage.json"), []byte(`{"turns":[]}`), 0o644))
	must(t, os.WriteFile(filepath.Join(sess, "updates.jsonl"), []byte(
		`{"timestamp":1792026049,"method":"session/update","params":{"sessionId":"01a1grok","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Done."}},"_meta":{"totalTokens":144127,"eventId":"01a1grok-8001","promptId":"p1","updateType":"AgentMessageChunk"}}}
{"timestamp":1792026049,"method":"_x.ai/session/update","params":{"sessionId":"01a1grok","update":{"sessionUpdate":"turn_completed","usage":{"inputTokens":9132422,"totalTokens":9256896,"cachedReadTokens":8753664,"modelCalls":88}},"_meta":{"eventId":"01a1grok-8006"}}}
`), 0o644))
	day := filepath.Join(codexHome, "sessions", "2026", "10", "14")
	must(t, os.MkdirAll(day, 0o755))
	must(t, os.WriteFile(filepath.Join(day, "rollout-2026-10-14T10-22-37-01a1codex.jsonl"), []byte(
		`{"timestamp":"2026-10-14T14:22:37.100Z","type":"session_meta","payload":{"id":"01a1codex","cwd":"`+cw+`"}}
{"timestamp":"2026-10-14T14:22:39.478Z","type":"turn_context","payload":{"cwd":"`+cw+`","model":"gpt-5.6-sol"}}
{"timestamp":"2026-10-14T14:39:55.469Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":4298911,"cached_input_tokens":4215296,"output_tokens":9011,"total_tokens":4307922},"last_token_usage":{"input_tokens":129200,"cached_input_tokens":128584,"output_tokens":133,"total_tokens":129333},"model_context_window":258400}}}
`), 0o644))

	s := readStats(&app.App{Root: tmp, Store: st, Cfg: config.Default()}, now)
	if got := ctxCell(s["t1"]); !strings.Contains(got, "56%") {
		t.Errorf("grok worker renders %q, want 144K of 256K = 56%%", got)
	}
	if got := ctxCell(s["t2"]); !strings.Contains(got, "50%") {
		t.Errorf("codex worker renders %q, want 129K of 258K = 50%%", got)
	}
	for _, id := range []string{"t1", "t2"} {
		if w := lipgloss.Width(ctxCell(s[id])); w != ctxW {
			t.Errorf("%s context cell is %d wide, want %d", id, w, ctxW)
		}
	}
}
