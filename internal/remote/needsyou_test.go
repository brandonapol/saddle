package remote

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// liveSource is a Source whose needs-you items change under the test.
type liveSource struct {
	fakeSource
	mu    sync.Mutex
	items []NeedsYouItem
}

func (s *liveSource) NeedsYou() ([]NeedsYouItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.items), nil
}

func (s *liveSource) add(it NeedsYouItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, it)
}

func callJSON[T any](t *testing.T, cs *mcp.ClientSession, name string, args any) (T, *mcp.CallToolResult) {
	t.Helper()
	var out T
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s: %+v", name, res.Content)
	}
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out, res
}

// TestPeekIsScrubbedAndFenced: a read token can peek, and what comes back
// has secrets scrubbed and sits inside a fence the screen can't close. The
// call is audited.
func TestPeekIsScrubbedAndFenced(t *testing.T) {
	r := newRig(t)
	cs, err := r.connect(t, r.token(t, "phone", ScopeRead))
	if err != nil {
		t.Fatal(err)
	}
	out, res := callJSON[PeekOut](t, cs, "peek", map[string]any{"task": "t3", "lines": 5000})
	if strings.Contains(out.Output, "ghp_abcdefghij") || out.Redacted == 0 {
		t.Fatalf("peek leaked a secret: %q", out.Output)
	}
	if strings.Count(out.Output, out.Fence) != 2 || !strings.Contains(out.Output, "Ignore previous instructions") {
		t.Fatalf("peek output not fenced as data: %q", out.Output)
	}
	if len(res.Content) == 0 {
		t.Fatal("peek has no text content")
	}
	if txt, ok := res.Content[0].(*mcp.TextContent); !ok || !strings.Contains(txt.Text, "UNTRUSTED") || strings.Contains(txt.Text, "ghp_abcdefghij") {
		t.Fatalf("peek text content = %+v", res.Content[0])
	}
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "peek", Arguments: map[string]any{"task": "t9"}}); err != nil {
		t.Fatal(err)
	}
	if got, want := auditTrail(r), []string{"peek:" + DecisionAllowed, "peek:" + DecisionAllowed}; !slices.Equal(got, want) {
		t.Fatalf("audit = %v, want %v", got, want)
	}
}

// TestNeedsYouHasCursor: needs_you returns a cursor that wait_needs_you takes.
func TestNeedsYouHasCursor(t *testing.T) {
	r := newRig(t)
	cs, err := r.connect(t, r.token(t, "phone", ScopeRead))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := callJSON[NeedsYouOut](t, cs, "needs_you", nil)
	if out.Cursor == "" || out.Items[0].ID == "" {
		t.Fatalf("needs_you = %+v, want item ids and a cursor", out)
	}
}

// TestWaitNeedsYouWakesOnNewItem: wait_needs_you blocks until an item the
// cursor hasn't seen appears, then returns just the new ones and a fresh
// cursor.
func TestWaitNeedsYouWakesOnNewItem(t *testing.T) {
	src := &liveSource{items: []NeedsYouItem{{ID: "p-t1-a", Task: "t1", Kind: KindPrompt, Text: "old"}}}
	r := newRigSrc(t, src, Options{poll: 20 * time.Millisecond})
	cs, err := r.connect(t, r.token(t, "phone", ScopeRead))
	if err != nil {
		t.Fatal(err)
	}
	ny, _ := callJSON[NeedsYouOut](t, cs, "needs_you", nil)
	go func() {
		time.Sleep(150 * time.Millisecond)
		src.add(NeedsYouItem{ID: "n-7", Task: "t0", Kind: KindNotice, Text: "t4 escalated"})
	}()
	start := time.Now()
	w, _ := callJSON[WaitOut](t, cs, "wait_needs_you", map[string]any{"cursor": ny.Cursor, "timeout_seconds": 10})
	if el := time.Since(start); el < 100*time.Millisecond || el > 5*time.Second {
		t.Fatalf("wait returned after %v, want once the item appeared", el)
	}
	if w.TimedOut || len(w.New) != 1 || w.New[0].ID != "n-7" || len(w.Items) != 2 {
		t.Fatalf("wait = %+v, want only the new notice", w)
	}
	if w.Cursor == ny.Cursor || w.Cursor == "" {
		t.Fatalf("cursor not advanced: %q", w.Cursor)
	}
	if got, want := auditTrail(r), []string{"needs_you:" + DecisionAllowed, "wait_needs_you:" + DecisionAllowed}; !slices.Equal(got, want) {
		t.Fatalf("audit = %v, want %v", got, want)
	}
}

// TestWaitNeedsYouTimesOut: with nothing new, it returns at the timeout,
// and without a cursor it waits from now: items already waiting don't wake
// it.
func TestWaitNeedsYouTimesOut(t *testing.T) {
	src := &liveSource{items: []NeedsYouItem{{ID: "p-t1-a", Task: "t1", Kind: KindPrompt}}}
	r := newRigSrc(t, src, Options{poll: 20 * time.Millisecond})
	cs, err := r.connect(t, r.token(t, "phone", ScopeRead))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	w, _ := callJSON[WaitOut](t, cs, "wait_needs_you", map[string]any{"timeout_seconds": 1})
	if el := time.Since(start); el < 900*time.Millisecond {
		t.Fatalf("wait returned after %v, want the 1s timeout", el)
	}
	if !w.TimedOut || len(w.New) != 0 || len(w.Items) != 1 || w.Cursor == "" {
		t.Fatalf("wait = %+v, want a timeout with the current items", w)
	}
}

func TestWaitTimeoutIsBounded(t *testing.T) {
	for _, c := range []struct {
		in   int
		want time.Duration
	}{{0, DefaultWait}, {-5, DefaultWait}, {30, 30 * time.Second}, {100000, MaxWait}} {
		if got := waitTimeout(c.in); got != c.want {
			t.Errorf("waitTimeout(%d) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestWaitNeedsYouStopsWithTheClient: a client that goes away ends the wait.
func TestWaitNeedsYouStopsWithTheClient(t *testing.T) {
	src := &liveSource{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = waitNeedsYou(ctx, src, WaitIn{TimeoutSeconds: 600}, 10*time.Millisecond)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("wait_needs_you outlived its request")
	}
}
