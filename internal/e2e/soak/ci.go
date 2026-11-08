package soak

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/brandonapol/saddle/internal/e2e"
	"github.com/brandonapol/saddle/internal/e2e/fakegh"
)

// CheckName is the soak CI's one required check.
const CheckName = "ci"

// FlakyReports is how many times a flaky change's CI fails on a head
// before it passes there.
const FlakyReports = 2

// CI plays GitHub Actions for the soak: each open PR's head is checked out
// and built, and its "ci" check set from the build and the red markers.
// While RedBase is on, every PR is red with main's failure.
type CI struct {
	RedBase bool
	// Flaky reports whether a PR head branch is a flaky change's.
	Flaky func(branch string) bool

	w      *e2e.World
	logf   func(string, ...any)
	graded map[string]string // "pr@head" -> the verdict key last set
	flakes map[string]int    // "pr@head" -> failed reports so far
	tmp    string
}

// NewCI returns w's CI.
func NewCI(w *e2e.World, logf func(string, ...any)) *CI {
	dir := filepath.Join(w.Root, "soak-ci")
	must(w.T, os.MkdirAll(dir, 0o755))
	return &CI{w: w, logf: logf, graded: map[string]string{}, flakes: map[string]int{}, tmp: dir}
}

// Grade reports CI on every open PR whose head or base state changed, and
// re-reports flaky ones.
func (c *CI) Grade() {
	s, err := c.w.GH.Load()
	if err != nil {
		c.logf("ci: %v", err)
		return
	}
	type report struct {
		n     int
		check fakegh.Check
	}
	var reps []report
	for _, p := range s.Open() {
		head := strings.TrimSpace(gitOut(c.w.Origin, "rev-parse", "--verify", "-q", "refs/heads/"+p.Head))
		if head == "" {
			continue
		}
		key := fmt.Sprintf("%d@%s", p.Number, head)
		state, log := c.build(head)
		verdict := state
		if c.RedBase {
			state, log, verdict = fakegh.Fail, "--- FAIL: TestMainIsRed (red on main too)", "redbase"
		} else if state == fakegh.Pass && c.Flaky != nil && c.Flaky(p.Head) && c.flakes[key] < FlakyReports {
			c.flakes[key]++
			state, log = fakegh.Fail, "--- FAIL: TestFlaky (timed out talking to the test server)"
			verdict = fmt.Sprintf("flake%d", c.flakes[key])
		}
		if c.graded[key] == verdict && !c.hasNoCheck(p) {
			continue
		}
		c.graded[key] = verdict
		reps = append(reps, report{p.Number, fakegh.Check{Name: CheckName, Workflow: "CI", State: state,
			Log: "CI\tgo test\t2026-11-07T10:00:00.0000000Z " + strings.ReplaceAll(strings.TrimSpace(log), "\n", "\nCI\tgo test\t2026-11-07T10:00:00.0000000Z ")}})
	}
	if len(reps) == 0 {
		return
	}
	err = c.w.GH.Update(func(s *fakegh.State) error {
		for _, r := range reps {
			p := s.PR(r.n)
			if p == nil {
				continue
			}
			var keep []fakegh.Check
			for _, ch := range p.Checks {
				if ch.Name != CheckName {
					keep = append(keep, ch)
				}
			}
			p.Checks = append(keep, r.check)
		}
		return nil
	})
	if err != nil {
		c.logf("ci: %v", err)
	}
}

func (c *CI) hasNoCheck(p *fakegh.PR) bool {
	for _, ch := range p.Checks {
		if ch.Name == CheckName && ch.State != fakegh.Pending {
			return false
		}
	}
	return true
}

// build checks out head, builds it and looks for red markers.
func (c *CI) build(head string) (state, log string) {
	dir := filepath.Join(c.tmp, head)
	if _, err := os.Stat(dir); err != nil {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fakegh.Fail, err.Error()
		}
		cmd := exec.Command("sh", "-c", "git --git-dir="+shq(c.w.Origin)+" archive "+head+" | tar -x -C "+shq(dir))
		if out, err := cmd.CombinedOutput(); err != nil {
			return fakegh.Fail, "checkout: " + string(out)
		}
	}
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir, cmd.Env = dir, append(c.w.Env(), "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fakegh.Fail, "go build ./...: " + string(out)
	}
	if fails := GradeMarkers(dir); fails != "" {
		return fakegh.Fail, fails
	}
	return fakegh.Pass, ""
}

func gitOut(dir string, args ...string) string {
	out, _ := exec.Command("git", append([]string{"--git-dir=" + dir}, args...)...).Output()
	return string(out)
}
