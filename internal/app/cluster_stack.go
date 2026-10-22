package app

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/brandonapol/saddle/internal/config"
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
	Why   string // why it is stacked rather than on base; "" when it isn't
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
	why := map[int]string{} // layer -> why it joined a stack
	union := func(i, j int, reason string) {
		for _, k := range []int{i, j} {
			if _, ok := why[k]; !ok {
				why[k] = reason
			}
		}
		ri, rj := find(i), find(j)
		if ri > rj {
			ri, rj = rj, ri
		}
		parent[rj] = ri // the lower layer roots the group
	}
	linearWhy := "output = \"single\""
	linear := func() []prLayer {
		out := make([]prLayer, n)
		for i, l := range stack {
			out[i] = prLayer{Below: i - 1, Head: l.To}
			if i > 0 {
				out[i].Why = "every landed task is published as one linear stack: " + linearWhy
			}
		}
		return out
	}
	if a.Cfg.Train.Output == "single" || n < 2 {
		return linear(), nil
	}
	for _, l := range stack {
		if l.Lost != "" || l.From == "" || l.To == "" {
			linearWhy = fmt.Sprintf("%s's landed commits can't be found to replay", l.ID)
			return linear(), nil // nothing reliable to replay
		}
	}
	files := make([][]string, n)
	for i, l := range stack {
		f, err := gitx.ChangedFiles(a.Root, l.From, l.To)
		if err != nil {
			linearWhy = fmt.Sprintf("can't tell what %s changed: %v", l.ID, err)
			return linear(), nil //nolint:nilerr // can't tell what it changed: publish the safe layout
		}
		files[i] = f
	}
	if a.Cfg.Train.Output != "per-task" {
		if err := a.clusterLayers(stack, files, union); err != nil {
			linearWhy = "clustering failed: " + err.Error()
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
			if below >= 0 {
				out[i].Why = why[i]
			}
			if tip == l.From {
				continue
			}
			head, ok, err := a.layoutReplay(&dir, tip, l.From, l.To)
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
			linearWhy = "its commits don't apply on any layer below it alone"
			return linear(), nil
		}
		union(conflict, j, fmt.Sprintf("its commits don't apply on base without %s's work", stack[j].ID))
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
// files and derived files ([[regen]] paths) don't count as overlap: two
// tasks that both regenerate an API spec share no topic (#734).
func (a *App) clusterLayers(stack []landedTask, files [][]string, union func(i, j int, why string)) error {
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
		own := slices.DeleteFunc(slices.Clone(files[i]), func(f string) bool {
			return touched[f] >= hubTasks || regenMatch(a.Cfg.Regen, f)
		})
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
			union(idx[s.Tasks[0]], idx[id], s.Reason)
		}
	}
	return nil
}

// replayOnto replays the commits from..to onto tip in the scratch worktree
// dir and returns the new head. ok is false when one conflicts; dir is left
// clean either way.
func replayOnto(dir, tip, from, to string) (head string, ok bool, err error) {
	return replayRegen(dir, tip, from, to, nil)
}

// replayRegen is replayOnto that settles conflicts confined to derived files
// the way the train does: it takes tip's copy of them, and once every commit
// is replayed it reruns their [[regen]] commands and commits the result. Two
// unrelated tasks that both regenerate the same file (an API spec, generated
// queries) then publish side by side on base, not stacked one on the other
// (#734).
func replayRegen(dir, tip, from, to string, regen []config.Regen) (head string, ok bool, err error) {
	settleLayout(dir)
	if _, err := trainGit(dir, "checkout", "-q", "--detach", tip); err != nil {
		return "", false, err
	}
	commits, err := gitx.Run(dir, "rev-list", "--reverse", "--no-merges", from+".."+to)
	if err != nil {
		return "", false, err
	}
	var taken []string
	last := ""
	for _, c := range strings.Fields(commits) {
		ok, files, err := pickStable(dir, c, regen)
		if err != nil || !ok {
			return "", false, err
		}
		for _, f := range files {
			if !slices.Contains(taken, f) {
				taken = append(taken, f)
			}
		}
		last = c
	}
	if len(taken) > 0 {
		if err := regenStable(dir, regen, taken, last); err != nil {
			return "", false, err
		}
	}
	head, err = gitx.RevParse(dir, "HEAD")
	return head, err == nil, err
}

