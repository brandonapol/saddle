package gitx

import "time"

// Delta is one change in a watched worktree. The concrete types are
// FileDirty, Committed, RenameDetected, SymbolTouched and Rebased; switch on
// them. Every delta carries the snapshot it was computed from.
type Delta interface {
	From() Origin
}

// Origin says which worktree a delta came from and the state after it.
type Origin struct {
	Task  string
	At    time.Time
	State State
}

// From returns the delta's origin.
func (o Origin) From() Origin { return o }

// FileDirty reports a path whose git status changed. Clean means it is no
// longer dirty (committed, reverted or deleted); Status is then the zero value.
type FileDirty struct {
	Origin
	Path   string
	Status FileStatus
	Clean  bool
}

// Committed reports HEAD moving forward. Commits lists the new commits, oldest first.
type Committed struct {
	Origin
	Branch   string
	Old, New string
	Commits  []string
}

// Rebased reports HEAD moving to a commit that does not descend from the old
// one: a rebase, amend or reset. Base is the new base commit.
type Rebased struct {
	Origin
	Branch   string
	Old, New string
	Base     string
}

// RenameDetected reports a rename git sees, staged or committed on the branch.
type RenameDetected struct {
	Origin
	PathRename
}

// SymbolTouched reports a symbol whose lines changed since the base. Cleared
// means the symbol is no longer touched (the change was reverted).
type SymbolTouched struct {
	Origin
	TouchedSymbol
	Cleared bool
}

// diffStates lists the deltas that take prev to next. forward reports whether
// b descends from a and, if so, the commits a..b oldest first; it is only
// called when HEAD moved.
func diffStates(prev, next State, forward func(a, b string) ([]string, bool)) []Delta {
	o := Origin{Task: next.Task, At: next.At, State: next}
	var ds []Delta

	if prev.Head != "" && next.Head != "" && prev.Head != next.Head {
		if commits, ok := forward(prev.Head, next.Head); ok {
			ds = append(ds, Committed{Origin: o, Branch: next.Branch, Old: prev.Head, New: next.Head, Commits: commits})
		} else {
			ds = append(ds, Rebased{Origin: o, Branch: next.Branch, Old: prev.Head, New: next.Head, Base: next.Base})
		}
	}

	seenRename := map[PathRename]bool{}
	for _, r := range prev.Renames {
		seenRename[r] = true
	}
	for _, r := range next.Renames {
		if !seenRename[r] {
			ds = append(ds, RenameDetected{Origin: o, PathRename: r})
		}
	}

	prevFiles := map[string]FileStatus{}
	for _, f := range prev.Files {
		prevFiles[f.Path] = f
	}
	for _, f := range next.Files {
		if old, ok := prevFiles[f.Path]; !ok || old != f {
			ds = append(ds, FileDirty{Origin: o, Path: f.Path, Status: f})
		}
		delete(prevFiles, f.Path)
	}
	for _, f := range prev.Files {
		if _, gone := prevFiles[f.Path]; gone {
			ds = append(ds, FileDirty{Origin: o, Path: f.Path, Clean: true})
		}
	}

	prevSyms := map[string]bool{}
	for _, s := range prev.Symbols {
		prevSyms[s.Key()] = true
	}
	nextSyms := map[string]bool{}
	for _, s := range next.Symbols {
		nextSyms[s.Key()] = true
		if !prevSyms[s.Key()] {
			ds = append(ds, SymbolTouched{Origin: o, TouchedSymbol: s})
		}
	}
	for _, s := range prev.Symbols {
		if !nextSyms[s.Key()] {
			ds = append(ds, SymbolTouched{Origin: o, TouchedSymbol: s, Cleared: true})
		}
	}
	return ds
}
