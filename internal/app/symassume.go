package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/gitx/symbols"
	"github.com/brandonapol/saddle/internal/store"
)

// Rule 3, stale assumption (#592, docs/symbol-claims.md). In symbol mode, an
// edit that uses a symbol another live task is changing the signature of
// gets a note, once per (task, symbol, new hash). A signature that landed
// on integration since this task's base is a PreToolUse deny with rule 2's
// wording: Q2's recommendation, because that edit will not compile after
// the next sync. Body-only changes say nothing. Past the budget, or when
// the check cannot decide, it does nothing.

const (
	// RuleStaleAssumption names the rule in events.
	RuleStaleAssumption = "stale-assumption"
	// assumeOrder runs after rule 2, and after rule 1 when that one registers
	// below it, so a stale-overwrite deny is left alone and a landed deny
	// still applies to an edit rule 1 allowed.
	assumeOrder = 30
	// assumeNoteKind records a live note so the same (task, symbol, hash)
	// is not repeated.
	assumeNoteKind = "symbol_note"
)

// assumeBudget is the rule's time budget in nanoseconds, shared by the note
// and the landed deny. The design gives rule 3 300ms across every other
// task; past it the rule emits nothing. Tests swap it. Zero or negative
// is already expired.
var assumeBudget atomic.Int64

var assumeSymbols = sync.OnceValue(symbols.Default)

// SetAssumeBudget sets rule 3's budget, for tests here and in the hook's
// package, and returns a func that restores it.
func SetAssumeBudget(d time.Duration) (restore func()) {
	old := assumeBudget.Swap(int64(d))
	return func() { assumeBudget.Store(old) }
}

func init() {
	assumeBudget.Store(int64(300 * time.Millisecond))
	RegisterEditRule(EditRule{Name: RuleStaleAssumption, Order: assumeOrder, Check: checkAssume})
	RegisterNoteRule(NoteRule{Name: RuleStaleAssumption, Order: assumeOrder, Note: noteAssume})
}

// errAssumeFallback marks a reason to do nothing rather than a git failure.
type errAssumeFallback string

func (e errAssumeFallback) Error() string { return string(e) }

// sigChange is one symbol whose signature differs between two blobs, or
// that the new side deleted. newHash is empty when it was deleted.
type sigChange struct {
	path    string
	name    string
	kind    string
	oldText string
	newText string
	newHash string // empty when the symbol was deleted
}

type assumeResult struct {
	notes      []string
	keys       []string
	deny       string
	denySymbol string
}

// checkAssume is the landed half of rule 3. A signature integration grew
// since the task's base, used by this edit, is denied so the task syncs
// before it builds on the old shape.
func checkAssume(a *App, e Edit, d Decision) Decision {
	if !d.Allow && d.Owner == "" {
		return d
	}
	if !assumeSymbols().Supports(e.Rel) {
		return d
	}
	res, err := runAssume(a, e, landedMatches)
	if err != nil {
		assumeFallback(a, e, err)
		return d
	}
	if res.deny == "" {
		return d
	}
	a.Store.Event(e.Task, "symbol_deny", fmt.Sprintf("%s %s: %s", RuleStaleAssumption, e.Rel, res.denySymbol))
	return Decision{Reason: res.deny}
}

// noteAssume is the live half of rule 3.
func noteAssume(a *App, e Edit) string {
	if !assumeSymbols().Supports(e.Rel) {
		return ""
	}
	res, err := runAssume(a, e, liveMatches)
	if err != nil {
		assumeFallback(a, e, err)
		return ""
	}
	for _, k := range res.keys {
		a.Store.Event(e.Task, assumeNoteKind, k)
	}
	return strings.Join(res.notes, "\n\n")
}

func assumeFallback(a *App, e Edit, err error) {
	var fb errAssumeFallback
	reason := "git error: " + err.Error()
	if errors.As(err, &fb) {
		reason = string(fb)
	}
	a.Store.Event(e.Task, "symbol_fallback", fmt.Sprintf("%s %s: %s", RuleStaleAssumption, e.Rel, reason))
}

