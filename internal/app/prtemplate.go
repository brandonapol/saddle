package app

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/brandonapol/saddle/internal/gitx"
)

// prText is what saddle knows about a PR when it writes the body.
type prText struct {
	Title   string
	Desc    string   // the description; a trailing attribution footer stays last
	Issues  []int    // issues the PR closes that Desc doesn't already close
	Commits []string // commit subjects, oldest first
	Files   []string // paths the PR changes
	Note    string   // saddle's own footer, after a rule; "" for none
}

// prTemplatePaths are where GitHub looks for a repo's PR template.
var prTemplatePaths = []string{
	".github/pull_request_template.md", ".github/PULL_REQUEST_TEMPLATE.md",
	"docs/pull_request_template.md", "pull_request_template.md", "PULL_REQUEST_TEMPLATE.md",
}

// prTemplate is the repo's pull request template at rev, or "".
func (a *App) prTemplate(rev string) string {
	for _, p := range prTemplatePaths {
		if s, err := gitx.Run(a.Root, "show", rev+":"+p); err == nil && s != "" {
			return s
		}
	}
	return ""
}

// prRange is the commit subjects and changed paths of from..to.
func (a *App) prRange(from, to string) (commits, files []string) {
	log, _ := gitx.Run(a.Root, "log", "--reverse", "--format=%s", from+".."+to)
	diff, _ := gitx.Run(a.Root, "diff", "--name-only", from, to)
	return nonEmptyLines(log), nonEmptyLines(diff)
}

// stackPRBody is the body of l's stacked PR: its summary laid into the repo's
// PR template, or the summary alone when the repo has none (#361). Either way
// it closes l's issue when the summary doesn't already (#557). A PR that
// replaces one restack detached keeps that PR's body untouched (#338).
func (a *App) stackPRBody(l landedTask) string {
	if body := a.detachedPRBody(l); strings.TrimSpace(body) != "" {
		return body
	}
	p := prText{Title: l.Title, Desc: l.Summary}
	if l.Issue > 0 && !closesIssue(l.Summary, l.Issue) {
		p.Issues = []int{l.Issue}
	}
	tmpl := a.prTemplate(l.To)
	if strings.TrimSpace(tmpl) == "" {
		if len(p.Issues) == 0 {
			return l.Summary
		}
		return fillPRTemplate("", p)
	}
	if l.From != "" && l.To != "" {
		p.Commits, p.Files = a.prRange(l.From, l.To)
	}
	return fillPRTemplate(tmpl, p)
}

func closesIssue(s string, n int) bool {
	for _, m := range closesRe.FindAllStringSubmatch(s, -1) {
		if m[1] == fmt.Sprint(n) {
			return true
		}
	}
	return false
}

var (
	headingRe     = regexp.MustCompile(`^#{1,6}\s`)
	anyCommentRe  = regexp.MustCompile(`(?s)<!--.*?-->`)
	checkRe       = regexp.MustCompile(`^(\s*[-*] \[)[ xX](\] )(.*)$`)
	issueHolderRe = regexp.MustCompile(`#(?:<[^>\n]*>|\{[^}\n]*\}|\[[^\]\n]*\]|(?i:n+|x+|issue(?:[-_ ]?(?:number|num|no))?)\b)`)
	attributionRe = regexp.MustCompile(`(?i)^\s*(🤖|generated with \[?claude|co-authored-by:)`)
	convTypeRe    = regexp.MustCompile(`^(\w+)(?:\([^)]*\))?!?:`)
	descHeadRe    = regexp.MustCompile(`(?i)\b(what|summary|description|overview|about|purpose|context|motivation|why)\b`)
	changesHeadRe = regexp.MustCompile(`(?i)^(changes?|changelog|what changed|change ?list|list of changes)\b`)
	typeHeadRe    = regexp.MustCompile(`(?i)\b(type|kind|category|nature)\b`)
	surfaceHeadRe = regexp.MustCompile(`(?i)\b(surface|area|areas|scope|components?|affect\w*|layers?|modules?|platforms?)\b`)
	testHeadRe    = regexp.MustCompile(`(?i)\btest`)
)

// fillPRTemplate lays p into tmpl strictly (#361): every heading kept in
// order, each checkbox list kept with the boxes that apply ticked, issue
// placeholders filled (or their line dropped when nothing closes), prompt
// comments and placeholder text replaced by content or left empty. Without a
// template the body is the description and its closing lines; a template
// with no headings goes below them, as before. p.Note and then any
// attribution footer on the description come last.
func fillPRTemplate(tmpl string, p prText) string {
	desc, footer := splitAttribution(p.Desc)
	var body string
	switch {
	case strings.TrimSpace(tmpl) == "":
		body = withCloses(desc, p.Issues)
	case !hasHeading(tmpl):
		body = strings.TrimSpace(withCloses(desc, p.Issues) + "\n\n" + tmpl)
	default:
		body = fillSections(tmpl, desc, p)
	}
	if p.Note != "" {
		body += "\n\n---\n" + p.Note + "\n"
	}
	if footer != "" {
		body = strings.TrimRight(body, "\n") + "\n\n" + footer + "\n"
	}
	return body
}

