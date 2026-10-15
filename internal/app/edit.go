package app

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/brandonapol/saddle/internal/config"
)

// Edit is one file write by a tool call, as the hook sees it: the file
// before the call and, when the hook could work it out, after (#588,
// docs/symbol-claims.md "The edit seam").
type Edit struct {
	Task string
	Abs  string
	// Rel is Abs relative to the task's worktree, slash-separated. CheckEdit
	// and EditNotes fill it when it is empty; it stays empty for a path
	// outside the worktree.
	Rel string
	// Before is the file before the call (nil when it did not exist), After
	// the file after it. Known is false when the hook could not compute them
	// (an unsupported tool, an old_string that is missing or ambiguous), and
	// then the symbol rules fall back to the path verdict.
	Before, After []byte
	Known         bool
}

// EditRule is a PreToolUse symbol rule. Check gets the verdict so far and
// returns the new one. Rules run in Order, lowest first.
type EditRule struct {
	Name  string
	Order int
	Check func(a *App, e Edit, d Decision) Decision
}

// NoteRule is a PostToolUse symbol rule. Note returns text for the agent's
// additionalContext, or "".
type NoteRule struct {
	Name  string
	Order int
	Note  func(a *App, e Edit) string
}

// The symbol rule tables. Each rule registers itself from its own file's
// init (symstale.go, symshare.go, symassume.go), so rules never share a file.
var (
	editRules []EditRule
	noteRules []NoteRule
)

// RegisterEditRule adds r to the PreToolUse symbol rules.
func RegisterEditRule(r EditRule) {
	editRules = append(editRules, r)
	slices.SortStableFunc(editRules, func(x, y EditRule) int { return x.Order - y.Order })
}

// RegisterNoteRule adds r to the PostToolUse symbol rules.
func RegisterNoteRule(r NoteRule) {
	noteRules = append(noteRules, r)
	slices.SortStableFunc(noteRules, func(x, y NoteRule) int { return x.Order - y.Order })
}

// SymbolMode reports whether [claims] granularity is "symbol". In "file"
// mode no symbol rule runs and the hook skips computing proposed content.
func (a *App) SymbolMode() bool {
	return a.Cfg.Claims.Granularity == config.GranularitySymbol
}

// CheckEdit decides whether e.Task may make edit e. The base verdict is
// CheckWrite's; in symbol mode the PreToolUse symbol rules then run over it
// in order.
func (a *App) CheckEdit(e Edit) Decision {
	d := a.CheckWrite(e.Task, e.Abs)
	if !a.SymbolMode() || len(editRules) == 0 {
		return d
	}
	if e = a.withRel(e); e.Rel == "" {
		return d
	}
	for _, r := range editRules {
		d = r.Check(a, e, d)
	}
	return d
}

// EditNotes is the text the PostToolUse symbol rules have for the agent
// after edit e, or "". It is always "" in file mode.
func (a *App) EditNotes(e Edit) string {
	if !a.SymbolMode() || len(noteRules) == 0 {
		return ""
	}
	if e = a.withRel(e); e.Rel == "" {
		return ""
	}
	var notes []string
	for _, r := range noteRules {
		if n := strings.TrimSpace(r.Note(a, e)); n != "" {
			notes = append(notes, n)
		}
	}
	return strings.Join(notes, "\n\n")
}

// withRel fills e.Rel from the task's worktree when it is empty.
func (a *App) withRel(e Edit) Edit {
	if e.Rel != "" {
		return e
	}
	t, err := a.Store.Task(e.Task)
	if err != nil || t.Worktree == "" {
		return e
	}
	rel, err := filepath.Rel(filepath.Clean(t.Worktree), filepath.Clean(e.Abs))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return e
	}
	e.Rel = filepath.ToSlash(rel)
	return e
}
