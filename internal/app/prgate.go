package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// The pre-publish gate (#223). Each task passed its checks at its own tip in
// its worktree, but CI checks every PR head on its own, and a layer's PR head
// is not what its agent checked: the train re-cut it, prLayout may have
// replayed it onto another layer, and a word a later commit adds to the
// spelling allowlist is missing below it. So before prs or publish pushes a
// layer, saddle checks out that layer's own head in a scratch worktree and
// runs the repo's checks there: [train] lint.cmd, [test] cmd and [train]
// prepublish.cmd, bottom to top. A red layer is not published and neither is
// any layer above it in its stack (the ci-red hold, see above in cired.go);
// the layers below it are. Restack re-runs only prepublish.cmd on the layers
// it re-cut, since reordering changes what each tip holds.
//
// The escape hatches: fix the layer (or reorder the stack so what it needs
// comes first) and run prs again, unstack it, or set prepublish.off = true.

// GateCheck is one check the gate runs on a layer.
type GateCheck struct {
	Name string `json:"name"` // lint, test or prepublish
	Cmd  string `json:"cmd"`
}

// GateRed is a layer whose own head failed a check.
type GateRed struct {
	Task     string    `json:"task"`
	Head     string    `json:"head"`
	Check    GateCheck `json:"check"`
	Tail     string    `json:"tail"` // the last lines of the check's output
	TimedOut bool      `json:"timed_out,omitempty"`
	Source   string    `json:"source"` // prs, publish or restack
	Since    time.Time `json:"since"`
	// Held are the stacked layers above it that were not published for it.
	Held []string `json:"held,omitempty"`
}

// GateState is what the gate knows, saved under .saddle.
type GateState struct {
	Red []GateRed `json:"red,omitempty"`
	// Passed caches green checks by tree and command, so publishing the same
	// heads again runs nothing.
	Passed map[string]time.Time `json:"passed,omitempty"`
}

// gateTailLines is how much of a red check's output the gate reports.
const gateTailLines = 20

// gatePassedTTL is how long a green result is trusted.
const gatePassedTTL = 7 * 24 * time.Hour

// Gate sources.
const (
	GateSourcePRs     = "prs"
	GateSourcePublish = "publish"
	GateSourceRestack = "restack"
)

func (a *App) gatePath() string { return a.stateDir("prgate.json") }

// Gate reads the gate's state; empty when it never ran.
func (a *App) Gate() (GateState, error) {
	var s GateState
	b, err := os.ReadFile(a.gatePath())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", a.gatePath(), err)
	}
	return s, nil
}

func (a *App) setGate(s GateState) error {
	cut := time.Now().Add(-gatePassedTTL)
	for k, t := range s.Passed {
		if t.Before(cut) {
			delete(s.Passed, k)
		}
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.gatePath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.gatePath())
}

// GateChecks are the checks the gate runs on each layer: the repo's lint
// gate, [test] cmd and prepublish.cmd, each once. cheap keeps only
// prepublish.cmd, which restack runs. None when the gate is off.
func (a *App) GateChecks(cheap bool) []GateCheck {
	p := a.Cfg.Train.Prepublish
	if p.Off {
		return nil
	}
	var out []GateCheck
	add := func(name, cmd string) {
		cmd = strings.TrimSpace(cmd)
		if cmd == "" || cmd == NoTestCmd || slices.ContainsFunc(out, func(c GateCheck) bool { return c.Cmd == cmd }) {
			return
		}
		out = append(out, GateCheck{Name: name, Cmd: cmd})
	}
	if !cheap {
		add("lint", a.LintGate().Cmd)
		add("test", a.Cfg.Test.Cmd)
	}
	add("prepublish", p.Cmd)
	return out
}

// gateJob is one layer for the gate: index i of the stack, at head.
type gateJob struct {
	i    int
	task string
	head string
}

// gateResult is a red job, nil for a green or skipped one.
type gateResult struct {
	job      gateJob
	check    GateCheck
	out      string
	timedOut bool
}

