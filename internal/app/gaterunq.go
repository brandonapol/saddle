package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/runq"
	"github.com/brandonapol/saddle/internal/store"
)

// The gates saddle runs itself (the train's test gate, the pre-publish layer
// checks, and through the train the CI-red repairs' landings) take a slot
// in the machine's heavy-run queue at runq.PrioGate before they start
// (#239, docs/runq.md Q1(c)). The gate's children get the lease's token in
// SADDLE_RUNQ_LEASE, so the repo's pre-commit hooks, make check and shims
// inside the gate ride on it instead of queueing behind it.

// DefaultGateClass is the heavy-run class of a gate whose test.cmd matches
// no [classes] pattern.
const DefaultGateClass = "default"

// gateLeaseLost is the environment problem of a gate whose lease was reaped
// or killed while it ran: the queue stopped it, not the branch (#184).
var gateLeaseLost = GateEnvProblem{
	Signature: "heavy-run lease lost",
	Free:      "the queue: the gate's lease was killed or reaped (saddle runq status shows the queue)",
}

// GateClass is the heavy-run class of a gate running cmd: see GateClassIn.
func GateClass(cfg runq.Config, cmd string) string { return GateClassIn(cfg, cmd, "") }

// GateClassIn is the heavy-run class of a gate running cmd in root. cmd is
// split on &&, || and ; and each segment classified; the cmd takes the
// heaviest class among its segments (by weight, then name), so
// "flutter pub get && make check" is a flutter class run. A segment is
// classified by the first [classes] pattern it matches, classes taken in
// name order. A "make <target>" segment is classified by what root's Makefile
// recipe for the target runs when that resolves to a class, else by the
// patterns as above. Segments matching nothing, and commands matching no
// segment, are DefaultGateClass.
func GateClassIn(cfg runq.Config, cmd, root string) string {
	names := make([]string, 0, len(cfg.Classes))
	for name := range cfg.Classes {
		names = append(names, name)
	}
	slices.Sort(names)
	best := ""
	for _, seg := range splitCompound(cmd) {
		c := classifySegment(cfg, names, seg, root, 0)
		if c == "" {
			continue
		}
		if best == "" || classWeight(cfg, c) > classWeight(cfg, best) {
			best = c
		}
	}
	if best == "" {
		return DefaultGateClass
	}
	return best
}

func classWeight(cfg runq.Config, class string) int {
	return max(cfg.Classes[class].Weight, 1)
}

// matchPatterns is the first class (in name order) with a pattern matching
// seg, or "".
func matchPatterns(cfg runq.Config, names []string, seg string) string {
	for _, name := range names {
		for _, p := range cfg.Classes[name].Match {
			if matchCmd(p, seg) {
				return name
			}
		}
	}
	return ""
}

func classifySegment(cfg runq.Config, names []string, seg, root string, depth int) string {
	if root != "" && depth < 4 {
		if target, ok := makeTarget(seg); ok {
			if c := classifyMakeTarget(cfg, names, root, target, depth); c != "" {
				return c
			}
		}
	}
	return matchPatterns(cfg, names, seg)
}

// splitCompound splits cmd on the shell operators &&, || and ; outside
// quotes.
func splitCompound(cmd string) []string {
	var segs []string
	var cur strings.Builder
	var quote byte
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			segs = append(segs, s)
		}
		cur.Reset()
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
		case c == '\'' || c == '"':
			quote = c
			cur.WriteByte(c)
		case c == ';':
			flush()
		case (c == '&' || c == '|') && i+1 < len(cmd) && cmd[i+1] == c:
			flush()
			i++
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return segs
}

// makeTarget returns the target of a "make [flags] target" segment.
func makeTarget(seg string) (string, bool) {
	f := strings.Fields(seg)
	if len(f) < 2 || filepath.Base(f[0]) != "make" {
		return "", false
	}
	for _, w := range f[1:] {
		if strings.HasPrefix(w, "-") || strings.Contains(w, "=") {
			continue
		}
		return w, true
	}
	return "", false
}

// classifyMakeTarget classifies the recipe lines of target in root's
// Makefile, and of the targets it depends on, taking the heaviest class.
func classifyMakeTarget(cfg runq.Config, names []string, root, target string, depth int) string {
	b, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		return ""
	}
	best := ""
	consider := func(c string) {
		if c != "" && (best == "" || classWeight(cfg, c) > classWeight(cfg, best)) {
			best = c
		}
	}
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "\t") {
			if !in {
				continue
			}
			rec := strings.TrimLeft(line, "\t @-+")
			rec = strings.ReplaceAll(rec, "$(MAKE)", "make")
			for _, seg := range splitCompound(rec) {
				consider(classifySegment(cfg, names, seg, root, depth+1))
			}
			continue
		}
		head, deps, ok := strings.Cut(line, ":")
		in = false
		if !ok || strings.TrimSpace(head) != target || strings.HasPrefix(deps, "=") {
			continue
		}
		in = true
		for _, d := range strings.Fields(deps) {
			if d != target && !strings.ContainsAny(d, "$%|") {
				consider(classifyMakeTarget(cfg, names, root, d, depth+1))
			}
		}
	}
	return best
}

