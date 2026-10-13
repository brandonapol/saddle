package app

import "testing"

func TestDetectPrompt(t *testing.T) {
	t.Parallel()
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

// Codex's approval and trust screens (#480). Its trust screen is an ask, not
// PromptTrust: Down, Enter there picks "No, ask me to approve".
func TestDetectPromptCodex(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"Would you like to run the following command?\n\n  $ make check\n\n› 1. Yes, proceed (y)\n  2. Yes, and don't ask again for this command (a)\n  3. No, and tell Codex what to do differently (esc)\n\n  Press enter to confirm or esc to cancel":        PromptAsk,
		"Would you like to make the following edits?\n\n  internal/a.go (+3 -1)\n\n› 1. Yes, proceed (y)":                                                                                                                                                       PromptAsk,
		"> You are running Codex in /work/t1\n\n  Do you trust the contents of this directory? Working with untrusted contents comes with higher risk of prompt injection.\n\n› 1. Yes, continue\n  2. No, quit\n\n  Press enter to continue":                   PromptAsk,
		"  Since this folder is version controlled, you may wish to allow Codex to work in this folder without asking for approval.\n\n› 1. Yes, allow Codex to work in this folder without asking for approval\n  2. No, ask me to approve edits and commands": PromptAsk,
		"• Working (12s • esc to interrupt)\n\n› Ask Codex to do anything": PromptNone,
		"› fix the flaky test": PromptNone,
	}
	for screen, want := range cases {
		if got := DetectPrompt(screen); got != want {
			t.Errorf("DetectPrompt(%q) = %q, want %q", screen, got, want)
		}
	}
}
