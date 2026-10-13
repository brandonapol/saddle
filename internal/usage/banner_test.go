package usage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// issue180Screen is the pane t24 and t26 showed in #180.
const issue180Screen = `⏺ Running the tests now.

You've hit your session limit · resets 12:10pm (America/New_York)

● Usage limit reached · continuing automatically at 12:10pm · esc to cancel

╭──────────────────────────────────────────────╮
│ >                                            │
╰──────────────────────────────────────────────╯
  ⚠ Usage limit reached · limit resets 12:10pm
    Continuing automatically at 12:10pm · esc to cancel`

func TestDetectLimitBanner(t *testing.T) {
	cases := []struct {
		name, screen, resets string
		on                   bool
	}{
		{"issue 180", issue180Screen, "12:10pm (America/New_York)", true},
		{"weekly", "You’ve hit your weekly limit · resets Oct 9, 3pm (Europe/Berlin)\n> ", "Oct 9, 3pm (Europe/Berlin)", true},
		{"older wording", "Claude usage limit reached. Your limit will reset at 5pm (America/Chicago).", "5pm (America/Chicago)", true},
		{"five hour", "5-hour limit reached ∙ resets 3am\n", "3am", true},
		{"no reset given", "Usage limit reached\n", "", true},
		// #321: the other adapters' wordings rotate them too.
		{"codex", "■ You've hit your usage limit. Upgrade to Pro or try again at 4:05 PM.\n› ", "4:05 PM", true},
		{"openai quota", "stream error: You exceeded your current quota, please check your plan and billing details.\n> ", "", true},
		{"grok credits", "Error: out of credits for this billing period. Resets 6pm\n> ", "6pm", true},
		// #480: codex's relative and dated resets, xAI's spend cap.
		{"codex relative", "■ You've hit your usage limit. Upgrade to Pro (https://openai.com/chatgpt/pricing) or try again in 2 hours 5 minutes.\n› ", "in 2 hours 5 minutes", true},
		{"codex dated", "■ You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Oct 14th, 2026 3:04 PM.\n› ", "Oct 14th, 2026 3:04 PM", true},
		{"xai spend cap", "Error: Your team has either used all available credits or reached its monthly spending limit.\n> ", "", true},
		{"working", "⏺ Editing engine.go\n  esc to interrupt", "", false},
		{"prompt", "Do you want to proceed?\n❯ 1. Yes\n  2. No\nEsc to cancel", "", false},
		// An agent quoting the banner further up its scrollback is not parked.
		{"scrolled away", "You've hit your session limit · resets 12:10pm\n" + lines(20) + "> ", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, on := DetectLimitBanner(c.screen)
			if on != c.on || b.Resets != c.resets {
				t.Fatalf("got %+v %v, want resets %q %v", b, on, c.resets, c.on)
			}
		})
	}
}

func lines(n int) string {
	s := ""
	for range n {
		s += "⏺ more output\n"
	}
	return s
}

// screen reads a captured pane from testdata and puts msg where the CLI
// prints a turn's error: codex's "■ …" line, the end of grok's last turn.
func screen(t *testing.T, name, msg string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if msg == "" {
		return s
	}
	var anchor string
	switch name {
	case "codex_error":
		i := strings.Index(s, "■ ")
		anchor, msg = s[i:i+strings.Index(s[i:], "\n\n")], "■ "+msg
	case "grok_idle":
		i := strings.Index(s, "     done ")
		anchor, msg = s[i:i+strings.Index(s[i:], "\n")], "     "+msg
	}
	if !strings.Contains(s, anchor) {
		t.Fatalf("%s has no %q", name, anchor)
	}
	return strings.Replace(s, anchor, msg, 1)
}

// The quota errors codex 0.154 and grok 1.0.50 print, in the layout of a
// real pane of each (#488). The wordings come from the CLIs' binaries; a
// real out-of-quota pane couldn't be produced on demand.
func TestDetectLimitBannerCapturedPanes(t *testing.T) {
	cases := []struct {
		name, pane, msg, resets string
		on                      bool
	}{
		{"codex idle, reset available", "codex_error", "", "", false},
		{"codex plus", "codex_error", "You've hit your usage limit. Upgrade to Plus to continue using Codex (https://chatgpt.com/explore/plus), or try\nagain at 3:04 PM.", "3:04 PM", true},
		{"codex per model", "codex_error", "You've hit your usage limit for gpt-5.6-sol. Switch to another model now, or try again in 2 hours 5 minutes.", "in 2 hours 5 minutes", true},
		{"codex reached", "codex_error", "Usage limit reached. You've reached your usage limit. Increase your limits to continue using codex.", "", true},
		{"codex workspace credits", "codex_error", "Your workspace is out of credits. Add credits to continue.", "", true},
		{"codex spend cap", "codex_error", "You hit your spend cap set in your workspace. Increase your spend cap to continue.", "", true},
		{"codex credit limit", "codex_error", "You've reached your workspace credit limit", "", true},
		{"grok idle", "grok_idle", "", "", false},
		{"grok weekly", "grok_idle", "You hit your weekly limit. Upgrade to a higher tier for more usage", "", true},
		{"grok free", "grok_idle", "You hit your free usage limit. Unlock all features with SuperGrok.", "", true},
		{"grok credits", "grok_idle", "Error: out of credits or over your spending limit. Add credits and retry.", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, on := DetectLimitBanner(screen(t, c.pane, c.msg))
			if on != c.on || b.Resets != c.resets {
				t.Fatalf("got %+v %v, want resets %q %v", b, on, c.resets, c.on)
			}
		})
	}
}