// runGate checks each job's head with checks, in the order given. A job
// skip says to leave alone (a red layer is below it) is not checked. Up to
// prepublish.parallel jobs run at once, each worker in its own scratch
// worktree, reused from run to run. It returns the red jobs by index.
func (a *App) runGate(jobs []gateJob, checks []GateCheck, skip func(i int, red map[int]gateResult) bool) (map[int]gateResult, error) {
	red := map[int]gateResult{}
	if len(jobs) == 0 || len(checks) == 0 {
		return red, nil
	}
	s, err := a.Gate()
	if err != nil {
		return nil, err
	}
	if s.Passed == nil {
		s.Passed = map[string]time.Time{}
	}
	workers := min(max(a.Cfg.Train.Prepublish.Parallel, 1), len(jobs))
	var mu sync.Mutex
	next := 0
	var errs []error
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dir := ""
			defer func() {
				if dir != "" {
					_ = gitx.WorktreeRemove(a.Root, dir)
				}
			}()
			for {
				mu.Lock()
				if next >= len(jobs) {
					mu.Unlock()
					return
				}
				j := jobs[next]
				next++
				if skip != nil && skip(j.i, red) {
					mu.Unlock()
					continue
				}
				mu.Unlock()
				var r *gateResult
				var passed []string
				var err error
				if dir == "" {
					dir, err = a.gateWorktree(w)
				}
				if err == nil {
					r, passed, err = a.gateLayer(dir, j, checks, s.Passed, &mu)
				}
				mu.Lock()
				if err != nil {
					errs = append(errs, err)
				} else if r != nil {
					red[j.i] = *r
				}
				for _, k := range passed {
					s.Passed[k] = time.Now().UTC()
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	fresh, err := a.Gate() // the red layers may have changed while checks ran
	if err != nil {
		return nil, err
	}
	fresh.Passed = s.Passed
	return red, a.setGate(fresh)
}

// gateLayer runs checks on j's head in dir, stopping at the first red one.
// It returns that failure, or nil, and the cache keys of the checks that
// passed. Checks the cache knows passed on the same tree are not run.
func (a *App) gateLayer(dir string, j gateJob, checks []GateCheck, passed map[string]time.Time, mu *sync.Mutex) (*gateResult, []string, error) {
	tree, err := gitx.Run(a.Root, "rev-parse", j.head+"^{tree}")
	if err != nil {
		return nil, nil, err
	}
	var ok []string
	checkedOut := false
	for _, c := range checks {
		key := tree + " " + c.Cmd
		mu.Lock()
		_, cached := passed[key]
		mu.Unlock()
		if cached {
			continue
		}
		if !checkedOut {
			if err := gateCheckout(dir, j.head); err != nil {
				return nil, nil, err
			}
			checkedOut = true
		}
		out, timedOut, err := runGateCmd(dir, c.Cmd, a.Cfg.Train.Prepublish.Timeout)
		if err == nil {
			ok = append(ok, key)
			continue
		}
		a.Store.Event(j.task, "prepublish_red", fmt.Sprintf("%s %s: %s", short(j.head), c.Name, c.Cmd))
		return &gateResult{job: j, check: c, out: out, timedOut: timedOut}, ok, nil
	}
	return nil, ok, nil
}

// gateWorktree makes worker w's scratch worktree. A worker reuses it for
// every layer it checks in one run, each a checkout away, so build output
// the repo ignores carries over; the run removes it at the end.
func (a *App) gateWorktree(w int) (string, error) {
	dir := a.stateDir("prgate", strconv.Itoa(w))
	_ = os.RemoveAll(dir)
	_, _ = gitx.Run(a.Root, "worktree", "prune")
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	if _, err := gitx.Run(a.Root, "worktree", "add", "--detach", dir, "HEAD"); err != nil {
		return "", err
	}
	return dir, nil
}

// gateCheckout puts dir at head with nothing left over from the last layer
// but ignored files.
func gateCheckout(dir, head string) error {
	if _, err := gitx.Run(dir, "checkout", "-q", "--force", "--detach", head); err != nil {
		return err
	}
	_, err := gitx.Run(dir, "clean", "-q", "-fd")
	return err
}

// runGateCmd runs cmd in dir under sh, killing it and everything it started
// when it runs past timeout.
func runGateCmd(dir, cmd string, timeout time.Duration) (string, bool, error) {
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Dir = dir
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = 5 * time.Second
	out, err := c.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out) + fmt.Sprintf("\n(killed after %s: [train] prepublish.timeout)", timeout), true, ctx.Err()
	}
	return string(out), false, err
}

