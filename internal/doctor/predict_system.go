package doctor

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
)

// PredictSystem is the real PredictEnv for the repo whose main checkout is
// root. a supplies the repo's escalation history and the runtime auto-merge
// switch; nil (before saddle init) means no history and auto-merge from
// config.
func PredictSystem(root string, a *app.App) PredictEnv {
	return predictSystem{system: system(root), app: a}
}

type predictSystem struct {
	system
	app *app.App
}

func (s predictSystem) WorkflowFiles() bool {
	for _, pat := range []string{"*.yml", "*.yaml"} {
		if m, _ := filepath.Glob(filepath.Join(s.Root(), ".github", "workflows", pat)); len(m) > 0 {
			return true
		}
	}
	return false
}

func (s predictSystem) AutoMerge() bool {
	if s.app != nil {
		if st, err := s.app.AutomergeState(); err == nil {
			return st.Enabled
		}
		return s.app.Cfg.Train.AutoMerge
	}
	cfg, err := s.Config()
	return err == nil && cfg.Train.AutoMerge
}

// Disks measures where saddle writes: the worktrees, tmp and the user
// cache (build caches), once per filesystem.
func (s predictSystem) Disks() []Disk {
	wt := filepath.Join(s.Root(), ".saddle", "worktrees")
	if _, err := os.Stat(wt); err != nil {
		wt = s.Root()
	}
	paths := []struct{ label, path string }{{"the worktrees", wt}, {"tmp", os.TempDir()}}
	if c, err := os.UserCacheDir(); err == nil {
		paths = append(paths, struct{ label, path string }{"the build cache", c})
	}
	var out []Disk
	seen := map[string]bool{}
	for _, p := range paths {
		var st syscall.Statfs_t
		if syscall.Statfs(p.path, &st) != nil {
			continue
		}
		id := fmt.Sprint(st.Fsid)
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, Disk{Label: p.label, Path: p.path, Free: st.Bavail * uint64(st.Bsize), FreeInodes: st.Ffree, Inodes: st.Files})
	}
	return out
}

func (s predictSystem) Load() (float64, int) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, runtime.NumCPU()
	}
	f, _ := strconv.ParseFloat(strings.Fields(string(b) + " 0")[0], 64)
	return f, runtime.NumCPU()
}

func (s predictSystem) MemAvailable() uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "MemAvailable:"); ok {
			kb, _ := strconv.ParseUint(strings.Fields(v + " 0")[0], 10, 64)
			return kb * 1024
		}
	}
	return 0
}

func (s predictSystem) Getenv(key string) string { return os.Getenv(key) }

func (s predictSystem) ClaudeVersions() (now, last string) {
	cfg, err := s.Config()
	if err != nil {
		cfg = config.Default()
	}
	now, err = s.Exec(cmdOr(cfg.Claude.Cmd, "claude"), "--version")
	if err != nil {
		now = ""
	}
	b, _ := os.ReadFile(claudeVersionPath(s.Root()))
	return firstLine(now), strings.TrimSpace(string(b))
}

func (s predictSystem) Escalations(since time.Time) ([]Escalation, error) {
	if s.app == nil {
		return nil, nil
	}
	evs, err := s.app.Store.Events(5000)
	if err != nil {
		return nil, err
	}
	var out []Escalation
	for _, e := range evs {
		if e.TS.Before(since) {
			continue
		}
		if k, ok := app.BlockerKindOf(e.Kind); ok {
			out = append(out, Escalation{At: e.TS, Kind: k, Cause: e.Data})
		}
	}
	return out, nil
}

func cmdOr(cmd, def string) string {
	if f := strings.Fields(cmd); len(f) > 0 {
		return f[0]
	}
	return def
}

func claudeVersionPath(root string) string {
	return filepath.Join(root, ".saddle", "claude-version")
}

// RecordClaudeVersion saves the Claude Code version saddle up started with,
// so the next prediction can tell it changed.
func RecordClaudeVersion(root, version string) error {
	if version == "" {
		return nil
	}
	return os.WriteFile(claudeVersionPath(root), []byte(version+"\n"), 0o644)
}
