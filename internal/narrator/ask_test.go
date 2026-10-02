package narrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// fakeModel answers every request with text and records what it was asked.
type fakeModel struct {
	mu     sync.Mutex
	text   string
	tokens usage.Tokens
	err    error
	reqs   []Request
}

func (f *fakeModel) Complete(_ context.Context, r Request) (Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r)
	if f.err != nil {
		return Response{}, f.err
	}
	return Response{Text: f.text, Tokens: f.tokens}, nil
}

func (f *fakeModel) prompt(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b []string
	for _, bl := range f.reqs[i].Blocks {
		b = append(b, bl.Text)
	}
	return f.reqs[i].System + "\n" + strings.Join(b, "\n")
}

var testDay = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

type lockedLedger struct {
	mu    sync.Mutex
	spend map[string]float64
}

func (l *lockedLedger) NarratorSpend(day string) (float64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.spend[day], nil
}

func (l *lockedLedger) SetNarratorSpend(day string, usd float64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.spend[day] = usd
	return nil
}

func newAskRig(t *testing.T, m *fakeModel, l Ledger) *rig {
	t.Helper()
	r := &rig{src: &fakeSource{}, clock: &fakeClock{t: testDay}, doer: &fakeDoer{}, out: &sink{}}
	r.n = New(Config{APIKey: "k", DailyCapUSD: 1}, Deps{Source: r.src, Roster: roster, Clock: r.clock, Model: m, Sink: r.out, Ledger: l})
	return r
}

// Narration goes through an injected model instead of HTTP.
func TestNarrationUsesInjectedModel(t *testing.T) {
	m := &fakeModel{text: "t1: landed the login page"}
	r := newAskRig(t, m, nil)
	r.src.push(store.Event{Task: "t1", Kind: "landed", Data: "abc"})
	r.step(t)
	if len(r.doer.reqs) != 0 || len(m.reqs) != 1 {
		t.Fatalf("http calls %d, model calls %d", len(r.doer.reqs), len(m.reqs))
	}
	if len(r.out.lines) != 1 || r.out.lines[0].Text != "landed the login page" {
		t.Fatalf("lines = %+v", r.out.lines)
	}
	if bs := m.reqs[0].Blocks; len(bs) != 2 || !bs[0].Cache || bs[1].Cache {
		t.Errorf("the roster should be the cache breakpoint: %+v", bs)
	}
}

// A question carries the roster, live statuses and recent narration; the
// screen is sent only when the user opted in, and only its tail.
func TestAskSendsQuestionWithOptionalScreen(t *testing.T) {
	m := &fakeModel{text: "  t2 is waiting on a Bash permission prompt.  "}
	r := newAskRig(t, m, nil)
	r.src.push(store.Event{Task: "t1", Kind: "landed"})
	m.text = "t1: landed"
	r.step(t)
	m.text = "  t2 is waiting on a Bash permission prompt.  "

	l, err := r.n.Ask(context.Background(), Question{Text: "what is t2 doing?", Task: "t2"})
	if err != nil {
		t.Fatal(err)
	}
	if l.Task != "t2" || l.Text != "t2 is waiting on a Bash permission prompt." || l.NeedsYou {
		t.Errorf("answer = %+v", l)
	}
	p := m.prompt(1)
	for _, want := range []string{"what is t2 doing?", "t1 | worker", "t2 | running | Fix CI", "t1: landed", "about task t2"} {
		if !strings.Contains(p, want) {
			t.Errorf("question prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "terminal") {
		t.Errorf("no screen was offered, yet the prompt mentions one:\n%s", p)
	}
	if m.reqs[1].MaxTokens <= 0 {
		t.Error("an answer needs a token budget")
	}

	var screen []string
	for i := range 200 {
		screen = append(screen, fmt.Sprintf("line %03d", i))
	}
	if _, err := r.n.Ask(context.Background(), Question{Text: "why stuck?", Task: "t2", Screen: strings.Join(screen, "\n")}); err != nil {
		t.Fatal(err)
	}
	p = m.prompt(2)
	if !strings.Contains(p, "line 199") || strings.Contains(p, "line 000") {
		t.Errorf("the screen should be sent as its tail only:\n%s", p)
	}
	if _, err := r.n.Ask(context.Background(), Question{Text: "  "}); err == nil {
		t.Error("an empty question should be refused")
	}
}

// Questions spend from the same daily cap as narration and stop at it.
func TestAskRespectsDailyCap(t *testing.T) {
	l := &lockedLedger{spend: map[string]float64{}}
	// Haiku 4.5 at $1/M input: 600k input tokens is $0.60.
	m := &fakeModel{text: "answer", tokens: usage.Tokens{Input: 600_000}}
	r := newAskRig(t, m, l)
	if _, err := r.n.Ask(context.Background(), Question{Text: "q1"}); err != nil {
		t.Fatal(err)
	}
	day := testDay.Format("2006-01-02")
	if got := l.spend[day]; got < 0.59 || got > 0.61 {
		t.Fatalf("ledger after one answer = %.2f", got)
	}
	if _, err := r.n.Ask(context.Background(), Question{Text: "q2"}); err != nil {
		t.Fatal(err)
	}
	_, err := r.n.Ask(context.Background(), Question{Text: "q3"})
	if !errors.Is(err, ErrCapReached) {
		t.Fatalf("past the cap: err = %v", err)
	}
	if len(m.reqs) != 2 {
		t.Errorf("model called %d times; the cap should stop the third", len(m.reqs))
	}
}

// Ask runs on the UI's goroutine while Run steps on its own.
func TestAskAlongsideStep(t *testing.T) {
	m := &fakeModel{text: "t1: ok"}
	r := newAskRig(t, m, &lockedLedger{spend: map[string]float64{}})
	src := &lockedSource{}
	r.n.deps.Source = src
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 50 {
			src.push(store.Event{Task: "t1", Kind: "landed"})
			_ = r.n.Step(context.Background())
		}
	}()
	go func() {
		defer wg.Done()
		for range 50 {
			_, _ = r.n.Ask(context.Background(), Question{Text: "status?"})
		}
	}()
	wg.Wait()
}

type lockedSource struct {
	mu sync.Mutex
	es []store.Event
}

func (s *lockedSource) push(e store.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.es = append(s.es, e)
}

func (s *lockedSource) Poll(context.Context) ([]store.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	es := s.es
	s.es = nil
	return es, nil
}
