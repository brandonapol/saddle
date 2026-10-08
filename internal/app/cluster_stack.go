package app

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/planner"
)

// The train lands every task on one linear integration branch, and that
// branch stays the record the stack checks validate. What prs publishes is a
// layout over it (#52): tasks that depend on each other or share a topic form
// a stack, each PR on the one below it; unrelated tasks get PRs on base that
// hold only their own work. A task whose commits don't apply without the work
// below it in the train joins that work's stack, so in the worst case the
// layout is the one linear stack.

// prLayer is a stacked task's place in the published layout.
type prLayer struct {
	Below int    // index of the layer its PR targets; -1 targets base
	Head  string // the commit its PR branch is published at
	Group int    // the stack it is in, as the index of its bottom layer
}

// prLayout lays out stack, in train order, as PR stacks under
// [train] output. A layer whose PR sits on its train predecessor (or, at the
// bottom, where the train started) publishes the commit it landed; any other
// has its landed commits replayed onto the head of the layer it sits on, with
// their original committer and date so the same layout yields the same
// commits every time. No ref outside a scratch worktree moves.
func (a *App) prLayout(stack []landedTask) ([]prLayer, error) {
	n := len(stack)
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
		parent[rj] = ri // the lower layer roots the group
	}
	linear := func() []prLayer {
		out := make([]prLayer, n)
		for i, l := range stack {
			out[i] = prLayer{Below: i - 1, Head: l.To}
		}
		return out
	}
	if a.Cfg.Train.Output == "single" || n < 2 {
		return linear(), nil
	}
	for _, l := range stack {
		if l.Lost != "" || l.From == "" || l.To == "" {
			return linear(), nil // nothing reliable to replay
		}
	}
	files := make([][]string, n)
	for i, l := range stack {
		f, err := gitx.ChangedFiles(a.Root, l.From, l.To)
		if err != nil {
			return linear(), nil //nolint:nilerr // can't tell what it changed: publish the safe layout
		}
		files[i] = f
	}
	if a.Cfg.Train.Output != "per-task" {
		if err := a.clusterLayers(stack, files, union); err != nil {
			return linear(), nil //nolint:nilerr // bad planner input: publish the safe layout
		}
	}

	dir := ""
	defer func() {
		if dir != "" {
			_ = gitx.WorktreeRemove(a.Root, dir)
		}
	}()
	for {
		out := make([]prLayer, n)
		top := map[int]int{} // group -> its highest layer so far
		conflict := -1
		for i, l := range stack {
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
					return nil, err
				}
			}
			head, ok, err := replayOnto(dir, tip, l.From, l.To)
			if err != nil {
				return nil, err
			}
			if !ok {
				conflict = i
				break
			}
			out[i].Head = head
		}
		if conflict < 0 {
			return out, nil
		}
		j, ok := needs(conflict, files, find)
		if !ok {
			return linear(), nil
		}
		union(conflict, j)
	}
}

// needs picks the earlier layer, outside i's group, that layer i's work most
// likely needs: the latest one that changed a file i changes, else the latest
// one (#226). ok is false when every earlier layer is already in i's group.
func needs(i int, files [][]string, find func(int) int) (j int, ok bool) {
	mine := make(map[string]bool, len(files[i]))
	for _, f := range files[i] {
		mine[f] = true
	}
	fallback := -1
	for j := i - 1; j >= 0; j-- {
		if find(j) == find(i) {
			continue
		}
		if slices.ContainsFunc(files[j], func(f string) bool { return mine[f] }) {
			return j, true
		}
		if fallback < 0 {
			fallback = j
		}
	}
	return fallback, fallback >= 0
}

// hubTasks is how many stacked tasks must change a file before it counts as
// shared infrastructure (docs, registries, central config) rather than a
// topic. Overlap on a hub doesn't link tasks; if their edits to it really
// conflict, replay stacks them anyway (#226).
const hubTasks = 3

