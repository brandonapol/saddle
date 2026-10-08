package autopilot

import (
	"regexp"
	"strconv"
	"strings"
	"time"
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
	afterLine  = regexp.MustCompile(`(?i)^\s*(?:[-*]\s+)?\**after\**\s*:?\s*\**\s*((?:#\d+[\s,]*)+)$`)
	issueRef   = regexp.MustCompile(`#(\d+)`)
	scopeLine  = regexp.MustCompile(`(?i)model scope\s*:\s*\**\s*(opus|sonnet|haiku|fable)`)
)

// ParseClaims reads the path globs an issue declares it will change: a
// "claims:" line with the globs after it, or a bullet list under it. Nil
// means none were declared.
func ParseClaims(body string) []string {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		m := claimsLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		out := globs(m[1])
		if len(out) > 0 {
			return out
		}
		for _, l := range lines[i+1:] {
			b := bulletLine.FindStringSubmatch(l)
			if b == nil {
				break
			}
			out = append(out, globs(b[1])...)
		}
		return out
	}
	return nil
}

func globs(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if f = strings.Trim(f, "`\"'"); f != "" {
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

// ModelScope is the model a ticket is scoped for: sonnet only when its
// "Model scope:" line says so, opus otherwise.
func ModelScope(body string) string {
	if m := scopeLine.FindStringSubmatch(body); m != nil && strings.EqualFold(m[1], "sonnet") {
		return "sonnet"
	}
	return "opus"
}

// Prompt is the brief for an agent working on is: the issue, the repo's
// rules and when it is done.
func Prompt(is Issue, rules, doneWhen string) string {
	var b strings.Builder
	b.WriteString("Implement GitHub issue #" + strconv.Itoa(is.Number) + ": " + is.Title + "\n\n")
	if !scopeLine.MatchString(is.Body) {
		b.WriteString("**Model scope: Opus.**\n\n")
	}
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
