package store

import (
	"testing"
	"time"
)

// PeekNotices leaves notices pending until MarkDelivered, so a notice typed
// into a pane is only marked once the pane showed it was submitted (#183).
func TestPeekThenMarkDelivered(t *testing.T) {
	s := openTest(t)
	if err := s.Notify("t1", NoticeAction, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Notify("t1", NoticeInfo, "b"); err != nil {
		t.Fatal(err)
	}
	ns, err := s.PeekNotices("t1", false)
	if err != nil || len(ns) != 2 {
		t.Fatalf("PeekNotices = %v, %v", ns, err)
	}
	if n, _ := s.PendingNotices("t1"); n != 2 {
		t.Fatalf("peek marked notices delivered: %d pending", n)
	}
	if err := s.Notify("t1", NoticeAction, "c"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDelivered(ns); err != nil {
		t.Fatal(err)
	}
	left, _ := s.PeekNotices("t1", false)
	if len(left) != 1 || left[0].Text != "c" {
		t.Fatalf("left %v, want only the notice that arrived after the peek", left)
	}
}

func TestOldestPendingAction(t *testing.T) {
	s := openTest(t)
	if _, ok, err := s.OldestPendingAction("t1"); ok || err != nil {
		t.Fatalf("no notices: ok=%v err=%v", ok, err)
	}
	_ = s.Notify("t1", NoticeInfo, "info")
	if _, ok, _ := s.OldestPendingAction("t1"); ok {
		t.Fatal("info notices don't count")
	}
	before := time.Now().Add(-time.Second)
	_ = s.Notify("t1", NoticeAction, "act")
	ts, ok, err := s.OldestPendingAction("t1")
	if err != nil || !ok || ts.Before(before.Truncate(time.Second)) || ts.After(time.Now()) {
		t.Fatalf("OldestPendingAction = %v, %v, %v", ts, ok, err)
	}
}