// prGate is the publish hook: it checks every layer prs would publish (in
// order, the layout's order, skipping those want leaves out) at its own head
// and returns the layers to hold, each mapped to the red layer it is held
// for, the red layer included, with an error naming each red layer and
// check. It records what it found, so status and the stuck-stack alarm see
// it, and drops red records the run proved green.
func (a *App) prGate(landed []landedTask, layout []prLayer, order []int, want func(int) bool) (map[int]string, error) {
	checks := a.GateChecks(false)
	if len(checks) == 0 {
		return nil, a.forgetGate(nil, GateSourcePRs, GateSourceRestack)
	}
	var jobs []gateJob
	for _, i := range order {
		if want(i) {
			jobs = append(jobs, gateJob{i: i, task: landed[i].ID, head: layout[i].Head})
		}
	}
	redBelow := func(i int, red map[int]gateResult) bool {
		for r := range red {
			if above(layout, i, r) {
				return true
			}
		}
		return false
	}
	red, err := a.runGate(jobs, checks, redBelow)
	if err != nil {
		return nil, err
	}
	held := map[int]string{}
	var reds []GateRed
	for _, i := range order {
		r, ok := red[i]
		if !ok || redBelow(i, red) {
			continue
		}
		g := GateRed{Task: landed[i].ID, Head: layout[i].Head, Check: r.check, Tail: tail(r.out, gateTailLines),
			TimedOut: r.timedOut, Source: GateSourcePRs}
		held[i] = g.Task
		for j := range landed {
			if j != i && above(layout, j, i) {
				held[j] = g.Task
				g.Held = append(g.Held, landed[j].ID)
			}
		}
		reds = append(reds, g)
	}
	checked := map[string]bool{}
	for _, j := range jobs {
		checked[j.task] = true
	}
	if err := a.recordGate(reds, checked, GateSourcePRs); err != nil {
		return nil, err
	}
	return held, gateErr(reds, landed, held)
}

// recordGate saves reds and drops the older red records of the tasks
// checked (or no longer stacked) that this run found green.
func (a *App) recordGate(reds []GateRed, checked map[string]bool, source string) error {
	s, err := a.Gate()
	if err != nil {
		return err
	}
	stack, err := a.landedStack()
	if err != nil {
		return err
	}
	inStack := map[string]bool{}
	for _, l := range stack {
		inStack[l.ID] = true
	}
	now := time.Now().UTC()
	var next []GateRed
	for _, r := range s.Red {
		if !inStack[r.Task] || checked[r.Task] && !slices.ContainsFunc(reds, func(g GateRed) bool { return g.Task == r.Task }) {
			if inStack[r.Task] {
				a.Store.Event(r.Task, "prepublish_green", short(r.Head))
			}
			continue
		}
		next = append(next, r)
	}
	for _, g := range reds {
		g.Since, g.Source = now, source
		if i := slices.IndexFunc(next, func(r GateRed) bool { return r.Task == g.Task }); i >= 0 {
			g.Since = next[i].Since
			next[i] = g
			continue
		}
		next = append(next, g)
	}
	s.Red = next
	return a.setGate(s)
}

// forgetGate drops every red record from the given sources: the gate is off,
// so nothing it found holds anything back.
func (a *App) forgetGate(keep func(GateRed) bool, sources ...string) error {
	s, err := a.Gate()
	if err != nil || len(s.Red) == 0 {
		return err
	}
	n := len(s.Red)
	s.Red = slices.DeleteFunc(s.Red, func(r GateRed) bool {
		return slices.Contains(sources, r.Source) && (keep == nil || !keep(r))
	})
	if len(s.Red) == n {
		return nil
	}
	return a.setGate(s)
}

