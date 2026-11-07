// Package contract is the forge contract suite (#823): a table of GitHub
// behaviours saddle depends on, each run against a recorded transcript of
// real gh calls (a Recorder over testdata/*.json) and against fakegh. Both
// must give the same typed outcome (#822). A scenario fakegh fails is a
// fakegh bug: a fake that isn't checked against the real thing drifts.
//
// The fixtures are hand-written until #824's canary records them from a
// real repo, with secrets scrubbed (make contract-record).
package contract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
)

// Outcome is what a scenario's action came to, in #822's terms: Kind is
// ok, pending, refused, rate_limited, transient or failed; Reason is the
// rule a refusal names; Value is what an ok action read back.
type Outcome struct {
	Kind   string
	Reason string
	Value  string
}

func (o Outcome) String() string {
	s := o.Kind
	if o.Reason != "" {
		s += "(" + o.Reason + ")"
	}
	if o.Value != "" {
		s += " " + o.Value
	}
	return s
}

// OutcomeOf types an action's result.
func OutcomeOf(value string, err error) Outcome {
	if err == nil {
		return Outcome{Kind: "ok", Value: value}
	}
	if errors.Is(err, forge.ErrMergePending) {
		return Outcome{Kind: "pending"}
	}
	if errors.Is(err, forge.ErrRateLimited) {
		return Outcome{Kind: "rate_limited"}
	}
	if r, ok := forge.RefusalOf(err); ok {
		return Outcome{Kind: "refused", Reason: r.String()}
	}
	if errors.Is(err, forge.ErrTransient) {
		return Outcome{Kind: "transient"}
	}
	return Outcome{Kind: "failed"}
}

// Call is one recorded forge CLI call: its args, what it printed and how
// it exited.
type Call struct {
	Args   []string `json:"args"`
	Stdout string   `json:"stdout,omitempty"`
	Stderr string   `json:"stderr,omitempty"`
	Exit   int      `json:"exit,omitempty"`
}

// Fixture is one scenario's recorded transcript.
type Fixture struct {
	Scenario string `json:"scenario"`
	// Source says where the transcript came from: hand-written, or the
	// canary's run against a real repo.
	Source string `json:"source"`
	// Params are the PR numbers and branches the recording used.
	Params Params `json:"params"`
	Calls  []Call `json:"calls"`
}

// Params name what a scenario acts on (PR numbers, branches), which differ
// between a recording and a fakegh world.
type Params map[string]string

// LoadFixture reads a fixture file.
func LoadFixture(path string) (Fixture, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Fixture{}, err
	}
	var f Fixture
	if err := json.Unmarshal(b, &f); err != nil {
		return Fixture{}, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// ExitError is a CLI's non-zero exit, as exec reports it.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// cliResult is what forge.CLI makes of a gh run: trimmed stdout, and on a
// non-zero exit an error naming gh and its first two args with stderr.
func cliResult(args []string, stdout, stderr string, code int) (string, error) {
	out := strings.TrimSpace(stdout)
	if code == 0 {
		return out, nil
	}
	return out, fmt.Errorf("gh %s: %w: %s", strings.Join(args[:min(2, len(args))], " "), &ExitError{Code: code}, strings.TrimSpace(stderr))
}

// Recorder replays a fixture's calls in order as a forge.Runner. A call
// that isn't the next recorded one fails, and Err says so; so does a
// recorded call nobody made.
type Recorder struct {
	mu    sync.Mutex
	calls []Call
	next  int
	errs  []string
}

// NewRecorder replays f.
func NewRecorder(f Fixture) *Recorder { return &Recorder{calls: f.Calls} }

// Run is the forge.Runner.
func (r *Recorder) Run(_ context.Context, args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.next >= len(r.calls) {
		r.errs = append(r.errs, fmt.Sprintf("unrecorded call: gh %s", strings.Join(args, " ")))
		return "", fmt.Errorf("gh %s: %w: contract: no recorded call left", strings.Join(args[:min(2, len(args))], " "), &ExitError{Code: 1})
	}
	c := r.calls[r.next]
	if !slices.Equal(c.Args, args) {
		r.errs = append(r.errs, fmt.Sprintf("call %d: gh %s, recorded gh %s", r.next, strings.Join(args, " "), strings.Join(c.Args, " ")))
		return "", fmt.Errorf("gh %s: %w: contract: call doesn't match the recording", strings.Join(args[:min(2, len(args))], " "), &ExitError{Code: 1})
	}
	r.next++
	return cliResult(args, c.Stdout, c.Stderr, c.Exit)
}

// Err reports calls that didn't match the recording, and recorded calls
// left unmade.
func (r *Recorder) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	errs := slices.Clone(r.errs)
	for _, c := range r.calls[r.next:] {
		errs = append(errs, "recorded call not made: gh "+strings.Join(c.Args, " "))
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.New(strings.Join(errs, "\n"))
}

// GitHub is a forge.GitHub over run that never sleeps between merge polls.
func GitHub(run forge.Runner) *forge.GitHub {
	return &forge.GitHub{Run: run, MergePoll: []time.Duration{0, 0, 0}, Sleep: func(time.Duration) {}}
}
