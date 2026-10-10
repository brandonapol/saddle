package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/ghstack"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// Custom stacks (#211) are stacks the owner defines instead of leaving the
// layout to clustering: `saddle stack create [name] <task|PR#|branch>...`
// names landed tasks bottom to top, and prs and restack publish them as one
// stack in that order, each PR on the one below it, even when their files
// don't overlap. They live in .saddle/stacks.json. The integration branch
// stays one linear history in train order; a custom stack is a layout over
// it like any other.

// CustomStack is an owner-defined stack.
type CustomStack struct {
	Name    string    `json:"name"`
	Tasks   []string  `json:"tasks"` // bottom to top
	Created time.Time `json:"created"`
}

// stacksFile is .saddle/stacks.json.
type stacksFile struct {
	Stacks []CustomStack `json:"stacks"`
	// Fallback is why the last publish couldn't link stacks with gh-stack,
	// so the owner hears it once rather than on every prs.
	Fallback string `json:"gh_stack_fallback,omitempty"`
}

func (a *App) stacksPath() string { return a.stateDir("stacks.json") }

func (a *App) loadStacks() (stacksFile, error) {
	var f stacksFile
	b, err := os.ReadFile(a.stacksPath())
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("%s: %w; fix or delete it", a.stacksPath(), err)
	}
	return f, nil
}

func (a *App) saveStacks(f stacksFile) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.stacksPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.stacksPath())
}

// CustomStacks lists the owner's stacks in the order they were created.
func (a *App) CustomStacks() ([]CustomStack, error) {
	f, err := a.loadStacks()
	return f.Stacks, err
}

// StackReport is what a stack change did.
type StackReport struct {
	Stack CustomStack `json:"stack"`
	PRs   []string    `json:"prs,omitempty"` // the PRs prs published
	// PublishErr is why publishing stopped; the stack is recorded either way.
	PublishErr string `json:"publish_error,omitempty"`
	// Backend is the stack backend: "saddle" chains PR bases, "gh-stack"
	// also links the stack on GitHub.
	Backend string `json:"backend"`
	Linked  bool   `json:"linked"`         // linked natively on GitHub with gh-stack
	Note    string `json:"note,omitempty"` // why it isn't linked, when it should be
}

var stackName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// StackArgs splits `stack create` args into an optional name and the refs:
// the first arg is the name unless it names a task, PR or branch.
func (a *App) StackArgs(args []string) (name string, refs []string) {
	if len(args) == 0 {
		return "", nil
	}
	if _, err := a.stackRef(args[0]); err == nil {
		return "", args
	}
	return args[0], args[1:]
}

// stackRef finds the task ref names: task id, PR URL or number, or branch.
func (a *App) stackRef(ref string) (store.Task, error) {
	if t, err := a.taskByRef(ref); err == nil {
		return t, nil
	}
	ts, err := a.Store.Tasks()
	if err != nil {
		return store.Task{}, err
	}
	for _, t := range ts {
		if t.Branch != "" && t.Branch == strings.TrimSpace(ref) {
			return t, nil
		}
	}
	return store.Task{}, fmt.Errorf("no task, PR or branch %q", ref)
}

// resolveMembers turns refs into stack members, checking each is landed,
// still in the PR stack with a branch or PR, listed once, and in no other
// custom stack than skip.
func (a *App) resolveMembers(f stacksFile, skip string, refs []string) ([]string, error) {
	all, err := a.landedAll()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, ref := range refs {
		t, err := a.stackRef(ref)
		if err != nil {
			return nil, err
		}
		i := slices.IndexFunc(all, func(l landedTask) bool { return l.ID == t.ID })
		switch {
		case i < 0:
			return nil, fmt.Errorf("%s hasn't landed; only landed tasks can be stacked (call done and let the train land it first)", t.ID)
		case !all[i].stacked():
			return nil, fmt.Errorf("%s is out of the PR stack (%s), so it can't be stacked", t.ID, all[i].State)
		case t.PR == "" && (t.Branch == "" || !gitx.BranchExists(a.Root, t.Branch)):
			return nil, fmt.Errorf("%s has no PR and no branch to publish", t.ID)
		case slices.Contains(ids, t.ID):
			return nil, fmt.Errorf("%s is listed twice", t.ID)
		}
		for _, s := range f.Stacks {
			if s.Name != skip && slices.Contains(s.Tasks, t.ID) {
				return nil, fmt.Errorf("%s is already in stack %s; `saddle stack remove %s %s` first", t.ID, s.Name, s.Name, t.ID)
			}
		}
		ids = append(ids, t.ID)
	}
	return ids, nil
}

