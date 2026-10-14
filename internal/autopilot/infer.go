package autopilot

import (
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/brandonapol/saddle/internal/claims"
)

// A ticket with no claims runs alone (#504), so a lane with four slots
// would run one worker. When a ticket declares no Claims: line, autopilot
// infers them from the paths its body names, checked against the repo:
//
//   - a directory claims everything under it (dir/**);
//   - a Go file claims its package directory, since its tests and siblings
//     usually change with it;
//   - any other file claims itself;
//   - a path that doesn't exist yet claims itself (or dir/** when it has no
//     extension) if its top directory exists, so new files count too;
//   - a Go import path (example.com/mod/internal/x) claims the longest
//     suffix that exists in the repo.
//
// Without a repo to check against, only paths that clearly are paths count:
// globs, names with an extension, a trailing slash, or code spans.
// Claims only reserve: a worker may still write unclaimed files, which the
// claim hook then claims for it, so a narrow guess costs nothing but a
// later collision check.

var (
	urlRe = regexp.MustCompile(`(?i)\b(?:https?|ftp)://\S+|\bwww\.\S+`)
	// codeSpan is an inline `code span`; paths inside one are meant literally.
	codeSpan = regexp.MustCompile("`([^`\n]+)`")
	// pathTok is a run of path characters; everything else separates tokens.
	pathTok = regexp.MustCompile(`[A-Za-z0-9_.\-/*]+`)
	segRe   = regexp.MustCompile(`^[A-Za-z0-9_.\-*]+$`)
)

// lockfiles are generated manifests a ticket may mention but a worker
// should never hold: the merge train regenerates them.
var lockfiles = map[string]bool{
	"go.mod": true, "go.sum": true, "go.work.sum": true,
	"package-lock.json": true, "yarn.lock": true, "pnpm-lock.yaml": true,
	"Cargo.lock": true, "poetry.lock": true, "Gemfile.lock": true,
}

// TicketClaims is the claims autopilot spawns is with: the globs its
// Claims: line declares, else the ones InferClaims reads from its body.
// inferred says which; nil claims mean the ticket runs alone.
func TicketClaims(body string, repo fs.FS) (globs []string, inferred bool) {
	if c := ParseClaims(body); c != nil {
		return c, false
	}
	c := InferClaims(body, repo)
	return c, c != nil
}

// InferClaims reads the paths a ticket body names and turns them into
// claim globs, checking them against repo when it isn't nil. It returns
// nil when the body names no path.
func InferClaims(body string, repo fs.FS) []string {
	body = urlRe.ReplaceAllString(body, " ")
	quoted := map[string]bool{}
	for _, m := range codeSpan.FindAllStringSubmatch(body, -1) {
		for _, tok := range pathTok.FindAllString(m[1], -1) {
			quoted[trimTok(tok)] = true
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, tok := range pathTok.FindAllString(body, -1) {
		tok = trimTok(tok)
		if seen[tok] {
			continue
		}
		seen[tok] = true
		if g := inferOne(tok, quoted[tok], repo); g != "" && !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return prune(out)
}

// trimTok drops the sentence punctuation a path picks up in prose.
func trimTok(t string) string {
	return strings.TrimLeft(strings.TrimRight(t, ".-"), "-")
}

// inferOne maps one token to a claim glob, or "" when it isn't a path.
func inferOne(tok string, quoted bool, repo fs.FS) string {
	dirSlash := strings.HasSuffix(tok, "/")
	p := claims.Clean(tok)
	if p == "" || p == "." || strings.HasPrefix(tok, "/") || strings.Contains(p, "//") {
		return ""
	}
	segs := strings.Split(p, "/")
	for _, s := range segs {
		if s == ".." || s == "." || !segRe.MatchString(s) {
			return ""
		}
	}
	if lockfiles[p] {
		return ""
	}
	ext := path.Ext(segs[len(segs)-1]) != "" && !strings.HasPrefix(segs[len(segs)-1], ".")
	if strings.ContainsAny(p, "*?") {
		// A glob is meant literally; it only needs a root that exists.
		if len(segs) == 1 {
			return ""
		}
		if repo != nil && !isDir(repo, strings.SplitN(p, "/", 2)[0]) {
			return ""
		}
		return p
	}
	if repo == nil {
		switch {
		case len(segs) == 1 || domainLike(segs[0]):
			return ""
		case ext:
			return fileClaim(p)
		case dirSlash || quoted:
			return p + "/**"
		}
		return ""
	}
	if len(segs) == 1 {
		// A bare word is prose ("update the docs") unless it is marked as
		// a path or names a file the repo has.
		fi, err := fs.Stat(repo, p)
		switch {
		case err != nil:
			return ""
		case fi.IsDir() && (dirSlash || quoted):
			return p + "/**"
		case !fi.IsDir() && (ext || quoted):
			return p
		}
		return ""
	}
	// A Go import path names a package by its module path: keep the
	// longest suffix the repo has.
	if domainLike(segs[0]) {
		for i := 1; i < len(segs); i++ {
			if q := strings.Join(segs[i:], "/"); exists(repo, q) {
				return inferOne(q, quoted, repo)
			}
		}
		return ""
	}
	if fi, err := fs.Stat(repo, p); err == nil {
		if fi.IsDir() {
			return p + "/**"
		}
		return fileClaim(p)
	}
	// Not there yet: a new file or package, if its top directory exists.
	if !isDir(repo, segs[0]) {
		return ""
	}
	if ext {
		return fileClaim(p)
	}
	return p + "/**"
}

// fileClaim claims a file, or its package directory for a Go file.
func fileClaim(p string) string {
	if path.Ext(p) == ".go" {
		if d := path.Dir(p); d != "." {
			return d + "/**"
		}
	}
	return p
}

// domainLike reports a module host such as github.com: a dot inside the
// first segment of a path that isn't a dotfile or a filename.
func domainLike(seg string) bool {
	i := strings.Index(seg, ".")
	return i > 0 && i < len(seg)-1 && !strings.Contains(seg, "_")
}

func exists(repo fs.FS, p string) bool {
	_, err := fs.Stat(repo, p)
	return err == nil
}

func isDir(repo fs.FS, p string) bool {
	fi, err := fs.Stat(repo, p)
	return err == nil && fi.IsDir()
}

// prune drops claims another one already covers, keeping first-seen order.
func prune(gs []string) []string {
	var out []string
	for i, g := range gs {
		covered := false
		for j, h := range gs {
			if i != j && h != g && strings.HasSuffix(h, "/**") && claims.Match(h, strings.TrimSuffix(g, "/**")) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, g)
		}
	}
	return out
}