// clusterLayers joins the layers planner.Cluster puts in one stack: tasks
// that changed the same files (serial files aside), a task and the stacked
// task that spawned it, a task and those it was spawned to run after (#193),
// and tasks for the same issue. files holds what each layer changed; hub
// files don't count as overlap.
func (a *App) clusterLayers(stack []landedTask, files [][]string, union func(i, j int)) error {
	idx := make(map[string]int, len(stack))
	for i, l := range stack {
		idx[l.ID] = i
	}
	touched := map[string]int{}
	for _, fs := range files {
		for _, f := range fs {
			touched[f]++
		}
	}
	tasks := make([]planner.Task, len(stack))
	for i, l := range stack {
		own := slices.DeleteFunc(slices.Clone(files[i]), func(f string) bool { return touched[f] >= hubTasks })
		pt := planner.Task{ID: l.ID, Title: l.Title, Claims: own}
		if j, ok := idx[l.Parent]; ok && j < i {
			pt.After = []string{l.Parent}
		}
		for _, d := range a.TaskAfter(l.ID) {
			if j, ok := idx[d]; ok && j < i && !slices.Contains(pt.After, d) {
				pt.After = append(pt.After, d)
			}
		}
		if l.Issue > 0 {
			pt.Issues = []string{strconv.Itoa(l.Issue)}
		}
		tasks[i] = pt
	}
	stacks, err := planner.Cluster(tasks, a.Cfg.Serial)
	if err != nil {
		return err
	}
	for _, s := range stacks {
		for _, id := range s.Tasks[1:] {
			union(idx[s.Tasks[0]], idx[id])
		}
	}
	return nil
}

// replayOnto replays the commits from..to onto tip in the scratch worktree
// dir and returns the new head. ok is false when one conflicts; dir is left
// clean either way.
func replayOnto(dir, tip, from, to string) (head string, ok bool, err error) {
	if _, err := trainGit(dir, "checkout", "-q", "--detach", tip); err != nil {
		return "", false, err
	}
	commits, err := gitx.Run(dir, "rev-list", "--reverse", "--no-merges", from+".."+to)
	if err != nil {
		return "", false, err
	}
	for _, c := range strings.Fields(commits) {
		ok, err := pickStable(dir, c)
		if err != nil || !ok {
			return "", false, err
		}
	}
	head, err = gitx.RevParse(dir, "HEAD")
	return head, err == nil, err
}

// pickStable cherry-picks c in dir as the train, keeping c's committer and
// date so replaying the same commit onto the same tip gives the same SHA. A
// commit that comes out empty is skipped; ok is false on conflict.
func pickStable(dir, c string) (ok bool, err error) {
	meta, err := gitx.Run(dir, "log", "-1", "--format=%cn%x00%ce%x00%cI", c)
	if err != nil {
		return false, err
	}
	who := strings.SplitN(meta, "\x00", 3)
	if len(who) != 3 {
		return false, fmt.Errorf("can't read committer of %s", c)
	}
	cmd := exec.Command("git", "-C", dir, "-c", "merge.directoryRenames=true", "-c", "merge.renames=true",
		"-c", "core.editor=true", "cherry-pick", "--allow-empty-message", c)
	cmd.Env = append(os.Environ(), "SADDLE_TRAIN=1",
		"GIT_COMMITTER_NAME="+who[0], "GIT_COMMITTER_EMAIL="+who[1], "GIT_COMMITTER_DATE="+who[2])
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err == nil {
		return true, nil
	}
	if conf, _ := gitx.Run(dir, "diff", "--name-only", "--diff-filter=U"); conf != "" {
		_, _ = trainGit(dir, "cherry-pick", "--abort")
		return false, nil
	}
	if _, e := gitx.RevParse(dir, "CHERRY_PICK_HEAD"); e == nil {
		_, err := trainGit(dir, "cherry-pick", "--skip")
		return err == nil, err
	}
	return false, fmt.Errorf("cherry-pick %s: %s", short(c), strings.TrimSpace(out.String()))
}
