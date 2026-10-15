package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/gitx/symbols"
	"github.com/brandonapol/saddle/internal/store"
)

// Rule 1, symbol-granular writes in a shared file (#591, docs/symbol-claims.md
// "Rule 1"): in symbol mode, a task may write a file another task claims
// when the two edits touch different symbols and sit more than shareMargin
// lines apart, so git's merge can't conflict. The first writer of a symbol
// wins it, the path owner included (owner question Q1): a later write that
// overlaps is denied with the earlier writer's hunk.

const (
	// RuleSymbolShare names the rule in events.
	RuleSymbolShare = "symbol-share"
	// shareOrder runs rule 1 after rule 2 (staleOrder).
	shareOrder = 20
	// shareMargin is the conflict test's margin in base lines.
	shareMargin = 2
)

// shareBudget is the rule's time budget in nanoseconds. Tests swap it.
var shareBudget atomic.Int64

// SetShareBudget sets rule 1's budget, for tests here and in the hook's
// package, and returns a func that restores it.
func SetShareBudget(d time.Duration) (restore func()) {
	old := shareBudget.Swap(int64(d))
	return func() { shareBudget.Store(old) }
}

func init() {
	shareBudget.Store(int64(250 * time.Millisecond))
	RegisterEditRule(EditRule{Name: RuleSymbolShare, Order: shareOrder, Check: checkShare})
}

// shareHit is a conflict between the writer's change set and a candidate's.
type shareHit struct {
	task   string // the candidate the edit conflicts with
	symbol string // the shared symbol, or "" for a range-only conflict
}

// shareResult is what the rule's git work found: a conflict, or else the
// writer's change set to record.
type shareResult struct {
	hit  *shareHit
	mine gitx.ChangeSet
}

// checkShare is rule 1. It acts on a path-claim deny (Owner set), which it
// may turn into an allow, and on an allow when another live task holds
// symbol claims in the file, which it may turn into a deny. Serial,
// outside-worktree and stale denies (no Owner) stand.
func checkShare(a *App, e Edit, d Decision) Decision {
	if !d.Allow && d.Owner == "" {
		return d
	}
	x := staleSymbols()
	if !x.Supports(e.Rel) {
		return d
	}
	t, err := a.Store.Task(e.Task)
	if err != nil || t.Role != store.RoleWorker || !t.Active() || t.Worktree == "" {
		return d
	}
	rows, err := a.Store.SymbolClaims(e.Rel)
	if err != nil {
		return a.shareFallback(e, d, "store: "+err.Error())
	}
	var cands []string
	if !d.Allow {
		cands = append(cands, d.Owner)
	}
	for _, r := range rows {
		cands = append(cands, r.Task)
	}
	slices.Sort(cands)
	cands = slices.Compact(cands)
	cands = slices.DeleteFunc(cands, func(c string) bool { return c == e.Task })
	if len(cands) == 0 {
		return d
	}
	var tasks []store.Task
	for _, c := range cands {
		ct, err := a.Store.Task(c)
		if err != nil || !ct.Active() {
			continue
		}
		if a.hookless(ct) {
			return a.shareFallback(e, d, c+" runs without hooks")
		}
		if ct.Worktree == "" {
			return a.shareFallback(e, d, c+" has no worktree")
		}
		tasks = append(tasks, ct)
	}

	type result struct {
		r   shareResult
		err error
	}
	ch := make(chan result, 1)
	go func() {
		r, err := shareCheck(t.Worktree, a.Cfg.Integration, e, tasks, x)
		ch <- result{r, err}
	}()
	timer := time.NewTimer(time.Duration(shareBudget.Load()))
	defer timer.Stop()
	var r result
	select {
	case r = <-ch:
	case <-timer.C:
		r.err = errStaleFallback("budget exceeded")
	}
	if r.err != nil {
		var fb errStaleFallback
		reason := "git error: " + r.err.Error()
		if errors.As(r.err, &fb) {
			reason = string(fb)
		}
		return a.shareFallback(e, d, reason)
	}
	if r.r.hit != nil {
		return a.shareDeny(e, d, *r.r.hit)
	}
	want := make([]store.SymbolClaim, 0, len(r.r.mine.Symbols))
	for _, s := range r.r.mine.Symbols {
		want = append(want, store.SymbolClaim{Task: e.Task, Path: e.Rel, Symbol: s.Name, Kind: s.Kind})
	}
	clash, err := a.Store.ClaimSymbols(e.Task, e.Rel, want)
	if err != nil {
		return a.shareFallback(e, d, "store: "+err.Error())
	}
	if clash != nil {
		return a.shareDeny(e, d, shareHit{task: clash.Task, symbol: clash.Symbol})
	}
	a.Store.Event(e.Task, "symbol_allow", fmt.Sprintf("%s %s: %s", RuleSymbolShare, e.Rel, strings.Join(cands, ",")))
	return Decision{Allow: true}
}

