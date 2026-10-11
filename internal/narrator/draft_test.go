package narrator

import (
	"context"
	"errors"
	"testing"

	"github.com/brandonapol/saddle/internal/usage"
)

func TestDraftSendsFactsAndRespectsCap(t *testing.T) {
	l := &lockedLedger{spend: map[string]float64{}}
	// Haiku 4.5 at $1/M input: 600k input tokens is $0.60 against a $1 cap.
	m := &fakeModel{text: "  CI stayed red on a history check.  ", tokens: usage.Tokens{Input: 600_000}}
	r := newAskRig(t, m, l)
	got, err := r.n.Draft(context.Background(), "system", "the facts", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != "CI stayed red on a history check." {
		t.Errorf("draft = %q", got)
	}
	if req := m.reqs[0]; req.System != "system" || req.Blocks[0].Text != "the facts" || req.MaxTokens <= 0 {
		t.Errorf("request = %+v", req)
	}
	if _, err := r.n.Draft(context.Background(), "s", "f", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := r.n.Draft(context.Background(), "s", "f", 100); !errors.Is(err, ErrCapReached) {
		t.Fatalf("past the cap: err = %v", err)
	}
}

func TestDraftRejectsEmptyText(t *testing.T) {
	r := newAskRig(t, &fakeModel{text: "  "}, nil)
	if _, err := r.n.Draft(context.Background(), "s", "f", 100); err == nil {
		t.Fatal("an empty draft should be an error, so the caller uses its template")
	}
}
