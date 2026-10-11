package remote

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
)

type hookCall struct {
	header http.Header
	body   string
}

type hookRecorder struct {
	mu    sync.Mutex
	calls []hookCall
}

func (h *hookRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	h.calls = append(h.calls, hookCall{r.Header.Clone(), string(b)})
	h.mu.Unlock()
}

func (h *hookRecorder) got() []hookCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hookCall(nil), h.calls...)
}

func pushStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestPushSendsInterruptNoticesOnly: the webhook fires for interrupt-class
// orchestrator notices that arrive after it starts, and for nothing else:
// not digest or silent notices, not other events, not history. The payload
// is the task id and one scrubbed line, never more.
func TestPushSendsInterruptNoticesOnly(t *testing.T) {
	st := pushStore(t)
	st.Event(app.OrchestratorID, app.EventNoticeInterrupt, "t1 \"old\" is done and queued in the merge train: history")
	rec := &hookRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	p, err := NewPusher(st, PushConfig{URL: srv.URL, Token: "hooksecret"}, "saddle")
	if err != nil {
		t.Fatal(err)
	}
	st.Event(app.OrchestratorID, app.EventNoticeDigest, "[landed] t2 \"x\" landed on saddle/integration at abc")
	st.Event(app.OrchestratorID, app.EventNoticeSilent, "[repeat] t2 again")
	st.Event("t5", "notification", "Claude needs your permission to use Bash")
	st.Event(app.OrchestratorID, app.EventNoticeInterrupt, "t5 \"meter\" is done and queued in the merge train: GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789\nRun the saddle land tool when you're ready.")
	if err := p.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := rec.got()
	if len(calls) != 1 {
		t.Fatalf("webhook got %d calls, want 1: %+v", len(calls), calls)
	}
	var pl PushPayload
	if err := json.Unmarshal([]byte(calls[0].body), &pl); err != nil {
		t.Fatal(err)
	}
	if pl.Task != "t5" || pl.Repo != "saddle" || pl.Class != "interrupt" {
		t.Fatalf("payload = %+v", pl)
	}
	if strings.Contains(pl.Summary, "\n") || strings.Contains(pl.Summary, "ghp_") || !strings.HasPrefix(pl.Summary, "t5 \"meter\" is done") {
		t.Fatalf("summary = %q, want one scrubbed line", pl.Summary)
	}
	if len([]rune(pl.Summary)) > maxPushSummary {
		t.Fatalf("summary is %d runes", len([]rune(pl.Summary)))
	}
	if calls[0].header.Get("Authorization") != "Bearer hooksecret" || calls[0].header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", calls[0].header)
	}
	// Nothing new: no call.
	if err := p.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(rec.got()); n != 1 {
		t.Fatalf("webhook got %d calls after an idle step, want 1", n)
	}
}

// TestPushNtfy: an ntfy topic gets the summary as the body and the task in
// the title.
func TestPushNtfy(t *testing.T) {
	st := pushStore(t)
	rec := &hookRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	p, err := NewPusher(st, PushConfig{Ntfy: srv.URL + "/saddle-topic"}, "quark")
	if err != nil {
		t.Fatal(err)
	}
	st.Event(app.OrchestratorID, app.EventNoticeInterrupt, "t3 asks: which schema?")
	if err := p.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := rec.got()
	if len(calls) != 1 || calls[0].body != "t3 asks: which schema?" {
		t.Fatalf("ntfy calls = %+v", calls)
	}
	if title := calls[0].header.Get("Title"); !strings.Contains(title, "quark") || !strings.Contains(title, "t3") {
		t.Fatalf("ntfy title = %q", title)
	}
}

// TestPushFailureDoesNotLoseLaterNotices: a failed POST is reported and
// the pusher moves on; it neither blocks nor replays forever.
func TestPushFailureDoesNotLoseLaterNotices(t *testing.T) {
	st := pushStore(t)
	var mu sync.Mutex
	fail := true
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if fail {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		bodies = append(bodies, string(b))
	}))
	defer srv.Close()
	p, err := NewPusher(st, PushConfig{Ntfy: srv.URL}, "saddle")
	if err != nil {
		t.Fatal(err)
	}
	st.Event(app.OrchestratorID, app.EventNoticeInterrupt, "t1 first")
	if err := p.Step(context.Background()); err == nil {
		t.Fatal("a 502 from the webhook was not reported")
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	st.Event(app.OrchestratorID, app.EventNoticeInterrupt, "t2 second")
	if err := p.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 || bodies[0] != "t2 second" {
		t.Fatalf("bodies = %v", bodies)
	}
}