type prSection struct {
	head  string
	lines []string
}

func fillSections(tmpl, desc string, p prText) string {
	var pre []string
	var secs []prSection
	placed := false
	for _, l := range strings.Split(anyCommentRe.ReplaceAllString(tmpl, ""), "\n") {
		l = strings.TrimRight(l, " \t\r")
		if headingRe.MatchString(l) {
			secs = append(secs, prSection{head: l})
			continue
		}
		if issueHolderRe.MatchString(l) {
			placed = true
			for _, n := range p.Issues {
				add(&pre, &secs, issueHolderRe.ReplaceAllString(l, fmt.Sprintf("#%d", n)), true)
			}
			continue
		}
		add(&pre, &secs, l, false)
	}
	if !placed {
		desc = withCloses(desc, p.Issues)
	}

	f := facts(p)
	descAt, changesAt := -1, -1
	for i, s := range secs {
		name := strings.TrimSpace(strings.TrimLeft(s.head, "#"))
		switch {
		case hasChecks(s.lines):
		case changesAt < 0 && changesHeadRe.MatchString(name):
			changesAt = i
		case descAt < 0 && descHeadRe.MatchString(name):
			descAt = i
		}
	}
	if descAt < 0 {
		for i, s := range secs {
			if i != changesAt && !hasChecks(s.lines) {
				descAt = i
				break
			}
		}
	}

	var b strings.Builder
	block := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			b.WriteString(s + "\n\n")
		}
	}
	var kept []string
	for _, l := range pre {
		if l != "" {
			kept = append(kept, l)
		}
	}
	block(strings.Join(kept, "\n"))
	if descAt < 0 {
		block(desc)
	}
	for i, s := range secs {
		b.WriteString(s.head + "\n\n")
		name := strings.TrimSpace(strings.TrimLeft(s.head, "#"))
		switch {
		case i == descAt:
			block(desc)
		case i == changesAt:
			var bs []string
			for _, c := range p.Commits {
				bs = append(bs, "- "+c)
			}
			block(strings.Join(bs, "\n"))
		case hasChecks(s.lines):
			var cs []string
			for _, l := range s.lines {
				m := checkRe.FindStringSubmatch(l)
				if m == nil {
					continue
				}
				mark := " "
				if f.tick(name, m[3]) {
					mark = "x"
				}
				cs = append(cs, m[1]+mark+m[2]+m[3])
			}
			block(strings.Join(cs, "\n"))
		}
	}
	return strings.TrimSpace(b.String())
}

// add appends l to the last section, or to the preamble before any. Preamble
// lines are template prose and only kept when filled.
func add(pre *[]string, secs *[]prSection, l string, filled bool) {
	if n := len(*secs); n > 0 {
		(*secs)[n-1].lines = append((*secs)[n-1].lines, l)
	} else if filled {
		*pre = append(*pre, l)
	}
}

func hasHeading(tmpl string) bool {
	for _, l := range strings.Split(tmpl, "\n") {
		if headingRe.MatchString(l) {
			return true
		}
	}
	return false
}

func hasChecks(lines []string) bool {
	for _, l := range lines {
		if checkRe.MatchString(l) {
			return true
		}
	}
	return false
}

func withCloses(desc string, issues []int) string {
	for _, n := range issues {
		desc += fmt.Sprintf("\n\nCloses #%d", n)
	}
	return strings.TrimSpace(desc)
}