// changeStacks applies fn to the stacks under the train lock and saves them.
func (a *App) changeStacks(fn func(*stacksFile) (CustomStack, error)) (CustomStack, error) {
	unlock, err := a.lockTrain()
	if err != nil {
		return CustomStack{}, err
	}
	defer unlock()
	f, err := a.loadStacks()
	if err != nil {
		return CustomStack{}, err
	}
	s, err := fn(&f)
	if err != nil {
		return s, err
	}
	return s, a.saveStacks(f)
}

func stackIndex(f *stacksFile, name string) int {
	return slices.IndexFunc(f.Stacks, func(s CustomStack) bool { return s.Name == name })
}

// CreateStack records a custom stack of refs (tasks, PRs or branches),
// bottom to top, then publishes it: prs lays its PRs out in that order. An
// empty name names it after its bottom task. Publishing problems don't undo
// the stack; the report says what happened.
func (a *App) CreateStack(name string, refs []string) (StackReport, error) {
	s, err := a.changeStacks(func(f *stacksFile) (CustomStack, error) {
		if name != "" && !stackName.MatchString(name) {
			return CustomStack{}, fmt.Errorf("stack name %q: use letters, digits, '.', '_' and '-'", name)
		}
		if name != "" && stackIndex(f, name) >= 0 {
			return CustomStack{}, fmt.Errorf("stack %s already exists; add to it with `saddle stack add %s <task>`", name, name)
		}
		if len(refs) < 2 {
			return CustomStack{}, fmt.Errorf("a stack needs at least two tasks, bottom to top; got %d", len(refs))
		}
		ids, err := a.resolveMembers(*f, "", refs)
		if err != nil {
			return CustomStack{}, err
		}
		if name == "" {
			name = ids[0] + "-stack"
			if stackIndex(f, name) >= 0 {
				return CustomStack{}, fmt.Errorf("stack %s already exists; name this one", name)
			}
		}
		s := CustomStack{Name: name, Tasks: ids, Created: time.Now().UTC()}
		f.Stacks = append(f.Stacks, s)
		return s, nil
	})
	if err != nil {
		return StackReport{}, err
	}
	a.Store.Event("", "stack_create", s.Name+": "+strings.Join(s.Tasks, " → "))
	return a.publishStack(s), nil
}

// AddToStack puts refs on top of stack name, in order, and republishes.
func (a *App) AddToStack(name string, refs []string) (StackReport, error) {
	s, err := a.changeStacks(func(f *stacksFile) (CustomStack, error) {
		i := stackIndex(f, name)
		if i < 0 {
			return CustomStack{}, fmt.Errorf("no stack %s", name)
		}
		ids, err := a.resolveMembers(*f, name, refs)
		if err != nil {
			return CustomStack{}, err
		}
		for _, id := range ids {
			if slices.Contains(f.Stacks[i].Tasks, id) {
				return CustomStack{}, fmt.Errorf("%s is already in stack %s", id, name)
			}
		}
		f.Stacks[i].Tasks = append(f.Stacks[i].Tasks, ids...)
		return f.Stacks[i], nil
	})
	if err != nil {
		return StackReport{}, err
	}
	a.Store.Event("", "stack_add", s.Name+": "+strings.Join(s.Tasks, " → "))
	return a.publishStack(s), nil
}

