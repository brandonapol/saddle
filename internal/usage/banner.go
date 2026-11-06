package usage

import (
	"regexp"
	"slices"
	"strings"
)

// LimitBanner is Claude Code's notice that its session is parked on a plan
// usage limit and will continue on its own after the reset (#180), or
// another agent CLI's out-of-quota error (#321).
type LimitBanner struct {
	Resets string // when it resets, as the banner words it; empty when it doesn't say
	// Credits is set for an out-of-credits or spend-cap banner (#796):
	// that never resets on a timer, only when the owner adds credits.
	Credits bool
}

// bannerTail is how many non-blank lines from the bottom of a screen the
// banner must sit in. Claude Code draws it just above its input box and in
// its status bar; the same words higher up are scrollback, e.g. an agent
// reading an issue that quotes them.
const bannerTail = 12

var (
	limitRe = regexp.MustCompile(`(?i)usage limit reached|you(?:(?:'|’)ve)? hit your [\w-]+(?: usage)? limit|hit your spend cap|reached your (?:workspace credit|usage) limit|\b(?:session|weekly|5-hour|opus) limit reached|exceeded your current quota|\bquota exceeded\b|free usage exceeded|\bout of credits\b|insufficient credits|used all available credits|reached its monthly spending limit`)
	// creditsRe are the banners a timer never clears (#796).
	creditsRe = regexp.MustCompile(`(?i)hit your spend cap|reached your workspace credit limit|\bout of credits\b|insufficient credits|used all available credits|reached its monthly spending limit|payment required`)
	// authRe is an agent CLI whose login expired or was revoked (#798):
	// Claude Code's "Please run /login", a 401, an expired OAuth token.
	authRe = regexp.MustCompile(`(?i)please run /login|\binvalid api key\b|"authentication_error"|\b401\b[^\n]*(?:unauthori[sz]ed|authenticat|token)|oauth token (?:has )?expired|authentication token has expired|try signing in again|please (?:log|sign) in again|error:\s*not logged in`)
	// "reset in" is opencode's relative reset (#520): "It will reset in 2 hours 5 minutes."
	resetRe = regexp.MustCompile(`(?i)\b(?:resets(?:\s+at)?|reset\s+at|try\s+again\s+at|(?:reset|try\s+again)\s+(in))\s+([^·∙\n]+)`)
)

// DetectLimitBanner reports whether a terminal screen shows Claude Code
// parked on a usage limit (or Codex, Grok or opencode out of quota), and
// when the banner says the limit resets.
func DetectLimitBanner(screen string) (LimitBanner, bool) {
	text := screenTail(screen)
	if !limitRe.MatchString(text) {
		return LimitBanner{}, false
	}
	// The fullest wording wins: "12:10pm (America/New_York)" over "12:10pm".
	b := LimitBanner{Credits: creditsRe.MatchString(text)}
	for _, m := range resetRe.FindAllStringSubmatch(text, -1) {
		r := strings.TrimSpace(m[2])
		if m[1] != "" {
			// A relative reset can sit mid-sentence ("reset in 2 hours 5 minutes. To continue…").
			if i := strings.IndexByte(r, '.'); i >= 0 {
				r = strings.TrimSpace(r[:i])
			}
			r = "in " + strings.TrimRight(r, ".│ ") // codex and opencode (#480, #520)
		} else {
			r = strings.TrimRight(r, ".│ ")
		}
		if len(r) > len(b.Resets) {
			b.Resets = r
		}
	}
	return b, true
}

// DetectAuthBanner reports whether a terminal screen shows an agent CLI
// stopped on an expired or revoked login (#798): Claude Code's "Please run
// /login", a 401, codex asking to sign in again. Like a limit banner it
// must sit in the bottom lines; the same words higher up are scrollback.
func DetectAuthBanner(screen string) bool {
	return authRe.MatchString(screenTail(screen))
}

// screenTail is the last bannerTail non-blank lines of screen, in order.
func screenTail(screen string) string {
	var tail []string
	ls := strings.Split(screen, "\n")
	for i := len(ls) - 1; i >= 0 && len(tail) < bannerTail; i-- {
		if strings.TrimSpace(ls[i]) != "" {
			tail = append(tail, ls[i])
		}
	}
	slices.Reverse(tail) // screen order, so a reset wrapped onto the next line reads on
	return strings.Join(tail, "\n")
}
