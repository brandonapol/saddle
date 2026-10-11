package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/lintgate"
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
	Directory string    `json:"directory,omitempty"`
	Task      string    `json:"task"`
	Head      string    `json:"head"`
	Check     GateCheck `json:"check"`
	Tail      string    `json:"tail"` // the last lines of the check's output
	TimedOut  bool      `json:"timed_out,omitempty"`
	Source    string    `json:"source"` // prs, publish or restack
	Since     time.Time `json:"since"`
	// Held are the stacked layers above it that were not published for it.
	Held []string `json:"held,omitempty"`
}

// GateState is what the gate knows, saved under .saddle.
type GateState struct {
	// Environment explains a shared setup failure that marked no layer red.
	Environment string    `json:"environment,omitempty"`
	Red         []GateRed `json:"red,omitempty"`
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
		if cmd == "" || cmd == NoTestCmd || slices.ContainsFunc(out, func(c GateCheck) bool { return lintgate.SameCmd(c.Cmd, cmd) }) {
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

// gateResult is a red job, nil for a green or skipped one. env is set when
// the check failed on the environment, not the layer, after its retries.
type gateResult struct {
	dir      string
	retries  int
	job      gateJob
	check    GateCheck
	out      string
	timedOut bool
	env      *GateEnvProblem
}

// runGate checks each job's head with checks, in the order given. A job
// skip says to leave alone (a red layer is below it) is not checked. Up to
// prepublish.parallel jobs run at once, each worker in its own scratch
// worktree, reused for every layer it checks. It returns the red jobs by
// index.
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
	// Register scratch worktrees serially: pruning while another worker is
	// still writing its commondir can delete that worker's registration.
	dirs := make([]string, workers)
	defer func() {
		for _, dir := range dirs {
			if dir != "" {
				_ = gitx.WorktreeRemove(a.Root, dir)
			}
		}
	}()
	for w := range workers {
		dirs[w], err = a.gateWorktree(w)
		if err != nil {
			return nil, err
		}
	}
	var mu sync.Mutex
	next := 0
	var errs []error
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dir := dirs[w]
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
	// The same check failing identically on every checked stack bottom is
	// evidence of shared setup trouble. Keep it out of per-task red records.
	if len(red) >= 2 {
		var first *gateResult
		identical := true
		for _, r := range red {
			if r.env != nil || r.timedOut {
				identical = false
				break
			}
			if first == nil {
				copy := r
				first = &copy
			} else if r.check != first.check || strings.TrimSpace(r.out) != strings.TrimSpace(first.out) {
				identical = false
				break
			}
		}
		for _, j := range jobs {
			if _, failed := red[j.i]; !failed && (skip == nil || !skip(j.i, red)) {
				identical = false
			}
		}
		if identical {
			problem := GateEnvProblem{Signature: "identical failure across every checked layer", Free: "dependency setup: configure [train] prepublish.setup and verify the gate command in its reported directory"}
			for i, r := range red {
				r.env = &problem
				red[i] = r
			}
		}
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
// passed. Checks the cache knows passed on the same tree are not run. Each
// runs through runGateEnv, so a full disk is retried, and if it persists
// comes back with env set rather than as the layer's failure (#274).
func (a *App) gateLayer(dir string, j gateJob, checks []GateCheck, passed map[string]time.Time, mu *sync.Mutex) (*gateResult, []string, error) {
	tree, err := gitx.Run(a.Root, "rev-parse", j.head+"^{tree}")
	if err != nil {
		return nil, nil, err
	}
	var ok []string
	checkedOut := false
	for _, c := range checks {
		key := tree + " " + c.Cmd
		if setup := a.Cfg.Train.Prepublish.Setup; setup != "" {
			key += " setup=" + setup
		}
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
			if setup := strings.TrimSpace(a.Cfg.Train.Prepublish.Setup); setup != "" {
				g := a.runGateEnv(context.Background(), j.task, ShellGate(dir, setup, a.Cfg.Train.Prepublish.Timeout), false)
				if errors.Is(g.Err, ErrGateInterrupted) {
					return nil, nil, g.Err
				}
				if g.Err != nil {
					problem := g.Env
					if problem == nil {
						problem = &GateEnvProblem{Signature: "dependency setup failed", Free: "dependency setup: fix [train] prepublish.setup and rerun prs"}
					}
					return &gateResult{job: j, check: GateCheck{Name: "setup", Cmd: setup}, out: g.Output, env: problem, dir: dir}, ok, nil
				}
			}
		}
		g := a.runGateEnv(context.Background(), j.task, ShellGate(dir, c.Cmd, a.Cfg.Train.Prepublish.Timeout), false)
		out, err := g.Output, g.Err
		if errors.Is(err, ErrGateInterrupted) {
			return nil, nil, err
		}
		if err == nil {
			ok = append(ok, key)
			continue
		}
		if g.Env != nil {
			return &gateResult{job: j, check: c, out: out, env: g.Env, dir: dir, retries: g.Retries}, ok, nil
		}
		var te *GateTimeoutError
		timedOut := errors.As(err, &te)
		if c.Name == "lint" && !timedOut && a.brokenGate(j.task, a.LintGate(), out) {
			continue // the gate itself is wrong, not the layer
		}
		return &gateResult{job: j, check: c, out: out, timedOut: timedOut, dir: dir}, ok, nil
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
	var envs []gateResult
	for _, i := range order {
		r, ok := red[i]
		if !ok || redBelow(i, red) {
			continue
		}
		if r.env != nil {
			// Not the layer's fault: hold it and what's above, flag nothing.
			held[i] = landed[i].ID
			for j := range landed {
				if j != i && above(layout, j, i) {
					held[j] = landed[i].ID
				}
			}
			envs = append(envs, r)
			continue
		}
		g := GateRed{Directory: r.dir, Task: landed[i].ID, Head: layout[i].Head, Check: r.check, Tail: tail(r.out, gateTailLines),
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
		if r, ok := red[j.i]; !ok || r.env == nil {
			checked[j.task] = true // an environment failure proved nothing
		}
	}
	if err := a.recordGate(reds, checked, GateSourcePRs); err != nil {
		return nil, err
	}
	envErr := gateEnvErr(envs, landed, layout)
	state, err := a.Gate()
	if err != nil {
		return nil, err
	}
	state.Environment = ""
	if envErr != nil {
		state.Environment = envErr.Error()
	}
	if err := a.setGate(state); err != nil {
		return nil, err
	}
	return held, errors.Join(gateErr(reds, landed, held), envErr)
}

// gateEnvErr explains the layers the gate held because their checks failed
// on the environment, not on the layer (#274).
func gateEnvErr(envs []gateResult, landed []landedTask, layout []prLayer) error {
	if len(envs) == 0 {
		return nil
	}
	var b strings.Builder
	for _, r := range envs {
		var held []string
		for j := range landed {
			if j != r.job.i && above(layout, j, r.job.i) {
				held = append(held, landed[j].ID)
			}
		}
		aboveIt := ""
		if len(held) > 0 {
			aboveIt = " or the layers above it (" + strings.Join(held, ", ") + ")"
		}
		fmt.Fprintf(&b, "pre-publish gate hit the environment (%s), not the layer: %s check (`%s`) on %s's tip %s failed after %d retries, so %s%s was not published. Nothing was marked red.\nFree %s, then run prs again.\nLast lines:\n%s\n",
			r.env.Signature, r.check.Name, r.check.Cmd, r.job.task, short(r.job.head), r.retries, r.job.task, aboveIt, r.env.Free, tail(r.out, 5))
		fmt.Fprintf(&b, "Gate directory: %s\n", r.dir)
	}
	return errors.New(strings.TrimRight(b.String(), "\n"))
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
		a.Store.Event(g.Task, "prepublish_red", fmt.Sprintf("%s %s: %s", short(g.Head), g.Check.Name, g.Check.Cmd))
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
		if g.Directory != "" {
			fmt.Fprintf(&b, "Gate directory: %s\n", g.Directory)
		}
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
	if r.env != nil {
		return fmt.Errorf("pre-publish gate hit the environment (%s), not %s: its %s check (`%s`) failed after %d retries, so nothing was pushed. Free %s, then publish again.\n%s",
			r.env.Signature, target, r.check.Name, r.check.Cmd, gateEnvRetries, r.env.Free, tail(r.out, 5))
	}
	return fmt.Errorf("pre-publish gate: %s replayed onto its base at %s fails its %s check (`%s`), so nothing was pushed.\nLast %d lines:\n%s\n"+
		"Fix it and publish again; [train] prepublish.off = true turns the gate off",
		target, short(head), r.check.Name, r.check.Cmd, gateTailLines, tail(r.out, gateTailLines))
}

// gateRestack runs the cheap checks (prepublish.cmd) on the PR head of each
// layer restack re-cut or moved: the head its PR will show, on its PR's own
// base, as prs checks it, not the integration tip, which also holds layers
// from other stacks (#358). It records the lowest red layer of each stack,
// tells the orchestrator, and returns the layers republish must leave alone,
// each mapped to the red layer it is held for. Layers restack left as they
// were keep what the gate knew of them.
func (a *App) gateRestack(rs restackPlan) ([]GateRed, map[int]string) {
	checks := a.GateChecks(true)
	if len(checks) == 0 {
		return nil, nil
	}
	var jobs []gateJob
	for _, i := range rs.order {
		r, head := rs.live[i], rs.layout[i].Head
		published := ""
		if r.PR != "" {
			published, _ = gitx.RevParse(a.Root, "refs/remotes/origin/"+r.Branch)
		}
		if r.NewTo != r.To || (r.PR != "" && head != published) {
			jobs = append(jobs, gateJob{i: i, task: r.ID, head: head})
		}
	}
	// A PR head holds everything below it in its stack, so only the lowest
	// red layer is flagged, and the layers above it are held for it.
	redBelow := func(i int, red map[int]gateResult) bool {
		for r := range red {
			if above(rs.layout, i, r) {
				return true
			}
		}
		return false
	}
	red, err := a.runGate(jobs, checks, redBelow)
	if err != nil {
		a.Store.Event("", "prepublish_error", "restack: "+err.Error())
		return nil, nil
	}
	var reds []GateRed
	held := map[int]string{}
	checked := map[string]bool{}
	for _, j := range jobs {
		r, ok := red[j.i]
		if ok && r.env != nil {
			a.Store.Event(j.task, "prepublish_env", r.env.Signature)
			continue // proved nothing about the layer (#274)
		}
		checked[j.task] = true
		if !ok || redBelow(j.i, red) {
			continue
		}
		g := GateRed{Task: j.task, Head: j.head, Check: r.check, Tail: tail(r.out, gateTailLines), TimedOut: r.timedOut}
		held[j.i] = j.task
		for k := range rs.live {
			if k != j.i && above(rs.layout, k, j.i) {
				held[k] = j.task
				g.Held = append(g.Held, rs.live[k].ID)
			}
		}
		reds = append(reds, g)
	}
	if err := a.recordGate(reds, checked, GateSourceRestack); err != nil {
		a.Store.Event("", "prepublish_error", "restack: "+err.Error())
	}
	for _, g := range reds {
		_ = a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
			"Restack re-cut %s, and its new PR head %s fails its %s check (`%s`): reordering changed what that layer holds. "+
				"Restack didn't push it, and prs won't publish it or the layers above it until it passes. Fix its own commits or reorder the stack so what it needs comes first.\n%s",
			g.Task, short(g.Head), g.Check.Name, g.Check.Cmd, tail(g.Tail, 5)))
	}
	return reds, held
}

// gateSeed records that cmds just passed on head's tree, as the train's
// tests and lint gate did when it landed head, so the gate doesn't run them
// on that tree again.
func (a *App) gateSeed(head string, cmds ...string) {
	tree, err := gitx.Run(a.Root, "rev-parse", head+"^{tree}")
	if err != nil {
		return
	}
	s, err := a.Gate()
	if err != nil {
		return
	}
	if s.Passed == nil {
		s.Passed = map[string]time.Time{}
	}
	for _, c := range cmds {
		if c = strings.TrimSpace(c); c != "" && c != NoTestCmd {
			s.Passed[tree+" "+c] = time.Now().UTC()
		}
	}
	_ = a.setGate(s)
}
