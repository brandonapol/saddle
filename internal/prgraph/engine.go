package prgraph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Source reads the actual state every tick. The host converts its own
// forge and git types into these.
type Source interface {
	Snapshot(ctx context.Context, changes []ChangeRef) (ForgeSnapshot, error)
	GitFacts(ctx context.Context, changes []ChangeRef) (GitFacts, error)
}

// Executor applies one action. It checks the action's guard against a fresh
// read and applies it with a compare-and-set. A failed guard is
// OutcomeStale; an effect that is already there is OutcomeOK. It never
// returns an error: failures are classified.
type Executor interface {
	Apply(ctx context.Context, a Action) Outcome
}

// Locker serializes ticks across processes.
type Locker interface {
	Lock(ctx context.Context) (unlock func(), err error)
}

// Store holds intent and history: the prgraph_ tables.
type Store interface {
	// Load returns the stored state.
	Load(ctx context.Context) (State, error)
	// Transition applies a Transition action to its row with
	// UPDATE ... WHERE gen = a.Guard.Gen. A stale gen is (false, nil).
	Transition(ctx context.Context, a Action, now time.Time) (applied bool, err error)
	// PutAttempt upserts one attempt row.
	PutAttempt(ctx context.Context, att Attempt) error
	// RecordTick stores one tick's input and plan, for replay.
	RecordTick(ctx context.Context, t Tick) error
}

// Tick is one recorded reconcile: prgraph_tick.
type Tick struct {
	TS        time.Time `json:"ts"`
	InputHash string    `json:"input_hash"`
	Input     []byte    `json:"input"`
	Plan      []byte    `json:"plan"`
}

// NewTick encodes in and plan. Replaying Reconcile on the decoded input
// must give the recorded plan byte for byte.
func NewTick(in Input, plan Plan) (Tick, error) {
	ib, err := json.Marshal(in)
	if err != nil {
		return Tick{}, err
	}
	pb, err := json.Marshal(plan)
	if err != nil {
		return Tick{}, err
	}
	sum := sha256.Sum256(ib)
	return Tick{TS: in.Now, InputHash: hex.EncodeToString(sum[:]), Input: ib, Plan: pb}, nil
}

// Replay decodes a recorded tick, reconciles its input again and reports
// whether the plan is the recorded one.
func Replay(t Tick) (Plan, bool, error) {
	var in Input
	if err := json.Unmarshal(t.Input, &in); err != nil {
		return Plan{}, false, err
	}
	plan := Reconcile(in)
	pb, err := json.Marshal(plan)
	if err != nil {
		return Plan{}, false, err
	}
	return plan, string(pb) == string(t.Plan), nil
}

// Engine is read, reconcile, apply one action under the lock, record,
// repeat.
type Engine struct {
	Store    Store
	Source   Source
	Executor Executor
	Locker   Locker
	// Clock defaults to time.Now. It is the only clock; Reconcile sees it
	// as Input.Now.
	Clock func() time.Time
	// MaxSteps bounds the actions one Tick applies (default 64).
	MaxSteps int
}

// Tick runs one reconcile pass: it applies effectful actions one at a time,
// reconciling again after each, until a plan has only waits or MaxSteps
// actions ran. It returns the actions it applied with their outcomes.
func (e Engine) Tick(ctx context.Context) ([]Applied, error) {
	if e.Store == nil || e.Source == nil || e.Executor == nil {
		return nil, errors.New("prgraph: engine needs a store, a source and an executor")
	}
	if e.Locker != nil {
		unlock, err := e.Locker.Lock(ctx)
		if err != nil {
			return nil, fmt.Errorf("prgraph: lock: %w", err)
		}
		defer unlock()
	}
	clock := e.Clock
	if clock == nil {
		clock = time.Now
	}
	steps := e.MaxSteps
	if steps <= 0 {
		steps = 64
	}
	var done []Applied
	// An action applied this tick that came back stale or as a wait isn't
	// retried within the same tick.
	skip := map[string]bool{}
	for range steps {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		in, err := e.read(ctx, clock())
		if err != nil {
			return done, err
		}
		plan := Reconcile(in)
		t, err := NewTick(in, plan)
		if err != nil {
			return done, err
		}
		if err := e.Store.RecordTick(ctx, t); err != nil {
			return done, err
		}
		a, ok := firstEffectful(plan, skip)
		if !ok {
			return done, nil
		}
		o, err := e.apply(ctx, in, a)
		if err != nil {
			return done, err
		}
		done = append(done, Applied{Action: a, Outcome: o})
		if o.Kind != OutcomeOK {
			skip[a.AttemptKey()+"|"+a.String()] = true
		}
	}
	return done, nil
}

// Applied is one action an Engine tick applied.
type Applied struct {
	Action  Action
	Outcome Outcome
}

func firstEffectful(p Plan, skip map[string]bool) (Action, bool) {
	for _, a := range p.Actions {
		if a.Effectful() && !skip[a.AttemptKey()+"|"+a.String()] {
			return a, true
		}
	}
	return Action{}, false
}

func (e Engine) read(ctx context.Context, now time.Time) (Input, error) {
	st, err := e.Store.Load(ctx)
	if err != nil {
		return Input{}, fmt.Errorf("prgraph: load: %w", err)
	}
	refs := make([]ChangeRef, 0, len(st.Changes))
	for _, c := range st.Changes {
		refs = append(refs, ChangeRef{ID: c.ID, Branch: c.Branch, Base: c.Base, PR: c.PR})
	}
	snap, err := e.Source.Snapshot(ctx, refs)
	if err != nil {
		return Input{}, fmt.Errorf("prgraph: snapshot: %w", err)
	}
	gf, err := e.Source.GitFacts(ctx, refs)
	if err != nil {
		return Input{}, fmt.Errorf("prgraph: git facts: %w", err)
	}
	return Input{Now: now, State: st, Forge: snap, Git: gf}, nil
}

func (e Engine) apply(ctx context.Context, in Input, a Action) (Outcome, error) {
	if a.Kind == ActTransition {
		ok, err := e.Store.Transition(ctx, a, in.Now)
		if err != nil {
			return Outcome{}, fmt.Errorf("prgraph: transition %s: %w", a.Change, err)
		}
		if !ok {
			return Outcome{Kind: OutcomeStale}, nil
		}
		return Outcome{Kind: OutcomeOK}, nil
	}
	o := e.Executor.Apply(ctx, a)
	var prev Attempt
	key := a.AttemptKey()
	for _, att := range in.State.Attempts {
		if att.Key == key {
			prev = att
			break
		}
	}
	if o.Kind == OutcomeStale {
		return o, nil
	}
	if err := e.Store.PutAttempt(ctx, Record(prev, a, o, in.Now, in.State.Policy)); err != nil {
		return o, fmt.Errorf("prgraph: record attempt %s: %w", key, err)
	}
	return o, nil
}
