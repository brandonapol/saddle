Unedited `tmux capture-pane -e -p` captures of Codex 0.154.0 and Grok 1.0.50
in a 120x40 pane (#488), with only the working directory renamed. Tests read
them through `tmux.StripANSI`, the plain capture the watchers see.

- `codex_trust.ansi`: "Do you trust the contents of this directory?" with
  the `› 1. Yes, continue` menu.
- `codex_approval.ansi`: "Would you like to run the following command?"
  (`-a on-request -s read-only`).
- `codex_working.ansi`: "• Working (6s • esc to interrupt)".
- `codex_idle.ansi`: after a turn, the placeholder in the composer.
- `grok_approval.ansi`: `--permission-mode default`; options drawn as
  `┃  1 (●) Yes, …` with a `Ctrl+c:cancel` footer.
- `grok_working.ansi`: a spinner line and the `Ctrl+c:cancel` footer.
- `grok_idle.ansi`: after a turn.