// RemoveFromStack takes refs out of stack name and republishes: they go
// back to the automatic layout. GitHub's own stack, if gh-stack linked one,
// keeps them; gh-stack never removes PRs from a stack on link.
func (a *App) RemoveFromStack(name string, refs []string) (StackReport, error) {
	s, err := a.changeStacks(func(f *stacksFile) (CustomStack, error) {
		i := stackIndex(f, name)
		if i < 0 {
			return CustomStack{}, fmt.Errorf("no stack %s", name)
		}
		for _, ref := range refs {
			id := strings.TrimSpace(ref)
			if t, err := a.stackRef(ref); err == nil {
				id = t.ID
			}
			j := slices.Index(f.Stacks[i].Tasks, id)
			if j < 0 {
				return CustomStack{}, fmt.Errorf("%s isn't in stack %s", id, name)
			}
			f.Stacks[i].Tasks = slices.Delete(f.Stacks[i].Tasks, j, j+1)
		}
		return f.Stacks[i], nil
	})
	if err != nil {
		return StackReport{}, err
	}
	a.Store.Event("", "stack_remove", s.Name+": "+strings.Join(refs, ", "))
	return a.publishStack(s), nil
}

// DeleteStack forgets stack name and republishes, so its tasks go back to
// the automatic layout. Saddle doesn't unstack it on GitHub.
func (a *App) DeleteStack(name string) (StackReport, error) {
	s, err := a.changeStacks(func(f *stacksFile) (CustomStack, error) {
		i := stackIndex(f, name)
		if i < 0 {
			return CustomStack{}, fmt.Errorf("no stack %s", name)
		}
		s := f.Stacks[i]
		f.Stacks = slices.Delete(f.Stacks, i, i+1)
		return s, nil
	})
	if err != nil {
		return StackReport{}, err
	}
	a.Store.Event("", "stack_delete", s.Name)
	return a.publishStack(s), nil
}

// publishStack runs prs after a stack change and reports it.
func (a *App) publishStack(s CustomStack) StackReport {
	rep := StackReport{Stack: s, Backend: a.stackBackend()}
	res, err := a.publish()
	rep.PRs = res.urls
	if err != nil {
		rep.PublishErr = err.Error()
	}
	for _, l := range res.links {
		if len(l.Tasks) == 0 || !slices.Contains(s.Tasks, l.Tasks[0]) {
			continue
		}
		if l.Err != nil {
			rep.Note = fallbackNote(l.Err)
		} else {
			rep.Linked = true
		}
	}
	return rep
}

// StackMember is one task of a custom stack as it stands.
type StackMember struct {
	Task  string `json:"task"`
	Title string `json:"title,omitempty"`
	PR    string `json:"pr,omitempty"`
	Base  string `json:"base,omitempty"`  // what its PR targets in the layout; "" when out of the stack
	State string `json:"state,omitempty"` // its train state: ok while stacked, else merged, superseded...
}

// StackView is a custom stack with its members' state.
type StackView struct {
	CustomStack
	Members []StackMember `json:"members"`
	// GitHub is the native stack gh-stack linked, with stack_backend = "gh-stack".
	GitHub *ghstack.Stack `json:"github,omitempty"`
}

