package usage

import "testing"

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
