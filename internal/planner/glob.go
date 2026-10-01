package planner

import (
	"path"
	"strings"
)

// TODO: switch to claims.Overlap once internal/claims is on the integration
// branch.
//
// This mirrors the conservative overlap test in internal/claims (claims.Clean
// and claims.Overlap). Its answer only depends on literal path prefixes, so it
// needs no glob library.

func hasMeta(g string) bool { return strings.ContainsAny(g, "*?[{") }

// clean normalizes a glob or path to slash form without ./ or trailing /.
func clean(g string) string {
	g = strings.TrimSpace(strings.ReplaceAll(g, "\\", "/"))
	g = strings.TrimSuffix(g, "/")
	if !hasMeta(g) {
		g = path.Clean(g)
	}
	return strings.TrimPrefix(g, "./")
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

// overlap conservatively reports whether two claims could cover a common
// path. A plain path may name a directory, so it covers everything under it.
func overlap(a, b string) bool {
	pa, pb := prefix(clean(a)), prefix(clean(b))
	return underOrEqual(pa, pb) || underOrEqual(pb, pa)
}
