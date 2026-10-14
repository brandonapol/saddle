package autopilot

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/config"
)

// Issue is one open issue in the ready queue.
type Issue struct {
	Number  int       `json:"number"`
	Title   string    `json:"title"`
	Body    string    `json:"body"`
	Created time.Time `json:"created,omitzero"`
	HasPR   bool      `json:"has_pr,omitempty"` // an open PR already closes it
}

var (
	// claimsLine is "claims: a, b", "**Claims:** a b" or "- claims: a".
	claimsLine = regexp.MustCompile(`(?i)^\s*(?:[-*]\s+)?\**claims\**\s*:\s*\**\s*(.*)$`)
	bulletLine = regexp.MustCompile(`^\s*[-*]\s+(.*)$`)
	// claimsHead is the "### Claims" heading a GitHub issue form renders.
	claimsHead = regexp.MustCompile(`(?i)^\s*#{1,6}\s+claims\s*$`)
	heading    = regexp.MustCompile(`^\s*#{1,6}\s`)
	afterLine  = regexp.MustCompile(`(?i)^\s*(?:[-*]\s+)?\**after\**\s*:?\s*\**\s*((?:#\d+[\s,]*)+)$`)
	issueRef   = regexp.MustCompile(`#(\d+)`)
	scopeLine  = regexp.MustCompile(`(?i)model scope\s*:\s*\**\s*(opus|sonnet|haiku|fable)`)
	// difficultyLine is "**Difficulty: hard (design judgment).**" or the
	// "### Difficulty" section an issue form renders (#505).
	difficultyLine = regexp.MustCompile(`(?i)\bdifficulty\**\s*:\s*\**\s*([a-z][a-z-]*)`)
	difficultyHead = regexp.MustCompile(`(?i)^\s*#{1,6}\s+difficulty\s*$`)
	needsLine      = regexp.MustCompile(`(?im)^\s*(?:[-*]\s+)?\**needs\**\s*:\s*\**\s*(.*)$`)
	providerLine   = regexp.MustCompile(`(?im)^\s*(?:[-*]\s+)?\**provider\**\s*:\s*\**\s*([a-z]+)`)
)

// ParseClaims reads the path globs an issue declares it will change: a
// "claims:" line with the globs after it, or a bullet list under it, or
// the "### Claims" section an issue form renders. Issue refs, bare words
// and URLs are dropped, and trailing punctuation is stripped. Nil means
// none were declared.
func ParseClaims(body string) []string {
	c, ok := scanClaims(body)
	if !ok || len(c) == 0 {
		return nil
	}
	return c
}

// scanClaims reads a Claims line or section. ok is false when the body
// declares none, so a declared empty list is not "no list".
func scanClaims(body string) ([]string, bool) {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	found := false
	for i, l := range lines {
		if claimsHead.MatchString(l) {
			found = true
			if out := section(lines[i+1:]); len(out) > 0 {
				return out, true
			}
			continue
		}
		m := claimsLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		out := globs(m[1])
		if len(out) > 0 {
			return out, true
		}
		for _, bl := range lines[i+1:] {
			b := bulletLine.FindStringSubmatch(bl)
			if b == nil {
				break
			}
			out = append(out, globs(b[1])...)
		}
		return out, true
	}
	return nil, found
}

// section reads the globs under a heading, up to the next one. An issue
// form's "_No response_" counts as none.
func section(lines []string) []string {
	var out []string
	for _, l := range lines {
		if heading.MatchString(l) {
			break
		}
		if b := bulletLine.FindStringSubmatch(l); b != nil {
			l = b[1]
		}
		if strings.Contains(l, "_No response_") {
			continue
		}
		out = append(out, globs(l)...)
	}
	return out
}

func globs(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if f, ok := cleanClaimToken(f); ok {
			out = append(out, f)
		}
	}
	return out
}

// ParseAfter reads "after: #12, #7" lines: issues this one builds on.
func ParseAfter(body string) []int {
	var out []int
	for _, l := range strings.Split(body, "\n") {
		m := afterLine.FindStringSubmatch(strings.TrimRight(l, " \r"))
		if m == nil {
			continue
		}
		for _, r := range issueRef.FindAllStringSubmatch(m[1], -1) {
			n, _ := strconv.Atoi(r[1])
			out = append(out, n)
		}
	}
	return out
}

// ModelScope is the model an old-style ticket's "Model scope:" line names,
// lower case, or "" without one. It is a hint, not a pin (#505): the
// chooser picks the provider, and the model applies only when the pick
// runs that model's family.
func ModelScope(body string) string {
	if m := scopeLine.FindStringSubmatch(body); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

// Difficulty is a ticket's difficulty: hard, standard or mechanical, from
// its "Difficulty:" line or section (#505). Without one, an old "Model
// scope:" line hints at it: Sonnet is standard and Haiku mechanical. Opus
// was every ticket's default, so it says nothing and gives "".
func Difficulty(body string) string {
	if m := difficultyLine.FindStringSubmatch(body); m != nil {
		if d := config.NormalizeDifficulty(m[1]); d != "" {
			return d
		}
	}
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	for i, l := range lines {
		if !difficultyHead.MatchString(l) {
			continue
		}
		for _, n := range lines[i+1:] {
			if heading.MatchString(n) {
				break
			}
			if d := config.NormalizeDifficulty(n); d != "" {
				return d
			}
		}
	}
	switch ModelScope(body) {
	case "sonnet":
		return config.DifficultyStandard
	case "haiku":
		return config.DifficultyMechanical
	}
	return ""
}

// Needs are the capabilities a ticket's "Needs:" line asks for, e.g.
// "Needs: review, image-gen" (#505).
func Needs(body string) []string {
	m := needsLine.FindStringSubmatch(body)
	if m == nil {
		return nil
	}
	var out []string
	for _, f := range strings.FieldsFunc(m[1], func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '*' }) {
		if n := config.NormalizeNeed(f); n != "" && n != "none" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// Provider is the adapter a ticket's "Provider:" line pins it to, lower
// case, or "" to let the chooser pick (#505).
func Provider(body string) string {
	if m := providerLine.FindStringSubmatch(body); m != nil && slices.Contains(config.AdapterNames(), strings.ToLower(m[1])) {
		return strings.ToLower(m[1])
	}
	return ""
}

// Prompt is the brief for an agent working on is: the issue, the repo's
// rules and when it is done.
func Prompt(is Issue, rules, doneWhen string) string {
	var b strings.Builder
	b.WriteString("Implement GitHub issue #" + strconv.Itoa(is.Number) + ": " + is.Title + "\n\n")
	if body := strings.TrimSpace(is.Body); body != "" {
		b.WriteString(body + "\n\n")
	}
	if rules = strings.TrimSpace(rules); rules != "" {
		b.WriteString("## Repo rules (AGENTS.md)\n" + rules + "\n\n")
	}
	b.WriteString("## Done when\n")
	if doneWhen = strings.TrimSpace(doneWhen); doneWhen != "" {
		b.WriteString(doneWhen + "\n")
	}
	b.WriteString("The issue's acceptance criteria hold, new behavior ships with tests (a failing test first for a bug), " +
		"everything is committed, and you call done with a short summary that names the covering tests. " +
		"Saddle started this task on autopilot: don't stop to ask whether to continue.\n")
	return b.String()
}
