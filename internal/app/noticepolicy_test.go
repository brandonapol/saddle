package app

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// #222: only questions and real decisions interrupt the orchestrator;
// routine news goes to the digest and no-op chatter is silenced.
func TestClassifyNotice(t *testing.T) {
	cases := []struct {
		name, kind, text string
		autoMerge        bool
		want             NoticeClass
		topic            string
	}{
		// Interrupts: questions, decisions, escalations, unhandled failures.
		{"needs you", store.NoticeAction, `▲ t4 "x" needs you: it failed to land 3 times (last: conflict: a.go), so the train stopped returning it. Fix it yourself.`, false, NoticeInterrupt, ""},
		{"question", store.NoticeAction, `t7 asks: should the cache be per-user or global?`, false, NoticeInterrupt, ""},
		{"done needs a land", store.NoticeAction, `t3 "x" is done and queued in the merge train: did it` + "\nRun the saddle land tool when you're ready.", false, NoticeInterrupt, ""},
		{"restack conflict", store.NoticeAction, `Restack stopped: t2's landed commit abc conflicts with origin/main in a.go. Nothing was moved; t2 has the conflict.`, false, NoticeInterrupt, ""},
		{"automerge stopped", store.NoticeAction, `Auto-merge stopped: merge failed. It won't retry; fix it (or merge by hand), then ` + "`saddle automerge on`" + ` to resume.`, false, NoticeInterrupt, ""},
		{"ci failed, nobody on it", store.NoticeAction, `CI failed on t5's PR #12: check "ci" in step "test".`, false, NoticeInterrupt, ""},
		{"ci red exhausted", store.NoticeAction, `CI on t5's PR #12 is still red after 2 repair attempts (ci at abc). Saddle won't spawn another. Decide what to do: fix it.`, false, NoticeInterrupt, ""},
		{"info asking a decision", store.NoticeInfo, `t9 landed but couldn't be folded into t8's layer: boom` + "\nIt stays in the stack as its own layer; decide whether to `saddle unstack t9` or keep it.", false, NoticeInterrupt, ""},
		{"compact", store.NoticeAction, `Your context is at 71%. At the next natural breakpoint, compact.`, false, NoticeInterrupt, ""},
		// Digest: routine progress.
		{"landed", store.NoticeInfo, `t1 "alpha work" landed on saddle/integration at 0123456789ab.`, false, NoticeDigest, TopicLanded},
		{"auto-merged", store.NoticeInfo, `Auto-merged https://github.com/o/r/pull/12 (t1) into main and restacked the rest of stack t1.`, true, NoticeDigest, TopicMerged},
		{"stack merged", store.NoticeInfo, `Merged the stack t1 into main atomically with gh stack merge.`, false, NoticeDigest, TopicMerged},
		{"collapsed", store.NoticeInfo, `Collapsed stack t1: t3's PR #9 carried t1, t2 into main (squash). Closed #7, #8.`, false, NoticeDigest, TopicMerged},
		{"restacked", store.NoticeInfo, `Restacked 3 landed tasks onto origin/main at abc: 2 refs moved, 1 commits already in base dropped.`, false, NoticeDigest, TopicRestacked},
		{"left stack superseded", store.NoticeInfo, `t3 left the PR stack (superseded): replaced. Saddle won't touch its PR again.`, false, NoticeDigest, TopicLeft},
		{"left stack merged, auto-merge off", store.NoticeInfo, `t3 left the PR stack (merged): its PR merged. Saddle won't touch its PR again.`, false, NoticeDigest, TopicLeft},
		{"ci recovered", store.NoticeInfo, `CI is passing again on t5's PR #12: check "ci" passed.`, false, NoticeDigest, TopicCIGreen},
		{"ci-red hold lifted", store.NoticeInfo, `CI on t5's PR #12 is green again (or it left the stack), so the ci-red hold is lifted; prs and land carry on.`, false, NoticeDigest, TopicCIGreen},
		{"stack clean", store.NoticeInfo, `The PR stack checks clean again; the needs-human flag on t2 is lifted and prs and land work again.`, false, NoticeDigest, TopicStackClean},
		{"ci fix spawned", store.NoticeAction, `CI failed on t5's PR #12: check "ci".` + "\nt5 has landed, so I spawned t6 to fix it.", false, NoticeDigest, TopicCIFixing},
		{"ci owner told", store.NoticeAction, `CI failed on t5's PR #12: check "ci".` + "\nt5 was told to fix it.", false, NoticeDigest, TopicCIFixing},
		{"ci red hold", store.NoticeInfo, `CI is red on t5's PR #12 at abc (ci). Holding the layers above it: t6. No action needed unless that stalls.`, false, NoticeDigest, TopicCIFixing},
		{"spawned sub-task", store.NoticeInfo, `t4 spawned sub-task t9 "helper".`, false, NoticeDigest, TopicSpawned},
		{"other info", store.NoticeInfo, `Held stack t2 is 3 commits behind main.`, false, NoticeDigest, TopicOther},
		// Silent: already visible in status and the log.
		{"restacked nothing", store.NoticeInfo, `Restacked 2 landed tasks onto origin/main at abc: 0 refs moved, 0 commits already in base dropped.`, false, NoticeSilent, ""},
		{"left stack merged, auto-merge on", store.NoticeInfo, `t3 left the PR stack (merged): its PR merged. Saddle won't touch its PR again.`, true, NoticeSilent, ""},
	}
	for _, c := range cases {
		got, topic := ClassifyNotice(c.kind, c.text, c.autoMerge)
		if got != c.want || topic != c.topic {
			t.Errorf("%s: got %s/%q, want %s/%q", c.name, got, topic, c.want, c.topic)
		}
	}
}