// ShowStack shows the custom stack named ref, or the one holding the task,
// PR or branch ref.
func (a *App) ShowStack(ref string) (StackView, error) {
	f, err := a.loadStacks()
	if err != nil {
		return StackView{}, err
	}
	i := stackIndex(&f, ref)
	if i < 0 {
		if t, err := a.stackRef(ref); err == nil {
			i = slices.IndexFunc(f.Stacks, func(s CustomStack) bool { return slices.Contains(s.Tasks, t.ID) })
		}
	}
	if i < 0 {
		return StackView{}, fmt.Errorf("no custom stack %s, and no stack holds it", ref)
	}
	v := StackView{CustomStack: f.Stacks[i]}
	unlock, err := a.lockTrain()
	if err != nil {
		return v, err
	}
	defer unlock()
	all, err := a.landedAll()
	if err != nil {
		return v, err
	}
	stack := stacked(all)
	layout, _, err := a.stackLayout(stack)
	if err != nil {
		return v, err
	}
	for _, id := range v.Tasks {
		m := StackMember{Task: id}
		if t, err := a.Store.Task(id); err == nil {
			m.Title, m.PR = t.Title, t.PR
		}
		if j := slices.IndexFunc(all, func(l landedTask) bool { return l.ID == id }); j >= 0 {
			m.State = all[j].State
		}
		if j := slices.IndexFunc(stack, func(l landedTask) bool { return l.ID == id }); j >= 0 {
			m.Base = a.prBase(stack, layout, j)
		}
		v.Members = append(v.Members, m)
	}
	if a.stackBackend() == config.StackBackendGhStack {
		for _, m := range v.Members {
			if m.PR == "" || m.Base == "" {
				continue
			}
			if gs, err := a.GhStack().View(m.PR); err == nil {
				v.GitHub = &gs
			}
			break
		}
	}
	return v, nil
}

// customGroups is each custom stack's layers in stack, as indexes in the
// stack's order, for stacks with at least two layers stacked.
func (a *App) customGroups(stack []landedTask) ([][]int, error) {
	f, err := a.loadStacks()
	if err != nil || len(f.Stacks) == 0 {
		return nil, err
	}
	idx := make(map[string]int, len(stack))
	for i, l := range stack {
		idx[l.ID] = i
	}
	var out [][]int
	for _, s := range f.Stacks {
		var g []int
		for _, id := range s.Tasks {
			if i, ok := idx[id]; ok {
				g = append(g, i)
			}
		}
		if len(g) >= 2 {
			out = append(out, g)
		}
	}
	return out, nil
}