// matchCmd reports whether cmd matches pattern p, where * matches any run
// of characters (spaces and slashes too) and runs of whitespace compare
// equal: "go test*" matches "go test  ./...".
func matchCmd(p, cmd string) bool {
	return globMatch(strings.Join(strings.Fields(p), " "), strings.Join(strings.Fields(cmd), " "))
}

func globMatch(p, s string) bool {
	pi, si, star, mark := 0, 0, -1, 0
	for si < len(s) {
		switch {
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, si
			pi++
		case pi < len(p) && (p[pi] == '?' || p[pi] == s[si]):
			pi++
			si++
		case star >= 0:
			// Let the last * eat one more character and try again.
			mark++
			si, pi = mark, star+1
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// gateLease is a gate's hold on the heavy-run queue.
type gateLease struct {
	q     *runq.Queue
	l     *runq.Lease
	class string
	env   []string // what the gate's children need to ride on the lease
	held  time.Time
}

// lost is closed if the queue took the lease away while the gate ran; nil
// (never ready) when the gate runs unqueued.
func (g *gateLease) lost() <-chan struct{} {
	if g == nil || g.l == nil {
		return nil
	}
	return g.l.Lost()
}

// isLost reports whether the lease was taken away.
func (g *gateLease) isLost() bool {
	select {
	case <-g.lost():
		return true
	default:
		return false
	}
}

// gateLease waits for a heavy-run slot for task's gate. While it waits,
// the train's entry for task (train true) reads "queued: position …", so
// the train's status says what the gate waits on. It never fails: a queue
// that can't be opened, or a wait past wait_max, runs the gate unqueued,
// because landing is what frees capacity. The returned lease may be nil.
func (a *App) gateLease(ctx context.Context, task string, train bool) *gateLease {
	h := a.Heavy()
	q, cfg, err := h.Open()
	if err != nil {
		a.Store.Event(task, EventRunQueued, "gate runs unqueued: "+err.Error())
		return nil
	}
	class := GateClassIn(cfg, a.Cfg.Test.Cmd, a.Root)
	wait := DefaultWaitMax
	if cfg.WaitMax.D > 0 {
		wait = cfg.WaitMax.D
	}
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	prev, queued := "", false
	l, err := q.Acquire(wctx, runq.Request{
		Class: class,
		Prio:  runq.PrioGate,
		Label: task,
		Repo:  repoName(a.Root),
		Cmd:   a.Cfg.Test.Cmd,
		OnWait: func(w runq.Wait) {
			line := gateWaitLine(w)
			if !queued {
				queued = true
				prev = a.trainNote(task)
				a.Store.Event(task, EventRunQueued, class+": "+line)
			}
			if train {
				_ = a.Store.SetTrain(task, store.Queued, line, false)
			}
		},
	})
	if queued && train {
		_ = a.Store.SetTrain(task, store.Queued, prev, false)
	}
	if err != nil {
		_ = q.Close()
		a.Store.Event(task, EventRunQueued, fmt.Sprintf("%s: gate runs unqueued: %v", class, err))
		return nil
	}
	g := &gateLease{q: q, l: l, class: class, held: time.Now()}
	if tok := l.Token(); tok != "" {
		g.env = []string{runq.EnvLease + "=" + tok, runq.EnvPath + "=" + q.Path()}
	}
	if q.Mode() != runq.ModeOff && !l.Nested() {
		a.Store.Event(task, EventRunStarted, fmt.Sprintf("%s waited %s: gate", class, l.Waited().Round(time.Millisecond)))
	}
	return g
}

// release frees the slot and logs how the gate went.
func (g *gateLease) release(a *App, task string, err error) {
	if g == nil {
		return
	}
	defer func() { _ = g.q.Close() }()
	_ = g.l.Release()
	if g.l.Nested() || g.l.Bypassed() {
		return
	}
	how := "ok"
	switch {
	case g.isLost():
		how = "lost"
	case err != nil:
		how = "failed"
	}
	a.Store.Event(task, EventRunFinished, fmt.Sprintf("%s %s in %s (waited %s, %s): gate",
		g.class, how, time.Since(g.held).Round(time.Millisecond), g.l.Waited().Round(time.Millisecond), g.q.Mode()))
}

// gateWaitLine is w for the train's status: "position 1 of 2 for go-test,
// holder t83 (make check, 40s)". Shown after the entry's state, it reads
// "queued: position …".
func gateWaitLine(w runq.Wait) string {
	s := strings.TrimPrefix(w.String(), "queued: ")
	if i := strings.LastIndex(s, " (starts automatically"); i >= 0 {
		s = s[:i]
	}
	return s
}

// trainNote is task's train entry note, "" when it has none.
func (a *App) trainNote(task string) string {
	es, err := a.Store.Train()
	if err != nil {
		return ""
	}
	for _, e := range es {
		if e.Task == task {
			return e.Note
		}
	}
	return ""
}
