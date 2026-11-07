package doctor

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
)

// CheckInvariants lists the open violations of ADR 0004's invariants that
// the sentinel's invariant monitor found (#825).
const CheckInvariants = "invariants"

const invariantsAbout = "the invariants of ADR 0004 (work done means on base, PR bases, no latches, every state has an exit, no self-repair, one spawn policy), checked every sentinel tick"

// InvariantsEnv is an Env that can list open invariant violations. The
// check is skipped for envs without it.
type InvariantsEnv interface {
	ViolationLines() ([]string, error)
}

func invariantsCheck(ie InvariantsEnv) Result {
	ls, err := ie.ViolationLines()
	if err != nil {
		return warn(CheckInvariants, "could not read: "+err.Error(), "")
	}
	if len(ls) == 0 {
		return ok(CheckInvariants, "no open violations")
	}
	return warn(CheckInvariants, strings.Join(ls, "; "),
		"saddle only detects these for now; each was reported to the orchestrator once and clears by itself once fixed")
}

// ViolationLines lists the open invariant violations. An uninitialized
// repo has none.
func (s system) ViolationLines() ([]string, error) {
	if _, err := os.Stat(filepath.Join(string(s), ".saddle", "state.db")); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	a, err := app.Open(string(s))
	if err != nil {
		return nil, err
	}
	defer a.Close()
	vs, err := a.OpenViolations()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range vs {
		out = append(out, v.String())
	}
	return out, nil
}
