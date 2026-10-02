// Package automerge merges ready PR stacks bottom-up when the owner turns it
// on (#152). It is off by default: [train] auto_merge sets the default and
// `saddle automerge on|off` overrides it at runtime. Each tick it reads every
// stacked task's PR, builds the stacks as a graph from the PRs' bases, and
// merges the bottom PR of the first stack that is ready (checks green,
// GitHub mergeable and CLEAN, not a draft, not labeled needs-human, not under
// a stack-at-risk flag, based on base), then runs restack so the next PR
// retargets to base and its CI runs again. Only one PR merges per tick.
//
// A held stack (`saddle automerge hold <stack|pr>`) is never merged, but it is
// still tracked and restacked; when it falls behind base the orchestrator
// hears about it once, as info. Any merge or restack failure stops the
// watcher with a notice to the orchestrator; it never retries in a loop.
// Turning it on again is the way back. Every merge and every refusal is an
// event with its reason. All GitHub access goes through GitHub.
//
// The runtime toggle, the holds and the last check live in one small JSON
// file under .saddle/, not in the store.
package automerge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

// NeedsHuman is the label the stack sentinel puts on PRs a human must decide.
const NeedsHuman = "needs-human"

// Event kinds in the events table.
const (
	EventMerged  = "automerge_merged"  // a PR it merged
	EventRefused = "automerge_refused" // a PR it won't merge until something changes, and why
	EventWaiting = "automerge_waiting" // a PR it waits on (pending CI, GitHub computing), and why
	EventFailed  = "automerge_failed"  // a merge or restack failed; the watcher stopped
	EventBehind  = "automerge_behind"  // a held stack fell behind base
	EventToggle  = "automerge_toggle"  // on or off
	EventHold    = "automerge_hold"    // a stack held or released
)

// Where the on/off state came from.
const (
	SourceConfig  = "config"
	SourceRuntime = "runtime"
)

// Check summaries.
const (
	ChecksPass    = "pass"
	ChecksPending = "pending"
	ChecksFail    = "fail"
	ChecksNone    = "none"
)

// DefaultInterval is how often Run checks, the sentinel's cadence.
const DefaultInterval = 2 * time.Minute

// Entry is a task in the PR stack, in train order.
type Entry struct {
	Task   string `json:"task"`
	Branch string `json:"branch"`
	PR     string `json:"pr"`
}

// PR is what GitHub says about a pull request.
type PR struct {
	URL        string   `json:"url"`
	State      string   `json:"state"`            // OPEN, CLOSED or MERGED
	Draft      bool     `json:"isDraft"`          //
	Mergeable  string   `json:"mergeable"`        // MERGEABLE, CONFLICTING or UNKNOWN
	MergeState string   `json:"mergeStateStatus"` // CLEAN, BLOCKED, BEHIND, DIRTY, UNSTABLE, UNKNOWN...
	Base       string   `json:"baseRefName"`
	Head       string   `json:"headRefName"`
	HeadSHA    string   `json:"headRefOid"`
	Labels     []string `json:"labels"`
	Checks     string   `json:"checks"` // ChecksPass, ChecksPending, ChecksFail or ChecksNone
}

// GitHub is the part of GitHub the watcher uses.
type GitHub interface {
	// PR reads a PR: state, draft, mergeability, base, head, labels and checks.
	PR(url string) (PR, error)
	// MergeMethod is the method the repo allows: squash when it can.
	MergeMethod() (string, error)
	// Merge merges url with method, only if its head is still head. It must
	// never bypass branch protection.
	Merge(url, method, head string) error
}

