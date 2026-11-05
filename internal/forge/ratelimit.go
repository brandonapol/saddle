package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// The shared GitHub rate-limit governor (#764). When gh reports a rate
// limit, Output writes RateLimitFile under the repo's .saddle/ with the time
// the limit resets. Every saddle process that runs gh in the repo (the
// engine, each agent's MCP server, hooks, the CLI) reads it before running
// gh and, while it is in the future, returns ErrRateLimited instead of
// spending another call. Callers treat that as "wait", not "failed".

// ErrRateLimited is what a gh call returns, wrapped in a *RateLimitError,
// while GitHub's rate limit is spent. Test for it with errors.Is.
var ErrRateLimited = errors.New("GitHub rate-limited")

// RateLimitError carries when the limit resets. It matches ErrRateLimited
// and unwraps to the gh failure that tripped the limit, if there was one.
type RateLimitError struct {
	Until time.Time
	Err   error // the gh failure that hit the limit; nil when gh wasn't run
}

func (e *RateLimitError) Error() string {
	// "rate limit" stays in the text: older checks match on it.
	msg := fmt.Sprintf("GitHub rate limit: gh calls held until %s", e.Until.Local().Format("15:04"))
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Is makes errors.Is(err, ErrRateLimited) true.
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

func (e *RateLimitError) Unwrap() error { return e.Err }

// RateLimitUntil returns when the limit behind err resets, if err is a
// rate-limit error.
func RateLimitUntil(err error) (time.Time, bool) {
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return rl.Until, true
	}
	return time.Time{}, false
}

// RateLimitFile is the marker's name under .saddle/.
const RateLimitFile = "forge-ratelimit.json"

// RateLimitHold is the marker: until when gh calls are held, and the call that
// found the limit.
type RateLimitHold struct {
	Until  time.Time `json:"until"`
	Source string    `json:"source"`
}

// rateLimitFallback is how long calls are held when gh can't say when the
// limit resets.
var rateLimitFallback = 15 * time.Minute

// rateLimitMax caps a reset time gh reports, so a bad clock or answer can't
// hold calls for a day. GitHub's primary window is an hour.
const rateLimitMax = 2 * time.Hour

var (
	rateLimitNow = time.Now
	rateLimitRe  = regexp.MustCompile(`(?i)rate limit|abuse detection|secondary rate`)

	notifyMu sync.Mutex
	notify   func(root string, rl RateLimitHold)
)

// OnRateLimited sets f to be called once per new rate-limit window, with
// the saddle root the marker was written under, by the process that wrote
// it. The app logs its forge_rate_limited event through it.
func OnRateLimited(f func(root string, rl RateLimitHold)) {
	notifyMu.Lock()
	defer notifyMu.Unlock()
	notify = f
}

// IsRateLimitMessage reports whether gh's output says a rate limit was hit.
func IsRateLimitMessage(s string) bool { return rateLimitRe.MatchString(s) }

// saddleRoot finds the nearest directory at or above dir that saddle set
// up (its .saddle/ holds config.toml or state.db), or "". It only stats,
// since it runs before every gh call; a worktree under .saddle/worktrees
// resolves to the main checkout.
func saddleRoot(dir string) string {
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for d := abs; ; d = filepath.Dir(d) {
		for _, f := range []string{"config.toml", "state.db"} {
			if _, err := os.Stat(filepath.Join(d, ".saddle", f)); err == nil {
				return d
			}
		}
		if d == filepath.Dir(d) {
			return ""
		}
	}
}

func rateLimitPath(root string) string { return filepath.Join(root, ".saddle", RateLimitFile) }

// ReadRateLimitHold returns the marker under root's .saddle/ when it still holds
// calls at now.
func ReadRateLimitHold(root string, now time.Time) (RateLimitHold, bool) {
	if root == "" {
		return RateLimitHold{}, false
	}
	b, err := os.ReadFile(rateLimitPath(root))
	if err != nil {
		return RateLimitHold{}, false
	}
	var rl RateLimitHold
	if json.Unmarshal(b, &rl) != nil || !now.Before(rl.Until) {
		return RateLimitHold{}, false
	}
	return rl, true
}

// RateLimited reports whether gh calls from dir are held, and until when.
func RateLimited(dir string) (time.Time, bool) {
	rl, ok := ReadRateLimitHold(saddleRoot(dir), rateLimitNow())
	return rl.Until, ok
}

// held returns the error to give instead of running name in dir, or nil.
func held(dir, name string) error {
	if name != GH {
		return nil
	}
	if until, ok := RateLimited(dir); ok {
		return &RateLimitError{Until: until}
	}
	return nil
}

// tripped records that a gh call in dir hit the rate limit and returns the
// error for it. source names the call.
func tripped(dir, source string, err error) error {
	now := rateLimitNow()
	root := saddleRoot(dir)
	if rl, ok := ReadRateLimitHold(root, now); ok {
		// Another call already opened this window.
		return &RateLimitError{Until: rl.Until, Err: err}
	}
	rl := RateLimitHold{Until: rateLimitReset(dir, now), Source: source}
	if root != "" {
		if b, merr := json.Marshal(rl); merr == nil {
			p := rateLimitPath(root)
			tmp := p + fmt.Sprintf(".%d.tmp", os.Getpid())
			if os.WriteFile(tmp, b, 0o644) == nil {
				_ = os.Rename(tmp, p)
			}
		}
		notifyMu.Lock()
		f := notify
		notifyMu.Unlock()
		if f != nil {
			f(root, rl)
		}
	}
	return &RateLimitError{Until: rl.Until, Err: err}
}

// rateLimitReset asks gh when the spent bucket resets (rate_limit itself
// is free), else now plus rateLimitFallback.
func rateLimitReset(dir string, now time.Time) time.Time {
	fallback := now.Add(rateLimitFallback)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, GH, "api", "rate_limit")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return fallback
	}
	var resp struct {
		Resources map[string]struct {
			Remaining int   `json:"remaining"`
			Reset     int64 `json:"reset"`
		} `json:"resources"`
	}
	if json.Unmarshal(out, &resp) != nil {
		return fallback
	}
	var until time.Time
	for _, k := range []string{"core", "graphql"} {
		r, ok := resp.Resources[k]
		if !ok || r.Remaining > 0 || r.Reset == 0 {
			continue
		}
		if t := time.Unix(r.Reset, 0); t.After(until) {
			until = t
		}
	}
	// Neither bucket is spent (a secondary limit), or the answer is off.
	if !until.After(now) || until.After(now.Add(rateLimitMax)) {
		return fallback
	}
	return until
}
