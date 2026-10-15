package gitx

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ChangeSet is one side's edits to a file, in base coordinates. Ranges come
// from LineDiff's old side, so an insertion is a zero-width range (End <
// Start) anchored at its base line. Symbols are the base symbols those
// ranges touch (edited or deleted, including an insertion strictly inside a
// symbol) plus symbols that exist only on the new side.
type ChangeSet struct {
	Ranges  []LineRange
	Symbols []Symbol
}

// SpanParse extracts the symbols and parse-error ranges of one file.
type SpanParse func(path string, src []byte) (Span, error)

// ChangeSetFrom builds the change set from base content to next content.
// parse may be nil when only ranges are needed. path is the repo-relative
// name passed to parse so it can pick a language.
func ChangeSetFrom(path string, base, next []byte, parse SpanParse) (ChangeSet, error) {
	h := LineDiff(base, next)
	cs := ChangeSet{Ranges: h.Old}
	if parse == nil || (len(h.Old) == 0 && len(h.New) == 0) {
		return cs, nil
	}
	baseSyms, err := parseSide(parse, path, base)
	if err != nil {
		return ChangeSet{}, err
	}
	nextSyms, err := parseSide(parse, path, next)
	if err != nil {
		return ChangeSet{}, err
	}
	seen := map[string]bool{}
	add := func(s Symbol) {
		if s.Name == "" {
			return
		}
		k := s.Kind + "\x00" + s.Name
		if seen[k] {
			return
		}
		seen[k] = true
		cs.Symbols = append(cs.Symbols, s)
	}
	for _, s := range enclosing(baseSyms, realRanges(h.Old)) {
		add(s)
	}
	for _, r := range h.Old {
		if r.End >= r.Start {
			continue
		}
		if s, ok := symbolAtInsertion(baseSyms, r.Start); ok {
			add(s)
		}
	}
	have := map[string]bool{}
	for _, s := range baseSyms {
		have[s.Kind+"\x00"+s.Name] = true
	}
	for _, s := range enclosing(nextSyms, realRanges(h.New)) {
		if !have[s.Kind+"\x00"+s.Name] {
			add(s)
		}
	}
	return cs, nil
}

func parseSide(parse SpanParse, path string, src []byte) ([]Symbol, error) {
	if len(src) == 0 {
		return nil, nil
	}
	sp, err := parse(path, src)
	if err != nil {
		return nil, err
	}
	return sp.Symbols, nil
}

func realRanges(rs []LineRange) []LineRange {
	var out []LineRange
	for _, r := range rs {
		if r.End >= r.Start && r.Start > 0 {
			out = append(out, r)
		}
	}
	return out
}

// symbolAtInsertion returns the innermost symbol that strictly contains an
// insertion anchored after line. An insertion after a symbol's last line is
// not inside it.
func symbolAtInsertion(syms []Symbol, line int) (Symbol, bool) {
	best := -1
	bestSpan := int(^uint(0) >> 1)
	for i, s := range syms {
		if s.Name == "" || line < s.StartLine || line >= s.EndLine {
			continue
		}
		span := s.EndLine - s.StartLine
		if span < bestSpan {
			best = i
			bestSpan = span
		}
	}
	if best < 0 {
		return Symbol{}, false
	}
	return syms[best], true
}

// Conflicts reports whether a and b clash on the same base blob. conflict is
// true when they share a symbol (same qualified name and kind, which includes
// two new symbols of one name) or a pair of ranges is within margin base
// lines of each other. Touching lines are 1 apart, so margin 2 conflicts on
// a gap of 1 or 2 and not on a gap of 3. why is "symbol <kind> <name>" or
// "edits within <margin> lines".
func Conflicts(a, b ChangeSet, margin int) (why string, conflict bool) {
	for _, sa := range a.Symbols {
		for _, sb := range b.Symbols {
			if sa.Name != "" && sa.Name == sb.Name && sa.Kind == sb.Kind {
				return fmt.Sprintf("symbol %s %s", sa.Kind, sa.Name), true
			}
		}
	}
	for _, ra := range a.Ranges {
		for _, rb := range b.Ranges {
			if rangeGap(ra, rb) <= margin {
				return fmt.Sprintf("edits within %d lines", margin), true
			}
		}
	}
	return "", false
}

// rangeGap is the distance in base lines between two ranges. Overlap is 0.
// A zero-width range is anchored at Start, matching LineDiff's insertion.
func rangeGap(a, b LineRange) int {
	alo, ahi := rangeAnchor(a)
	blo, bhi := rangeAnchor(b)
	if ahi < blo {
		return blo - ahi
	}
	if bhi < alo {
		return alo - bhi
	}
	return 0
}

func rangeAnchor(r LineRange) (lo, hi int) {
	if r.End < r.Start {
		return r.Start, r.Start
	}
	return r.Start, r.End
}

// WorktreeChangeSet is the change set of rel in the worktree dir against the
// merge-base of HEAD and integration. baseBlob is the blob id of rel at that
// base, empty when the path does not exist there, so callers can tell when
// two tasks are not on the same base. The new side is the file on disk,
// committed and dirty; a missing file is a deletion.
func WorktreeChangeSet(dir, integration, rel string, parse SpanParse) (cs ChangeSet, baseBlob string, err error) {
	rel, err = repoRel(dir, rel)
	if err != nil {
		return ChangeSet{}, "", err
	}
	if integration == "" {
		return ChangeSet{}, "", fmt.Errorf("change set: empty integration ref")
	}
	base, err := Run(dir, "merge-base", integration, "HEAD")
	if err != nil {
		return ChangeSet{}, "", err
	}
	id, content, ok, err := blobAt(dir, base, rel)
	if err != nil {
		return ChangeSet{}, "", err
	}
	if ok {
		baseBlob = id
	}
	next, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil && !os.IsNotExist(err) {
		return ChangeSet{}, "", err
	}
	if err != nil {
		next = nil
	}
	cs, err = ChangeSetFrom(rel, content, next, parse)
	if err != nil {
		return ChangeSet{}, "", err
	}
	return cs, baseBlob, nil
}

func repoRel(dir, path string) (string, error) {
	if path == "" || filepath.IsAbs(path) {
		return "", fmt.Errorf("change set: path %q is not repo-relative", path)
	}
	abs := filepath.Join(dir, filepath.Clean(filepath.FromSlash(path)))
	rel, err := filepath.Rel(dir, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("change set: path %q escapes the worktree", path)
	}
	return filepath.ToSlash(rel), nil
}

// blobAt returns the blob id and bytes of rev:rel. ok is false when that
// path is not in rev.
func blobAt(dir, rev, rel string) (id string, content []byte, ok bool, err error) {
	spec := rev + ":" + rel
	if _, err = Run(dir, "cat-file", "-e", spec); err != nil {
		if pathMissing(err) {
			return "", nil, false, nil
		}
		return "", nil, false, err
	}
	id, err = Run(dir, "rev-parse", "--verify", spec)
	if err != nil {
		return "", nil, false, err
	}
	content, err = gitStdout(dir, "cat-file", "blob", id)
	if err != nil {
		return "", nil, false, err
	}
	return id, content, true, nil
}

func pathMissing(err error) bool {
	s := err.Error()
	return strings.Contains(s, "does not exist") || strings.Contains(s, "exists on disk, but not")
}

// gitStdout runs git and returns stdout untrimmed. Run trims, which would
// drop a trailing newline and change a line diff.
func gitStdout(dir string, args ...string) ([]byte, error) {
	calls.Add(1)
	cmd := exec.Command("git", append([]string{"-C", dir, "--no-optional-locks"}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return stdout.Bytes(), nil
}
