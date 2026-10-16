// Package procreap finds and kills the processes left running under one of
// saddle's own temp dirs (#663). A gate's tests start tmux servers, fake
// agents and shells that daemonize out of the gate's process group, so
// killing the group misses them, and they lived on for days.
//
// A process matches a dir when its cwd is under it, one of its arguments
// names a path under it, or (with Env) its TMPDIR or GOTMPDIR is under it.
// Only this user's processes are candidates, and never this process, its
// ancestors or init. Where /proc is missing (macOS) nothing matches.
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

// Match says which of a process's traits tie it to a dir.
type Match struct {
	// Env also matches a process whose TMPDIR or GOTMPDIR is under the dir:
	// everything a gate or test started inherits its temp dir.
	Env bool
	// SkipTmux leaves tmux servers alone. A server started from a task's
	// first window carries that window's command line, but it serves every
	// window, so killing it would take the other tasks down with it.
	SkipTmux bool
}

// procRoot is /proc; tests point it elsewhere.
var procRoot = "/proc"

// Find lists the pids of the processes under dir, sorted.
func Find(dir string, m Match) []int {
	dir, ok := safeDir(dir)
	if !ok {
		return nil
	}
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	skip := ancestors()
	uid := os.Getuid()
	var pids []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || skip[pid] {
			continue
		}
		if under(pid, uid, dir, m) {
			pids = append(pids, pid)
		}
	}
	slices.Sort(pids)
	return pids
}

// Reap kills the processes under dir: SIGTERM, then SIGKILL for whatever
// still matches after grace. It returns how many it signalled. It looks
// again after each round, since a dying shell's children can be reparented
// rather than killed.
func Reap(dir string, grace time.Duration, m Match) int {
	seen := map[int]bool{}
	for range 3 {
		pids := Find(dir, m)
		if len(pids) == 0 {
			break
		}
		for _, p := range pids {
			seen[p] = true
			_ = syscall.Kill(p, syscall.SIGTERM)
		}
		deadline := time.Now().Add(grace)
		for time.Now().Before(deadline) && len(Find(dir, m)) > 0 {
			time.Sleep(20 * time.Millisecond)
		}
		// Look again rather than trust pids: one may have exited and its
		// pid been reused by something that isn't ours.
		for _, p := range Find(dir, m) {
			seen[p] = true
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return len(seen)
}

// safeDir cleans dir and refuses one too broad to reap under: relative,
// the root, or a top-level dir such as /tmp or /home.
func safeDir(dir string) (string, bool) {
	if !filepath.IsAbs(dir) {
		return "", false
	}
	dir = filepath.Clean(dir)
	if strings.Count(dir, string(filepath.Separator)) < 2 {
		return "", false
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(home) == dir {
		return "", false
	}
	return dir, true
}

// ancestors is this process and every process above it.
func ancestors() map[int]bool {
	out := map[int]bool{}
	for pid := os.Getpid(); pid > 1 && !out[pid]; {
		out[pid] = true
		b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
		if err != nil {
			break
		}
		// pid (comm) state ppid ...; comm may hold spaces and parens.
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			break
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 2 {
			break
		}
		pid, _ = strconv.Atoi(f[1])
	}
	return out
}

// under reports whether pid is this user's and tied to dir.
func under(pid, uid int, dir string, m Match) bool {
	base := filepath.Join(procRoot, strconv.Itoa(pid))
	var st syscall.Stat_t
	if syscall.Stat(base, &st) != nil || int(st.Uid) != uid {
		return false
	}
	if m.SkipTmux {
		if b, err := os.ReadFile(filepath.Join(base, "comm")); err == nil && strings.HasPrefix(string(b), "tmux") {
			return false
		}
	}
	if cwd, err := os.Readlink(filepath.Join(base, "cwd")); err == nil && within(dir, cwd) {
		return true
	}
	if b, err := os.ReadFile(filepath.Join(base, "cmdline")); err == nil && mentions(dir, nulFields(b)) {
		return true
	}
	if !m.Env {
		return false
	}
	b, err := os.ReadFile(filepath.Join(base, "environ"))
	if err != nil {
		return false
	}
	for _, kv := range nulFields(b) {
		for _, k := range []string{"TMPDIR=", "GOTMPDIR="} {
			if v, ok := strings.CutPrefix(kv, k); ok && within(dir, v) {
				return true
			}
		}
	}
	return false
}

// mentions reports whether an argument names dir or a path under it, alone
// or inside a longer argument (sh -c 'cd DIR/x && …', --flag=DIR/x).
func mentions(dir string, args []string) bool {
	for _, a := range args {
		if a == dir || strings.Contains(a, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func within(root, p string) bool {
	p = strings.TrimSuffix(p, " (deleted)")
	rel, err := filepath.Rel(root, filepath.Clean(p))
	return err == nil && filepath.IsAbs(p) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func nulFields(b []byte) []string {
	return strings.FieldsFunc(string(b), func(r rune) bool { return r == 0 })
}
