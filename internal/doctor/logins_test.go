package doctor

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
)

// loginsEnv is a healthy env with LoginsEnv: adapters saddle marked out
// until cleared, and what a probe of each says.
type loginsEnv struct {
	*fakeEnv
	out     []LoginOut
	probes  map[string]error
	probed  []string
	cleared []string
}

func (e *loginsEnv) OutUntilCleared() ([]LoginOut, error) { return e.out, nil }
func (e *loginsEnv) ProbeAuth(_ config.Config, name string) error {
	e.probed = append(e.probed, name)
	if err, ok := e.probes[name]; ok {
		return err
	}
	return agent.ErrNoProbe
}
func (e *loginsEnv) ClearOut(name string) error {
	e.cleared = append(e.cleared, name)
	for i, o := range e.out {
		if o.Adapter == name {
			e.out = append(e.out[:i], e.out[i+1:]...)
			break
		}
	}
	return nil
}

func findResult(rs []Result, name string) (Result, bool) {
	for _, r := range rs {
		if r.Name == name {
			return r, true
		}
	}
	return Result{}, false
}

// #798: doctor probes real auth, not a credentials file: a harness whose
// probe gets a 401 fails with how to log in.
func TestLoginsProbeFindsDeadLogin(t *testing.T) {
	e := &loginsEnv{fakeEnv: healthy(t), probes: map[string]error{"claude": fmt.Errorf("%w: Please run /login", agent.ErrLoggedOut)}}
	r, ok := findResult(Run(e), CheckLogins)
	if !ok || r.Status != Fail || !strings.Contains(r.Fix, agent.LoginHint("claude")) {
		t.Fatalf("logins = %+v, want a failure with the login hint", r)
	}
}

// #798: an adapter saddle marked logged out whose probe now passes is
// fixable, and doctor --fix puts it back; one still logged out stays out.
func TestLoginsFixClearsAdapterThatLoggedBackIn(t *testing.T) {
	e := &loginsEnv{
		fakeEnv: healthy(t),
		out:     []LoginOut{{Adapter: "claude", Reason: "login"}, {Adapter: "codex", Reason: "login"}},
		probes:  map[string]error{"claude": nil, "codex": agent.ErrLoggedOut},
	}
	r, _ := findResult(Run(e), CheckLogins)
	if r.Status != Fail || !r.Fixable || !strings.Contains(r.Detail, "claude") || !strings.Contains(r.Detail, "codex") {
		t.Fatalf("logins = %+v", r)
	}
	if _, err := Fix(e, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(e.cleared) != 1 || e.cleared[0] != "claude" {
		t.Fatalf("cleared %v, want only claude", e.cleared)
	}
}

// #798: an adapter out of credits with no probe is cleared by --fix: the
// owner says it is back, and the next banner marks it out again if not.
func TestLoginsFixClearsUnprobedAdapter(t *testing.T) {
	e := &loginsEnv{fakeEnv: healthy(t), out: []LoginOut{{Adapter: "grok", Reason: "credits"}}}
	r, _ := findResult(Run(e), CheckLogins)
	if r.Status == OK || !r.Fixable {
		t.Fatalf("logins = %+v, want fixable", r)
	}
	rs, err := Fix(e, func() error { return errors.New("init must not be needed") })
	if err != nil {
		t.Fatal(err)
	}
	if len(e.cleared) != 1 || e.cleared[0] != "grok" {
		t.Fatalf("cleared %v", e.cleared)
	}
	if r, _ := findResult(rs, CheckLogins); !r.Fixed {
		t.Fatalf("after --fix: %+v, want fixed", r)
	}
}
