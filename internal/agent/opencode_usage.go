package agent

import (
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/usage"
)

// OpenCodeExhaustFor is how long opencode stays out when its limit error
// names no reset (the free tier, a provider's billing error).
const OpenCodeExhaustFor = time.Hour

// openCodeBannerTail is how many non-blank lines from the bottom of the
// screen a limit message must sit in. opencode prints the error in the
// conversation, above the composer and footer.
const openCodeBannerTail = 12

// OpenCodeOut says opencode, or one provider/model behind it, is out of
// quota until Until. It carries what app.MarkModelExhausted(Adapter,
// Model, Until, Resets) takes; an empty Model means all of opencode.
type OpenCodeOut struct {
	Adapter string
	Model   string // provider/model; empty when the failing model is not known
	Until   time.Time
	Resets  string // the error's own wording, e.g. "in 2 hours 5 minutes"
}

func openCodeOut(l usage.OpenCodeLimit, model string, from, now time.Time) (OpenCodeOut, bool) {
	until := from.Add(OpenCodeExhaustFor)
	switch {
	case l.ResetIn > 0:
		until = from.Add(l.ResetIn)
	case l.RetryIn > OpenCodeExhaustFor:
		// opencode is waiting out the provider's retry-after (Zen's free
		// tier resets at UTC midnight). A shorter wait may be a backoff, not
		// a reset, so it keeps the default.
		until = from.Add(l.RetryIn)
	}
	if !until.After(now) {
		return OpenCodeOut{}, false
	}
	return OpenCodeOut{Adapter: OpenCodeName, Model: model, Until: until, Resets: l.Resets}, true
}

// OpenCodeScreenLimit reports whether a capture of opencode's screen shows
// a quota or limit error, and marks model (the worker's provider/model,
// empty for all of opencode) out until the reset the message gives, or for
// OpenCodeExhaustFor when it gives none. opencode retries Zen's limits,
// waiting out their retry-after, so these show on the status line while
// the turn is still running and reach the session database only once the
// retries run out.
func OpenCodeScreenLimit(screen, model string, now time.Time) (OpenCodeOut, bool) {
	var tail []string
	ls := strings.Split(screen, "\n")
	for i := len(ls) - 1; i >= 0 && len(tail) < openCodeBannerTail; i-- {
		if strings.TrimSpace(ls[i]) != "" {
			tail = append(tail, ls[i])
		}
	}
	for i, j := 0, len(tail)-1; i < j; i, j = i+1, j-1 {
		tail[i], tail[j] = tail[j], tail[i]
	}
	l, ok := usage.ParseOpenCodeLimit(strings.Join(tail, "\n"))
	if !ok {
		return OpenCodeOut{}, false
	}
	return openCodeOut(l, model, now, now)
}

// OpenCodeDBLimit reads opencode's session database for the newest limit
// error after afterMs in the sessions run in dir (all when empty). The
// failing provider/model comes from the failed turn, and the reset counts
// from when it failed. A limit already past marks nothing.
func OpenCodeDBLimit(dbPath, dir string, afterMs int64, now time.Time) (OpenCodeOut, bool, error) {
	ls, err := usage.ReadOpenCodeLimits(dbPath, dir, afterMs)
	if err != nil || len(ls) == 0 {
		return OpenCodeOut{}, false, err
	}
	l := ls[len(ls)-1]
	out, ok := openCodeOut(l, l.Model(), l.Time, now)
	return out, ok, nil
}