// gateErr explains the layers the gate held: for each red layer, its check,
// command and output tail, what was held above it and the ways out.
func gateErr(reds []GateRed, landed []landedTask, held map[int]string) error {
	if len(reds) == 0 {
		return nil
	}
	var b strings.Builder
	var pub []string
	for i, l := range landed {
		if _, ok := held[i]; !ok {
			pub = append(pub, l.ID)
		}
	}
	for _, g := range reds {
		above := "nothing above it"
		if len(g.Held) > 0 {
			above = "the layers above it (" + strings.Join(g.Held, ", ") + ")"
		}
		fmt.Fprintf(&b, "pre-publish gate: layer %s's own tip %s fails its %s check (`%s`), so %s and %s were not published.\n",
			g.Task, short(g.Head), g.Check.Name, g.Check.Cmd, g.Task, above)
		if g.TimedOut {
			b.WriteString("It ran past [train] prepublish.timeout.\n")
		}
		fmt.Fprintf(&b, "Last %d lines:\n%s\n", gateTailLines, g.Tail)
	}
	if len(pub) > 0 {
		fmt.Fprintf(&b, "Every other layer was checked and published as usual.\n")
	}
	b.WriteString("CI checks each PR head on its own, so each layer must pass at its own tip. " +
		"Fix the red layer's own commits (or reorder the stack so the commit it needs comes first) and run prs again; " +
		"`saddle unstack <task>` drops it, and [train] prepublish.off = true turns the gate off")
	return errors.New(b.String())
}

// gatePublish checks the head publish is about to push, already checked out
// in dir, and refuses when a check is red.
func (a *App) gatePublish(dir, task, target, head string) error {
	checks := a.GateChecks(false)
	if len(checks) == 0 {
		return nil
	}
	s, err := a.Gate()
	if err != nil {
		return err
	}
	if s.Passed == nil {
		s.Passed = map[string]time.Time{}
	}
	var mu sync.Mutex
	r, passed, err := a.gateLayer(dir, gateJob{task: task, head: head}, checks, s.Passed, &mu)
	if err != nil {
		return err
	}
	for _, k := range passed {
		s.Passed[k] = time.Now().UTC()
	}
	if err := a.setGate(s); err != nil {
		return err
	}
	if r == nil {
		return nil
	}
	return fmt.Errorf("pre-publish gate: %s replayed onto its base at %s fails its %s check (`%s`), so nothing was pushed.\nLast %d lines:\n%s\n"+
		"Fix it and publish again; [train] prepublish.off = true turns the gate off",
		target, short(head), r.check.Name, r.check.Cmd, gateTailLines, tail(r.out, gateTailLines))
}

// gateRestack runs the cheap checks (prepublish.cmd) on each layer restack
// re-cut, records the lowest red one and tells the orchestrator. Layers restack
// left as they were keep what the gate knew of them.
func (a *App) gateRestack(plan []restacked) []GateRed {
	checks := a.GateChecks(true)
	if len(checks) == 0 {
		return nil
	}
	var jobs []gateJob
	for i, r := range plan {
		if !r.gone() && r.NewTo != r.To {
			jobs = append(jobs, gateJob{i: i, task: r.ID, head: r.NewTo})
		}
	}
	// Integration is linear, so every tip above a red one holds its change
	// too: only the lowest red layer is flagged, and prs holds the rest.
	redBelow := func(i int, red map[int]gateResult) bool {
		for r := range red {
			if r < i {
				return true
			}
		}
		return false
	}
	red, err := a.runGate(jobs, checks, redBelow)
	if err != nil {
		a.Store.Event("", "prepublish_error", "restack: "+err.Error())
		return nil
	}
	var reds []GateRed
	checked := map[string]bool{}
	for _, j := range jobs {
		checked[j.task] = true
		if r, ok := red[j.i]; ok && !redBelow(j.i, red) {
			reds = append(reds, GateRed{Task: j.task, Head: j.head, Check: r.check, Tail: tail(r.out, gateTailLines), TimedOut: r.timedOut})
		}
	}
	if err := a.recordGate(reds, checked, GateSourceRestack); err != nil {
		a.Store.Event("", "prepublish_error", "restack: "+err.Error())
	}
	for _, g := range reds {
		_ = a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
			"Restack re-cut %s, and its new tip %s fails its %s check (`%s`): reordering changed what that layer holds. "+
				"prs won't publish it or the layers above it until it passes. Fix its own commits or reorder the stack so what it needs comes first.\n%s",
			g.Task, short(g.Head), g.Check.Name, g.Check.Cmd, tail(g.Tail, 5)))
	}
	return reds
}
