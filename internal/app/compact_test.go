package app

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

func TestContextWindowTable(t *testing.T) {
	for _, c := range []struct {
		model string
		want  int64
	}{
		{"claude-opus-4-5-20251101", 200_000},
		{"claude-sonnet-5-5[1m]", 1_000_000},
		{"grok-4", 256_000},
		{"grok-code-fast-1", 256_000},
		{"gpt-5-codex", 272_000},
		{"", DefaultContextWindow},
		{"something-new", DefaultContextWindow},
	} {
		if got := ContextWindow(c.model); got != c.want {
			t.Errorf("ContextWindow(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}

func TestContextFraction(t *testing.T) {
	tok := usage.Tokens{Input: 10_000, CacheRead: 120_000, CacheCreation: 10_000, Output: 50_000}
	u := NewContextUse("claude-opus-4-5", "", tok)
	if u.Prompt != 140_000 || u.Window != 200_000 {
		t.Fatalf("use = %+v; output tokens must not count", u)
	}
	if f := u.Fraction(); f < 0.699 || f > 0.701 {
		t.Fatalf("fraction = %v, want 0.7", f)
	}
	// The configured model's [1m] marks a 1M window the transcript model lacks.
	if u := NewContextUse("claude-opus-4-5", "opus[1m]", tok); u.Window != 1_000_000 {
		t.Fatalf("window = %d, want 1M from the configured model", u.Window)
	}
	// A prompt bigger than the table's window means the session has more.
	big := usage.Tokens{CacheRead: 300_000}
	if u := NewContextUse("claude-opus-4-5", "", big); u.Window != 1_000_000 || u.Fraction() > 1 {
		t.Fatalf("use = %+v; a Claude prompt over 200k implies the 1M window", u)
	}
	if (ContextUse{}).Fraction() != 0 {
		t.Fatal("no data must read as 0")
	}
}

func TestContextReaderKeepsLatestPrompt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	line := func(id string, cache int64) string {
		return `{"type":"assistant","timestamp":"2026-10-03T10:00:00Z","message":{"id":"` + id +
			`","model":"claude-opus-4-5","usage":{"input_tokens":1,"output_tokens":5,"cache_read_input_tokens":` +
			strconv.FormatInt(cache, 10) + `}}}` + "\n"
	}
	must(t, os.WriteFile(p, []byte(line("a", 50_000)+line("b", 150_000)), 0o644))
	r := &ContextReader{Agent: usage.Claude}
	u, err := r.Read(p)
	must(t, err)
	if u.Prompt != 150_001 {
		t.Fatalf("prompt = %d, want the latest message's 150001", u.Prompt)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = f.WriteString(line("c", 20_000)) // after a compaction
	must(t, err)
	must(t, f.Close())
	if u, err = r.Read(p); err != nil || u.Prompt != 20_001 {
		t.Fatalf("prompt = %d (%v), want 20001 after the append", u.Prompt, err)
	}
	if u, err = r.Read(p); err != nil || u.Prompt != 20_001 {
		t.Fatalf("an unchanged transcript must keep the last reading, got %d (%v)", u.Prompt, err)
	}
}

func TestCompactAtDefaultAndConfig(t *testing.T) {
	a, _ := setup(t)
	if got := a.CompactAt(); got != DefaultCompactAt {
		t.Fatalf("CompactAt = %v, want default %v", got, DefaultCompactAt)
	}
	write(t, a.Root, ".saddle/config.toml", "[orchestrator]\ncompact_at = 0.55\n")
	if got := a.CompactAt(); got != 0.55 {
		t.Fatalf("CompactAt = %v, want 0.55 from config", got)
	}
	// Out-of-range values fall back to the default instead of compacting always or never.
	write(t, a.Root, ".saddle/config.toml", "[orchestrator]\ncompact_at = 1.5\n")
	if got := a.CompactAt(); got != DefaultCompactAt {
		t.Fatalf("CompactAt = %v, want default for an out-of-range value", got)
	}
}

func TestCompactCommandPerHarness(t *testing.T) {
	cmd, ok := CompactCommand(usage.Claude)
	if !ok || !strings.HasPrefix(cmd, "/compact ") {
		t.Fatalf("claude: %q %v, want /compact with an instruction", cmd, ok)
	}
	for _, keep := range []string{"running tasks", "open PRs", "queued follow-ups", "owner decisions pending", "rules in force"} {
		if !strings.Contains(cmd, keep) {
			t.Errorf("claude compact instruction misses %q: %s", keep, cmd)
		}
	}
	if strings.Contains(cmd, "\n") {
		t.Errorf("the command must be one line, or it submits early: %q", cmd)
	}
	if cmd, ok := CompactCommand(usage.Codex); !ok || cmd != "/compact" {
		t.Fatalf("codex: %q %v, want bare /compact", cmd, ok)
	}
	if cmd, ok := CompactCommand(usage.Grok); ok || cmd != "" {
		t.Fatalf("grok: %q %v, want no command", cmd, ok)
	}
}

// fakeTarget is the orchestrator's input as the watcher sees it.
type fakeTarget struct {
	busy, drafting bool
	sent           []string
	err            error
}

func (f *fakeTarget) Busy() bool     { return f.busy }
func (f *fakeTarget) Drafting() bool { return f.drafting }
func (f *fakeTarget) Send(s string) error {
	f.sent = append(f.sent, s)
	return f.err
}

type compactRig struct {
	a    *App
	w    *CompactWatcher
	frac float64
	now  time.Time
	tgt  *fakeTarget
}

func newCompactRig(t *testing.T, harness string) *compactRig {
	a, _ := setup(t)
	_, err := a.EnsureOrchestrator()
	must(t, err)
	r := &compactRig{a: a, now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC), tgt: &fakeTarget{}}
	r.w = a.NewCompactWatcher()
	r.w.Harness = harness
	r.w.Now = func() time.Time { return r.now }
	r.w.Usage = func() (ContextUse, error) {
		return ContextUse{Prompt: int64(r.frac * 100_000), Window: 100_000}, nil
	}
	r.w.SetTarget(r.tgt)
	return r
}

func (r *compactRig) step(t *testing.T, frac float64) CompactReport {
	t.Helper()
	r.frac = frac
	rep, err := r.w.Check()
	must(t, err)
	return rep
}

func (r *compactRig) notices(t *testing.T) []store.Notice {
	t.Helper()
	ns, err := r.a.Store.TakeNotices(OrchestratorID, false)
	must(t, err)
	return ns
}

func (r *compactRig) events(t *testing.T, kind string) []store.Event {
	t.Helper()
	es, err := r.a.Store.Events(200)
	must(t, err)
	var out []store.Event
	for _, e := range es {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestCompactNoticeOncePerCrossing(t *testing.T) {
	r := newCompactRig(t, usage.Claude)
	r.tgt.busy = true // keep it from compacting so only the notices are in play
	r.step(t, 0.5)
	if ns := r.notices(t); len(ns) != 0 {
		t.Fatalf("below the threshold: %v", ns)
	}
	if rep := r.step(t, 0.71); !rep.Crossed {
		t.Fatal("crossing 0.7 must report Crossed")
	}
	ns := r.notices(t)
	if len(ns) != 1 || ns[0].Kind != store.NoticeAction || !strings.Contains(ns[0].Text, "71%") {
		t.Fatalf("notices = %+v, want one action notice naming 71%%", ns)
	}
	r.step(t, 0.75)
	r.step(t, 0.9)
	if ns := r.notices(t); len(ns) != 0 {
		t.Fatalf("staying above must not repeat the notice: %v", ns)
	}
	r.step(t, 0.3) // compacted: re-arms
	if rep := r.step(t, 0.72); !rep.Crossed {
		t.Fatal("a second crossing must notify again")
	}
	if ns := r.notices(t); len(ns) != 1 {
		t.Fatalf("second crossing: %d notices, want 1", len(ns))
	}
	if es := r.events(t, EventCompactThreshold); len(es) != 2 {
		t.Fatalf("%d threshold events, want 2", len(es))
	}
}

func TestCompactWaitsWhileBusyOrDrafting(t *testing.T) {
	r := newCompactRig(t, usage.Claude)
	r.tgt.busy = true
	if rep := r.step(t, 0.8); rep.Injected || rep.Waiting != "busy" {
		t.Fatalf("busy: %+v", rep)
	}
	r.tgt.busy, r.tgt.drafting = false, true
	if rep := r.step(t, 0.8); rep.Injected || rep.Waiting != "drafting" {
		t.Fatalf("drafting: %+v", rep)
	}
	if len(r.tgt.sent) != 0 {
		t.Fatalf("sent %q while busy or drafting", r.tgt.sent)
	}
	r.tgt.drafting = false
	if rep := r.step(t, 0.8); !rep.Injected {
		t.Fatalf("idle: %+v, want injected", rep)
	}
	if len(r.tgt.sent) != 1 || !strings.HasPrefix(r.tgt.sent[0], "/compact ") {
		t.Fatalf("sent = %q", r.tgt.sent)
	}
	r.step(t, 0.8)
	if len(r.tgt.sent) != 1 {
		t.Fatalf("injected again in the same crossing: %q", r.tgt.sent)
	}
}

func TestCompactWithoutTargetOnlyNotifies(t *testing.T) {
	r := newCompactRig(t, usage.Claude)
	r.w.SetTarget(nil) // the plugin: saddle can't type into the owner's session
	rep := r.step(t, 0.9)
	if !rep.Crossed || rep.Injected || rep.Waiting != "no target" {
		t.Fatalf("report = %+v", rep)
	}
	ns := r.notices(t)
	if len(ns) != 1 || !strings.Contains(ns[0].Text, "/compact") {
		t.Fatalf("notice must tell the orchestrator to compact: %+v", ns)
	}
}

func TestCompactInjectsHarnessCommand(t *testing.T) {
	r := newCompactRig(t, usage.Codex)
	r.step(t, 0.8)
	if len(r.tgt.sent) != 1 || r.tgt.sent[0] != "/compact" {
		t.Fatalf("codex sent %q, want /compact", r.tgt.sent)
	}

	g := newCompactRig(t, usage.Grok)
	rep := g.step(t, 0.8)
	if rep.Injected || len(g.tgt.sent) != 0 {
		t.Fatalf("grok has no compact command, sent %q", g.tgt.sent)
	}
	ns := g.notices(t)
	if len(ns) != 1 || !strings.Contains(ns[0].Text, "no compact command") {
		t.Fatalf("grok notice = %+v, want it to say there is no compact command", ns)
	}
	if es := g.events(t, EventCompactSkip); len(es) != 1 {
		t.Fatalf("%d skip events, want 1", len(es))
	}
}

func TestCompactLogsEvent(t *testing.T) {
	r := newCompactRig(t, usage.Claude)
	r.step(t, 0.85)
	es := r.events(t, EventCompact)
	if len(es) != 1 || es[0].Task != OrchestratorID || !strings.Contains(es[0].Data, "85%") {
		t.Fatalf("compact events = %+v", es)
	}

	f := newCompactRig(t, usage.Claude)
	f.tgt.err = errors.New("pipe closed")
	f.frac = 0.85
	if _, err := f.w.Check(); err == nil {
		t.Fatal("a failed send must surface as an error")
	}
	if es := f.events(t, EventCompact); len(es) != 0 {
		t.Fatalf("a failed send must not log a compaction: %+v", es)
	}
}

func TestCompactGivesUpAfterPendingFor(t *testing.T) {
	r := newCompactRig(t, usage.Claude)
	r.tgt.busy = true
	r.step(t, 0.8)
	r.now = r.now.Add(r.w.PendingFor - time.Second)
	if rep := r.step(t, 0.8); rep.GaveUp {
		t.Fatal("gave up early")
	}
	r.now = r.now.Add(2 * time.Second)
	if rep := r.step(t, 0.8); !rep.GaveUp {
		t.Fatalf("report = %+v, want GaveUp after PendingFor", rep)
	}
	r.tgt.busy = false
	r.step(t, 0.8)
	if len(r.tgt.sent) != 0 {
		t.Fatalf("injected after giving up: %q", r.tgt.sent)
	}
	if es := r.events(t, EventCompactSkip); len(es) != 1 {
		t.Fatalf("%d skip events, want 1 for giving up", len(es))
	}
}

func TestCompactWatcherDefaults(t *testing.T) {
	a, _ := setup(t)
	w := a.NewCompactWatcher()
	if w.Threshold != DefaultCompactAt || w.Interval <= 0 || w.PendingFor <= 0 || w.Harness != usage.Claude {
		t.Fatalf("defaults = %+v", w)
	}
	a.Cfg.Harness = config.HarnessGrok
	if w := a.NewCompactWatcher(); w.Harness != usage.Grok {
		t.Fatalf("harness = %q under grok", w.Harness)
	}
}

func TestPaneTargetUsesSafeSend(t *testing.T) {
	a, ft := setup(t)
	ft.windows["@9"] = true
	p := PaneTarget{Tmux: a.Tmux, Window: "@9"}
	if p.Busy() || p.Drafting() {
		t.Fatal("a live pane with an empty prompt is idle")
	}
	must(t, p.Send("/compact"))
	if got := ft.sent["@9"]; len(got) != 1 || got[0] != "/compact" {
		t.Fatalf("sent = %q", got)
	}
	delete(ft.windows, "@9")
	if !p.Busy() {
		t.Fatal("a dead pane must read as busy so nothing is sent")
	}
}

func TestCompactUsesAppTarget(t *testing.T) {
	r := newCompactRig(t, usage.Claude)
	r.w.SetTarget(nil)
	tgt := &fakeTarget{}
	r.a.SetCompactTarget(tgt)
	t.Cleanup(func() { r.a.SetCompactTarget(nil) })
	if rep := r.step(t, 0.8); !rep.Injected || len(tgt.sent) != 1 {
		t.Fatalf("report = %+v, sent %q; want the App-wide target used", rep, tgt.sent)
	}
}