// Node is one PR in a stack graph.
type Node struct {
	Task       string   `json:"task"`
	Branch     string   `json:"branch"`
	PR         string   `json:"pr"`
	Base       string   `json:"base"` // the branch it targets: base or the PR below it
	Draft      bool     `json:"draft,omitempty"`
	Checks     string   `json:"checks,omitempty"`
	Mergeable  string   `json:"mergeable,omitempty"`
	MergeState string   `json:"merge_state,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	AtRisk     string   `json:"at_risk,omitempty"` // why a stack-at-risk flag covers it
	Error      string   `json:"error,omitempty"`   // GitHub couldn't be asked
	head       string
}

// Stack is one PR stack, bottom first.
type Stack struct {
	ID     string `json:"id"` // its bottom task
	Nodes  []Node `json:"nodes"`
	Held   bool   `json:"held,omitempty"`
	Behind int    `json:"behind,omitempty"` // commits base has that its top lacks
	Next   string `json:"next"`             // the PR it merges next: the bottom one
	Ready  bool   `json:"ready"`            // Next can merge now
	Why    string `json:"why,omitempty"`    // why Next can't, when not ready
	wait   bool   // Why is something to wait out, not a refusal
}

// Status is the watcher's state and what it would do.
type Status struct {
	Enabled bool      `json:"enabled"`
	Source  string    `json:"source"`            // config or runtime
	Stopped string    `json:"stopped,omitempty"` // why it stopped; on clears it
	Busy    bool      `json:"busy,omitempty"`    // the train held its lock; nothing was checked
	Holds   []string  `json:"holds,omitempty"`
	Stacks  []Stack   `json:"stacks,omitempty"`
	Merged  string    `json:"merged,omitempty"` // the PR this check merged
	Checked time.Time `json:"checked"`
}

// State is the file under .saddle/ that outlives restarts.
type State struct {
	Enabled *bool          `json:"enabled,omitempty"` // runtime override; nil follows config
	Holds   []string       `json:"holds,omitempty"`   // held tasks or PRs; any one holds its stack
	Stopped string         `json:"stopped,omitempty"`
	Behind  map[string]int `json:"behind,omitempty"` // held stacks already flagged behind
	Last    Status         `json:"last"`             // the last check, for readers without GitHub
}

// Load reads the state file; a missing one is the zero state.
func Load(path string) (State, error) {
	var s State
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Save writes the state file atomically.
func (s State) Save(path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Watcher merges one repo's ready stacks.
type Watcher struct {
	Path    string // the state file
	Default bool   // [train] auto_merge
	Base    string // the branch stacks merge into
	GH      GitHub
	// Entries lists the stacked tasks with PRs, in train order.
	Entries func() ([]Entry, error)
	// AtRisk says why the stack-at-risk flag covers task; "" if it doesn't.
	AtRisk func(task string) string
	// Behind counts the commits base has that head lacks.
	Behind func(head string) int
	// Restack rebuilds the stack on base after a merge.
	Restack func() error
	// Lock takes the train lock if it is free; nil means don't lock.
	Lock   func() (unlock func(), ok bool, err error)
	Event  func(task, kind, data string)
	Notify func(action bool, text string) // to the orchestrator
	// Interval is how often Run checks.
	Interval time.Duration
	Now      func() time.Time

	logged map[string]string // the last decision logged per PR
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watcher) event(task, kind, data string) {
	if w.Event != nil {
		w.Event(task, kind, data)
	}
}

func (w *Watcher) notify(action bool, text string) {
	if w.Notify != nil {
		w.Notify(action, text)
	}
}

func (w *Watcher) enabled(s State) (bool, string) {
	if s.Enabled != nil {
		return *s.Enabled, SourceRuntime
	}
	return w.Default, SourceConfig
}

// SetEnabled turns auto-merge on or off at runtime, overriding config. On
// also clears a stop after a failure.
func (w *Watcher) SetEnabled(on bool) error {
	s, err := Load(w.Path)
	if err != nil {
		return err
	}
	s.Enabled = &on
	why := "off"
	if on {
		why = "on"
		if s.Stopped != "" {
			why += "; cleared the stop after: " + s.Stopped
		}
		s.Stopped = ""
	}
	s.Last.Enabled, s.Last.Source, s.Last.Stopped = on, SourceRuntime, s.Stopped
	if err := s.Save(w.Path); err != nil {
		return err
	}
	w.event("", EventToggle, why)
	return nil
}

// Hold keeps the stack holding ref (a task or PR) from merging, until Release.
func (w *Watcher) Hold(ref string) error {
	s, err := Load(w.Path)
	if err != nil {
		return err
	}
	if !slices.Contains(s.Holds, ref) {
		s.Holds = append(s.Holds, ref)
	}
	s.Last.Holds = s.Holds
	if err := s.Save(w.Path); err != nil {
		return err
	}
	w.event(ref, EventHold, "held")
	return nil
}

// Release lifts every hold on the stack holding ref, by the live graph or,
// when GitHub can't be read, the last check's.
func (w *Watcher) Release(ref string) error {
	s, err := Load(w.Path)
	if err != nil {
		return err
	}
	stacks := s.Last.Stacks
	if st, err := w.Plan(); err == nil {
		stacks = st.Stacks
	}
	drop := []string{ref}
	for _, st := range stacks {
		if st.ID == ref || slices.ContainsFunc(st.Nodes, func(n Node) bool { return n.Task == ref || n.PR == ref }) {
			for _, n := range st.Nodes {
				drop = append(drop, n.Task, n.PR)
			}
		}
	}
	n := len(s.Holds)
	s.Holds = slices.DeleteFunc(s.Holds, func(h string) bool { return slices.Contains(drop, h) })
	if len(s.Holds) == n {
		return fmt.Errorf("%s isn't held", ref)
	}
	s.Last.Holds = s.Holds
	if err := s.Save(w.Path); err != nil {
		return err
	}
	w.event(ref, EventHold, "released")
	return nil
}

// Status is the saved state with the last check's stacks; it asks GitHub nothing.
func (w *Watcher) Status() (Status, error) {
	s, err := Load(w.Path)
	if err != nil {
		return Status{}, err
	}
	st := s.Last
	st.Enabled, st.Source = w.enabled(s)
	st.Stopped, st.Holds = s.Stopped, s.Holds
	return st, nil
}

// Plan reads GitHub and reports what Check would do, without merging,
// logging or saving anything.
func (w *Watcher) Plan() (Status, error) {
	s, err := Load(w.Path)
	if err != nil {
		return Status{}, err
	}
	return w.plan(s)
}

func (w *Watcher) plan(s State) (Status, error) {
	st := Status{Stopped: s.Stopped, Holds: s.Holds, Checked: w.now()}
	st.Enabled, st.Source = w.enabled(s)
	es, err := w.Entries()
	if err != nil {
		return st, err
	}
	st.Stacks = w.graph(es, s.Holds)
	return st, nil
}

// graph reads every entry's PR and groups the open ones into stacks: a PR
// whose base is another open PR's branch sits on that PR.
func (w *Watcher) graph(es []Entry, holds []string) []Stack {
	var nodes []Node
	for _, e := range es {
		if e.PR == "" {
			continue
		}
		n := Node{Task: e.Task, Branch: e.Branch, PR: e.PR}
		pr, err := w.GH.PR(e.PR)
		if err != nil {
			n.Error = err.Error()
		} else if pr.State != "" && pr.State != "OPEN" {
			continue // merged or closed: out of the stack
		} else {
			n.Base, n.Draft, n.Checks, n.Mergeable, n.MergeState, n.Labels, n.head =
				pr.Base, pr.Draft, pr.Checks, pr.Mergeable, pr.MergeState, pr.Labels, pr.HeadSHA
		}
		if w.AtRisk != nil {
			n.AtRisk = w.AtRisk(e.Task)
		}
		nodes = append(nodes, n)
	}
	byBranch := map[string]int{}
	for i, n := range nodes {
		byBranch[n.Branch] = i
	}
	root := make([]int, len(nodes))
	var find func(int, int) int
	find = func(i, depth int) int {
		j, ok := byBranch[nodes[i].Base]
		if !ok || j == i || depth > len(nodes) {
			return i
		}
		return find(j, depth+1)
	}
	var order []int // roots in train order
	members := map[int][]int{}
	for i := range nodes {
		root[i] = find(i, 0)
		if _, ok := members[root[i]]; !ok {
			order = append(order, root[i])
		}
		members[root[i]] = append(members[root[i]], i)
	}
	slices.Sort(order)
	var out []Stack
	for _, r := range order {
		st := Stack{ID: nodes[r].Task}
		for _, i := range members[r] {
			n := nodes[i]
			st.Nodes = append(st.Nodes, n)
			if slices.Contains(holds, n.Task) || slices.Contains(holds, n.PR) {
				st.Held = true
			}
		}
		if slices.Contains(holds, st.ID) {
			st.Held = true
		}
		top := st.Nodes[len(st.Nodes)-1]
		if w.Behind != nil && top.head != "" {
			st.Behind = w.Behind(top.head)
		}
		bottom := st.Nodes[0]
		st.Next = bottom.PR
		st.Why, st.wait = w.refusal(bottom)
		if st.Why == "" && st.Held {
			st.Why = "the stack is held; `saddle automerge release " + st.ID + "` lets it merge"
		}
		st.Ready = st.Why == ""
		out = append(out, st)
	}
	return out
}

// refusal says why n can't merge now, "" if it can; wait marks reasons that
// clear by themselves (pending CI, GitHub still computing, restack pending).
func (w *Watcher) refusal(n Node) (why string, wait bool) {
	switch {
	case n.Error != "":
		return "GitHub couldn't be asked about it: " + n.Error, true
	case n.Draft:
		return "it is a draft", false
	case slices.Contains(n.Labels, NeedsHuman):
		return "it is labeled " + NeedsHuman, false
	case n.AtRisk != "":
		return "the stack is flagged at risk: " + n.AtRisk, false
	case n.Base != w.Base:
		return fmt.Sprintf("it targets %s, not %s; waiting for restack to retarget it", n.Base, w.Base), true
	case n.Mergeable == "CONFLICTING" || n.MergeState == "DIRTY":
		return "it conflicts with " + w.Base, false
	case n.Checks == ChecksFail:
		return "its CI is red", false
	case n.Checks == ChecksPending:
		return "its CI is pending", true
	case n.Mergeable != "MERGEABLE" || n.MergeState == "UNKNOWN" || n.MergeState == "":
		return "GitHub is still computing whether it can merge", true
	case n.MergeState != "CLEAN":
		return fmt.Sprintf("GitHub's merge state is %s, not CLEAN (branch protection, reviews or checks)", n.MergeState), true
	}
	return "", false
}

// Check runs one tick: it rebuilds the graph, flags held stacks that fell
// behind, and, when on and not stopped, merges the bottom PR of the first
// ready stack that isn't held and restacks.
func (w *Watcher) Check() (Status, error) {
	s, err := Load(w.Path)
	if err != nil {
		return Status{}, err
	}
	unlock := func() {}
	if w.Lock != nil {
		u, ok, err := w.Lock()
		if err != nil {
			return Status{}, err
		}
		if !ok {
			st, _ := w.Status()
			st.Busy = true
			return st, nil
		}
		unlock = u
	}
	locked := true
	release := func() {
		if locked {
			unlock()
			locked = false
		}
	}
	defer release()

	st, err := w.plan(s)
	if err != nil {
		return st, err
	}
	w.flagBehind(&s, st.Stacks)

	if st.Enabled && s.Stopped == "" {
		w.logDecisions(st.Stacks)
		if i := slices.IndexFunc(st.Stacks, func(x Stack) bool { return x.Ready }); i >= 0 {
			release() // restack takes the train lock itself
			w.merge(&s, &st, st.Stacks[i])
		}
	}
	st.Stopped = s.Stopped
	s.Last = st
	return st, s.Save(w.Path)
}

// merge merges stack's bottom PR and restacks; a failure of either stops
// the watcher.
func (w *Watcher) merge(s *State, st *Status, stack Stack) {
	n := stack.Nodes[0]
	stop := func(why string) {
		s.Stopped = why
		w.event(n.Task, EventFailed, why)
		w.notify(true, fmt.Sprintf("Auto-merge stopped: %s. It won't retry; fix it (or merge by hand), then `saddle automerge on` to resume.", why))
	}
	method, err := w.GH.MergeMethod()
	if err != nil {
		stop(fmt.Sprintf("couldn't read the repo's merge method for %s: %v", n.PR, err))
		return
	}
	if err := w.GH.Merge(n.PR, method, n.head); err != nil {
		stop(fmt.Sprintf("merging %s (%s) failed: %v", n.PR, n.Task, err))
		return
	}
	st.Merged = n.PR
	w.event(n.Task, EventMerged, fmt.Sprintf("%s by %s: checks %s, mergeable, CLEAN", n.PR, method, n.Checks))
	delete(w.logged, n.PR)
	if err := w.Restack(); err != nil {
		stop(fmt.Sprintf("restack after merging %s failed: %v", n.PR, err))
		return
	}
	w.notify(false, fmt.Sprintf("Auto-merged %s (%s) into %s and restacked the rest of stack %s.", n.PR, n.Task, w.Base, stack.ID))
}

// logDecisions records why each stack's next PR isn't merging, once per
// change of reason.
func (w *Watcher) logDecisions(stacks []Stack) {
	if w.logged == nil {
		w.logged = map[string]string{}
	}
	for _, st := range stacks {
		if st.Ready || st.Next == "" {
			continue
		}
		if w.logged[st.Next] == st.Why {
			continue
		}
		w.logged[st.Next] = st.Why
		kind := EventRefused
		if st.wait && !st.Held {
			kind = EventWaiting
		}
		w.event(st.Nodes[0].Task, kind, st.Next+": "+st.Why)
	}
}

// flagBehind tells the orchestrator, once, when a held stack falls behind
// base. It is information, not a needs-human problem.
func (w *Watcher) flagBehind(s *State, stacks []Stack) {
	seen := map[string]bool{}
	for _, st := range stacks {
		if !st.Held || st.Behind == 0 {
			continue
		}
		seen[st.ID] = true
		if s.Behind[st.ID] > 0 {
			continue
		}
		if s.Behind == nil {
			s.Behind = map[string]int{}
		}
		s.Behind[st.ID] = st.Behind
		w.event(st.ID, EventBehind, fmt.Sprintf("%d commits behind %s", st.Behind, w.Base))
		w.notify(false, fmt.Sprintf("Held stack %s is %d commits behind %s. It won't auto-merge while held; `saddle stack rebase %s` rebases it when you want.",
			st.ID, st.Behind, w.Base, st.ID))
	}
	for id := range s.Behind {
		if !seen[id] {
			delete(s.Behind, id)
		}
	}
}

// Run checks now and then every Interval until ctx ends. A failed check is
// recorded as an event once per distinct error and retried next tick.
func (w *Watcher) Run(ctx context.Context) error {
	iv := w.Interval
	if iv <= 0 {
		iv = DefaultInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	last := ""
	for {
		if _, err := w.Check(); err != nil {
			if msg := err.Error(); msg != last {
				last = msg
				w.event("", EventFailed, "check: "+msg)
			}
		} else {
			last = ""
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Check is one entry of GitHub's statusCheckRollup: a check run (Status,
// Conclusion) or a commit status (State).
type Check struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

// summarize folds a rollup into one of the Checks* values: any failure is a
// fail, else anything unfinished is pending.
func summarize(rollup []Check) string {
	if len(rollup) == 0 {
		return ChecksNone
	}
	pending := false
	for _, c := range rollup {
		v := strings.ToUpper(c.Conclusion)
		if c.State != "" {
			v = strings.ToUpper(c.State)
		} else if !strings.EqualFold(c.Status, "COMPLETED") {
			pending = true
			continue
		}
		switch v {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
		case "PENDING", "EXPECTED", "":
			pending = true
		default: // FAILURE, ERROR, CANCELLED, TIMED_OUT, ACTION_REQUIRED, STALE, STARTUP_FAILURE
			return ChecksFail
		}
	}
	if pending {
		return ChecksPending
	}
	return ChecksPass
}
