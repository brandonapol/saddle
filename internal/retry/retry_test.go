package retry

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func rig() (*Retrier, *clock, *[]Attempt) {
	c := &clock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	var escalated []Attempt
	r := &Retrier{Store: NewMemStore(), Now: c.now, Escalate: func(a Attempt) { escalated = append(escalated, a) }}
	return r, c, &escalated
}

// tryWhenDue fails key with err at the next moment it is due, n times.
func tryWhenDue(t *testing.T, r *Retrier, c *clock, key, inputs string, n int, err error) int {
	t.Helper()
	tries := 0
	for range n {
		for {
			_, due, derr := r.Due(key, inputs)
			if derr != nil {
				t.Fatal(derr)
			}
			if due {
				break
			}
			c.add(time.Minute)
		}
		if _, ferr := r.Fail(key, "merge", inputs, err); ferr != nil {
			t.Fatal(ferr)
		}
		tries++
	}
	return tries
}

// #826: a transient or rate-limited failure, however often, backs off and
// never escalates.
func TestTransientNeverEscalates(t *testing.T) {
	r, c, esc := rig()
	transient := fmt.Errorf("gh: %w", forge.ErrTransient)
	tryWhenDue(t, r, c, "merge:pr/1", "sha1", 50, transient)
	limited := &forge.RateLimitError{Until: c.now().Add(time.Hour), Err: errors.New("API rate limit exceeded")}
	tryWhenDue(t, r, c, "merge:pr/1", "sha1", 50, limited)
	if len(*esc) != 0 {
		t.Fatalf("a transient failure escalated: %+v", *esc)
	}
	a, _, _ := r.Due("merge:pr/1", "sha1")
	if a.N != 0 || a.Outcome != RateLimited {
		t.Fatalf("attempt = %+v; transient tries must not count", a)
	}
}

// A rate limit waits until the forge says it resets.
func TestRateLimitWaitsUntil(t *testing.T) {
	r, c, _ := rig()
	until := c.now().Add(40 * time.Minute)
	a, err := r.Fail("k", "merge", "", &forge.RateLimitError{Until: until, Err: errors.New("rate limit")})
	if err != nil || !a.NextAt.Equal(until) {
		t.Fatalf("next = %s, %v; want %s", a.NextAt, err, until)
	}
}

// A deterministic failure backs off 1m, 5m, 30m, then 2h, escalates exactly
// once after three tries at the same error, and keeps retrying.
func TestDeterministicEscalatesOnceAndKeepsRetrying(t *testing.T) {
	r, c, esc := rig()
	err := errors.New("required review missing")
	var waits []time.Duration
	for range 10 {
		a, ferr := r.Fail("merge:pr/1", "merge", "sha1", err)
		if ferr != nil {
			t.Fatal(ferr)
		}
		waits = append(waits, a.NextAt.Sub(c.now()))
		c.t = a.NextAt
		if _, due, _ := r.Due("merge:pr/1", "sha1"); !due {
			t.Fatal("not due at NextAt")
		}
	}
	want := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 2 * time.Hour}
	for i, w := range want {
		if waits[i] != w {
			t.Fatalf("waits = %v, want %v then 2h", waits, want)
		}
	}
	if len(*esc) != 1 || (*esc)[0].N != EscalateAfter || (*esc)[0].LastErr != err.Error() {
		t.Fatalf("escalated %+v, want once at try %d", *esc, EscalateAfter)
	}
}

// Transient failures between deterministic ones neither count nor reset
// the count.
func TestTransientBetweenDeterministicDoesNotCount(t *testing.T) {
	r, _, esc := rig()
	det := errors.New("required review missing")
	for _, e := range []error{det, forge.ErrTransient, det, forge.ErrTransient, forge.ErrTransient} {
		if _, err := r.Fail("k", "merge", "", e); err != nil {
			t.Fatal(err)
		}
	}
	if len(*esc) != 0 {
		t.Fatalf("escalated after two deterministic tries: %+v", *esc)
	}
	if _, err := r.Fail("k", "merge", "", det); err != nil {
		t.Fatal(err)
	}
	if len(*esc) != 1 {
		t.Fatalf("escalated %d times after three deterministic tries", len(*esc))
	}
}

// Three different errors are not three tries at the same error.
func TestDifferentErrorRestartsCount(t *testing.T) {
	r, _, esc := rig()
	for _, m := range []string{"review missing", "checks failing", "review missing"} {
		if _, err := r.Fail("k", "merge", "", errors.New(m)); err != nil {
			t.Fatal(err)
		}
	}
	if len(*esc) != 0 {
		t.Fatalf("escalated on three different errors: %+v", *esc)
	}
}

// A change to the key's inputs (a new head) resets the attempt.
func TestInputsChangeResetsAttempt(t *testing.T) {
	r, _, esc := rig()
	det := errors.New("required review missing")
	for range 2 {
		if _, err := r.Fail("k", "merge", "sha1", det); err != nil {
			t.Fatal(err)
		}
	}
	if _, due, _ := r.Due("k", "sha1"); due {
		t.Fatal("due inside the backoff")
	}
	if a, due, _ := r.Due("k", "sha2"); !due || a.N != 0 {
		t.Fatalf("new head still backing off: %+v", a)
	}
	a, _ := r.Fail("k", "merge", "sha2", det)
	if a.N != 1 || a.FirstErr != det.Error() || len(*esc) != 0 {
		t.Fatalf("attempt after reset = %+v, escalated %d", a, len(*esc))
	}
}

// A refusal the caller handles is due at once, so the next tick routes it
// to the handler; an unknown one counts as deterministic.
func TestRefusalRouting(t *testing.T) {
	r, c, _ := rig()
	r.Handled = func(why forge.Reason) bool { return why == forge.StackedNeedsAsyncMerge }
	a, _ := r.Fail("k", "merge", "", &forge.RefusedError{Reason: forge.StackedNeedsAsyncMerge, Err: errors.New("use merge-async")})
	if a.Outcome != Refused || a.Reason != forge.StackedNeedsAsyncMerge.String() || a.NextAt.After(c.now()) || a.N != 0 {
		t.Fatalf("handled refusal = %+v", a)
	}
	b, _ := r.Fail("j", "merge", "", &forge.RefusedError{Reason: forge.ReviewRequired, Err: errors.New("review required")})
	if b.Outcome != Deterministic || b.N != 1 {
		t.Fatalf("unhandled refusal = %+v", b)
	}
}

func TestSucceedClears(t *testing.T) {
	r, _, _ := rig()
	_, _ = r.Fail("k", "merge", "", errors.New("x"))
	if err := r.Succeed("k"); err != nil {
		t.Fatal(err)
	}
	if a, due, _ := r.Due("k", ""); !due || a.N != 0 {
		t.Fatalf("after success = %+v", a)
	}
}
