package usage

import (
	"regexp"
	"strings"
)

// LimitBanner is Claude Code's notice that its session is parked on a plan
// usage limit and will continue on its own after the reset (#180), or
// another agent CLI's out-of-quota error (#321).
type LimitBanner struct {
	Resets string // when it resets, as the banner words it; empty when it doesn't say
}

// bannerTail is how many non-blank lines from the bottom of a screen the
// banner must sit in. Claude Code draws it just above its input box and in
// its status bar; the same words higher up are scrollback, e.g. an agent
// reading an issue that quotes them.
const bannerTail = 12

var (
	limitRe = regexp.MustCompile(`(?i)usage limit reached|you(?:'|’)ve hit your [\w-]+ limit|\b(?:session|weekly|5-hour|opus) limit reached|exceeded your current quota|\bout of credits\b|insufficient credits`)
	resetRe = regexp.MustCompile(`(?i)\b(?:resets?(?: at)?|try again at)\s+([^·∙\n]+)`)
)

// DetectLimitBanner reports whether a terminal screen shows Claude Code
// parked on a usage limit (or Codex or Grok out of quota), and when the
// banner says the limit resets.
func DetectLimitBanner(screen string) (LimitBanner, bool) {
	var tail []string
	ls := strings.Split(screen, "\n")
	for i := len(ls) - 1; i >= 0 && len(tail) < bannerTail; i-- {
		if strings.TrimSpace(ls[i]) != "" {
			tail = append(tail, ls[i])
		}
	}
	text := strings.Join(tail, "\n")
	if !limitRe.MatchString(text) {
		return LimitBanner{}, false
	}
	// The fullest wording wins: "12:10pm (America/New_York)" over "12:10pm".
	var b LimitBanner
	for _, m := range resetRe.FindAllStringSubmatch(text, -1) {
		if r := strings.TrimRight(strings.TrimSpace(m[1]), ".│ "); len(r) > len(b.Resets) {
			b.Resets = r
		}
	}
	return b, true
}
