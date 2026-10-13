Pane captures of agent input boxes, as `tmux capture-pane -e -p` prints
them (SGR escapes kept). Tests read each one styled and through `StripANSI`,
which is what a plain capture shows.

Claude Code. Each file is one real capture of an idle session with only the
`❯` input line swapped for a form seen in real panes:

- `claude_empty.ansi`: nothing typed.
- `claude_suggestion.ansi`: the grey next-prompt suggestion, drawn faint
  (`\x1b[2m`) after `❯` and a no-break space (#183).
- `claude_placeholder.ansi`: the faint `Try "…"` placeholder.
- `claude_draft.ansi`: text the user typed, in the default colour.
- `claude_draft_after_suggestion.ansi`: typed text with a faint completion
  after it. The typed part makes it a draft.

Codex 0.154.0 and Grok 1.0.50, unedited captures (#488) of a 120x40 pane,
with only the working directory renamed:

- `codex_placeholder.ansi`: a fresh session, the faint "Ask Codex to do
  anything" placeholder after `›`.
- `codex_draft.ansi`: "fix the flaky test" typed.
- `codex_idle.ansi`: after a turn; the user's earlier message is also drawn
  behind a `›` higher up.
- `codex_working.ansi`: mid-turn, "• Working (6s • esc to interrupt)".
- `codex_approval.ansi`: "Would you like to run the following command?" with
  the `› 1. Yes, proceed (y)` menu.
- `grok_empty.ansi`: the splash screen and an empty `│ ❯ │` composer box.
- `grok_draft.ansi`: "fix the flaky test" typed.
- `grok_idle.ansi`: after a turn.
- `grok_working.ansi`: mid-turn, a spinner and the `Ctrl+c:cancel` footer.
