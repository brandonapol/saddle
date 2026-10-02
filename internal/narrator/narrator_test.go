package narrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

type fakeSource struct{ queue [][]store.Event }

func (f *fakeSource) push(es ...store.Event) { f.queue = append(f.queue, es) }

func (f *fakeSource) Poll(context.Context) ([]store.Event, error) {
	if len(f.queue) == 0 {
		return nil, nil
	}
	es := f.queue[0]
	f.queue = f.queue[1:]
	return es, nil
}

type fakeRoster []store.Task

func (r fakeRoster) Tasks() ([]store.Task, error) { return r, nil }

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

type reply struct {
	status int
	text   string
	usage  apiUsage
	err    error
}

type fakeDoer struct {
	replies []reply
	reqs    []*http.Request
	bodies  []messagesRequest
	raw     []string
}

func (d *fakeDoer) Do(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	var body messagesRequest
	if err := json.Unmarshal(b, &body); err != nil {
		return nil, err
	}
	d.reqs = append(d.reqs, r)
	d.bodies = append(d.bodies, body)
	d.raw = append(d.raw, string(b))
	rep := reply{status: 200, text: "ok"}
	if len(d.replies) > 0 {
		rep = d.replies[0]
		d.replies = d.replies[1:]
	}
	if rep.err != nil {
		return nil, rep.err
	}
	var out string
	if rep.status == 200 {
		resp, _ := json.Marshal(messagesResponse{
			Content: []contentBlock{{Type: "text", Text: rep.text}},
			Usage:   rep.usage,
		})
		out = string(resp)
	} else {
		out = `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
	}
	return &http.Response{StatusCode: rep.status, Body: io.NopCloser(strings.NewReader(out))}, nil
}

type sink struct{ lines []Line }

func (s *sink) Emit(l Line) { s.lines = append(s.lines, l) }

var roster = fakeRoster{
	{ID: "t1", Title: "Add login", Role: store.RoleWorker, Status: store.Running},
	{ID: "t2", Title: "Fix CI", Role: store.RoleWorker, Status: store.Running},
}

type rig struct {
	n     *Narrator
	src   *fakeSource
	clock *fakeClock
	doer  *fakeDoer
	out   *sink
}

func newRig(t *testing.T, cfg Config) *rig {
	t.Helper()
	r := &rig{
		src:   &fakeSource{},
		clock: &fakeClock{t: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)},
		doer:  &fakeDoer{},
		out:   &sink{},
	}
	if cfg.APIKey == "" {
		cfg.APIKey = "test-key"
	}
	r.n = New(cfg, Deps{Source: r.src, Roster: roster, Clock: r.clock, HTTP: r.doer, Sink: r.out})
	return r
}

func (r *rig) step(t *testing.T) {
	t.Helper()
	if err := r.n.Step(context.Background()); err != nil {
		t.Fatalf("Step: %v", err)
	}
}

func ev(task, kind, data string) store.Event { return store.Event{Task: task, Kind: kind, Data: data} }

func userDelta(t *testing.T, b messagesRequest) string {
	t.Helper()
	if len(b.Messages) != 1 || len(b.Messages[0].Content) != 2 {
		t.Fatalf("want one user message with roster + delta blocks, got %+v", b.Messages)
	}
	return b.Messages[0].Content[1].Text
}

func TestRoutineEventsWaitForBatchInterval(t *testing.T) {
	r := newRig(t, Config{BatchInterval: 20 * time.Second})
	r.src.push(ev("t1", "tool", "Edit"))
	r.step(t)
	r.clock.advance(10 * time.Second)
	r.src.push(ev("t1", "tool", "Bash"))
	r.step(t)
	if len(r.doer.reqs) != 0 {
		t.Fatalf("called API before the batch interval: %d requests", len(r.doer.reqs))
	}
	r.clock.advance(10 * time.Second)
	r.step(t)
	if len(r.doer.reqs) != 1 {
		t.Fatalf("want 1 request after 20s, got %d", len(r.doer.reqs))
	}
	if d := userDelta(t, r.doer.bodies[0]); !strings.Contains(d, `"tool":2`) {
		t.Errorf("both routine events should be in one batch, got %s", d)
	}
	r.clock.advance(time.Minute)
	r.step(t)
	if len(r.doer.reqs) != 1 {
		t.Errorf("an empty batch must not call the API, got %d requests", len(r.doer.reqs))
	}
}

func TestSalientEventFlushesImmediately(t *testing.T) {
	for _, kind := range []string{"landed", "train_conflict", "restack_conflict", "train_test_failed", "notification", "spawn"} {
		t.Run(kind, func(t *testing.T) {
			r := newRig(t, Config{})
			r.src.push(ev("t1", "tool", "Edit"))
			r.step(t)
			r.clock.advance(time.Second)
			r.src.push(ev("t2", kind, "x"))
			r.step(t)
			if len(r.doer.reqs) != 1 {
				t.Fatalf("%s should flush at once, got %d requests", kind, len(r.doer.reqs))
			}
			d := userDelta(t, r.doer.bodies[0])
			if !strings.Contains(d, `"t1"`) || !strings.Contains(d, `"t2"`) {
				t.Errorf("flush should carry the pending routine events too: %s", d)
			}
		})
	}
}

func TestDeltaIsCompactAndOmitsRawText(t *testing.T) {
	r := newRig(t, Config{})
	var es []store.Event
	for i := 0; i < 200; i++ {
		es = append(es, ev("t1", "tool", fmt.Sprintf("SECRET PANE TEXT %d %s", i, strings.Repeat("x", 500))))
	}
	es = append(es, ev("t2", "notification", "Claude needs your permission to use Bash"+strings.Repeat("y", 1000)))
	r.src.push(es...)
	r.step(t)
	if len(r.doer.reqs) != 1 {
		t.Fatalf("want 1 request, got %d", len(r.doer.reqs))
	}
	d := userDelta(t, r.doer.bodies[0])
	if strings.Contains(d, "SECRET") {
		t.Errorf("routine event data leaked into the delta: %.200s", d)
	}
	if !strings.Contains(d, `"tool":200`) {
		t.Errorf("tool events should be counted, got %s", d)
	}
	if !strings.Contains(d, "needs your permission") {
		t.Errorf("salient note missing: %s", d)
	}
	if len(d) > 1000 {
		t.Errorf("delta is %d bytes; want compact (<1000)", len(d))
	}
	var deltas []TaskDelta
	if err := json.Unmarshal([]byte(d), &deltas); err != nil {
		t.Fatalf("delta is not JSON: %v\n%s", err, d)
	}
	if len(deltas) != 2 || deltas[0].Task != "t1" || deltas[1].Task != "t2" {
		t.Errorf("want one delta per task sorted by id, got %+v", deltas)
	}
	if deltas[1].Title != "Fix CI" || deltas[1].Status != store.NeedsYou {
		t.Errorf("delta should carry title and derived status: %+v", deltas[1])
	}
}

func TestRequestCachesSystemPromptAndRoster(t *testing.T) {
	r := newRig(t, Config{APIKey: "sk-test"})
	r.src.push(ev("t1", "spawn", "parent=t0"))
	r.step(t)
	if len(r.doer.reqs) != 1 {
		t.Fatalf("want 1 request, got %d", len(r.doer.reqs))
	}
	req := r.doer.reqs[0]
	if req.URL.String() != "https://api.anthropic.com/v1/messages" || req.Method != http.MethodPost {
		t.Errorf("request = %s %s", req.Method, req.URL)
	}
	if req.Header.Get("x-api-key") != "sk-test" || req.Header.Get("anthropic-version") == "" {
		t.Errorf("missing auth/version headers: %v", req.Header)
	}
	b := r.doer.bodies[0]
	if b.Model != "claude-haiku-4-5" {
		t.Errorf("model = %q", b.Model)
	}
	if len(b.System) != 1 || b.System[0].CacheControl == nil || b.System[0].CacheControl.Type != "ephemeral" {
		t.Errorf("system prompt must carry cache_control: %+v", b.System)
	}
	blocks := b.Messages[0].Content
	if blocks[0].CacheControl == nil || blocks[0].CacheControl.Type != "ephemeral" {
		t.Errorf("roster block must carry cache_control: %+v", blocks[0])
	}
	if !strings.Contains(blocks[0].Text, "t1") || !strings.Contains(blocks[0].Text, "Add login") {
		t.Errorf("roster block should list tasks: %q", blocks[0].Text)
	}
	if strings.Contains(blocks[0].Text, store.Running) {
		t.Errorf("roster must not include volatile status (breaks the cache): %q", blocks[0].Text)
	}
	if blocks[1].CacheControl != nil {
		t.Errorf("the delta changes every call and must not be a cache breakpoint")
	}
	if !strings.Contains(r.doer.raw[0], `"cache_control":{"type":"ephemeral"}`) {
		t.Errorf("wire JSON lacks cache_control: %s", r.doer.raw[0])
	}
}

func TestDailyCostCapStopsAPICalls(t *testing.T) {
	r := newRig(t, Config{DailyCapUSD: 0.01})
	// 10k output tokens on Haiku 4.5 is $0.05: over the cap after one call.
	r.doer.replies = []reply{{status: 200, text: "t1: started", usage: apiUsage{InputTokens: 100, OutputTokens: 10000}}}
	r.src.push(ev("t1", "spawn", ""))
	r.step(t)
	if got := r.n.SpentToday(); got < 0.05 || got > 0.051 {
		t.Errorf("SpentToday = %v, want ~0.0501", got)
	}
	r.clock.advance(time.Minute)
	r.src.push(ev("t2", "notification", "needs permission"))
	r.step(t)
	r.clock.advance(time.Minute)
	r.src.push(ev("t1", "tool", ""))
	r.clock.advance(time.Minute)
	r.step(t)
	if len(r.doer.reqs) != 1 {
		t.Fatalf("API called after the daily cap: %d requests", len(r.doer.reqs))
	}
	last := r.out.lines[len(r.out.lines)-1]
	if last.Task != "t2" || !last.NeedsYou {
		t.Errorf("over the cap, salient changes still get a local line; got %+v", r.out.lines)
	}
	if !r.n.CapReached() {
		t.Errorf("CapReached should be true")
	}
	// A new day resets the budget.
	r.clock.t = time.Date(2026, 10, 3, 0, 0, 1, 0, time.UTC)
	r.src.push(ev("t1", "landed", "abc123"))
	r.step(t)
	if len(r.doer.reqs) != 2 {
		t.Errorf("a new day should reset the cap, got %d requests", len(r.doer.reqs))
	}
}

func TestNeedsYouLinesAreHighlighted(t *testing.T) {
	r := newRig(t, Config{})
	r.doer.replies = []reply{{status: 200, text: "t1: landed the login page\n\nt2: waiting for permission to run Bash\n"}}
	r.src.push(ev("t1", "landed", "abc"), ev("t2", "notification", "permission to use Bash"))
	r.step(t)
	if len(r.out.lines) != 2 {
		t.Fatalf("want one line per salient change, got %+v", r.out.lines)
	}
	l1, l2 := r.out.lines[0], r.out.lines[1]
	if l1.Task != "t1" || l1.NeedsYou || l1.Text != "landed the login page" {
		t.Errorf("line 1 = %+v", l1)
	}
	if l2.Task != "t2" || !l2.NeedsYou {
		t.Errorf("line 2 should be a highlighted needs-you line: %+v", l2)
	}
	if !strings.HasPrefix(l2.String(), "‼ ") || strings.HasPrefix(l1.String(), "‼") {
		t.Errorf("String highlights needs-you only: %q / %q", l1.String(), l2.String())
	}
}

func TestNeedsYouNeverDroppedByModel(t *testing.T) {
	r := newRig(t, Config{})
	r.doer.replies = []reply{{status: 200, text: "t1: still editing"}}
	r.src.push(ev("t1", "tool", ""), ev("t2", "notification", "permission to use Bash"))
	r.step(t)
	var found bool
	for _, l := range r.out.lines {
		if l.Task == "t2" && l.NeedsYou {
			found = true
		}
	}
	if !found {
		t.Errorf("a needs-you item the model skipped must still be emitted: %+v", r.out.lines)
	}
}

func TestAPIErrorKeepsEventsForRetry(t *testing.T) {
	for name, rep := range map[string]reply{
		"status":    {status: 529},
		"transport": {err: errors.New("connection reset")},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, Config{BatchInterval: 20 * time.Second})
			r.doer.replies = []reply{rep}
			r.src.push(ev("t1", "spawn", ""))
			err := r.n.Step(context.Background())
			if err == nil {
				t.Fatalf("Step should report the API error")
			}
			if len(r.out.lines) != 0 {
				t.Errorf("no lines on failure, got %+v", r.out.lines)
			}
			// A new salient event inside the backoff does not hammer the API.
			r.clock.advance(time.Second)
			r.src.push(ev("t2", "notification", "permission"))
			r.step(t)
			if len(r.doer.reqs) != 1 {
				t.Fatalf("retried inside backoff: %d requests", len(r.doer.reqs))
			}
			r.clock.advance(20 * time.Second)
			r.step(t)
			if len(r.doer.reqs) != 2 {
				t.Fatalf("want a retry after backoff, got %d requests", len(r.doer.reqs))
			}
			d := userDelta(t, r.doer.bodies[1])
			if !strings.Contains(d, `"spawn"`) || !strings.Contains(d, `"t2"`) {
				t.Errorf("retry lost events: %s", d)
			}
			if r.n.Pending() != 0 {
				t.Errorf("pending should drain after success, got %d", r.n.Pending())
			}
		})
	}
}

func TestPendingIsBounded(t *testing.T) {
	r := newRig(t, Config{MaxPending: 10})
	r.doer.replies = []reply{{status: 500}}
	r.src.push(ev("t1", "spawn", ""))
	_ = r.n.Step(context.Background())
	var es []store.Event
	for i := 0; i < 50; i++ {
		es = append(es, ev("t1", "tool", ""))
	}
	r.src.push(append(es, ev("t2", "landed", ""))...)
	r.step(t)
	if r.n.Pending() > 10 {
		t.Errorf("pending = %d, want <= 10", r.n.Pending())
	}
	r.clock.advance(time.Minute)
	r.step(t)
	d := userDelta(t, r.doer.bodies[len(r.doer.bodies)-1])
	if !strings.Contains(d, `"spawn"`) || !strings.Contains(d, `"landed"`) {
		t.Errorf("trimming must keep salient events: %s", d)
	}
}
