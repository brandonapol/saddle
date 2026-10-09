package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// meterApp is an App with just a store and config: enough for the meter.
func meterApp(t *testing.T) (*App, *UsageMeter) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, ".saddle", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := &App{Root: root, Cfg: config.Default(), Store: st}
	m := a.NewUsageMeter()
	m.ClaudeDir = filepath.Join(root, "claude")
	return a, m
}

// appendTurn writes one assistant response the way Claude Code does: one
// line per content block, each repeating the message id and usage.
func appendTurn(t *testing.T, path, id, model string, ts time.Time, in, out, cacheRead int64) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	must(t, err)
	defer f.Close()
	line := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":%q,"model":%q,"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":0,"cache_read_input_tokens":%d}}}`+"\n",
		ts.UTC().Format(time.RFC3339Nano), id, model, in, out, cacheRead)
	_, err = fmt.Fprintf(f, `{"type":"user","timestamp":%q,"message":{"role":"user","content":"go"}}`+"\n%s%s", ts.UTC().Format(time.RFC3339Nano), line, line)
	must(t, err)
}

func modelTotals(t *testing.T, a *App) map[string]usage.Tokens {
	t.Helper()
	u, err := a.Usage(time.Now())
	must(t, err)
	out := map[string]usage.Tokens{}
	for _, m := range u.Models {
		out[m.Key] = m.Tokens
	}
	return out
}

func TestUsageMeterPersistsSessions(t *testing.T) {
	t.Parallel()
	a, m := meterApp(t)
	wt := filepath.Join(a.Root, ".saddle", "worktrees", "t1-meter")
	must(t, a.Store.CreateTask(store.Task{ID: "t1", Title: "meter", Role: store.RoleWorker, Worktree: wt, Status: store.Running, SessionID: "s1"}))
	must(t, a.Store.CreateTask(store.Task{ID: "t2", Title: "no session yet", Role: store.RoleWorker, Status: store.Running}))
	path := usage.ClaudeTranscriptPath(m.ClaudeDir, wt, "s1")
	now := time.Now()

	// No transcript yet: nothing is written, nothing fails.
	must(t, m.Sync())
	if got := modelTotals(t, a); len(got) != 0 {
		t.Fatalf("usage before transcript: %+v", got)
	}

	appendTurn(t, path, "m1", "claude-opus-5-5", now.Add(-time.Hour), 10, 20, 1000)
	must(t, m.Sync())
	appendTurn(t, path, "m2", "claude-opus-5-5", now.Add(-time.Minute), 1, 2, 100)
	appendTurn(t, path, "m3", "claude-haiku-4-5", now.Add(-6*time.Hour), 5, 5, 0)
	must(t, m.Sync())
	must(t, m.Sync()) // nothing new: no change
	want := map[string]usage.Tokens{
		"claude-opus-5-5":  {Input: 11, Output: 22, CacheRead: 1100},
		"claude-haiku-4-5": {Input: 5, Output: 5},
	}
	if got := modelTotals(t, a); len(got) != 2 || got["claude-opus-5-5"] != want["claude-opus-5-5"] || got["claude-haiku-4-5"] != want["claude-haiku-4-5"] {
		t.Fatalf("model totals = %+v, want %+v", got, want)
	}

	// A restarted meter re-reads the transcript but doesn't double count.
	m2 := a.NewUsageMeter()
	m2.ClaudeDir = m.ClaudeDir
	must(t, m2.Sync())
	must(t, m2.Sync())
	if got := modelTotals(t, a); got["claude-opus-5-5"] != want["claude-opus-5-5"] {
		t.Fatalf("after restart: %+v", got)
	}

	// Once the task ends, its last lines are still collected, then the session is dropped.
	appendTurn(t, path, "m4", "claude-opus-5-5", now, 100, 0, 0)
	must(t, a.Store.SetStatus("t1", store.Landed))
	must(t, m2.Sync())
	if len(m2.sessions) != 0 {
		t.Fatalf("ended session still tracked: %d", len(m2.sessions))
	}
	u, err := a.Usage(now)
	must(t, err)
	if len(u.Tasks) != 1 || u.Tasks[0].Key != "t1" || u.Tasks[0].Input != 116 {
		t.Fatalf("task totals: %+v", u.Tasks)
	}
}

func TestUsageWindowsAgainstCaps(t *testing.T) {
	t.Parallel()
	a, m := meterApp(t)
	a.Cfg.Usage.Windows = []config.Window{
		{Name: "5h", Span: 5 * time.Hour, Cap: 1000},
		{Name: "7d", Span: 7 * 24 * time.Hour},
	}
	must(t, a.Store.CreateTask(store.Task{ID: "t0", Title: "orchestrator", Role: store.RoleOrchestrator, Worktree: a.Root, Status: store.Running, SessionID: "orch"}))
	path := usage.ClaudeTranscriptPath(m.ClaudeDir, a.Root, "orch")
	now := time.Now()
	appendTurn(t, path, "a", "claude-sonnet-5-5", now.Add(-time.Hour), 200, 50, 9999)
	appendTurn(t, path, "b", "claude-sonnet-5-5", now.Add(-48*time.Hour), 300, 0, 0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx, func(err error) { t.Error(err) }); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		u, err := a.Usage(now)
		must(t, err)
		if len(u.Models) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	u, err := a.Usage(now)
	must(t, err)
	if len(u.Windows) != 2 {
		t.Fatalf("windows: %+v", u.Windows)
	}
	w5, w7 := u.Windows[0], u.Windows[1]
	if w5.Used != 250 || !w5.HasBar() || w5.Fraction() != 0.25 {
		t.Fatalf("5h window (cache reads excluded): %+v", w5)
	}
	if w7.Used != 550 || w7.HasBar() || w7.Fraction() != 0 {
		t.Fatalf("7d window without cap: %+v", w7)
	}
	a.Cfg.Usage.CountCacheReads = true
	if u, err = a.Usage(now); err != nil || u.Windows[0].Used != 250+9999 {
		t.Fatalf("with cache reads: %+v, %v", u.Windows, err)
	}
}