func noticeEvents(t *testing.T, a *App, kind string) []string {
	t.Helper()
	es, err := a.Store.Events(500)
	must(t, err)
	var out []string
	for _, e := range es {
		if e.Kind == kind {
			out = append(out, e.Data)
		}
	}
	return out
}

func pendingOrch(t *testing.T, a *App) []store.Notice {
	t.Helper()
	ns, err := a.Store.PeekNotices(OrchestratorID, false)
	must(t, err)
	return ns
}

// #222: an interrupt reaches the orchestrator's queue at once, a digest item
// and a silent one don't, and every one is logged with its class.
func TestAdmitNoticeRoutesByClass(t *testing.T) {
	a, _ := setup(t)
	conflict := `Restack stopped: t2's landed commit abc conflicts with origin/main in a.go. Nothing was moved; t2 has the conflict.`
	if !a.admitNotice(OrchestratorID, store.NoticeAction, conflict) {
		t.Fatal("a conflict needing a decision was held back")
	}
	if a.admitNotice(OrchestratorID, store.NoticeInfo, `t1 "x" landed on saddle/integration at 0123456789ab.`) {
		t.Fatal("a landing interrupted the orchestrator")
	}
	if a.admitNotice(OrchestratorID, store.NoticeInfo, `Restacked 2 landed tasks onto origin/main at abc: 0 refs moved, 0 commits already in base dropped.`) {
		t.Fatal("a no-op restack interrupted the orchestrator")
	}
	if got := noticeEvents(t, a, EventNoticeInterrupt); len(got) != 1 || got[0] != conflict {
		t.Fatalf("interrupt events = %q", got)
	}
	if got := noticeEvents(t, a, EventNoticeDigest); len(got) != 1 || !strings.Contains(got[0], "landed on") {
		t.Fatalf("digest events = %q", got)
	}
	if got := noticeEvents(t, a, EventNoticeSilent); len(got) != 1 || !strings.Contains(got[0], "0 refs moved") {
		t.Fatalf("silent events = %q", got)
	}
}

// #222: worker agents keep getting their own action notices unchanged, even
// ones that would be digested for the orchestrator.
func TestAdmitNoticeLeavesWorkersAlone(t *testing.T) {
	a, _ := setup(t)
	for _, text := range []string{
		"Your branch conflicts with saddle/integration in a.go.\nFix it on your branch, commit, and call the saddle done tool again.",
		`CI is passing again on t5's PR #12: check "ci" passed.`,
		`Restacked 2 landed tasks onto origin/main at abc: 0 refs moved, 0 commits already in base dropped.`,
	} {
		for _, kind := range []string{store.NoticeAction, store.NoticeInfo} {
			if !a.admitNotice("t5", kind, text) {
				t.Fatalf("worker notice %q (%s) was held back", text, kind)
			}
			if !a.admitNotice("t5", kind, text) {
				t.Fatalf("repeated worker notice %q (%s) was silenced", text, kind)
			}
		}
	}
	for _, k := range []string{EventNoticeInterrupt, EventNoticeDigest, EventNoticeSilent} {
		if got := noticeEvents(t, a, k); len(got) != 0 {
			t.Fatalf("worker notices logged as %s: %q", k, got)
		}
	}
}

// #222: the same notice repeated while a merge queue drains is said once.
// A repeated interrupt still waiting in the queue is silenced too, but once
// delivered the next one interrupts again.
func TestAdmitNoticeSilencesRepeats(t *testing.T) {
	a, _ := setup(t)
	clean := `The PR stack checks clean again; the needs-human flag on t2 is lifted and prs and land work again.`
	for i := range 3 {
		if a.admitNotice(OrchestratorID, store.NoticeInfo, clean) {
			t.Fatalf("stack-clean #%d interrupted", i)
		}
	}
	if got := noticeEvents(t, a, EventNoticeDigest); len(got) != 1 {
		t.Fatalf("digest events = %q, want the first only", got)
	}
	if got := noticeEvents(t, a, EventNoticeSilent); len(got) != 2 {
		t.Fatalf("silent events = %q, want two repeats", got)
	}

	risk := `Stack at risk: t2's PR was force-pushed by hand. Run restack.`
	if !a.admitNotice(OrchestratorID, store.NoticeAction, risk) {
		t.Fatal("first at-risk notice held back")
	}
	must(t, a.Store.Notify(OrchestratorID, store.NoticeAction, risk))
	if a.admitNotice(OrchestratorID, store.NoticeAction, risk) {
		t.Fatal("identical at-risk notice still queued was delivered twice")
	}
	if _, err := a.Store.TakeNotices(OrchestratorID, false); err != nil {
		t.Fatal(err)
	}
	if !a.admitNotice(OrchestratorID, store.NoticeAction, risk) {
		t.Fatal("at-risk notice after the first was read was silenced")
	}
}
