package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/retry"
)

// #826: attempts round-trip through the retry_attempts table and outlive
// a reopen, so a restart doesn't forget a backoff or re-escalate.
func TestRetryAttemptsPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	var escalated int
	r := &retry.Retrier{Store: s, Now: func() time.Time { return now }, Escalate: func(retry.Attempt) { escalated++ }}
	for range retry.EscalateAfter {
		if _, err := r.Fail("merge:pr/1", "merge", "sha1", errors.New("review required")); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	r.Store = s
	a, ok, err := s.RetryAttempt("merge:pr/1")
	if err != nil || !ok || a.N != 3 || !a.Escalated || a.Inputs != "sha1" || a.Outcome != retry.Deterministic || !a.NextAt.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("attempt = %+v ok %v err %v", a, ok, err)
	}
	if _, err := r.Fail("merge:pr/1", "merge", "sha1", errors.New("review required")); err != nil {
		t.Fatal(err)
	}
	if escalated != 1 {
		t.Fatalf("escalated %d times across a reopen, want once", escalated)
	}
	if err := r.Succeed("merge:pr/1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.RetryAttempt("merge:pr/1"); ok {
		t.Fatal("attempt survived success")
	}
}
