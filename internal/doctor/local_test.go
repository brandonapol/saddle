package doctor

import (
	"errors"
	"strings"
	"testing"
)

// slowSpy fails the test on any call RunLocal must not make: GitHub,
// a push, the gh CLI, the leftovers scan or the scratch walk.
type slowSpy struct {
	*fakeEnv
	t *testing.T
}

func (s slowSpy) GH(args ...string) (string, error) {
	s.t.Errorf("RunLocal called gh %s", strings.Join(args, " "))
	return s.fakeEnv.GH(args...)
}

func (s slowSpy) Glab(args ...string) (string, error) {
	s.t.Errorf("RunLocal called glab %s", strings.Join(args, " "))
	return s.fakeEnv.Glab(args...)
}

func (s slowSpy) Exec(name string, args ...string) (string, error) {
	if name == "gh" || name == "glab" {
		s.t.Errorf("RunLocal ran gh %s", strings.Join(args, " "))
	}
	return s.fakeEnv.Exec(name, args...)
}

func (s slowSpy) TrainGit(args ...string) (string, error) {
	s.t.Errorf("RunLocal ran git %s as the train", strings.Join(args, " "))
	return s.fakeEnv.TrainGit(args...)
}

func (s slowSpy) Leftovers() (int, int, error) {
	s.t.Error("RunLocal counted leftovers")
	return s.fakeEnv.Leftovers()
}

func (s slowSpy) Scratch() (ScratchReport, error) {
	s.t.Error("RunLocal measured scratch")
	return ScratchReport{}, nil
}

// TestRunLocalSkipsTheSlowChecks: saddle up blocks on RunLocal before its
// first frame (#635), so it makes no network call and walks no branches;
// Run still makes every check, the slow ones included.
func TestRunLocalSkipsTheSlowChecks(t *testing.T) {
	f := healthy(t)
	f.git["symbolic-ref --short refs/remotes/origin/HEAD"] = res{err: errors.New("no remote HEAD")}
	rs := RunLocal(slowSpy{f, t})
	got := map[string]bool{}
	for _, r := range rs {
		got[r.Name] = true
		if Slow(r.Name) {
			t.Errorf("RunLocal reported slow check %q", r.Name)
		}
	}
	for _, name := range []string{CheckConfig, CheckRemote, CheckDefaultBranch, CheckTestCmd, CheckTmux, CheckClaude, CheckHooks, CheckOrchAllow, CheckGate, CheckIgnored, CheckStateDB, CheckInhibit} {
		if !got[name] {
			t.Errorf("RunLocal skipped local check %q", name)
		}
	}
	full := map[string]bool{}
	for _, r := range Run(healthy(t)) {
		full[r.Name] = true
	}
	for name := range slowChecks {
		if name == CheckScratch || name == CheckSkills {
			continue // healthy's env has neither
		}
		if !full[name] {
			t.Errorf("Run skipped %q", name)
		}
	}
}