// splitAttribution cuts a trailing block of AI attribution lines off desc.
func splitAttribution(desc string) (string, string) {
	lines := strings.Split(strings.TrimRight(desc, "\n "), "\n")
	i := len(lines)
	for i > 0 && (attributionRe.MatchString(lines[i-1]) || (strings.TrimSpace(lines[i-1]) == "" && i < len(lines))) {
		i--
	}
	if i == len(lines) {
		return desc, ""
	}
	return strings.TrimSpace(strings.Join(lines[:i], "\n")), strings.TrimSpace(strings.Join(lines[i:], "\n"))
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// prFacts is what ticking a checkbox goes on.
type prFacts struct {
	kind     string          // fix, feat, refactor, docs, chore, test, perf
	surfaces map[string]bool // categories of the changed paths
	tests    bool            // the change adds or edits tests
}

// kindWords are the words that name each kind of change in a type list.
var kindWords = map[string][]string{
	"fix":      {"bug", "bugfix", "fix", "hotfix"},
	"feat":     {"feature", "feat", "enhancement"},
	"refactor": {"refactor", "refactoring"},
	"docs":     {"docs", "doc", "documentation"},
	"chore":    {"chore", "tooling", "build", "maintenance"},
	"test":     {"test", "tests", "testing"},
	"perf":     {"perf", "performance"},
}

// surfaceWords are the words that name each category of path in a surface
// list.
var surfaceWords = map[string][]string{
	"go":       {"go", "golang", "backend", "server"},
	"python":   {"python", "backend", "server"},
	"rust":     {"rust", "backend"},
	"flutter":  {"flutter", "dart", "frontend", "mobile", "app"},
	"web":      {"frontend", "web", "ui", "javascript", "typescript", "js", "ts", "css"},
	"api":      {"api", "swagger", "openapi", "proto", "grpc"},
	"ci":       {"ci", "workflows", "workflow", "pipeline", "pipelines", "actions"},
	"docs":     {"docs", "doc", "documentation", "content"},
	"database": {"database", "db", "sql", "migrations", "schema"},
	"infra":    {"infra", "infrastructure", "terraform", "deploy", "deployment", "helm"},
}

func facts(p prText) prFacts {
	f := prFacts{surfaces: map[string]bool{}}
	for _, file := range p.Files {
		for _, s := range surfacesOf(file) {
			f.surfaces[s] = true
		}
		if isTestPath(file) {
			f.tests = true
		}
	}
	f.kind = kindOf(p, f)
	return f
}

func surfacesOf(file string) []string {
	low := strings.ToLower(file)
	var out []string
	if strings.HasPrefix(low, ".github/workflows/") || strings.HasPrefix(low, ".gitlab-ci") || strings.HasPrefix(low, ".circleci/") {
		out = append(out, "ci")
	}
	if strings.Contains(low, "swagger") || strings.Contains(low, "openapi") || strings.HasSuffix(low, ".proto") {
		out = append(out, "api")
	}
	if strings.Contains(low, "migration") || strings.HasSuffix(low, ".sql") {
		out = append(out, "database")
	}
	switch ext := path.Ext(low); ext {
	case ".go":
		out = append(out, "go")
	case ".py":
		out = append(out, "python")
	case ".rs":
		out = append(out, "rust")
	case ".dart":
		out = append(out, "flutter")
	case ".ts", ".tsx", ".js", ".jsx", ".vue", ".svelte", ".css", ".scss", ".html":
		out = append(out, "web")
	case ".md", ".mdx", ".rst", ".adoc":
		out = append(out, "docs")
	case ".tf", ".hcl":
		out = append(out, "infra")
	default:
		if strings.HasPrefix(low, "docs/") {
			out = append(out, "docs")
		}
	}
	return out
}

func isTestPath(file string) bool {
	low := strings.ToLower(file)
	base := path.Base(low)
	return strings.Contains(base, "_test.") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") ||
		strings.HasPrefix(base, "test_") || strings.Contains("/"+low, "/test/") || strings.Contains("/"+low, "/tests/") ||
		strings.Contains(low, "__tests__/")
}

// kindOf reads the change's kind from a conventional commit prefix on its
// title or first commit, else from what it touches and its title's words.
func kindOf(p prText, f prFacts) string {
	alias := map[string]string{
		"feat": "feat", "feature": "feat", "fix": "fix", "bugfix": "fix", "hotfix": "fix",
		"docs": "docs", "doc": "docs", "refactor": "refactor", "test": "test", "tests": "test",
		"chore": "chore", "build": "chore", "ci": "chore", "style": "chore", "perf": "perf",
	}
	heads := []string{p.Title}
	if len(p.Commits) > 0 {
		heads = append(heads, p.Commits[0])
	}
	for _, h := range heads {
		if m := convTypeRe.FindStringSubmatch(strings.TrimSpace(h)); m != nil {
			if k, ok := alias[strings.ToLower(m[1])]; ok {
				return k
			}
		}
	}
	if len(f.surfaces) == 1 && f.surfaces["docs"] {
		return "docs"
	}
	if len(p.Files) > 0 {
		all := true
		for _, file := range p.Files {
			all = all && isTestPath(file)
		}
		if all {
			return "test"
		}
	}
	words := wordSet(p.Title)
	switch {
	case words["fix"] || words["fixes"] || words["bug"]:
		return "fix"
	case words["refactor"]:
		return "refactor"
	}
	return "feat"
}

// tick reports whether the box labelled label, in the section named head,
// applies. Boxes saddle can't vouch for (manual testing, sign-offs) stay
// unticked.
func (f prFacts) tick(head, label string) bool {
	words := wordSet(label)
	switch {
	case typeHeadRe.MatchString(head):
		return anyWord(words, kindWords[f.kind])
	case surfaceHeadRe.MatchString(head):
		if len(f.surfaces) == 0 {
			return false
		}
		if words["only"] {
			for s := range f.surfaces {
				if !anyWord(words, surfaceWords[s]) {
					return false
				}
			}
			return true
		}
		for s := range f.surfaces {
			if anyWord(words, surfaceWords[s]) {
				return true
			}
		}
		return false
	case testHeadRe.MatchString(head):
		if words["manual"] || words["manually"] || words["not"] {
			return false
		}
		return f.tests && (words["automated"] || words["unit"] ||
			((words["new"] || words["added"] || words["adds"] || words["includes"]) && (words["test"] || words["tests"])))
	}
	return false
}

func wordSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		out[w] = true
	}
	return out
}

func anyWord(words map[string]bool, want []string) bool {
	for _, w := range want {
		if words[w] {
			return true
		}
	}
	return false
}
