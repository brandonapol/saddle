package forge

import (
	"context"
	"errors"
	"strings"
)

// Outcomes (#822). Every forge write returns one of:
//
//   - nil: done.
//   - ErrMergePending (MergePR only): GitHub accepted an async merge and is
//     still merging; read the PR's state later.
//   - ErrTransient: the network, a 5xx, a timeout, a killed CLI. Retry later.
//   - ErrRateLimited, as a *RateLimitError with Until: retry after Until.
//   - ErrRefused, as a *RefusedError: the forge refused by a rule saddle
//     knows; Reason says which.
//   - ErrDeterministic: anything else, with the CLI's raw message.
//
// Test them with errors.Is, RefusalOf and RateLimitUntil. Callers branch on
// these, never on the CLI's text: Classify is the one place that reads it
// (TestNoStderrMatchingOutsideForge).
var (
	ErrTransient     = errors.New("forge: transient failure")
	ErrRefused       = errors.New("forge: refused")
	ErrDeterministic = errors.New("forge: failed")
)

// Reason is a forge rule that refused a write.
type Reason int

const (
	_ Reason = iota
	// StackedNeedsAsyncMerge: a PR in a native stack merges only through
	// the async merge API (MergePR does that itself).
	StackedNeedsAsyncMerge
	// StackedBaseLocked: a PR in a native stack can't change base (use MovePR).
	StackedBaseLocked
	// InMergeQueue: the PR is in the merge queue, which locks it.
	InMergeQueue
	// LabelMissing: the label doesn't exist in the repo.
	LabelMissing
	// RequiredCheckFailing: a required status check is failing or missing.
	RequiredCheckFailing
	// ReviewRequired: the branch needs an approving review.
	ReviewRequired
	// HeadMoved: the PR's head isn't the head the write was guarded on.
	HeadMoved
	// PRClosed: the PR is closed.
	PRClosed
	// PRExists: a PR for this head and base is already open; its URL is in
	// the message.
	PRExists
	// NotFound: the forge answered 404.
	NotFound
	// StackToolMissing: the gh-stack extension isn't installed.
	StackToolMissing
	// StacksDisabled: the repo doesn't have Stacked PRs.
	StacksDisabled
	// StackConflict: a PR already belongs to another native stack.
	StackConflict
	reasonEnd
)

var reasonNames = [...]string{"unknown", "stacked_needs_async_merge", "stacked_base_locked", "in_merge_queue",
	"label_missing", "required_check_failing", "review_required", "head_moved", "pr_closed", "pr_exists",
	"not_found", "stack_tool_missing", "stacks_disabled", "stack_conflict"}

func (r Reason) String() string {
	if r <= 0 || r >= reasonEnd {
		return "unknown"
	}
	return reasonNames[r]
}

// RefusedError is the forge refusing a write by rule Reason. It matches
// ErrRefused, unwraps to the CLI's failure and reads as it.
type RefusedError struct {
	Reason Reason
	Err    error
}

func (e *RefusedError) Error() string        { return e.Err.Error() }
func (e *RefusedError) Is(target error) bool { return target == ErrRefused }
func (e *RefusedError) Unwrap() error        { return e.Err }

// RefusalOf is the rule that refused err, if the forge refused it.
func RefusalOf(err error) (Reason, bool) {
	var r *RefusedError
	if errors.As(err, &r) {
		return r.Reason, true
	}
	return 0, false
}

// outcome is a transient or deterministic failure: it matches kind, unwraps
// to the raw failure and reads as it.
type outcome struct {
	kind error
	err  error
}

func (e *outcome) Error() string        { return e.err.Error() }
func (e *outcome) Is(target error) bool { return target == e.kind }
func (e *outcome) Unwrap() error        { return e.err }

// refusals map the CLI's text, lower-cased, to the rule behind it. The
// first match wins, so the async-merge text ("part of a stack and must be
// merged…") is checked before the base lock's.
var refusals = []struct {
	reason Reason
	marks  []string
}{
	{StackedNeedsAsyncMerge, []string{"asynchronous merge", "merge-async"}},
	{StackedBaseLocked, []string{"pull request is part of a stack"}},
	{InMergeQueue, []string{"merge queue"}},
	{RequiredCheckFailing, []string{"required status check"}},
	{ReviewRequired, []string{"approving review", "review required", "changes requested"}},
	{HeadMoved, []string{"head branch was modified", "head commit", "head sha"}},
	{PRClosed, []string{"closed pull request", "pull request is closed"}},
	{PRExists, []string{"already exists"}},
	{StackToolMissing, []string{`unknown command "stack"`}},
	{StacksDisabled, []string{"not enabled", "is not available for this repository"}},
	{StackConflict, []string{"belongs to multiple stacks", "belongs to a different stack", "already contains"}},
	{NotFound, []string{"http 404", "(404)"}},
}

// transientMarks are CLI text for failures that clear by themselves.
var transientMarks = []string{
	"signal: killed", "timeout", "timed out", "deadline exceeded",
	"http 500", "http 502", "http 503", "http 504", "bad gateway", "service unavailable", "internal server error",
	"something went wrong", "connection reset", "connection refused", "unexpected eof", "tls handshake",
	"no such host", "could not resolve host", "network is unreachable", "is still merging", "try again",
}

// Classify types a raw forge CLI failure as one of the outcomes above. An
// error that already is one, or ErrUnsupported, comes back unchanged.
func Classify(err error) error {
	if err == nil {
		return nil
	}
	var (
		rl *RateLimitError
		rf *RefusedError
		oc *outcome
	)
	switch {
	case errors.As(err, &rl), errors.As(err, &rf), errors.As(err, &oc), errors.Is(err, ErrUnsupported):
		return err
	case errors.Is(err, ErrNetTimeout), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return &outcome{kind: ErrTransient, err: err}
	}
	msg := strings.ToLower(err.Error())
	if IsRateLimitMessage(msg) {
		return &RateLimitError{Until: rateLimitNow().Add(rateLimitFallback), Err: err}
	}
	for _, r := range refusals {
		for _, m := range r.marks {
			if strings.Contains(msg, m) {
				return &RefusedError{Reason: r.reason, Err: err}
			}
		}
	}
	for _, m := range transientMarks {
		if strings.Contains(msg, m) {
			return &outcome{kind: ErrTransient, err: err}
		}
	}
	return &outcome{kind: ErrDeterministic, err: err}
}

// refuse is err as refused by r when its text alone didn't say more than
// "failed" or "not found": for a write whose op says which rule such a
// failure means (a missing label's "not found").
func refuse(r Reason, err error) error {
	c := Classify(err)
	if reason, ok := RefusalOf(c); errors.Is(c, ErrDeterministic) || (ok && reason == NotFound) {
		return &RefusedError{Reason: r, Err: errors.Unwrap(c)}
	}
	return c
}
