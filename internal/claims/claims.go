// Package claims matches repo-relative paths against claim globs.
//
// A claim is a doublestar glob ("pkg/billing/**") or a plain path. A plain
// path that names a directory also covers everything under it.
package claims

import (
	"path"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

func hasMeta(g string) bool { return strings.ContainsAny(g, "*?[{") }

// Clean normalizes a glob or path to slash form without ./ or trailing /.
func Clean(g string) string {
	g = strings.TrimSpace(strings.ReplaceAll(g, "\\", "/"))
	g = strings.TrimSuffix(g, "/")
	if !hasMeta(g) {
		g = path.Clean(g)
	}
	return strings.TrimPrefix(g, "./")
}

// Match reports whether a repo-relative path falls under glob.
func Match(glob, p string) bool {
	glob, p = Clean(glob), Clean(p)
	if !hasMeta(glob) {
		return p == glob || strings.HasPrefix(p, glob+"/")
	}
	ok, _ := doublestar.Match(glob, p)
	return ok
}

// prefix is the literal leading directory part of a glob.
func prefix(g string) string {
	if !hasMeta(g) {
		return g
	}
	i := strings.IndexAny(g, "*?[{")
	return strings.TrimSuffix(path.Dir(g[:i]+"x"), ".")
}

func underOrEqual(a, b string) bool {
	return b == "" || a == b || strings.HasPrefix(a, b+"/")
}

// Overlap conservatively reports whether two claims could cover a common path.
// It compares the globs segment by segment, so sibling filename globs such as
// "sync*.go" and "briefs_test.go" do not overlap. A plain path also covers
// everything under it.
func Overlap(a, b string) bool {
	a, b = Clean(a), Clean(b)
	if strings.Contains(a, "{") || strings.Contains(b, "{") {
		// Brace alternatives can span segments; fall back to the literal prefixes.
		pa, pb := prefix(a), prefix(b)
		return underOrEqual(pa, pb) || underOrEqual(pb, pa)
	}
	return overlapSegs(claimSegs(a), claimSegs(b))
}

// claimSegs splits a claim into path segments. A plain path covers its
// directory contents, so it gains a trailing "**".
func claimSegs(g string) []string {
	if !hasMeta(g) {
		g += "/**"
	}
	return strings.Split(g, "/")
}

func overlapSegs(a, b []string) bool {
	switch {
	case len(a) == 0 && len(b) == 0:
		return true
	case len(a) == 0:
		return allGlobstar(b)
	case len(b) == 0:
		return allGlobstar(a)
	}
	if a[0] == "**" || b[0] == "**" {
		if a[0] == "**" && (overlapSegs(a[1:], b) || overlapSegs(a, b[1:])) {
			return true
		}
		return b[0] == "**" && (overlapSegs(a, b[1:]) || overlapSegs(a[1:], b))
	}
	return segOverlap(a[0], b[0]) && overlapSegs(a[1:], b[1:])
}

func allGlobstar(s []string) bool {
	for _, x := range s {
		if x != "**" {
			return false
		}
	}
	return true
}

// segOverlap reports whether two single path segments could match a common
// name. Two wildcard segments overlap only if their literal prefixes and
// suffixes are compatible, which over-reports but never under-reports.
func segOverlap(a, b string) bool {
	ma, mb := hasMeta(a), hasMeta(b)
	switch {
	case !ma && !mb:
		return a == b
	case !ma:
		ok, _ := doublestar.Match(b, a)
		return ok
	case !mb:
		ok, _ := doublestar.Match(a, b)
		return ok
	}
	pa, sa := a[:strings.IndexAny(a, "*?[{")], a[strings.LastIndexAny(a, "*?[]{}")+1:]
	pb, sb := b[:strings.IndexAny(b, "*?[{")], b[strings.LastIndexAny(b, "*?[]{}")+1:]
	return (strings.HasPrefix(pa, pb) || strings.HasPrefix(pb, pa)) &&
		(strings.HasSuffix(sa, sb) || strings.HasSuffix(sb, sa))
}

// Owner returns the first task other than self whose claims cover p.
func Owner(all map[string][]string, self, p string) (task, glob string) {
	for t, gs := range all {
		if t == self {
			continue
		}
		for _, g := range gs {
			if Match(g, p) {
				return t, g
			}
		}
	}
	return "", ""
}

// Conflicts lists, for each requested glob, any other task's overlapping claim.
func Conflicts(all map[string][]string, self string, want []string) map[string]string {
	out := map[string]string{}
	for _, w := range want {
		for t, gs := range all {
			if t == self {
				continue
			}
			for _, g := range gs {
				if Overlap(w, g) {
					out[w] = t + " (" + g + ")"
				}
			}
		}
	}
	return out
}

type Rename struct{ Old, New string }

// Remap moves a claim through a set of file renames. When every renamed file
// under the claim's literal prefix moved to one common new directory, the
// prefix is rewritten to it. It returns the claim unchanged otherwise.
func Remap(glob string, rs []Rename) string {
	glob = Clean(glob)
	p := prefix(glob)
	if p == "" {
		return glob
	}
	var dst []string
	for _, r := range rs {
		if r.Old == p {
			return r.New + strings.TrimPrefix(glob, p)
		}
		if strings.HasPrefix(r.Old, p+"/") {
			rel := strings.TrimPrefix(r.Old, p+"/")
			if !strings.HasSuffix(r.New, rel) {
				dst = append(dst, path.Dir(r.New))
				continue
			}
			dst = append(dst, strings.TrimSuffix(strings.TrimSuffix(r.New, rel), "/"))
		}
	}
	if len(dst) == 0 {
		return glob
	}
	for _, d := range dst[1:] {
		if d != dst[0] {
			return glob
		}
	}
	return dst[0] + strings.TrimPrefix(glob, p)
}