// stackLayout is prLayout with the owner's custom stacks honored (see
// prLayout for the rest). Each custom stack is one stack in the order given;
// its members leave automatic clustering. Layers are visited in order:
// train order, except that the slots a custom stack's layers hold take them
// in the stack's order, so every layer comes after the one its PR sits on;
// prs and restack publish in that order. If a custom stack's order can't be
// replayed (a layer needs work that landed after it), that stack falls back
// to train order. Without custom stacks this is prLayout, in train order;
// with output = "single" custom stacks are ignored.
func (a *App) stackLayout(stack []landedTask) ([]prLayer, []int, error) {
	n := len(stack)
	identity := make([]int, n)
	for i := range identity {
		identity[i] = i
	}
	customs, err := a.customGroups(stack)
	if err != nil {
		return nil, nil, err
	}
	if len(customs) == 0 || a.Cfg.Train.Output == "single" {
		l, err := a.prLayout(stack)
		return l, identity, err
	}
	linear := func() ([]prLayer, []int, error) {
		out := make([]prLayer, n)
		for i, l := range stack {
			out[i] = prLayer{Below: i - 1, Head: l.To}
		}
		return out, identity, nil
	}
	for _, l := range stack {
		if l.Lost != "" || l.From == "" || l.To == "" {
			return linear() // nothing reliable to replay
		}
	}
	files := make([][]string, n)
	for i, l := range stack {
		f, err := gitx.ChangedFiles(a.Root, l.From, l.To)
		if err != nil {
			return linear() //nolint:nilerr // can't tell what it changed: publish the safe layout
		}
		files[i] = f
	}
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	union := func(i, j int) {
		ri, rj := find(i), find(j)
		if ri > rj {
			ri, rj = rj, ri
		}
		parent[rj] = ri
	}
	inCustom := map[int]int{}
	for c, g := range customs {
		for _, i := range g {
			inCustom[i] = c
		}
	}
	if a.Cfg.Train.Output != "per-task" {
		var rest []landedTask
		var restFiles [][]string
		var at []int
		for i, l := range stack {
			if _, ok := inCustom[i]; !ok {
				rest = append(rest, l)
				restFiles = append(restFiles, files[i])
				at = append(at, i)
			}
		}
		if len(rest) >= 2 {
			if err := a.clusterLayers(rest, restFiles, func(i, j int) { union(at[i], at[j]) }); err != nil {
				return linear() //nolint:nilerr // bad planner input: publish the safe layout
			}
		}
	}
	ordered := make([]bool, len(customs))
	for c, g := range customs {
		for _, i := range g[1:] {
			union(g[0], i)
		}
		ordered[c] = !slices.IsSorted(g)
	}

	dir := ""
	defer func() {
		if dir != "" {
			_ = gitx.WorktreeRemove(a.Root, dir)
		}
	}()
	for {
		order := slices.Clone(identity)
		for c, g := range customs {
			if !ordered[c] {
				continue
			}
			slots := slices.Sorted(slices.Values(g))
			for k, p := range slots {
				order[p] = g[k]
			}
		}
		out := make([]prLayer, n)
		top := map[int]int{}
		conflict := -1
		for _, i := range order {
			l := stack[i]
			g := find(i)
			below, ok := top[g]
			if !ok {
				below = -1
			}
			top[g] = i
			tip := stack[0].From
			if below >= 0 {
				tip = out[below].Head
			}
			out[i] = prLayer{Below: below, Head: l.To, Group: g}
			if tip == l.From {
				continue
			}
			if dir == "" && !a.previewLayout {
				dir = a.stateDir("layout")
				_ = os.RemoveAll(dir)
				_, _ = gitx.Run(a.Root, "worktree", "prune")
				if _, err := gitx.Run(a.Root, "worktree", "add", "--detach", dir, tip); err != nil {
					dir = ""
					return nil, nil, err
				}
			}
			head, ok, err := a.replayLayout(dir, tip, l.From, l.To)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				conflict = i
				break
			}
			out[i].Head = head
		}
		if conflict < 0 {
			return out, order, nil
		}
		if c, ok := inCustom[conflict]; ok && ordered[c] {
			ordered[c] = false
			if !a.previewLayout {
				a.Store.Event(stack[conflict].ID, "custom_stack_order",
					"its commits don't apply in the stack's order, so the stack is published in train order")
			}
			continue
		}
		j, ok := needs(conflict, files, find)
		if !ok {
			return linear()
		}
		union(conflict, j)
	}
}

// stackLink is one stack prs or restack linked on GitHub, or tried to.
type stackLink struct {
	Tasks []string // bottom first
	Err   error
}

// ghStackRun runs gh for gh-stack in dir. Tests replace it.
var ghStackRun = func(dir string, args ...string) (string, error) { return gh(dir, args...) }

// GhStack is the gh-stack extension in the repo.
func (a *App) GhStack() ghstack.Client {
	return ghstack.Client{Run: func(args ...string) (string, error) { return ghStackRun(a.Root, args...) }}
}

// stackBackend is [train] stack_backend.
func (a *App) stackBackend() string {
	if a.Cfg.Train.StackBackend == "" {
		return config.StackBackendSaddle
	}
	return a.Cfg.Train.StackBackend
}

