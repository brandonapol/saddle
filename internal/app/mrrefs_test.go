package app

import (
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// #652: commands that take a task, PR URL or number find a GitLab MR by
// "!N", its number or its URL (with a page after it), not only GitHub's
// /pull/N.
func TestTaskByRefGitLabMR(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	mr := "https://gitlab.com/g/r/-/merge_requests/"
	ciTask(t, a, "t1", store.Landed, mr+"1")
	ciTask(t, a, "t2", store.Landed, mr+"12")
	ciTask(t, a, "t3", store.Landed, "https://github.com/o/r/pull/3")
	for ref, want := range map[string]string{
		"t1":                                  "t1",
		mr + "12":                             "t2",
		mr + "12/diffs":                       "t2",
		"!12":                                 "t2",
		"12":                                  "t2",
		"#12":                                 "t2",
		" !1 ":                                "t1",
		"#3":                                  "t3",
		"https://github.com/o/r/pull/3/files": "t3",
	} {
		got, err := a.taskByRef(ref)
		if err != nil || got.ID != want {
			t.Errorf("taskByRef(%q) = %q, %v; want %s", ref, got.ID, err, want)
		}
	}
	for _, ref := range []string{"!3", "!2", "#99", mr + "99"} {
		if got, err := a.taskByRef(ref); err == nil {
			t.Errorf("taskByRef(%q) = %s, want no task", ref, got.ID)
		}
	}
}

// #652: a task's label names its MR as GitLab does, "!12".
func TestTaskLabelGitLabMR(t *testing.T) {
	t.Parallel()
	for pr, want := range map[string]string{
		"https://gitlab.com/g/r/-/merge_requests/12": "t1 (!12)",
		"https://github.com/o/r/pull/12":             "t1 (#12)",
		"#12":                                        "t1 (#12)",
		"":                                           "t1",
	} {
		if got := taskLabel(store.Task{ID: "t1", PR: pr}); got != want {
			t.Errorf("taskLabel(PR %q) = %q, want %q", pr, got, want)
		}
	}
}

// #652: the notice digest names merged GitLab MRs "!12", not by full URL.
func TestDigestNamesGitLabMRs(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	a.Cfg.Notices.DigestEvery = 15 * time.Minute
	for _, text := range []string{
		`Auto-merged https://gitlab.com/g/r/-/merge_requests/12 (t1) into main and restacked the rest of stack t1.`,
		`Auto-merged https://gitlab.com/g/r/-/merge_requests/13 (t2) into main and restacked the rest of stack t2.`,
	} {
		if a.admitNotice(OrchestratorID, store.NoticeInfo, text) {
			t.Fatalf("%q interrupted", text)
		}
	}
	sent, err := a.FlushDigest(time.Now().Add(16 * time.Minute))
	must(t, err)
	if !strings.Contains(sent, "2 PRs merged (!12 !13)") {
		t.Fatalf("digest %q doesn't name the MRs !12 !13", sent)
	}
}
