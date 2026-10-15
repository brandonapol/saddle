package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/gitx/symbols"
	"github.com/brandonapol/saddle/internal/store"
)

// Rule 2, stale overwrite (#590, docs/symbol-claims.md "Rule 2: stale
// overwrite"): in symbol mode, deny an edit whose lines or symbols
// integration changed after the task's base. The train would hit a conflict
// there, or the edit would silently redo landed work on an old version.

const (
	// RuleStaleOverwrite names the rule in events.
	RuleStaleOverwrite = "stale-overwrite"
	// staleOrder runs rule 2 before rule 1, so a stale deny is final.
	staleOrder = 10
	// staleMargin is the conflict test's margin in base lines.
	staleMargin = 2
	// staleMaxFile mirrors gitx's maxSymbolFile: bigger files fall back.
	staleMaxFile = 2 << 20
	// zeroBlob prefixes git's id for a missing side in diff --raw.
	zeroBlob = "0000000"
)

// staleBudget is the rule's time budget in nanoseconds, the 250ms the
// PreToolUse symbol rules get. Tests swap it.
var staleBudget atomic.Int64

// staleSymbols is the extractor rule 2 parses with, built once.
var staleSymbols = sync.OnceValue(symbols.Default)

// SetStaleBudget sets rule 2's budget, for tests here and in the hook's
// package, and returns a func that restores it.
func SetStaleBudget(d time.Duration) (restore func()) {
	old := staleBudget.Swap(int64(d))
	return func() { staleBudget.Store(old) }
}

func init() {
	staleBudget.Store(int64(250 * time.Millisecond))
	RegisterEditRule(EditRule{Name: RuleStaleOverwrite, Order: staleOrder, Check: checkStale})
}

// errStaleFallback marks a reason to fall back rather than a git failure.
type errStaleFallback string

func (e errStaleFallback) Error() string { return string(e) }

// staleHit is a conflict between the edit and integration's changes.
type staleHit struct {
	symbol  string // the shared symbol, or "" for a range-only conflict
	subject string // the newest commit on integration since the base that touched f
}

// checkStale is rule 2. It only acts on an allow or a path-claim deny (which
// rule 1 might override); serial and outside-worktree denies stand.
func checkStale(a *App, e Edit, d Decision) Decision {
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
	type result struct {
		hit *staleHit
		err error
	}
	ch := make(chan result, 1)
	go func() {
		hit, err := staleCheck(t.Worktree, a.Cfg.Integration, e, x)
		ch <- result{hit, err}
	}()
	timer := time.NewTimer(time.Duration(staleBudget.Load()))
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
		a.Store.Event(e.Task, "symbol_fallback", fmt.Sprintf("%s %s: %s", RuleStaleOverwrite, e.Rel, reason))
		return d
	}
	if r.hit == nil {
		return d
	}
	what := "Code within " + fmt.Sprint(staleMargin) + " lines of your edit"
	logged := "edits within " + fmt.Sprint(staleMargin) + " lines"
	if r.hit.symbol != "" {
		what = "`" + r.hit.symbol + "`"
		logged = r.hit.symbol
	}
	reason := fmt.Sprintf("%s in %s changed on %s since your base (%s). Run `saddle sync`, then redo this edit on the new version.",
		what, e.Rel, a.Cfg.Integration, r.hit.subject)
	if !d.Allow {
		reason += "\n\n" + d.Reason
	}
	a.Store.Event(e.Task, "symbol_deny", fmt.Sprintf("%s %s: %s", RuleStaleOverwrite, e.Rel, logged))
	return Decision{Reason: reason}
}

// staleCheck compares the edit with integration's changes to e.Rel since
// the task's base. A nil hit with a nil error means no conflict (or nothing
// changed on integration).
func staleCheck(dir, integration string, e Edit, x *symbols.Extractor) (*staleHit, error) {
	if integration == "" {
		return nil, errors.New("no integration branch configured")
	}
	base, err := gitx.Run(dir, "merge-base", "HEAD", integration)
	if err != nil {
		return nil, err
	}
	raw, err := gitx.Run(dir, "diff", "--raw", "--no-abbrev", "--no-renames", base, integration, "--", e.Rel)
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, nil // M = I, or M:f = I:f
	}
	oldBlob, newBlob, err := parseRawBlobs(raw)
	if err != nil {
		return nil, err
	}
	if !e.Known {
		return nil, errStaleFallback("unknown proposed content")
	}
	if len(e.After) > staleMaxFile {
		return nil, errStaleFallback("file over 2 MiB")
	}
	parse := func(path string, src []byte) (gitx.Span, error) {
		syms, errs, err := x.Parse(path, src)
		return gitx.Span{Symbols: syms, Errors: errs}, err
	}
	common, err := gitx.Run(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	cache := gitx.OpenSpanCache(common)
	lang := x.Language(e.Rel)

	baseContent, err := blobContent(dir, oldBlob)
	if err != nil {
		return nil, err
	}
	var baseSpan gitx.Span
	if oldBlob != "" {
		if baseSpan, err = cache.Get(lang, oldBlob, func() (gitx.Span, error) { return parse(e.Rel, baseContent) }); err != nil {
			return nil, err
		}
	}
	integ, err := integrationChanges(cache, dir, lang, e.Rel, oldBlob, newBlob, baseContent, baseSpan, parse)
	if err != nil {
		return nil, err
	}
	mine, err := checkedChangeSet(e.Rel, baseContent, baseSpan, e.After, parse)
	if err != nil {
		return nil, err
	}
	if _, conflict := gitx.Conflicts(mine, integ, staleMargin); !conflict {
		return nil, nil
	}
	subject, err := gitx.Run(dir, "log", "-n1", "--format=%s", base+".."+integration, "--", e.Rel)
	if err != nil {
		return nil, err
	}
	return &staleHit{symbol: sharedSymbol(mine, integ), subject: subject}, nil
}