// linkStacks links each published stack of two or more PRs on GitHub with
// gh stack link, by PR URL bottom to top, when stack_backend = "gh-stack".
// stack is in layout order and groups[i] is stack[i]'s stack. Nothing here
// fails prs or restack: when gh-stack is missing or the repo lacks Stacked
// PRs the stacks stay chained by PR base only and the owner hears it once;
// any other failure is an event.
func (a *App) linkStacks(stack []store.Task, groups []int) []stackLink {
	if a.stackBackend() != config.StackBackendGhStack {
		return nil
	}
	var order []int
	members := map[int][]store.Task{}
	for i, t := range stack {
		if _, ok := members[groups[i]]; !ok {
			order = append(order, groups[i])
		}
		members[groups[i]] = append(members[groups[i]], t)
	}
	c := a.GhStack()
	var unavailable error
	var links []stackLink
	for _, g := range order {
		ts := members[g]
		if len(ts) < 2 {
			continue
		}
		var ids, refs []string
		for _, t := range ts {
			ids, refs = append(ids, t.ID), append(refs, t.PR)
		}
		if unavailable == nil && links == nil {
			if _, err := c.Available(); err != nil {
				unavailable = err
			}
		}
		if unavailable != nil {
			links = append(links, stackLink{Tasks: ids, Err: unavailable})
			continue
		}
		err := c.Link(a.Cfg.Base, refs...)
		links = append(links, stackLink{Tasks: ids, Err: err})
		switch {
		case errors.Is(err, ghstack.ErrNotEnabled), errors.Is(err, ghstack.ErrNotInstalled):
			unavailable = err
		case err != nil:
			a.Store.Event(ids[0], "gh_stack_link_failed", strings.Join(ids, " → ")+": "+err.Error())
		default:
			a.Store.Event(ids[0], "gh_stack_link", strings.Join(ids, " → "))
		}
	}
	if links != nil {
		a.noteFallback(unavailable)
	}
	return links
}

// noteFallback records why gh-stack can't link stacks, telling the owner
// once per reason, and that it can again. Called under the train lock.
func (a *App) noteFallback(why error) {
	f, err := a.loadStacks()
	if err != nil {
		return
	}
	reason := ""
	if why != nil {
		reason = why.Error()
	}
	if reason == f.Fallback {
		return
	}
	f.Fallback = reason
	if a.saveStacks(f) != nil {
		return
	}
	if reason == "" {
		a.Store.Event("", "gh_stack_restored", "gh-stack links stacks on GitHub again")
		return
	}
	a.Store.Event("", "gh_stack_fallback", reason)
	_ = a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf(
		"stack_backend is gh-stack, but stacks can't be linked on GitHub: %s. Saddle publishes them its own way, chaining PR bases, "+
			"and nothing fails over it; `saddle doctor` shows what gh-stack needs.", reason))
}

// fallbackNote is what a report says when a stack wasn't linked.
func fallbackNote(err error) string {
	return "not linked on GitHub (" + err.Error() + "); published the saddle way, chained by PR bases"
}

// layoutStack is the published stack holding ref, bottom first: a custom
// stack by name, or the stack, custom or clustered, of a task, PR or branch.
func (a *App) layoutStack(ref string) ([]store.Task, error) {
	f, err := a.loadStacks()
	if err != nil {
		return nil, err
	}
	var target string
	if i := stackIndex(&f, ref); i >= 0 && len(f.Stacks[i].Tasks) > 0 {
		target = f.Stacks[i].Tasks[0]
	} else {
		t, err := a.stackRef(ref)
		if err != nil {
			return nil, fmt.Errorf("no custom stack %s, and %w", ref, err)
		}
		target = t.ID
	}
	unlock, err := a.lockTrain()
	if err != nil {
		return nil, err
	}
	defer unlock()
	stack, err := a.landedStack()
	if err != nil {
		return nil, err
	}
	layout, order, err := a.stackLayout(stack)
	if err != nil {
		return nil, err
	}
	at := slices.IndexFunc(stack, func(l landedTask) bool { return l.ID == target })
	if at < 0 {
		return nil, fmt.Errorf("%s isn't in the PR stack", target)
	}
	var out []store.Task
	for _, i := range order {
		if layout[i].Group == layout[at].Group {
			out = append(out, stack[i].Task)
		}
	}
	return out, nil
}

