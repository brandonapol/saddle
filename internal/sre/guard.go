package sre

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The read-only production guard (#550). While an incident is open its
// investigators may only read non-code surfaces: logs, metrics, status. A
// mutating action (a restart, a rollback, a config push) unlocks only after
// one agent states a root cause and a different agent independently verifies
// it, and even then the action is not run: it is filed in the decision queue
// as irreversible, and runs only once the owner picks it there. Refuting the
// verified root cause locks the guard again. Closing the incident lifts it.

var (
	// ErrLocked is a mutating action proposed before any root cause has been
	// verified by a second agent.
	ErrLocked = errors.New("production is read-only until a root cause is stated and verified by a different agent")
	// ErrSameAgent is a root cause verified by the agent that stated it.
	ErrSameAgent = errors.New("a root cause must be verified by a different agent than the one that stated it")
	// ErrRefuted is a verification of a root cause already refuted.
	ErrRefuted = errors.New("that root cause was refuted")
)

// Incident is one open (or closed) SRE incident and its guard state.
type Incident struct {
	ID      string    `json:"id"`
	Summary string    `json:"summary,omitempty"`
	Opened  time.Time `json:"opened"`
	Closed  time.Time `json:"closed,omitzero"`
	// Investigators are the tasks the guard holds read-only.
	Investigators []string `json:"investigators"`
	// ReadOnly are argvs the owner declared read-only: the commands of the
	// sre.toml checks.
	ReadOnly   [][]string   `json:"read_only,omitempty"`
	Hypotheses []Hypothesis `json:"hypotheses,omitempty"`
	Proposals  []Proposal   `json:"proposals,omitempty"`
}

// Hypothesis is a root cause one agent stated.
type Hypothesis struct {
	ID        int           `json:"id"` // 1-based
	By        string        `json:"by"`
	RootCause string        `json:"root_cause"`
	Evidence  string        `json:"evidence,omitempty"`
	Stated    time.Time     `json:"stated"`
	Verified  *Verification `json:"verified,omitempty"`
	Refuted   *Verification `json:"refuted,omitempty"`
}

// Verification is a second agent's independent check of a hypothesis, or
// its refutation.
type Verification struct {
	By       string    `json:"by"`
	Evidence string    `json:"evidence"`
	At       time.Time `json:"at"`
}

// Proposal is a mutating action an unlocked guard let an agent propose. It
// goes to the decision queue; it is never run by the guard.
type Proposal struct {
	By         string    `json:"by"`
	Command    string    `json:"command"`
	Hypothesis int       `json:"hypothesis"`
	RootCause  string    `json:"root_cause"`
	StatedBy   string    `json:"stated_by"`
	VerifiedBy string    `json:"verified_by"`
	At         time.Time `json:"at"`
}

// Open reports whether the incident is still open.
func (inc *Incident) Open() bool { return inc != nil && inc.Closed.IsZero() }

// Guarded reports whether task is held read-only by this incident.
func (inc *Incident) Guarded(task string) bool {
	return inc.Open() && task != "" && slices.Contains(inc.Investigators, task)
}

// Close ends the incident and lifts the guard.
func (inc *Incident) Close(now time.Time) {
	if inc.Closed.IsZero() {
		inc.Closed = now.UTC()
	}
}

// AllowChecks declares the commands of c's checks read-only: the owner wrote
// them as read-only checks (#548).
func (inc *Incident) AllowChecks(c *Config) {
	for _, ch := range c.Checks {
		if len(ch.Command) > 0 && ch.Command[0] != "" {
			inc.ReadOnly = append(inc.ReadOnly, slices.Clone(ch.Command))
		}
	}
}

// State records by's root cause and returns its id.
func (inc *Incident) State(by, rootCause, evidence string, now time.Time) (int, error) {
	by, rootCause = strings.TrimSpace(by), strings.TrimSpace(rootCause)
	if by == "" || rootCause == "" {
		return 0, errors.New("state a root cause and who states it")
	}
	id := len(inc.Hypotheses) + 1
	inc.Hypotheses = append(inc.Hypotheses, Hypothesis{ID: id, By: by, RootCause: rootCause,
		Evidence: strings.TrimSpace(evidence), Stated: now.UTC()})
	return id, nil
}

// Verify records by's independent verification of hypothesis id. by must
// not be the agent that stated it, and must say what it checked.
func (inc *Incident) Verify(id int, by, evidence string, now time.Time) error {
	h, v, err := inc.check(id, by, evidence, now)
	if err != nil {
		return err
	}
	if h.Refuted != nil {
		return fmt.Errorf("root cause %d: %w by %s", id, ErrRefuted, h.Refuted.By)
	}
	if v.By == h.By {
		return fmt.Errorf("root cause %d was stated by %s: %w", id, h.By, ErrSameAgent)
	}
	if h.Verified == nil {
		h.Verified = &v
	}
	return nil
}

// Refute records that by found hypothesis id wrong. A refuted root cause no
// longer unlocks the guard.
func (inc *Incident) Refute(id int, by, evidence string, now time.Time) error {
	h, v, err := inc.check(id, by, evidence, now)
	if err != nil {
		return err
	}
	if h.Refuted == nil {
		h.Refuted = &v
	}
	return nil
}

func (inc *Incident) check(id int, by, evidence string, now time.Time) (*Hypothesis, Verification, error) {
	v := Verification{By: strings.TrimSpace(by), Evidence: strings.TrimSpace(evidence), At: now.UTC()}
	if id < 1 || id > len(inc.Hypotheses) {
		return nil, v, fmt.Errorf("no root cause %d in incident %s", id, inc.ID)
	}
	if v.By == "" {
		return nil, v, errors.New("say which agent checks the root cause")
	}
	if v.Evidence == "" {
		return nil, v, errors.New("give the evidence you checked independently")
	}
	return &inc.Hypotheses[id-1], v, nil
}

// Unlocked returns the first root cause a different agent verified and
// nobody refuted. Mutating actions may be proposed only when there is one.
func (inc *Incident) Unlocked() (Hypothesis, bool) {
	if !inc.Open() {
		return Hypothesis{}, false
	}
	for _, h := range inc.Hypotheses {
		if h.Verified != nil && h.Refuted == nil && h.Verified.By != h.By {
			return h, true
		}
	}
	return Hypothesis{}, false
}

// Propose records by's mutating action against the verified root cause and
// returns it for the decision queue. It fails with ErrLocked until a root
// cause is verified by a different agent.
func (inc *Incident) Propose(by, command string, now time.Time) (Proposal, error) {
	h, ok := inc.Unlocked()
	if !ok {
		return Proposal{}, ErrLocked
	}
	p := Proposal{By: by, Command: command, Hypothesis: h.ID, RootCause: h.RootCause,
		StatedBy: h.By, VerifiedBy: h.Verified.By, At: now.UTC()}
	if !slices.ContainsFunc(inc.Proposals, func(o Proposal) bool { return o.By == by && o.Command == command }) {
		inc.Proposals = append(inc.Proposals, p)
	}
	return p, nil
}
