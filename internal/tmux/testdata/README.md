Pane captures of Claude Code's input box, as `tmux capture-pane -e -p` prints
them (SGR escapes kept). Each file is one real capture of an idle Claude Code
session with only the `❯` input line swapped for a form seen in real panes:

- `claude_empty.ansi`: nothing typed.
- `claude_suggestion.ansi`: the grey next-prompt suggestion, drawn faint
  (`\x1b[2m`) after `❯` and a no-break space (#183).
- `claude_placeholder.ansi`: the faint `Try "…"` placeholder.
- `claude_draft.ansi`: text the user typed, in the default colour.
- `claude_draft_after_suggestion.ansi`: typed text with a faint completion
  after it. The typed part makes it a draft.
