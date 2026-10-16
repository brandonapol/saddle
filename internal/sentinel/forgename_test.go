package sentinel

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/config"
)

// gitlabGH is the sentinel's GitHub saying it is GitLab.
type gitlabGH struct{ GitHub }

func (gitlabGH) Provider() string { return config.ForgeGitLab }

// #667: on GitLab the conflict cause names GitLab and its MR, not GitHub.
func TestSentinelConflictNamesGitLab(t *testing.T) {
	a, _ := setup(t)
	gh := newFakeGH(t)
	t1 := landTask(t, a, "t1", "one")
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	t1, _ = a.Store.Task(t1.ID)
	gh.setPR(t1.PR, "OPEN", "CONFLICTING")
	s := New(a)
	s.GH = gitlabGH{s.GH}
	rep, err := s.Check()
	must(t, err)
	if want := "GitLab reports its MR " + t1.PR + " conflicts with its base"; !rep.AtRisk || !strings.Contains(rep.Cause, want) {
		t.Fatalf("report = %+v, want cause %q", rep, want)
	}
}
