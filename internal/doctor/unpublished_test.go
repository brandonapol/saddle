package doctor

import (
	"errors"
	"strings"
	"testing"
)

type unpublishedEnv struct {
	ids []string
	err error
}

func (e unpublishedEnv) Unpublished() ([]string, error) { return e.ids, e.err }

// #819: a landed task with no PR is an alarm in doctor too: its work is not
// on base, so nobody may call it done.
func TestUnpublishedCheckFlagsLandedTasksWithoutPR(t *testing.T) {
	if r := unpublishedCheck(unpublishedEnv{}); r.Status != OK {
		t.Fatalf("no landed tasks: %+v", r)
	}
	r := unpublishedCheck(unpublishedEnv{ids: []string{"t1", "t2"}})
	if r.Status != Warn || !strings.Contains(r.Detail, "t1, t2") || !strings.Contains(r.Detail, "not on base") || !strings.Contains(r.Fix, "saddle prs") {
		t.Fatalf("PR-less landed tasks: %+v", r)
	}
	if r := unpublishedCheck(unpublishedEnv{err: errors.New("locked")}); r.Status != Warn {
		t.Fatalf("unreadable: %+v", r)
	}
	if about[CheckUnpublished] == "" {
		t.Fatal("no about text for the check")
	}
}
