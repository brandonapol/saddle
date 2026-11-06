package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
)

// CheckLogins is whether the agent CLIs' logins work, by a real request
// (#798), and whether an adapter saddle took out until cleared (logged out,
// out of credits) can come back.
const CheckLogins = "agent logins"

// LoginOut is an adapter saddle marked out with no timer.
type LoginOut struct {
	Adapter string
	Reason  string // app.ReasonLogin or app.ReasonCredits
}

// LoginsEnv is an Env that can probe the agent CLIs' logins and put back
// adapters saddle marked out. The check is skipped for envs without it.
type LoginsEnv interface {
	OutUntilCleared() ([]LoginOut, error)
	// ProbeAuth makes one real request through adapter name's CLI:
	// agent.ProbeAuth's answers.
	ProbeAuth(cfg config.Config, name string) error
	// ClearOut puts adapter name back for spawns.
	ClearOut(name string) error
}

// clearable reports whether a probe's answer lets --fix put an adapter
// back: anything but a CLI that says its login is dead. An adapter with no
// probe is the owner's word; if it is still out, its next banner says so.
func clearable(probe error) bool { return !errors.Is(probe, agent.ErrLoggedOut) }

// logins probes the harness and every adapter marked out until cleared.
func (r *run) logins(le LoginsEnv) Result {
	outs, err := le.OutUntilCleared()
	if err != nil {
		return warn(CheckLogins, "could not read the adapters saddle marked out: "+err.Error(), "check .saddle/state.db")
	}
	var back, dead, notes []string
	marked := map[string]bool{}
	for _, o := range outs {
		marked[o.Adapter] = true
		why := "logged out"
		if o.Reason == app.ReasonCredits {
			why = "out of credits"
		}
		probe := le.ProbeAuth(r.cfg, o.Adapter)
		switch {
		case !clearable(probe):
			dead = append(dead, fmt.Sprintf("%s (marked %s; still: %v)", o.Adapter, why, probe))
		case probe == nil:
			back = append(back, fmt.Sprintf("%s (marked %s; its login works now)", o.Adapter, why))
		default:
			back = append(back, fmt.Sprintf("%s (marked %s; not probed: %v)", o.Adapter, why, probe))
		}
	}
	h := r.cfg.Harness
	if !marked[h] {
		switch probe := le.ProbeAuth(r.cfg, h); {
		case !clearable(probe):
			dead = append(dead, fmt.Sprintf("%s (harness: %v)", h, probe))
		case probe == nil:
			notes = append(notes, h+"'s login works")
		case errors.Is(probe, agent.ErrNoProbe):
			notes = append(notes, h+" has no login probe")
		default:
			notes = append(notes, fmt.Sprintf("%s's probe failed: %v", h, probe))
		}
	}
	if len(back) == 0 && len(dead) == 0 {
		return ok(CheckLogins, strings.Join(notes, "; "))
	}
	var detail, fix []string
	if len(dead) > 0 {
		detail = append(detail, "logged out: "+strings.Join(dead, ", "))
		for _, d := range dead {
			name, _, _ := strings.Cut(d, " ")
			fix = append(fix, "run "+agent.LoginHint(name))
		}
		fix[len(fix)-1] += ", then saddle doctor --fix"
	}
	if len(back) > 0 {
		detail = append(detail, "can come back: "+strings.Join(back, ", "))
		fix = append(fix, fixInit+" to put them back for spawns")
	}
	res := warn(CheckLogins, strings.Join(detail, "; "), strings.Join(fix, "; "))
	if len(dead) > 0 {
		res.Status = Fail
	}
	res.Fixable = len(back) > 0
	return res
}

// fixLogins puts back every adapter marked out whose probe allows it.
func fixLogins(env Env, le LoginsEnv) error {
	cfg, _ := env.Config()
	outs, err := le.OutUntilCleared()
	if err != nil {
		return err
	}
	var errs []error
	for _, o := range outs {
		if clearable(le.ProbeAuth(cfg, o.Adapter)) {
			errs = append(errs, le.ClearOut(o.Adapter))
		}
	}
	return errors.Join(errs...)
}

// OutUntilCleared reads the adapters marked out with no timer.
func (p probing) OutUntilCleared() ([]LoginOut, error) {
	if _, err := os.Stat(filepath.Join(p.Root(), ".saddle", "state.db")); err != nil {
		return nil, nil
	}
	a, err := app.Open(p.Root())
	if err != nil {
		return nil, err
	}
	defer a.Close()
	var out []LoginOut
	for _, e := range a.OutUntilCleared() {
		out = append(out, LoginOut{Adapter: e.Adapter, Reason: e.Reason})
	}
	return out, nil
}

// ProbeAuth probes name's CLI as configured.
func (p probing) ProbeAuth(cfg config.Config, name string) error {
	return agent.ProbeAuth(name, AgentCmds(cfg)[name])
}

// ClearOut puts name back for spawns.
func (p probing) ClearOut(name string) error {
	a, err := app.Open(p.Root())
	if err != nil {
		return err
	}
	defer a.Close()
	return a.ClearExhausted(name)
}