// StackMerge is what stack merge did.
type StackMerge struct {
	Tasks      []string       `json:"tasks"` // bottom first
	PR         string         `json:"pr"`    // the top PR: everything up to it merged
	Method     string         `json:"method,omitempty"`
	Restack    *RestackResult `json:"restack,omitempty"`
	RestackErr string         `json:"restack_error,omitempty"`
}

// MergeStack merges the stack holding ref (a custom stack's name, or a task,
// PR or branch in any stack) atomically with gh stack merge: every PR up to
// the top one merges into base, or none does. It links the stack first, so
// GitHub's stack matches saddle's, then restacks so the merged tasks leave
// the PR stack. It needs stack_backend = "gh-stack", the extension and a
// repo with Stacked PRs; otherwise auto-merge merges stacks bottom-up. An
// empty method uses the repo's (squash, else rebase).
func (a *App) MergeStack(ref, method string) (StackMerge, error) {
	var out StackMerge
	fallback := "`saddle automerge on` merges stacks bottom-up, one PR at a time, instead"
	if a.stackBackend() != config.StackBackendGhStack {
		return out, fmt.Errorf("atomic stack merges need [train] stack_backend = %q (it is %q); %s",
			config.StackBackendGhStack, a.stackBackend(), fallback)
	}
	c := a.GhStack()
	if _, err := c.Available(); err != nil {
		return out, fmt.Errorf("%w; %s", err, fallback)
	}
	if ok, err := c.Enabled(); !ok {
		return out, fmt.Errorf("can't merge with gh-stack: %w; %s", err, fallback)
	}
	ts, err := a.layoutStack(ref)
	if err != nil {
		return out, err
	}
	if len(ts) < 2 {
		return out, fmt.Errorf("%s isn't stacked with another PR; merge its PR on its own", ts[0].ID)
	}
	var refs []string
	for _, t := range ts {
		if t.PR == "" {
			return out, fmt.Errorf("%s has no PR yet; run `saddle prs` first", t.ID)
		}
		out.Tasks, refs = append(out.Tasks, t.ID), append(refs, t.PR)
	}
	out.PR = refs[len(refs)-1]
	if err := c.Link(a.Cfg.Base, refs...); err != nil {
		return out, err
	}
	if method == "" {
		method, _ = (&automerge.GH{Run: automerge.ExecRunner(a.Root)}).MergeMethod()
	}
	out.Method = method
	if err := c.Merge(strconv.Itoa(ghstack.PRNumber(out.PR)), method); err != nil {
		a.Store.Event(out.Tasks[0], "gh_stack_merge_failed", err.Error())
		return out, err
	}
	a.Store.Event(out.Tasks[0], "gh_stack_merge", strings.Join(out.Tasks, " → ")+" up to "+out.PR)
	_ = a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf("Merged the stack %s into %s atomically with gh stack merge.",
		strings.Join(out.Tasks, " → "), a.Cfg.Base))
	res, err := a.Restack()
	out.Restack = &res
	if err != nil {
		out.RestackErr = err.Error()
	}
	return out, nil
}

// StackLinked is one stack `saddle stack link` linked on GitHub, or tried to.
type StackLinked struct {
	Tasks []string `json:"tasks"` // bottom first
	Error string   `json:"error,omitempty"`
}

// LinkStacks publishes the stacks (prs) and links each on GitHub with
// gh-stack; it needs stack_backend = "gh-stack". A stack that couldn't be
// linked says why; publishing goes on regardless.
func (a *App) LinkStacks() ([]StackLinked, error) {
	if a.stackBackend() != config.StackBackendGhStack {
		return nil, fmt.Errorf("linking stacks on GitHub needs [train] stack_backend = %q (it is %q)", config.StackBackendGhStack, a.stackBackend())
	}
	res, err := a.publish()
	var out []StackLinked
	for _, l := range res.links {
		v := StackLinked{Tasks: l.Tasks}
		if l.Err != nil {
			v.Error = l.Err.Error()
		}
		out = append(out, v)
	}
	return out, err
}