// runAssume runs fn inside the budget. A zero budget is already expired, so
// tests can force the silent path without racing the timer.
func runAssume(a *App, e Edit, fn func(*App, store.Task, Edit, []symbols.Use) (assumeResult, error)) (assumeResult, error) {
	if time.Duration(assumeBudget.Load()) <= 0 {
		return assumeResult{}, errAssumeFallback("budget exceeded")
	}
	type out struct {
		res assumeResult
		err error
	}
	ch := make(chan out, 1)
	go func() {
		t, uses, err := prepareAssume(a, e)
		if err != nil || len(uses) == 0 {
			ch <- out{err: err}
			return
		}
		res, err := fn(a, t, e, uses)
		ch <- out{res, err}
	}()
	timer := time.NewTimer(time.Duration(assumeBudget.Load()))
	defer timer.Stop()
	select {
	case o := <-ch:
		return o.res, o.err
	case <-timer.C:
		return assumeResult{}, errAssumeFallback("budget exceeded")
	}
}

// prepareAssume reports the names the edit added. A nil uses and a nil error
// means there is nothing to match (unsupported, no added names, not a worker).
func prepareAssume(a *App, e Edit) (store.Task, []symbols.Use, error) {
	x := assumeSymbols()
	if !x.Supports(e.Rel) {
		return store.Task{}, nil, nil
	}
	if !e.Known {
		return store.Task{}, nil, errAssumeFallback("unknown proposed content")
	}
	if len(e.Before) > staleMaxFile || len(e.After) > staleMaxFile {
		return store.Task{}, nil, errAssumeFallback("file over 2 MiB")
	}
	t, err := a.Store.Task(e.Task)
	if err != nil || t.Role != store.RoleWorker || !t.Active() || t.Worktree == "" {
		return store.Task{}, nil, nil
	}
	uses, err := addedUses(x, e.Rel, e.Before, e.After)
	return t, uses, err
}

// addedUses lists identifiers and selectors on lines the edit added. A parse
// error on those lines falls back: half-written code would mis-name a use.
func addedUses(x *symbols.Extractor, rel string, before, after []byte) ([]symbols.Use, error) {
	h := gitx.LineDiff(before, after)
	lines := map[int]bool{}
	for _, r := range h.New {
		if r.End < r.Start || r.Start <= 0 {
			continue
		}
		for l := r.Start; l <= r.End; l++ {
			lines[l] = true
		}
	}
	if len(lines) == 0 {
		return nil, nil
	}
	_, errs, err := x.Parse(rel, after)
	if err != nil {
		return nil, err
	}
	if touchesError(errs, h.New) {
		return nil, errAssumeFallback("parse error in a changed range")
	}
	return x.Uses(rel, after, lines)
}

type liveChange struct {
	task string
	sig  sigChange
}

func liveMatches(a *App, self store.Task, e Edit, uses []symbols.Use) (assumeResult, error) {
	changes, err := liveSigChanges(a, self)
	if err != nil {
		return assumeResult{}, err
	}
	noted, err := notedSet(a, self.ID)
	if err != nil {
		return assumeResult{}, err
	}
	var res assumeResult
	seen := map[string]bool{}
	for _, c := range changes {
		if !anyUse(e.Rel, uses, c.sig) {
			continue
		}
		id := assumeNoteID(c.task, c.sig)
		if seen[id] || noted[id] {
			continue
		}
		seen[id] = true
		res.notes = append(res.notes, assumeNote(c.task, c.sig))
		res.keys = append(res.keys, id)
	}
	return res, nil
}

func landedMatches(a *App, self store.Task, e Edit, uses []symbols.Use) (assumeResult, error) {
	changes, subjects, err := landedSigChanges(self.Worktree, a.Cfg.Integration)
	if err != nil {
		return assumeResult{}, err
	}
	for _, c := range changes {
		if !anyUse(e.Rel, uses, c) {
			continue
		}
		return assumeResult{
			deny:       assumeDeny(a.Cfg.Integration, subjects[c.path], c),
			denySymbol: c.name,
		}, nil
	}
	return assumeResult{}, nil
}

func anyUse(rel string, uses []symbols.Use, c sigChange) bool {
	for _, u := range uses {
		if useMatches(rel, u, c) {
			return true
		}
	}
	return false
}

