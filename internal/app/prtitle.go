package app

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// conventionalRe is a conventional commit subject: a known type, an
// optional scope, then ": " and a description. Unknown types ("prgraph:",
// "saddle flatten:") are package prefixes, not conventions.
var conventionalRe = regexp.MustCompile(`^(feat|fix|chore|refactor|docs|test|perf|build|ci|style|revert)(\([^)]*\))?!?: \S`)

// prTitle is the title of a task's PR (#951): title when it is already
// conventional, else the first conventional subject among commits (oldest
// first), the worker having followed the repo's conventions. When neither
// is and base's history is conventional, it is the change's kind and title,
// "feat: add the login page". Otherwise it is title as it stands.
func (a *App) prTitle(title, base string, p prText) string {
	if conventionalRe.MatchString(strings.TrimSpace(title)) {
		return title
	}
	for _, c := range p.Commits {
		if conventionalRe.MatchString(c) {
			return c
		}
	}
	if !a.conventionalHistory(base) {
		return title
	}
	p.Title = title
	return facts(p).kind + ": " + lowerFirst(strings.TrimSpace(title))
}

// conventionalHistory reports whether most of base's recent commits are
// conventional, so the repo asks for conventional titles.
func (a *App) conventionalHistory(base string) bool {
	out, err := gitx.Run(a.Root, "log", "-n", "30", "--no-merges", "--format=%s", base)
	if err != nil {
		return false
	}
	subjects := nonEmptyLines(out)
	n := 0
	for _, s := range subjects {
		if conventionalRe.MatchString(s) {
			n++
		}
	}
	return n >= 3 && 2*n >= len(subjects)
}

// lowerFirst lowercases s's first letter, unless it starts an acronym.
func lowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	if next, _ := utf8.DecodeRuneInString(s[n:]); unicode.IsUpper(next) {
		return s
	}
	return string(unicode.ToLower(r)) + s[n:]
}

// prRangeBase is the ref a task's PR range starts from: base on the
// remote when saddle has it (the branch was rebased onto that), else base.
func (a *App) prRangeBase(base string) string {
	if r := a.remote(); r != "" {
		ref := "refs/remotes/" + r + "/" + base
		if _, err := gitx.RevParse(a.Root, ref); err == nil {
			return ref
		}
	}
	return base
}

// donePR is the title and body of t's PR against base: its summary (or
// title) laid into the repo's PR template, closing its issue, with no AI
// attribution footer, its Saddle-Task trailer in an HTML comment (which a
// squash merge carries into base), and a conventional title when the repo
// writes those (#951).
func (a *App) donePR(t store.Task, base string) (title, body string) {
	from := a.prRangeBase(base)
	desc := strings.TrimSpace(t.Summary)
	if desc == "" {
		desc = t.Title
	}
	p := prText{Title: t.Title, Desc: desc}
	if t.Issue > 0 && !closesIssue(desc, t.Issue) {
		p.Issues = []int{t.Issue}
	}
	if t.Branch != "" && gitx.BranchExists(a.Root, t.Branch) {
		p.Commits, p.Files = a.prRange(from, t.Branch)
	}
	title = a.prTitle(t.Title, from, p)
	p.Title = title
	return title, withTaskTrailer(fillPRTemplate(a.prTemplate(from), p), t.ID)
}
