package procreap

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Every agent saddle launches carries these in its environment, and its
// children inherit them across fork, reparenting and setsid (#690). A task's
// processes are found by them, not by cwd or argv: a backgrounded `yes` has
// a bare argv and sits in the worktree, which is never matched by cwd.
const (
	EnvRoot = "SADDLE_ROOT"
	EnvTask = "SADDLE_TASK"
)

// Tagged maps each task id to the pids of this user's processes tagged
// SADDLE_ROOT=root and SADDLE_TASK=<id>, sorted. It skips tmux servers (one
// serves every window), this process and its ancestors.
func Tagged(root string) map[string][]int {
	out := map[string][]int{}
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		return out
	}
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return out
	}
	skip := ancestors()
	uid := os.Getuid()
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || skip[pid] {
			continue
		}
		base := filepath.Join(procRoot, strconv.Itoa(pid))
		var st syscall.Stat_t
		if syscall.Stat(base, &st) != nil || int(st.Uid) != uid {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(base, "comm")); err == nil && strings.HasPrefix(string(b), "tmux") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(base, "environ"))
		if err != nil {
			continue
		}
		var r, task string
		for _, kv := range nulFields(b) {
			if v, ok := strings.CutPrefix(kv, EnvRoot+"="); ok {
				r = v
			} else if v, ok := strings.CutPrefix(kv, EnvTask+"="); ok {
				task = v
			}
		}
		if task != "" && r != "" && filepath.Clean(r) == root {
			out[task] = append(out[task], pid)
		}
	}
	for _, p := range out {
		slices.Sort(p)
	}
	return out
}

// FindTask lists the pids tagged with root and task, sorted.
func FindTask(root, task string) []int { return Tagged(root)[task] }

// ReapTask kills the processes tagged with root and task: SIGTERM, then
// SIGKILL for whatever still matches after grace. It returns how many it
// signalled, looking again each round since a dying shell's children can be
// reparented rather than killed.
func ReapTask(root, task string, grace time.Duration) int {
	seen := map[int]bool{}
	for range 3 {
		pids := FindTask(root, task)
		if len(pids) == 0 {
			break
		}
		for _, p := range pids {
			seen[p] = true
			_ = syscall.Kill(p, syscall.SIGTERM)
		}
		deadline := time.Now().Add(grace)
		for time.Now().Before(deadline) && len(FindTask(root, task)) > 0 {
			time.Sleep(20 * time.Millisecond)
		}
		// Look again rather than trust pids: one may have exited and its
		// pid been reused by something that isn't ours.
		for _, p := range FindTask(root, task) {
			seen[p] = true
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return len(seen)
}
