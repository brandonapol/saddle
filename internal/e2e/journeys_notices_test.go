//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/e2e/fakegh"
	"github.com/brandonapol/saddle/internal/store"
)

// orchNotices lists the orchestrator's undelivered notices.
func orchNotices(t *testing.T, a *app.App) []store.Notice {
	t.Helper()
	ns, err := a.Store.PeekNotices(app.OrchestratorID, false)
	must(t, err)
	return ns
}

// TestJourneyNoticesDigestAutomerges (#222): three PRs land and auto-merge
// in one window. None of it interrupts the orchestrator; it gets one digest
// line naming all three once the window has passed.
func TestJourneyNoticesDigestAutomerges(t *testing.T) {
	w := world(t, Options{})
	urls := landThree(t, w)
	must(t, w.GH.SetAllChecks(fakegh.Pass))
	w.MustSaddle("automerge", "on")
	for range 3 {
		if st := w.AutomergeTick(); st.Merged == "" {
			t.Fatalf("tick merged nothing: %+v", st)
		}
	}
	a := w.App()
	for _, n := range orchNotices(t, a) {
		if strings.Contains(n.Text, "Auto-merged") || strings.Contains(n.Text, "landed on") {
			t.Fatalf("routine notice reached the orchestrator: %q", n.Text)
		}
	}
	line, err := a.FlushDigest(time.Now().Add(a.Cfg.Notices.DigestEvery + time.Minute))
	must(t, err)
	s := w.GHState()
	var prs []string
	for _, id := range []string{"t1", "t2", "t3"} {
		prs = append(prs, "#"+itoa(prNumber(t, s, urls[id]).Number))
	}
	for _, want := range []string{"3 PRs merged", "3 tasks landed (t1 t2 t3)"} {
		if !strings.Contains(line, want) {
			t.Fatalf("digest %q lacks %q", line, want)
		}
	}
	for _, pr := range prs {
		if !strings.Contains(line, pr) {
			t.Fatalf("digest %q lacks %s", line, pr)
		}
	}
	var digests int
	for _, n := range orchNotices(t, a) {
		if n.Text == line {
			digests++
		}
	}
	if digests != 1 {
		t.Fatalf("digest queued %d times: %+v", digests, orchNotices(t, a))
	}
	if out := w.MustSaddle("notices", "--all").Stdout; strings.Count(out, "digest      [merged]") != 3 {
		t.Fatalf("notices --all doesn't list the three merges:\n%s", out)
	}
}

// TestJourneyNoticesConflictInterrupts (#222): a conflict the train gives up
// on needs the owner, so it reaches the orchestrator at once as an action
// notice, without waiting for the digest window.
func TestJourneyNoticesConflictInterrupts(t *testing.T) {
	w := world(t, Options{Tables: "[train]\nno_auto_rebase = true\nmax_attempts = 1\n"})
	conflictSetup(t, w)
	if r := w.Saddle("land"); !strings.Contains(r.Stdout, "escalated") {
		t.Fatalf("conflict wasn't escalated: %s", r)
	}
	a := w.App()
	var found bool
	for _, n := range orchNotices(t, a) {
		if n.Kind == store.NoticeAction && strings.Contains(n.Text, "needs you") {
			found = true
		}
		if strings.Contains(n.Text, "landed on") {
			t.Fatalf("t1's landing interrupted alongside the conflict: %q", n.Text)
		}
	}
	if !found {
		t.Fatalf("the conflict didn't interrupt: %+v", orchNotices(t, a))
	}
}
