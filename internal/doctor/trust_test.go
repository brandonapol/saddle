package doctor

import (
	"errors"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/trust"
)

// trustEnv adds a trust decision to the fake.
type trustEnv struct {
	*fakeEnv
	rep       trust.Report
	inherited bool
	err       error
}

func (e trustEnv) Trust() (trust.Report, bool, error) { return e.rep, e.inherited, e.err }

func trustResult(t *testing.T, env Env) (Result, bool) {
	t.Helper()
	for _, r := range Run(env) {
		if r.Name == CheckTrust {
			return r, true
		}
	}
	return Result{}, false
}

func TestTrustCheck(t *testing.T) {
	base := healthy(t)
	repo := trust.Repo{Path: "/src/demo", Origin: "git@github.com:o/demo.git"}
	for _, tc := range []struct {
		name   string
		env    trustEnv
		status Status
		detail string
		fix    string
	}{
		{"trusted", trustEnv{fakeEnv: base, rep: trust.Report{Repo: repo, State: trust.StateTrusted}}, OK, "trusted", ""},
		{"untrusted", trustEnv{fakeEnv: base, rep: trust.Report{Repo: repo, State: trust.StateUntrusted}}, Fail, "not trusted", "saddle trust"},
		{"origin changed", trustEnv{fakeEnv: base, rep: trust.Report{Repo: repo, State: trust.StateOriginChanged,
			Recorded: trust.Entry{Repo: trust.Repo{Path: repo.Path, Origin: "x"}}}}, Fail, "origin changed", "saddle trust"},
		{"agent", trustEnv{fakeEnv: base, rep: trust.Report{Repo: repo, State: trust.StateUntrusted}, inherited: true}, OK, "SADDLE_TASK", ""},
		{"unreadable", trustEnv{fakeEnv: base, err: errors.New("corrupt")}, Fail, "corrupt", "saddle trust"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := trustResult(t, tc.env)
			if !ok {
				t.Fatal("no trust check")
			}
			if r.Status != tc.status || !strings.Contains(r.Detail, tc.detail) || !strings.Contains(r.Fix, tc.fix) {
				t.Fatalf("got %+v, want %s with %q / fix %q", r, tc.status, tc.detail, tc.fix)
			}
		})
	}
	// Envs that can't tell (older fakes) skip it.
	if _, ok := trustResult(t, base); ok {
		t.Fatal("trust check ran on an env without Trust")
	}
}
