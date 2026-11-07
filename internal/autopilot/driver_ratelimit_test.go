package autopilot

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
)

// limitedEnv is fakeEnv with GitHub's rate limit spent: listing issues,
// landing and reconciling all come back ErrRateLimited.
type limitedEnv struct{ *fakeEnv }

func rateLimitErr(what string) error {
	return fmt.Errorf("%s: %w", what, &forge.RateLimitError{Until: time.Now().Add(time.Hour)})
}

func (l limitedEnv) Ready(string) ([]Issue, error) { return nil, rateLimitErr("gh issue list") }
func (l limitedEnv) Land() (int, error)            { return 0, rateLimitErr("gh pr view") }
func (l limitedEnv) Reconcile() ([]string, error)  { return nil, rateLimitErr("gh pr view") }

// #764: under the rate limit autopilot skips the tick as a wait. It logs no
// autopilot_error, spawns nothing, and says why it is waiting.
func TestTickUnderRateLimitWaits(t *testing.T) {
	r := newRig(t, 4, claimed(1, "a/**"))
	r.on(Options{})
	r.d.Env = limitedEnv{r.env}
	rep, err := r.d.Tick()
	if err != nil {
		t.Fatalf("rate-limited tick err = %v, want a skipped tick", err)
	}
	if !strings.Contains(rep.Decision, "rate limit") || len(r.env.spawns) != 0 {
		t.Fatalf("decision %q, spawns %v; want a wait", rep.Decision, r.env.spawns)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = r.d.Run(ctx)
	if r.env.hasEvent(EventError) {
		t.Fatalf("rate limit logged autopilot_error: %v", r.env.events)
	}
}

// timeoutEnv is fakeEnv with every forge call timing out on the network.
type timeoutEnv struct{ *fakeEnv }

func timeoutErr(what string) error { return fmt.Errorf("%s: %w", what, forge.ErrNetTimeout) }

func (l timeoutEnv) Ready(string) ([]Issue, error) { return nil, timeoutErr("gh issue list") }
func (l timeoutEnv) Land() (int, error)            { return 0, timeoutErr("gh pr view") }
func (l timeoutEnv) Reconcile() ([]string, error)  { return nil, timeoutErr("gh pr view") }

// #809: a net timeout is a transient wait like the rate limit: the tick is
// skipped, no autopilot_error is logged and nothing is spawned.
func TestTickOnNetTimeoutWaits(t *testing.T) {
	r := newRig(t, 4, claimed(1, "a/**"))
	r.on(Options{})
	r.d.Env = timeoutEnv{r.env}
	rep, err := r.d.Tick()
	if err != nil {
		t.Fatalf("timed-out tick err = %v, want a skipped tick", err)
	}
	if len(r.env.spawns) != 0 {
		t.Fatalf("spawns %v; want a wait", r.env.spawns)
	}
	_ = rep
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = r.d.Run(ctx)
	if r.env.hasEvent(EventError) {
		t.Fatalf("net timeout logged autopilot_error: %v", r.env.events)
	}
}
