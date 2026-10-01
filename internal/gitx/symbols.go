package gitx

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Symbol is a named function, method or type in a source file. Lines are
// 1-based and inclusive. Nested symbols carry qualified names ("T.Method").
type Symbol struct {
	Name      string
	Kind      string // "func", "method", "type", "class", "var", "const", ...
	StartLine int
	EndLine   int
}

// SymbolExtractor finds the symbols in a file. Implementations live outside
// this package (see gitx/symbols) so gitx does not link a parser it may not use.
type SymbolExtractor interface {
	// Supports reports whether the extractor understands path, by name alone.
	Supports(path string) bool
	// Symbols parses src, the contents of path. It must be safe for concurrent use.
	Symbols(path string, src []byte) ([]Symbol, error)
}

// maxSymbolFile skips symbol extraction for files larger than this.
const maxSymbolFile = 2 << 20

// LineRange is an inclusive 1-based range of lines.
type LineRange struct{ Start, End int }

// FileHunks are the changed line ranges of one file between two versions.
// Old ranges index the base version, New ranges the working tree.
type FileHunks struct {
	OldPath, NewPath string // empty for /dev/null
	Old, New         []LineRange
}

// parseUnifiedZero parses `git diff -U0` output into per-file changed ranges.
// Zero-length ranges (a pure insertion seen from the old side, or a pure
// deletion seen from the new side) are dropped: the other side covers them.
func parseUnifiedZero(out string) []FileHunks {
	var files []FileHunks
	var cur *FileHunks
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			files = append(files, FileHunks{})
			cur = &files[len(files)-1]
		case cur == nil:
		case strings.HasPrefix(line, "--- "):
			cur.OldPath = diffPath(line[4:], "a/")
		case strings.HasPrefix(line, "+++ "):
			cur.NewPath = diffPath(line[4:], "b/")
		case strings.HasPrefix(line, "rename from "):
			cur.OldPath = unquote(line[len("rename from "):])
		case strings.HasPrefix(line, "rename to "):
			cur.NewPath = unquote(line[len("rename to "):])
		case strings.HasPrefix(line, "@@ "):
			// @@ -a[,b] +c[,d] @@
			f := strings.Fields(line)
			if len(f) < 3 {
				continue
			}
			if r, ok := hunkRange(f[1]); ok {
				cur.Old = append(cur.Old, r)
			}
			if r, ok := hunkRange(f[2]); ok {
				cur.New = append(cur.New, r)
			}
		}
	}
	return files
}

func hunkRange(s string) (LineRange, bool) {
	s = s[1:]
	start, count := s, "1"
	if i := strings.IndexByte(s, ','); i >= 0 {
		start, count = s[:i], s[i+1:]
	}
	a, err1 := strconv.Atoi(start)
	n, err2 := strconv.Atoi(count)
	if err1 != nil || err2 != nil || n == 0 {
		return LineRange{}, false
	}
	return LineRange{a, a + n - 1}, true
}

func diffPath(s, prefix string) string {
	s = unquote(strings.TrimRight(s, "\t"))
	if s == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(s, prefix)
}

func unquote(s string) string {
	if strings.HasPrefix(s, `"`) {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
	}
	return s
}

// symbolCache memoises extraction by content hash. Agents rewrite the same
// files over and over; most refreshes reparse nothing.
type symbolCache struct {
	mu sync.Mutex
	m  map[[32]byte][]Symbol
}

func newSymbolCache() *symbolCache { return &symbolCache{m: map[[32]byte][]Symbol{}} }

func (c *symbolCache) symbols(x SymbolExtractor, path string, src []byte) []Symbol {
	key := sha256.Sum256(append([]byte(path+"\x00"), src...))
	c.mu.Lock()
	syms, ok := c.m[key]
	c.mu.Unlock()
	if ok {
		return syms
	}
	syms, err := x.Symbols(path, src)
	if err != nil {
		return nil
	}
	c.mu.Lock()
	if len(c.m) > 4096 {
		c.m = map[[32]byte][]Symbol{}
	}
	c.m[key] = syms
	c.mu.Unlock()
	return syms
}

// touchedSymbols maps the lines changed between st.Base and the working tree
// (committed, staged, unstaged and untracked) to their innermost symbols.
func touchedSymbols(dir string, st State, x SymbolExtractor, cache *symbolCache) ([]TouchedSymbol, error) {
	seen := map[string]bool{}
	var out []TouchedSymbol
	add := func(path string, syms []Symbol) {
		for _, s := range syms {
			t := TouchedSymbol{Path: path, Symbol: s}
			if !seen[t.Key()] {
				seen[t.Key()] = true
				out = append(out, t)
			}
		}
	}
	readNew := func(p string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil || len(b) > maxSymbolFile {
			return nil
		}
		return b
	}

	if st.Base != "" {
		diff, err := git(dir, "diff", "-U0", "--no-color", "--no-ext-diff", "-M", st.Base, "--")
		if err != nil {
			return nil, err
		}
		for _, h := range parseUnifiedZero(diff) {
			if h.NewPath != "" && len(h.New) > 0 && x.Supports(h.NewPath) {
				if src := readNew(h.NewPath); src != nil {
					add(h.NewPath, enclosing(cache.symbols(x, h.NewPath, src), h.New))
				}
			}
			if h.OldPath != "" && len(h.Old) > 0 && x.Supports(h.OldPath) {
				src, err := Run(dir, "cat-file", "blob", st.Base+":"+h.OldPath)
				if err == nil && len(src) <= maxSymbolFile {
					// Report deleted lines under the path the symbol lives at now.
					path := h.NewPath
					if path == "" {
						path = h.OldPath
					}
					add(path, enclosing(cache.symbols(x, h.OldPath, []byte(src)), h.Old))
				}
			}
		}
	}
	for _, f := range st.Files {
		if !f.Untracked || !x.Supports(f.Path) {
			continue
		}
		if src := readNew(f.Path); src != nil {
			add(f.Path, cache.symbols(x, f.Path, src))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		if out[i].StartLine != out[j].StartLine {
			return out[i].StartLine < out[j].StartLine
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// enclosing returns, for every changed line, the innermost symbol containing
// it. Lines outside any symbol (imports, blank lines) touch nothing.
func enclosing(syms []Symbol, ranges []LineRange) []Symbol {
	if len(syms) == 0 || len(ranges) == 0 {
		return nil
	}
	// Outer symbols first, so inner ones overwrite them line by line.
	order := make([]int, len(syms))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		sa, sb := syms[order[a]], syms[order[b]]
		return sa.EndLine-sa.StartLine > sb.EndLine-sb.StartLine
	})
	last := 0
	for _, s := range syms {
		last = max(last, s.EndLine)
	}
	owner := make([]int, last+1)
	for i := range owner {
		owner[i] = -1
	}
	for _, i := range order {
		for l := max(syms[i].StartLine, 1); l <= syms[i].EndLine; l++ {
			owner[l] = i
		}
	}
	hit := map[int]bool{}
	var out []Symbol
	for _, r := range ranges {
		for l := max(r.Start, 1); l <= min(r.End, last); l++ {
			if i := owner[l]; i >= 0 && !hit[i] {
				hit[i] = true
				out = append(out, syms[i])
			}
		}
	}
	return out
}