func TestPushTaskID(t *testing.T) {
	for in, want := range map[string]string{
		"t12 \"x\" is done":                 "t12",
		"CI failed on #44 for t7, fix it":   "t7",
		"The stack is at risk; decide now?": app.OrchestratorID,
		"t3a is not a task id; t9 is":       "t9",
	} {
		if got := pushTask(in); got != want {
			t.Errorf("pushTask(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPushConfig: [remote.push] is read from the user's config, needs one
// target, and refuses plain http to anything but loopback, since the
// notice text would cross the network in the clear.
func TestPushConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte("[remote.push]\nntfy = \"https://ntfy.sh/my-topic\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled || !cfg.Push.On() || cfg.Push.Ntfy != "https://ntfy.sh/my-topic" {
		t.Fatalf("config = %+v", cfg)
	}
	for _, c := range []struct {
		pc PushConfig
		ok bool
	}{
		{PushConfig{}, true},
		{PushConfig{Ntfy: "https://ntfy.sh/t"}, true},
		{PushConfig{URL: "http://127.0.0.1:8080/hook"}, true},
		{PushConfig{URL: "http://example.com/hook"}, false},
		{PushConfig{Ntfy: "ftp://example.com/x"}, false},
		{PushConfig{Ntfy: "https://ntfy.sh/a", URL: "https://example.com/b"}, false},
	} {
		if err := c.pc.Check(); (err == nil) != c.ok {
			t.Errorf("Check(%+v) = %v, want ok %v", c.pc, err, c.ok)
		}
	}
}

// TestRunPushIsOffByDefault: with no [remote.push], RunPush returns at once.
func TestRunPushIsOffByDefault(t *testing.T) {
	done := make(chan struct{})
	go func() {
		RunPush(context.Background(), nil, PushConfig{}, "", func(string, ...any) { t.Error("RunPush logged while off") })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunPush blocked with push off")
	}
}

// TestPushLockIsExclusive: saddle up and the plugin engine may both run
// the pusher for one repo; only one holds the lock and sends.
func TestPushLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	rel, err := acquirePushLock(dir, "/src/saddle", "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquirePushLock(dir, "/src/saddle", "second"); err == nil || !strings.Contains(err.Error(), "first") {
		t.Fatalf("second lock = %v, want refused naming the holder", err)
	}
	if r2, err := acquirePushLock(dir, "/src/quark", "other repo"); err != nil {
		t.Fatalf("another repo's lock = %v", err)
	} else {
		r2()
	}
	rel()
	if r3, err := acquirePushLock(dir, "/src/saddle", "third"); err != nil {
		t.Fatalf("lock after release = %v", err)
	} else {
		r3()
	}
}

// TestUnconfiguredPushSendsNothing: push is off unless [remote.push] names
// a target, whatever [remote] enabled says. With a config that has no push
// section, RunManaged returns at once and interrupt notices go nowhere.
func TestUnconfiguredPushSendsNothing(t *testing.T) {
	for _, conf := range []string{"", "[remote]\nenabled = false\n", "[remote]\nenabled = false\n[remote.push]\n"} {
		home := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", home)
		if conf != "" {
			if err := os.MkdirAll(filepath.Join(home, "saddle"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, "saddle", "config.toml"), []byte(conf), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cfg, err := LoadConfig(UserConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Push.On() {
			t.Fatalf("config %q turned push on", conf)
		}
		rec := &hookRecorder{}
		srv := httptest.NewServer(rec)
		st := pushStore(t)
		a := &app.App{Root: t.TempDir(), Store: st}
		st.Event(app.OrchestratorID, app.EventNoticeInterrupt, "t5 asks: which schema?")
		done := make(chan struct{})
		go func() {
			RunManaged(context.Background(), a, func(f string, args ...any) { t.Errorf("RunManaged logged with push off: "+f, args...) })
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("config %q: RunManaged blocked with push and the listener off", conf)
		}
		st.Event(app.OrchestratorID, app.EventNoticeInterrupt, "t6 asks: which table?")
		if n := len(rec.got()); n != 0 {
			t.Fatalf("config %q: %d pushes sent with push unconfigured", conf, n)
		}
		srv.Close()
	}
}
