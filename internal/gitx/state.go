package gitx

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FileStatus is one dirty path in a worktree, as git status reports it.
// Index and Worktree are the porcelain XY codes ('.' means unchanged).
type FileStatus struct {
	Path      string
	Index     byte // staged change: M A D R C T U or '.'
	Worktree  byte // unstaged change: M D T U or '.'
	Untracked bool
	Conflict  bool
}

// Staged reports whether the file has changes in the index.
func (f FileStatus) Staged() bool { return f.Index != '.' && f.Index != 0 }

// Code is the two-letter porcelain code, "??" for untracked files.
func (f FileStatus) Code() string {
	if f.Untracked {
		return "??"
	}
	return string([]byte{f.Index, f.Worktree})
}

// PathRename is a rename git detected. Committed means it is already in a
// commit on the branch (base..HEAD); otherwise it is staged in the index.
type PathRename struct {
	Old, New  string
	Committed bool
}

// TouchedSymbol is a function or type whose lines changed since the base.
type TouchedSymbol struct {
	Path string
	Symbol
}

// Key identifies the symbol across snapshots.
func (s TouchedSymbol) Key() string { return s.Path + "\x00" + s.Kind + "\x00" + s.Name }

// State is the live state of one worktree.
type State struct {
	Task   string
	Path   string
	Branch string // empty when HEAD is detached
	Head   string // commit id, empty in a repo with no commits

	Integration      string // commit id of the integration head, empty if unknown
	Base             string // commit the branch is measured from
	Ahead, Behind    int    // vs the integration head
	CommitsSinceBase int

	Files    []FileStatus // sorted by path
	Renames  []PathRename
	Symbols  []TouchedSymbol // sorted by path, then line
	Rebasing bool            // a rebase, merge, cherry-pick or revert is in progress

	At time.Time
}

// File returns the status of path, if it is dirty.
func (s State) File(path string) (FileStatus, bool) {
	i := sort.Search(len(s.Files), func(i int) bool { return s.Files[i].Path >= path })
	if i < len(s.Files) && s.Files[i].Path == path {
		return s.Files[i], true
	}
	return FileStatus{}, false
}

// SnapshotOptions configure Snapshot.
type SnapshotOptions struct {
	// Integration is the ref ahead/behind is measured against, e.g.
	// "saddle/integration". Empty disables ahead/behind.
	Integration string
	// Base is the ref or commit the branch was cut from. Empty means the
	// merge-base of HEAD and Integration, so it follows rebases.
	Base string
	// Symbols extracts functions and types from changed files. Nil disables
	// touched-symbol tracking.
	Symbols SymbolExtractor
	// cache memoises symbol extraction by content; set by the Watcher.
	cache *symbolCache
}

// Snapshot reads the current state of the worktree at dir. It never takes
// optional locks, so it does not rewrite the index and retrigger a watcher.
func Snapshot(dir string, opt SnapshotOptions) (State, error) {
	st := State{Path: dir, At: time.Now()}
	gitDir, err := Run(dir, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return st, err
	}
	st.Rebasing = inProgress(gitDir)

	out, err := git(dir, "status", "--porcelain=v2", "-z", "--branch", "--untracked-files=all", "--find-renames")
	if err != nil {
		return st, err
	}
	var staged []PathRename
	st.Head, st.Branch, st.Files, staged = parseStatus(out)

	if opt.Integration != "" {
		st.Integration, _ = RevParse(dir, opt.Integration)
	}
	if st.Head != "" && st.Integration != "" {
		if lr, err := git(dir, "rev-list", "--left-right", "--count", st.Integration+"..."+st.Head); err == nil {
			_, _ = fmt.Sscan(lr, &st.Behind, &st.Ahead)
		}
	}
	st.Base = resolveBase(dir, opt, st)
	if st.Base != "" && st.Head != "" && st.Base != st.Head {
		st.CommitsSinceBase, _ = CommitsBetween(dir, st.Base, st.Head)
		if rs, err := Renames(dir, st.Base, st.Head); err == nil {
			for _, r := range rs {
				st.Renames = append(st.Renames, PathRename{Old: r.Old, New: r.New, Committed: true})
			}
		}
	}
	st.Renames = append(st.Renames, staged...)

	if opt.Symbols != nil {
		cache := opt.cache
		if cache == nil {
			cache = newSymbolCache()
		}
		st.Symbols, err = touchedSymbols(dir, st, opt.Symbols, cache)
		if err != nil {
			return st, err
		}
	}
	return st, nil
}

func resolveBase(dir string, opt SnapshotOptions, st State) string {
	if st.Head == "" {
		return ""
	}
	if opt.Base != "" {
		if c, err := RevParse(dir, opt.Base); err == nil {
			return c
		}
	}
	if st.Integration != "" {
		if c, err := git(dir, "merge-base", st.Head, st.Integration); err == nil {
			return c
		}
	}
	return st.Head
}

// git runs a read-only git command that must not refresh the index.
func git(dir string, args ...string) (string, error) {
	return Run(dir, append([]string{"--no-optional-locks", "-c", "core.quotePath=false"}, args...)...)
}

func inProgress(gitDir string) bool {
	for _, p := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD"} {
		if _, err := os.Stat(filepath.Join(gitDir, p)); err == nil {
			return true
		}
	}
	return false
}

// parseStatus parses `git status --porcelain=v2 -z --branch`.
func parseStatus(out string) (head, branch string, files []FileStatus, renames []PathRename) {
	recs := strings.Split(out, "\x00")
	for i := 0; i < len(recs); i++ {
		r := recs[i]
		switch {
		case strings.HasPrefix(r, "# branch.oid "):
			if oid := strings.TrimPrefix(r, "# branch.oid "); oid != "(initial)" {
				head = oid
			}
		case strings.HasPrefix(r, "# branch.head "):
			if b := strings.TrimPrefix(r, "# branch.head "); b != "(detached)" {
				branch = b
			}
		case strings.HasPrefix(r, "1 "):
			// 1 XY sub mH mI mW hH hI path
			if f := strings.SplitN(r, " ", 9); len(f) == 9 {
				files = append(files, FileStatus{Path: f[8], Index: f[1][0], Worktree: f[1][1]})
			}
		case strings.HasPrefix(r, "2 "):
			// 2 XY sub mH mI mW hH hI Xscore path, then origPath as the next record.
			if f := strings.SplitN(r, " ", 10); len(f) == 10 && i+1 < len(recs) {
				orig := recs[i+1]
				i++
				files = append(files, FileStatus{Path: f[9], Index: f[1][0], Worktree: f[1][1]})
				if f[8][0] == 'R' {
					renames = append(renames, PathRename{Old: orig, New: f[9]})
				}
			}
		case strings.HasPrefix(r, "u "):
			// u XY sub m1 m2 m3 mW h1 h2 h3 path
			if f := strings.SplitN(r, " ", 11); len(f) == 11 {
				files = append(files, FileStatus{Path: f[10], Index: f[1][0], Worktree: f[1][1], Conflict: true})
			}
		case strings.HasPrefix(r, "? "):
			files = append(files, FileStatus{Path: r[2:], Index: '?', Worktree: '?', Untracked: true})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return head, branch, files, renames
}
