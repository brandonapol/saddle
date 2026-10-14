# opencode panes (#509)

Captured from opencode 1.19.0 in a 120x40 tmux pane with `capture-pane -p`
(`.txt`) and `capture-pane -p -e` (`.ansi`, with SGR escapes). Paths are
rewritten to `/repo/wt`.

- `busy.txt`: a turn in progress. The footer shows `esc interrupt`.
- `idle.txt`: the turn finished; the footer shows the cwd and context use.
- `approval.txt`: `permission: {"bash": "ask"}`; the `△ Permission required`
  panel with `Allow once   Allow always   Reject`.
- `worker-idle.txt`: a worker started by the adapter's launch.sh (`--auto`;
  the composer reads `Build auto · <model>`) after its task.
- `draft.txt`: the same pane with text typed into the composer.
- `quota.txt` is **synthetic** (#531), kept for the tests that use it: three
  opencode 1.19.0 strings placed where a turn's reply goes. The `limit-*`
  panes below are what opencode really shows.

## Limit panes (#573)

Captured the same way, from opencode 1.19.0 talking to a local stand-in for
the provider (`internal/usage/testdata/opencode/limitmock.py`). The stand-in
answers with the exact status, body and `retry-after` OpenCode Zen sends
for each limit (from Zen's handler: `routes/zen/util/handler.ts`,
`error.ts`, `ipRateLimiter.ts` and the console's `en.ts`) and OpenAI's
documented `insufficient_quota` 429. opencode was pointed at it with
`provider.opencode.options.baseURL` / `provider.openai.options.baseURL`. No
real account was run out: everything opencode did with the response
(retrying, the dialog, the status line, the stored error) is its own.

- `limit-free-retry.txt`: Zen free tier (429 `FreeUsageLimitError`, body
  message "Rate limit exceeded. Please try again later.", `retry-after` to
  UTC midnight). opencode does not fail the turn: it shows a "Free limit
  reached" dialog and the status line
  `Free usage exceeded, subscribe to Go [retrying in 9h 29m attempt #1]`.
- `limit-go-retry.txt`: Go 5-hour limit (429 `GoUsageLimitError`,
  `retry-after: 7500`). A "Go limit reached" dialog and
  `5 hour usage limit reached. It will reset in 2 hours 5 minutes. …` on the
  status line, still retrying.
- `limit-openai-retry.txt`: OpenAI `insufficient_quota` 429, retried with
  backoff (`[retrying in 1s attempt #3]`).
- `limit-openai-error.txt`: the same after five retries: the error in the
  conversation and a toast.
- `limit-credits.txt`: Zen with no balance (401 `CreditsError`), not
  retried: "Insufficient balance. Manage your billing here: …" in the
  conversation.

opencode's spinner draws `⬝` over the status line, sometimes over the
spaces of the message itself (`⬝You⬝exceeded`).