// regenStable reruns the regen commands covering files in dir and commits
// what they change with c's author and committer, so the same replay gives
// the same commit every time.
func regenStable(dir string, regen []config.Regen, files []string, c string) error {
	for _, r := range regenFor(regen, files) {
		if out, err := runShell(dir, r.Cmd); err != nil {
			return fmt.Errorf("regen `%s`: %w: %s", r.Cmd, err, tail(out, 20))
		}
	}
	if _, err := trainGit(dir, "add", "-A"); err != nil {
		return err
	}
	if _, err := gitx.Run(dir, "diff", "--cached", "--quiet"); err == nil {
		return nil // tip's copy was already right
	}
	meta, err := gitx.Run(dir, "log", "-1", "--format=%an%x00%ae%x00%aI%x00%cn%x00%ce%x00%cI", c)
	if err != nil {
		return err
	}
	who := strings.Split(meta, "\x00")
	if len(who) != 6 {
		return fmt.Errorf("can't read the author and committer of %s", c)
	}
	cmd := exec.Command("git", "-C", dir, "commit", "-q", "--no-verify", "-m", "saddle: regenerate "+strings.Join(files, ", "))
	cmd.Env = append(os.Environ(), "SADDLE_TRAIN=1",
		"GIT_AUTHOR_NAME="+who[0], "GIT_AUTHOR_EMAIL="+who[1], "GIT_AUTHOR_DATE="+who[2],
		"GIT_COMMITTER_NAME="+who[3], "GIT_COMMITTER_EMAIL="+who[4], "GIT_COMMITTER_DATE="+who[5])
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("commit regenerated files: %w: %s", err, out)
	}
	return nil
}

// pickStable cherry-picks c in dir as the train, keeping c's committer and
// date so replaying the same commit onto the same tip gives the same SHA. A
// commit that comes out empty is skipped; ok is false on conflict. A
// conflict only in files regen covers keeps HEAD's copy of them and goes on;
// taken lists those files, for the caller to regenerate.
func pickStable(dir, c string, regen []config.Regen) (ok bool, taken []string, err error) {
	meta, err := gitx.Run(dir, "log", "-1", "--format=%cn%x00%ce%x00%cI", c)
	if err != nil {
		return false, nil, err
	}
	who := strings.SplitN(meta, "\x00", 3)
	if len(who) != 3 {
		return false, nil, fmt.Errorf("can't read committer of %s", c)
	}
	cmd := exec.Command("git", "-C", dir, "-c", "merge.directoryRenames=true", "-c", "merge.renames=true",
		"-c", "core.editor=true", "cherry-pick", "--allow-empty-message", c)
	cmd.Env = append(os.Environ(), "SADDLE_TRAIN=1",
		"GIT_COMMITTER_NAME="+who[0], "GIT_COMMITTER_EMAIL="+who[1], "GIT_COMMITTER_DATE="+who[2])
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err == nil {
		return true, nil, nil
	}
	if conf, _ := gitx.Run(dir, "diff", "--name-only", "--diff-filter=U"); conf != "" {
		files := strings.Split(conf, "\n")
		if !allRegen(regen, files) {
			_, _ = trainGit(dir, "cherry-pick", "--abort")
			return false, nil, nil
		}
		ok, err := takeOurs(dir, files, who)
		if err != nil || !ok {
			_, _ = trainGit(dir, "cherry-pick", "--abort")
			return false, nil, err
		}
		return true, files, nil
	}
	if _, e := gitx.RevParse(dir, "CHERRY_PICK_HEAD"); e == nil {
		_, err := trainGit(dir, "cherry-pick", "--skip")
		return err == nil, nil, err
	}
	return false, nil, fmt.Errorf("cherry-pick %s: %s", short(c), strings.TrimSpace(out.String()))
}

// takeOurs settles a cherry-pick stopped on files by keeping HEAD's copy of
// them, then commits as committer who, or skips the pick if nothing else
// changed.
func takeOurs(dir string, files, who []string) (bool, error) {
	if _, err := trainGit(dir, append([]string{"checkout", "--ours", "--"}, files...)...); err != nil {
		return false, err
	}
	if _, err := trainGit(dir, append([]string{"add", "--"}, files...)...); err != nil {
		return false, err
	}
	if _, e := gitx.Run(dir, "diff", "--cached", "--quiet", "HEAD"); e == nil {
		_, err := trainGit(dir, "cherry-pick", "--skip") // the commit only changed derived files
		return err == nil, err
	}
	cmd := exec.Command("git", "-C", dir, "-c", "core.editor=true", "cherry-pick", "--continue")
	cmd.Env = append(os.Environ(), "SADDLE_TRAIN=1",
		"GIT_COMMITTER_NAME="+who[0], "GIT_COMMITTER_EMAIL="+who[1], "GIT_COMMITTER_DATE="+who[2])
	if out, err := cmd.CombinedOutput(); err != nil {
		return false, fmt.Errorf("cherry-pick --continue: %w: %s", err, out)
	}
	return true, nil
}