// parseRawBlobs reads the old and new blob ids from one `git diff --raw`
// line. A missing side (an added or deleted file) is "".
func parseRawBlobs(raw string) (oldBlob, newBlob string, err error) {
	line, _, _ := strings.Cut(raw, "\n")
	f := strings.Fields(line)
	if len(f) < 4 || !strings.HasPrefix(f[0], ":") {
		return "", "", fmt.Errorf("unexpected diff --raw output %q", line)
	}
	oldBlob, newBlob = f[2], f[3]
	if strings.HasPrefix(oldBlob, zeroBlob) && strings.Trim(oldBlob, "0") == "" {
		oldBlob = ""
	}
	if strings.HasPrefix(newBlob, zeroBlob) && strings.Trim(newBlob, "0") == "" {
		newBlob = ""
	}
	return oldBlob, newBlob, nil
}

// blobContent reads a blob untrimmed, nil for "".
func blobContent(dir, blob string) ([]byte, error) {
	if blob == "" {
		return nil, nil
	}
	cmd := exec.Command("git", "-C", dir, "--no-optional-locks", "cat-file", "blob", blob)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git cat-file blob %s: %w: %s", blob, err, strings.TrimSpace(stderr.String()))
	}
	if len(out) > staleMaxFile {
		return nil, errStaleFallback("file over 2 MiB")
	}
	return out, nil
}

// integrationChanges is integration's change set of rel from oldBlob to
// newBlob, cached on disk by the blob pair in the span cache (its Symbols
// are the change set's symbols, its Errors the change set's ranges), so
// every hook call after the first skips the read, diff and parse.
func integrationChanges(cache *gitx.SpanCache, dir, lang, rel, oldBlob, newBlob string, base []byte, baseSpan gitx.Span, parse gitx.SpanParse) (gitx.ChangeSet, error) {
	sum := sha256.Sum256([]byte(oldBlob + "\x00" + newBlob))
	sp, err := cache.Get("changes-"+lang, hex.EncodeToString(sum[:]), func() (gitx.Span, error) {
		next, err := blobContent(dir, newBlob)
		if err != nil {
			return gitx.Span{}, err
		}
		cs, err := checkedChangeSet(rel, base, baseSpan, next, parse)
		if err != nil {
			return gitx.Span{}, err
		}
		return gitx.Span{Symbols: cs.Symbols, Errors: cs.Ranges}, nil
	})
	if err != nil {
		return gitx.ChangeSet{}, err
	}
	return gitx.ChangeSet{Symbols: sp.Symbols, Ranges: sp.Errors}, nil
}

// checkedChangeSet is ChangeSetFrom with the fallback for parse errors: an
// error range on either side that touches a changed range of that side
// means the symbols there can't be trusted. baseSpan is base's parse.
func checkedChangeSet(rel string, base []byte, baseSpan gitx.Span, next []byte, parse gitx.SpanParse) (gitx.ChangeSet, error) {
	h := gitx.LineDiff(base, next)
	if len(h.Old) == 0 && len(h.New) == 0 {
		return gitx.ChangeSet{}, nil
	}
	var nextSpan gitx.Span
	if len(next) > 0 {
		var err error
		if nextSpan, err = parse(rel, next); err != nil {
			return gitx.ChangeSet{}, err
		}
	}
	if touchesError(baseSpan.Errors, h.Old) || touchesError(nextSpan.Errors, h.New) {
		return gitx.ChangeSet{}, errStaleFallback("parse error in a changed range")
	}
	return gitx.ChangeSetFrom(rel, base, next, func(_ string, src []byte) (gitx.Span, error) {
		if bytes.Equal(src, base) {
			return baseSpan, nil
		}
		return nextSpan, nil
	})
}

// touchesError reports whether a changed range overlaps or sits next to a
// parse-error range. An insertion counts against its anchor line and the
// line after it.
func touchesError(errs, changed []gitx.LineRange) bool {
	for _, c := range changed {
		lo, hi := c.Start, c.End
		if hi < lo {
			hi = lo + 1
		}
		for _, r := range errs {
			if r.Start <= hi && lo <= r.End {
				return true
			}
		}
	}
	return false
}

// sharedSymbol is the first symbol both change sets touch, by name and kind,
// or "" when they conflict only by range.
func sharedSymbol(a, b gitx.ChangeSet) string {
	for _, x := range a.Symbols {
		for _, y := range b.Symbols {
			if x.Name != "" && x.Name == y.Name && x.Kind == y.Kind {
				return x.Name
			}
		}
	}
	return ""
}
