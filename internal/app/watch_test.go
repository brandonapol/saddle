package app

import "testing"

func TestDetectPrompt(t *testing.T) {
	cases := map[string]string{
		"Quick safety check: Is this a project you created or one you trust?\n ❯ No, exit\n   Yes, I trust this folder": PromptTrust,
		" Do you want to create hello.txt?\n ❯ 1. Yes\n   2. Yes, and switch to accept edits\n   3. No":                 PromptAsk,
		"Bash command\n  rm -rf build\n Do you want to proceed?\n ❯ 1. Yes":                                             PromptAsk,
		"⏺ Update(pkg/meter.go)\n  ⎿  Updated with 3 additions\n✻ Cooking…":                                             PromptNone,
		"❯ ": PromptNone,
	}
	for screen, want := range cases {
		if got := DetectPrompt(screen); got != want {
			t.Errorf("DetectPrompt(%q) = %q, want %q", screen, got, want)
		}
	}
}