// shareFallback logs why rule 1 could not decide and keeps today's verdict.
func (a *App) shareFallback(e Edit, d Decision, reason string) Decision {
	a.Store.Event(e.Task, "symbol_fallback", fmt.Sprintf("%s %s: %s", RuleSymbolShare, e.Rel, reason))
	return d
}

// shareDeny denies an edit that overlaps h.task's. Owner is set to h.task,
// so the hook quotes that task's change to the file; no rule runs after
// rule 1 to read it as a path-claim deny.
func (a *App) shareDeny(e Edit, d Decision, h shareHit) Decision {
	what := fmt.Sprintf("Code within %d lines of your edit", shareMargin)
	logged := fmt.Sprintf("edits within %d lines", shareMargin)
	if h.symbol != "" {
		what = "`" + h.symbol + "`"
		logged = h.symbol
	}
	ot, _ := a.Store.Task(h.task)
	reason := fmt.Sprintf("%s in %s is already being changed by %s (%q). Tasks share a file only when their edits touch different symbols more than %d lines apart, and the first writer of a symbol keeps it. "+
		"Leave it to %s: change other symbols, depend on its new shape, or finish and mention the change you need in your done summary.",
		what, e.Rel, h.task, ot.Title, shareMargin, h.task)
	if !d.Allow && d.Owner != h.task {
		reason += "\n\n" + d.Reason
	}
	a.Store.Event(e.Task, "symbol_deny", fmt.Sprintf("%s %s: %s with %s", RuleSymbolShare, e.Rel, logged, h.task))
	return Decision{Reason: reason, Owner: h.task}
}

// shareCheck compares the writer's proposed change to e.Rel with each
// candidate's live change to it. All sides must start from the same base
// blob of the file, or their line numbers don't line up.
func shareCheck(dir, integration string, e Edit, cands []store.Task, x *symbols.Extractor) (shareResult, error) {
	if integration == "" {
		return shareResult{}, errors.New("no integration branch configured")
	}
	if !e.Known {
		return shareResult{}, errStaleFallback("unknown proposed content")
	}
	if len(e.After) > staleMaxFile {
		return shareResult{}, errStaleFallback("file over 2 MiB")
	}
	common, err := gitx.Run(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return shareResult{}, err
	}
	s := shareSides{cache: gitx.OpenSpanCache(common), lang: x.Language(e.Rel), rel: e.Rel, integration: integration,
		parse: func(path string, src []byte) (gitx.Span, error) {
			syms, errs, err := x.Parse(path, src)
			return gitx.Span{Symbols: syms, Errors: errs}, err
		}}
	blob, mine, err := s.changeSet(dir, e.After, true)
	if err != nil {
		return shareResult{}, err
	}
	for _, c := range cands {
		cblob, theirs, err := s.changeSet(c.Worktree, nil, false)
		if err != nil {
			return shareResult{}, err
		}
		if cblob != blob {
			return shareResult{}, errStaleFallback(fmt.Sprintf("%s's base of the file differs", c.ID))
		}
		if _, conflict := gitx.Conflicts(mine, theirs, shareMargin); conflict {
			return shareResult{hit: &shareHit{task: c.ID, symbol: sharedSymbol(mine, theirs)}}, nil
		}
	}
	return shareResult{mine: mine}, nil
}

// shareSides computes change sets of one file in several worktrees.
type shareSides struct {
	cache       *gitx.SpanCache
	lang, rel   string
	integration string
	parse       gitx.SpanParse
}

// changeSet is the change set of the file in the worktree dir against
// merge-base(HEAD, integration), and that base's blob id ("" when the file
// is new). The new side is next when given, else the file on disk.
func (s shareSides) changeSet(dir string, next []byte, given bool) (string, gitx.ChangeSet, error) {
	base, err := gitx.Run(dir, "merge-base", "HEAD", s.integration)
	if err != nil {
		return "", gitx.ChangeSet{}, err
	}
	tree, err := gitx.Run(dir, "ls-tree", base, "--", s.rel)
	if err != nil {
		return "", gitx.ChangeSet{}, err
	}
	var blob string
	if f := strings.Fields(tree); len(f) >= 3 {
		if f[1] != "blob" {
			return "", gitx.ChangeSet{}, errStaleFallback("not a regular file at the base")
		}
		blob = f[2]
	}
	baseContent, err := blobContent(dir, blob)
	if err != nil {
		return "", gitx.ChangeSet{}, err
	}
	var baseSpan gitx.Span
	if blob != "" {
		if baseSpan, err = s.cache.Get(s.lang, blob, func() (gitx.Span, error) { return s.parse(s.rel, baseContent) }); err != nil {
			return "", gitx.ChangeSet{}, err
		}
	}
	if !given {
		next, err = os.ReadFile(filepath.Join(dir, filepath.FromSlash(s.rel)))
		if err != nil && !os.IsNotExist(err) {
			return "", gitx.ChangeSet{}, err
		}
		if len(next) > staleMaxFile {
			return "", gitx.ChangeSet{}, errStaleFallback("file over 2 MiB")
		}
	}
	cs, err := checkedChangeSet(s.rel, baseContent, baseSpan, next, s.parse)
	return blob, cs, err
}