// useMatches reports whether u names c. A bare name matches when the symbol
// lives in the edited file's directory. A selector matches when its qualifier
// is the last element of the symbol's directory, which is how another package
// spells the import. Same-directory uses match on the bare name either way,
// so s.Claim in Claim's package matches too.
func useMatches(rel string, u symbols.Use, c sigChange) bool {
	if u.Name == "" || lastSegment(c.name) != u.Name {
		return false
	}
	symDir := path.Dir(c.path)
	if path.Dir(rel) == symDir {
		return true
	}
	return u.Qual != "" && u.Qual == path.Base(symDir)
}

func lastSegment(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

func assumeNote(task string, c sigChange) string {
	newText := c.newText
	if c.newHash == "" {
		newText = "deleted"
	}
	return fmt.Sprintf("%s is changing the signature of `%s` (%s): old `%s` → new `%s`. Depend on the new shape, stub it, or coordinate through the orchestrator.",
		task, c.name, c.path, c.oldText, newText)
}

func assumeDeny(integration, subject string, c sigChange) string {
	return fmt.Sprintf("`%s` in %s changed on %s since your base (%s). Run `saddle sync`, then redo this edit on the new version.",
		c.name, c.path, integration, subject)
}

func assumeNoteID(task string, c sigChange) string {
	return strings.Join([]string{task, c.kind, c.name, c.path, c.newHash}, " ")
}

func notedSet(a *App, task string) (map[string]bool, error) {
	evs, err := a.Store.EventsSince(0)
	if err != nil {
		return nil, err
	}
	m := map[string]bool{}
	for _, e := range evs {
		if e.Task == task && e.Kind == assumeNoteKind {
			m[e.Data] = true
		}
	}
	return m, nil
}

// liveSigChanges is every other live worker's signature changes against its
// own base, signature diffs cached by blob pair.
func liveSigChanges(a *App, self store.Task) ([]liveChange, error) {
	if a.Cfg.Integration == "" {
		return nil, errors.New("no integration branch configured")
	}
	tasks, err := a.Store.Tasks()
	if err != nil {
		return nil, err
	}
	var out []liveChange
	for _, t := range tasks {
		if t.ID == self.ID || t.Role != store.RoleWorker || !t.Active() || t.Worktree == "" {
			continue
		}
		cs, err := worktreeSigChanges(t.Worktree, a.Cfg.Integration)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			out = append(out, liveChange{task: t.ID, sig: c})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].task != out[j].task {
			return out[i].task < out[j].task
		}
		return sigLess(out[i].sig, out[j].sig)
	})
	return out, nil
}

// landedSigChanges is the signature diff of integration since the worktree's
// merge-base, with the newest commit subject per path. subjects is nil when
// nothing landed.
func landedSigChanges(dir, integration string) ([]sigChange, map[string]string, error) {
	if integration == "" {
		return nil, nil, errors.New("no integration branch configured")
	}
	base, err := gitx.Run(dir, "merge-base", "HEAD", integration)
	if err != nil {
		return nil, nil, err
	}
	head, err := gitx.Run(dir, "rev-parse", "--verify", integration)
	if err != nil {
		return nil, nil, err
	}
	if base == head {
		return nil, nil, nil
	}
	files, err := gitx.ChangedFiles(dir, base, head)
	if err != nil {
		return nil, nil, err
	}
	cache, _, err := assumeCache(dir)
	if err != nil {
		return nil, nil, err
	}
	x := assumeSymbols()
	var out []sigChange
	subjects := map[string]string{}
	for _, rel := range files {
		rel = filepath.ToSlash(rel)
		if !x.Supports(rel) {
			continue
		}
		cs, err := revPairChanges(cache, dir, rel, base, head)
		if err != nil {
			return nil, nil, err
		}
		if len(cs) == 0 {
			continue
		}
		subject, err := gitx.Run(dir, "log", "-n1", "--format=%s", base+".."+integration, "--", rel)
		if err != nil {
			return nil, nil, err
		}
		subjects[rel] = subject
		out = append(out, cs...)
	}
	sort.Slice(out, func(i, j int) bool { return sigLess(out[i], out[j]) })
	return out, subjects, nil
}

