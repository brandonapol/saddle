package app

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/narrator"
	"github.com/brandonapol/saddle/internal/store"
)

type lineSink struct{ lines []narrator.Line }

func (s *lineSink) Emit(l narrator.Line) { s.lines = append(s.lines, l) }

// doer answers every Messages API call with one line and a costly usage.
type doer struct{ calls int }

func (d *doer) Do(*http.Request) (*http.Response, error) {
	d.calls++
	body := `{"content":[{"type":"text","text":"t1: started"}],"usage":{"input_tokens":10,"output_tokens":100000}}`
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
}

// Without an API key or a daily cap there is no narrator at all: no API
// calls, no errors.
func TestNarratorOffWhenUnconfigured(t *testing.T) {
	a, _ := meterApp(t)
	if n := a.NewNarrator("", &lineSink{}, &doer{}); n != nil {
		t.Fatal("narrator built with no API key")
	}
	a.Cfg.Narrator.DailyCapUSD = 0
	if n := a.NewNarrator("key", &lineSink{}, &doer{}); n != nil {
		t.Fatal("narrator built with no daily cap")
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	a.Cfg.Narrator.DailyCapUSD = 1
	if n := a.NarratorFromEnv(&lineSink{}); n != nil {
		t.Fatal("narrator built from an empty environment")
	}
	if n := a.NewNarrator("key", &lineSink{}, &doer{}); n == nil {
		t.Fatal("narrator not built with a key and a cap")
	}
}

// The source starts after the events already logged, then hands out each new
// event once. The narrator's own error events are never fed back to it.
func TestNarratorSourceResumesFromCursor(t *testing.T) {
	a, _ := meterApp(t)
	a.Store.Event("t1", "spawn", "old")
	src, err := a.narratorSource()
	must(t, err)
	ctx := context.Background()
	if es, err := src.Poll(ctx); err != nil || len(es) != 0 {
		t.Fatalf("history replayed: %+v, %v", es, err)
	}
	a.Store.Event("t1", "landed", "new")
	a.Store.Event("", EventNarratorError, "boom")
	a.Store.Event("t2", "notification", "perm")
	es, err := src.Poll(ctx)
	must(t, err)
	if len(es) != 2 || es[0].Kind != "landed" || es[1].Kind != "notification" {
		t.Fatalf("poll = %+v", es)
	}
	if es, _ := src.Poll(ctx); len(es) != 0 {
		t.Fatalf("second poll repeated events: %+v", es)
	}
}

// Today's spend lives in the store: a narrator built after a restart is
// already at the cap and narrates locally without calling the API.
func TestNarratorSpendSurvivesRestart(t *testing.T) {
	a, _ := meterApp(t)
	a.Cfg.Narrator.DailyCapUSD = 0.05
	d := &doer{}
	n := a.NewNarrator("key", &lineSink{}, d)
	a.Store.Event("t1", "spawn", "")
	must(t, n.Step(context.Background()))
	if d.calls != 1 || !n.CapReached() {
		t.Fatalf("calls=%d spent=%v", d.calls, n.SpentToday())
	}

	path := filepath.Join(a.Root, ".saddle", "state.db")
	must(t, a.Store.Close())
	st, err := store.Open(path)
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	a.Store = st

	d2, out := &doer{}, &lineSink{}
	n2 := a.NewNarrator("key", out, d2)
	if !n2.CapReached() {
		t.Fatalf("restart reset the cap: spent %v", n2.SpentToday())
	}
	a.Store.Event("t1", "landed", "abc")
	must(t, n2.Step(context.Background()))
	if d2.calls != 0 || len(out.lines) != 1 {
		t.Fatalf("calls=%d lines=%+v", d2.calls, out.lines)
	}
}

// Run errors are recorded once per distinct message, not every poll.
func TestNarratorErrorsAreNotRepeated(t *testing.T) {
	a, _ := meterApp(t)
	report := a.narratorErrors()
	for range 3 {
		report(io.ErrUnexpectedEOF)
	}
	report(io.EOF)
	es, err := a.Store.Events(10)
	must(t, err)
	if len(es) != 2 {
		t.Fatalf("events = %+v", es)
	}
}
