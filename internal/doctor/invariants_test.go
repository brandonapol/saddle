package doctor

import (
	"errors"
	"strings"
	"testing"
)

type invariantsEnv struct {
	lines []string
	err   error
}

func (e invariantsEnv) ViolationLines() ([]string, error) { return e.lines, e.err }

// #825: open invariant violations are a line in doctor.
func TestInvariantsCheckListsOpenViolations(t *testing.T) {
	if r := invariantsCheck(invariantsEnv{}); r.Status != OK {
		t.Fatalf("none open: %+v", r)
	}
	r := invariantsCheck(invariantsEnv{lines: []string{"pr_base_wrong t397: #814 targets main"}})
	if r.Status != Warn || !strings.Contains(r.Detail, "pr_base_wrong t397") {
		t.Fatalf("open: %+v", r)
	}
	if r := invariantsCheck(invariantsEnv{err: errors.New("locked")}); r.Status != Warn {
		t.Fatalf("unreadable: %+v", r)
	}
	if about[CheckInvariants] == "" {
		t.Fatal("no about text for the check")
	}
}
