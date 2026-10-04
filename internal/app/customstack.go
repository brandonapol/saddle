package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

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
		var at []int
		for i, l := range stack {
			if _, ok := inCustom[i]; !ok {
				rest = append(rest, l)
				at = append(at, i)
			}
		}
		if len(rest) >= 2 {
			if err := a.clusterLayers(rest, func(i, j int) { union(at[i], at[j]) }); err != nil {
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
			if dir == "" {
				dir = a.stateDir("layout")
				_ = os.RemoveAll(dir)
				_, _ = gitx.Run(a.Root, "worktree", "prune")
				if _, err := gitx.Run(a.Root, "worktree", "add", "--detach", dir, tip); err != nil {
					dir = ""
					return nil, nil, err
				}
			}
			head, ok, err := replayOnto(dir, tip, l.From, l.To)
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
			a.Store.Event(stack[conflict].ID, "custom_stack_order",
				"its commits don't apply in the stack's order, so the stack is published in train order")
			continue
		}
		// Its work needs what landed just before it: stack it there.
		if conflict == 0 || find(conflict) == find(conflict-1) {
			return linear()
		}
		union(conflict, conflict-1)
	}
}

// stackLink is one stack prs or restack linked on GitHub, or tried to.
type stackLink struct {
	Tasks []string
	Err   error
}

// linkStacks links the published stacks on GitHub.
func (a *App) linkStacks(stack []store.Task, groups []int) []stackLink { return nil }

// stackBackend is [train] stack_backend.
func (a *App) stackBackend() string { return "saddle" }
