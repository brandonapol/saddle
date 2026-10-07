package app

import (
	"cmp"
	"fmt"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// EventDependsOn is the events-table kind recording a task's explicit after
// edges, comma-separated, when it is spawned.
const EventDependsOn = "depends_on"

// A task spawned with after builds on those tasks' unmerged work (#193):
// a CI job that runs a make target another task adds, say. Their files need
// not overlap, so clustering would otherwise publish them as unrelated PRs on
// base and the dependent one fails CI until the other merges. The edges live
// in .saddle/deps/<task>, one id per line, written once at spawn.

func (a *App) depsPath(id string) string { return a.stateDir("deps", id) }

// cleanAfter trims and dedupes after and checks every id names another task.
func (a *App) cleanAfter(id string, after []string) ([]string, error) {
	var out []string
	for _, d := range after {
		d = strings.TrimSpace(d)
		if d == "" || slices.Contains(out, d) {
			continue
		}
		if d == id {
			return nil, fmt.Errorf("spawn: after: %s can't run after itself", id)
		}
		if _, err := a.Store.Task(d); err != nil {
			return nil, fmt.Errorf("spawn: after: no task %s", d)
		}
		out = append(out, d)
	}
	return out, nil
}

// setAfter records id's after edges and logs them.
func (a *App) setAfter(id string, after []string) error {
	if len(after) == 0 {
		return nil
	}
	if err := os.MkdirAll(a.stateDir("deps"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(a.depsPath(id), []byte(strings.Join(after, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	a.Store.Event(id, EventDependsOn, strings.Join(after, ","))
	return nil
}

// TaskAfter returns the tasks id was spawned to run after, or nil.
func (a *App) TaskAfter(id string) []string {
	b, err := os.ReadFile(a.depsPath(id))
	if err != nil {
		return nil
	}
	return strings.Fields(string(b))
}

// A PR can fail CI only because a sibling it builds on hasn't merged yet:
// t64 ran make test/e2e, which t61 adds (#193). Before a fix is spawned, the
// CI watchers ask dependencyExplains whether a pending sibling's diff
// clearly adds what the failed log says is missing. If one does, no fix
// spawns: once the sibling merges, the restack re-runs CI, and a run still
// red then is a new failure that gets its fix.

// dependencyExplains finds a pending sibling of task whose unmerged work
// adds what log says is missing, and names that thing. Candidates are the
// stacked layers not already below task in its PR stack and live workers'
// unlanded branches; task's after tasks are tried first.
func (a *App) dependencyExplains(task, log string) (sib, thing string, ok bool) {
	if strings.TrimSpace(log) == "" {
		return "", "", false
	}
	type cand struct{ id, from, to string }
	var cands []cand
	seen := map[string]bool{task: true}
	if stack, err := a.landedStack(); err == nil {
		at := slices.IndexFunc(stack, func(l landedTask) bool { return l.ID == task })
		var layout []prLayer
		if at >= 0 {
			if layout, _, err = a.stackLayout(stack); err != nil {
				layout = nil
			}
		}
		for j, l := range stack {
			seen[l.ID] = true
			if j == at || l.From == "" || l.Lost != "" || (layout != nil && above(layout, at, j)) {
				continue // the layers below it are in its base already
			}
			cands = append(cands, cand{l.ID, l.From, l.To})
		}
	}
	if ts, err := a.Store.Tasks(); err == nil {
		for _, t := range ts {
			if seen[t.ID] || t.Role != store.RoleWorker || t.Branch == "" || !t.Active() || t.Status == StatusFailed {
				continue
			}
			if mb, err := gitx.Run(a.Root, "merge-base", a.Cfg.Integration, t.Branch); err == nil && mb != "" {
				cands = append(cands, cand{t.ID, mb, t.Branch})
			}
		}
	}
	after := a.TaskAfter(task)
	slices.SortStableFunc(cands, func(x, y cand) int {
		return cmp.Compare(boolInt(!slices.Contains(after, x.id)), boolInt(!slices.Contains(after, y.id)))
	})
	for _, c := range cands {
		out, err := gitx.Run(a.Root, "diff", "--no-color", "--no-renames", "--no-ext-diff", "-U0", c.from, c.to)
		if err != nil {
			continue
		}
		if thing, ok := missingAddedBy(log, parseDiff(out)); ok {
			return c.id, thing, true
		}
	}
	return "", "", false
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// fileDiff is one file in a unified diff: its new path, whether the diff
// creates it, and the lines it adds.
type fileDiff struct {
	Path  string
	New   bool
	Added []string
}

// parseDiff reads git diff output. Deleted files are dropped.
func parseDiff(s string) []fileDiff {
	var out []fileDiff
	var cur *fileDiff
	inHunk := false
	for _, l := range strings.Split(s, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			out = append(out, fileDiff{})
			cur, inHunk = &out[len(out)-1], false
		case cur == nil:
		case !inHunk && strings.HasPrefix(l, "new file mode"):
			cur.New = true
		case !inHunk && strings.HasPrefix(l, "+++ "):
			if p, ok := strings.CutPrefix(l, "+++ b/"); ok {
				cur.Path = p
			}
		case strings.HasPrefix(l, "@@"):
			inHunk = true
		case inHunk && strings.HasPrefix(l, "+"):
			cur.Added = append(cur.Added, l[1:])
		}
	}
	return slices.DeleteFunc(out, func(f fileDiff) bool { return f.Path == "" })
}

var (
	reMakeTarget    = regexp.MustCompile("No rule to make target [`'‘\"]([^'’\"]+)['’\"]")
	reNoFile        = regexp.MustCompile("([^\\s:'\"`]+): [Nn]o such file or directory")
	reUndefAt       = regexp.MustCompile(`([\w./-]+\.go):\d+(?::\d+)?: undefined: (\w+(?:\.\w+)?)`)
	reUndefPkg      = regexp.MustCompile(`undefined: (\w+)\.(\w+)`)
	reSpecialTarget = regexp.MustCompile(`^\.[A-Z_]+$`)
	reNoMethod      = regexp.MustCompile(`type \*?(?:[\w.]*\.)?(\w+) has no field or method (\w+)`)
)

// missingAddedBy names the first thing log says is missing that diff
// clearly adds: a make target it defines, a file it creates, or a Go func,
// type, var, const or method it declares in the package the log points at.
// It errs toward false: a fix task that wasn't needed costs less than a real
// failure left waiting on a sibling that never fixes it.
func missingAddedBy(log string, diff []fileDiff) (string, bool) {
	for _, m := range reMakeTarget.FindAllStringSubmatch(log, -1) {
		if definesMakeTarget(diff, m[1]) {
			return "make target " + m[1], true
		}
	}
	for _, m := range reNoFile.FindAllStringSubmatch(log, -1) {
		if f, ok := createsFile(diff, m[1]); ok {
			return "file " + f, true
		}
	}
	for _, m := range reUndefAt.FindAllStringSubmatch(log, -1) {
		pkg, name, qualified := strings.Cut(m[2], ".")
		if !qualified {
			name, pkg = pkg, ""
		}
		if declares(diff, name, func(p string) bool {
			if qualified {
				return path.Base(path.Dir(p)) == pkg
			}
			return sameDir(m[1], p)
		}) {
			return "symbol " + m[2], true
		}
	}
	for _, m := range reUndefPkg.FindAllStringSubmatch(log, -1) {
		if declares(diff, m[2], func(p string) bool { return path.Base(path.Dir(p)) == m[1] }) {
			return "symbol " + m[1] + "." + m[2], true
		}
	}
	for _, m := range reNoMethod.FindAllStringSubmatch(log, -1) {
		re := regexp.MustCompile(`^func\s*\(\s*\w*\s*\*?` + m[1] + `(?:\[[^\]]*\])?\s*\)\s*` + m[2] + `\s*[\[(]`)
		for _, f := range diff {
			if strings.HasSuffix(f.Path, ".go") && slices.ContainsFunc(f.Added, re.MatchString) {
				return "method " + m[1] + "." + m[2], true
			}
		}
	}
	return "", false
}

// definesMakeTarget reports whether diff adds a rule for target to a makefile.
func definesMakeTarget(diff []fileDiff, target string) bool {
	for _, f := range diff {
		base := path.Base(f.Path)
		if base != "Makefile" && base != "makefile" && base != "GNUmakefile" && path.Ext(base) != ".mk" {
			continue
		}
		for _, l := range f.Added {
			if strings.HasPrefix(l, "\t") {
				continue // a recipe line
			}
			lhs, rhs, ok := strings.Cut(l, ":")
			if !ok || strings.Contains(lhs, "=") || strings.HasPrefix(rhs, "=") || strings.HasPrefix(rhs, ":=") {
				continue // not a rule, or an assignment
			}
			if reSpecialTarget.MatchString(strings.TrimSpace(lhs)) {
				continue // .PHONY and friends define no target
			}
			if slices.Contains(strings.Fields(lhs), target) {
				return true
			}
		}
	}
	return false
}

// createsFile returns the file diff creates that missing names: the same
// repo path, or an absolute or deeper path ending in it.
func createsFile(diff []fileDiff, missing string) (string, bool) {
	p := path.Clean(missing)
	for _, f := range diff {
		if f.New && (p == f.Path || strings.HasSuffix(p, "/"+f.Path)) {
			return f.Path, true
		}
	}
	return "", false
}

// declares reports whether diff adds a top-level Go declaration of name in
// a file inPkg accepts.
func declares(diff []fileDiff, name string, inPkg func(string) bool) bool {
	re := regexp.MustCompile(`^(?:func(?:\s*\([^)]*\))?|type|var|const)\s+` + regexp.QuoteMeta(name) + `\b`)
	for _, f := range diff {
		if strings.HasSuffix(f.Path, ".go") && inPkg(f.Path) && slices.ContainsFunc(f.Added, re.MatchString) {
			return true
		}
	}
	return false
}

// sameDir reports whether the file the log names (relative to some
// directory, or absolute) is in the same directory as repo path p.
func sameDir(logged, p string) bool {
	d1, d2 := path.Dir(path.Clean(logged)), path.Dir(p)
	return d1 == d2 || (d2 != "." && strings.HasSuffix(d1, "/"+d2))
}