func worktreeSigChanges(dir, integration string) ([]sigChange, error) {
	base, err := gitx.Run(dir, "merge-base", "HEAD", integration)
	if err != nil {
		return nil, err
	}
	paths, err := changedPaths(dir, base)
	if err != nil {
		return nil, err
	}
	cache, format, err := assumeCache(dir)
	if err != nil {
		return nil, err
	}
	x := assumeSymbols()
	var out []sigChange
	for _, rel := range paths {
		if !x.Supports(rel) {
			continue
		}
		oldBlob, oldSrc, err := revBlob(dir, base, rel)
		if err != nil {
			return nil, err
		}
		newSrc, err := readWorktree(dir, rel)
		if err != nil {
			return nil, err
		}
		newBlob := ""
		if newSrc != nil {
			if newBlob, err = gitx.BlobID(newSrc, format); err != nil {
				return nil, err
			}
		}
		cs, err := cachedSigDiff(cache, x, rel, oldBlob, newBlob, oldSrc, newSrc)
		if err != nil {
			return nil, err
		}
		out = append(out, cs...)
	}
	sort.Slice(out, func(i, j int) bool { return sigLess(out[i], out[j]) })
	return out, nil
}

// revPairChanges diffs rel between two commits. Both sides are blobs, so the
// pair is cached without hashing a worktree file.
func revPairChanges(cache *gitx.SpanCache, dir, rel, oldRev, newRev string) ([]sigChange, error) {
	oldBlob, oldSrc, err := revBlob(dir, oldRev, rel)
	if err != nil {
		return nil, err
	}
	newBlob, newSrc, err := revBlob(dir, newRev, rel)
	if err != nil {
		return nil, err
	}
	return cachedSigDiff(cache, assumeSymbols(), rel, oldBlob, newBlob, oldSrc, newSrc)
}

func assumeCache(dir string) (*gitx.SpanCache, string, error) {
	format, err := gitx.ObjectFormat(dir)
	if err != nil {
		return nil, "", err
	}
	common, err := gitx.Run(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, "", err
	}
	return gitx.OpenSpanCache(common), format, nil
}

// changedPaths is `git diff --name-only base` plus dirty paths the diff omits
// (untracked files). Paths are slash-separated and repo-relative.
func changedPaths(dir, base string) ([]string, error) {
	out, err := gitx.Run(dir, "diff", "--name-only", base)
	if err != nil {
		return nil, err
	}
	var paths []string
	seen := map[string]bool{}
	add := func(p string) {
		p = filepath.ToSlash(strings.TrimSpace(p))
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		paths = append(paths, p)
	}
	if out != "" {
		for _, p := range strings.Split(out, "\n") {
			add(p)
		}
	}
	dirty, err := gitx.Dirty(dir)
	if err != nil {
		return nil, err
	}
	for _, line := range dirty {
		add(porcelainPath(line))
	}
	return paths, nil
}

// porcelainPath is the path in a `git status --porcelain` line. A rename's
// destination is the part after " -> ".
func porcelainPath(line string) string {
	if len(line) < 4 {
		return ""
	}
	rest := line[3:]
	if i := strings.LastIndex(rest, " -> "); i >= 0 {
		return rest[i+len(" -> "):]
	}
	return rest
}

func revBlob(dir, rev, rel string) (id string, content []byte, err error) {
	spec := rev + ":" + rel
	if _, err = gitx.Run(dir, "cat-file", "-e", spec); err != nil {
		if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "exists on disk, but not") {
			return "", nil, nil
		}
		return "", nil, err
	}
	id, err = gitx.Run(dir, "rev-parse", "--verify", spec)
	if err != nil {
		return "", nil, err
	}
	content, err = blobContent(dir, id)
	if err != nil {
		var fb errStaleFallback
		if errors.As(err, &fb) {
			return "", nil, errAssumeFallback(fb.Error())
		}
		return "", nil, err
	}
	return id, content, nil
}

func readWorktree(dir, rel string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) > staleMaxFile {
		return nil, errAssumeFallback("file over 2 MiB")
	}
	return b, nil
}

