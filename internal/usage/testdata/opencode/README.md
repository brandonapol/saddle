# opencode session rows

- `messages.jsonl`: synthetic `message` rows in opencode 1.19.0's layout
  (#531), for ses_a (`/repo/wt`) and ses_b (`/repo/other`).
- `limits.jsonl` (#573): assistant rows opencode 1.19.0 really wrote when
  `limitmock.py` refused it the way OpenCode Zen and OpenAI do for limits.
  Paths are rewritten to `/repo/wt` and every row put in ses_a; the rest is
  as stored. In order of capture: Zen free tier (429
  `FreeUsageLimitError`), Zen with no balance (401 `CreditsError`), OpenAI
  `insufficient_quota` (429), Zen Go 5-hour limit (429 `GoUsageLimitError`),
  Zen monthly spending limit (401 `MonthlyLimitError`), and a Zen free tier
  turn still retrying when opencode was closed (no `error`, no
  `completed`). The free tier and Go rows were captured with `retry-after`
  shortened to 1 so opencode's five retries finish; Zen really sends the
  time to the reset, which opencode waits out on every retry.
- `limitmock.py`: the stand-in. `MODE=free|go|credits|monthly|openai
  MOCKLOG=<file> python3 limitmock.py <port>` (`RA=<n>` overrides
  `retry-after`), with opencode's `provider.<id>.options.baseURL` set to
  `http://127.0.0.1:<port>/v1`. Run `opencode run` with `</dev/null`, or it
  waits on stdin.