// sigTextPair is how a cached signature diff stores the two texts. The span
// cache holds symbols, so the pair rides in SigText and the new hash in SigHash.
type sigTextPair struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// cachedSigDiff is the signature diff of one file between two blobs. Equal
// blobs are not a change. A parse error on either side yields no changes:
// a missed warning beats a note about a half-written file.
func cachedSigDiff(cache *gitx.SpanCache, x *symbols.Extractor, rel, oldBlob, newBlob string, oldSrc, newSrc []byte) ([]sigChange, error) {
	if oldBlob == newBlob {
		return nil, nil
	}
	lang := x.Language(rel)
	if lang == "" {
		return nil, nil
	}
	sum := sha256.Sum256([]byte(oldBlob + "\x00" + newBlob))
	sp, err := cache.Get("sigdiff-"+lang, hex.EncodeToString(sum[:]), func() (gitx.Span, error) {
		cs, err := diffSignatures(x, rel, oldSrc, newSrc)
		if err != nil {
			return gitx.Span{}, err
		}
		return encodeSigChanges(cs)
	})
	if err != nil {
		return nil, err
	}
	return decodeSigChanges(sp, rel)
}

func diffSignatures(x *symbols.Extractor, rel string, oldSrc, newSrc []byte) ([]sigChange, error) {
	oldSyms, oldErrs, err := parseSigSide(x, rel, oldSrc)
	if err != nil {
		return nil, err
	}
	newSyms, newErrs, err := parseSigSide(x, rel, newSrc)
	if err != nil {
		return nil, err
	}
	if len(oldErrs) > 0 || len(newErrs) > 0 {
		return nil, nil
	}
	key := func(s gitx.Symbol) string { return s.Kind + "\x00" + s.Name }
	next := map[string]gitx.Symbol{}
	for _, s := range newSyms {
		if s.Name != "" {
			next[key(s)] = s
		}
	}
	var out []sigChange
	for _, o := range oldSyms {
		if o.Name == "" {
			continue
		}
		n, ok := next[key(o)]
		if !ok {
			out = append(out, sigChange{name: o.Name, kind: o.Kind, oldText: o.SigText})
			continue
		}
		if o.SigHash != n.SigHash {
			out = append(out, sigChange{
				name: o.Name, kind: o.Kind,
				oldText: o.SigText, newText: n.SigText, newHash: n.SigHash,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return sigLess(out[i], out[j]) })
	return out, nil
}

func parseSigSide(x *symbols.Extractor, rel string, src []byte) ([]gitx.Symbol, []gitx.LineRange, error) {
	if len(src) == 0 {
		return nil, nil, nil
	}
	if len(src) > staleMaxFile {
		return nil, nil, errAssumeFallback("file over 2 MiB")
	}
	return x.Parse(rel, src)
}

func encodeSigChanges(cs []sigChange) (gitx.Span, error) {
	syms := make([]gitx.Symbol, 0, len(cs))
	for _, c := range cs {
		b, err := json.Marshal(sigTextPair{Old: c.oldText, New: c.newText})
		if err != nil {
			return gitx.Span{}, err
		}
		syms = append(syms, gitx.Symbol{Name: c.name, Kind: c.kind, SigHash: c.newHash, SigText: string(b)})
	}
	return gitx.Span{Symbols: syms}, nil
}

func decodeSigChanges(sp gitx.Span, rel string) ([]sigChange, error) {
	out := make([]sigChange, 0, len(sp.Symbols))
	for _, s := range sp.Symbols {
		var p sigTextPair
		if err := json.Unmarshal([]byte(s.SigText), &p); err != nil {
			return nil, fmt.Errorf("signature diff cache: %w", err)
		}
		out = append(out, sigChange{
			path: rel, name: s.Name, kind: s.Kind,
			oldText: p.Old, newText: p.New, newHash: s.SigHash,
		})
	}
	return out, nil
}

func sigLess(a, b sigChange) bool {
	if a.path != b.path {
		return a.path < b.path
	}
	if a.name != b.name {
		return a.name < b.name
	}
	return a.kind < b.kind
}
