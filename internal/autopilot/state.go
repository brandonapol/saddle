// Package autopilot is the deterministic driver behind `saddle autopilot`
// (#256): while it is on, saddle itself, not the orchestrator model, keeps
// the pipeline moving. Each tick it reconciles lost tasks, lands what is
// queued, tops up to the concurrency cap from a queue of labelled GitHub
// issues whose claims are disjoint from everything running, sleeps through
// plan-limit pressure until the reset, and nudges the orchestrator once
// when the pipeline stalls on something only it can decide. Merging,
// restacking and stack collapse stay with their own watchers.
//
// The standing goal (on/off, stop condition, ready label) and what the
// driver last did live in one small JSON file under .saddle/, so they
// survive restarts and compaction. All side effects go through Env.
package autopilot

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultReadyLabel marks the GitHub issues autopilot may pick up.
const DefaultReadyLabel = "saddle:ready"

// Stop is when a run ends. Zero fields don't stop it; an empty ready queue
// always does.
type Stop struct {
	Until      time.Time `json:"until,omitzero"`       // no new spawns from then on
	UntilUsage float64   `json:"until_usage,omitempty"` // plan-limit fraction (0, 1] at which spawning stops
	MaxTasks   int       `json:"max_tasks,omitempty"`   // tasks to spawn in this run
}

func (s Stop) String() string {
	var parts []string
	if !s.Until.IsZero() {
		parts = append(parts, "until "+s.Until.Local().Format("Mon 15:04"))
	}
	if s.UntilUsage > 0 {
		parts = append(parts, fmt.Sprintf("until usage %.0f%%", s.UntilUsage*100))
	}
	if s.MaxTasks > 0 {
		parts = append(parts, fmt.Sprintf("max %d tasks", s.MaxTasks))
	}
	if len(parts) == 0 {
		return "until the ready queue is empty"
	}
	return strings.Join(parts, ", ")
}

// Spawned is a task autopilot started for an issue.
type Spawned struct {
	Issue int       `json:"issue"`
	Task  string    `json:"task"`
	At    time.Time `json:"at"`
}

// State is .saddle/autopilot.json. The owner's controls (On, Paused, Stop,
// ReadyLabel) bump Gen; a tick that finds Gen moved under it drops its own
// write, so a toggle is never lost to a concurrent tick.
type State struct {
	Gen        int    `json:"gen"`
	On         bool   `json:"on"`
	Paused     bool   `json:"paused,omitempty"`
	Stop       Stop   `json:"stop"`
	ReadyLabel string `json:"ready_label,omitempty"`

	Started time.Time `json:"started,omitzero"`
	Spawned []Spawned `json:"spawned,omitempty"`
	// Draining says why no new tasks start: a stop condition was met and
	// the run ends once its tasks are done and landed.
	Draining string `json:"draining,omitempty"`
	// SleepUntil is the plan-limit reset a usage pause waits for.
	SleepUntil time.Time `json:"sleep_until,omitzero"`

	LastTick     time.Time `json:"last_tick,omitzero"`
	LastDecision string    `json:"last_decision,omitempty"`

	// StallSince is when the current stall began; Nudged and NudgedAt are
	// the last nudge's reason and time.
	StallSince time.Time `json:"stall_since,omitzero"`
	Nudged     string    `json:"nudged,omitempty"`
	NudgedAt   time.Time `json:"nudged_at,omitzero"`

	// Stopped and Summary describe how the last run ended.
	Stopped string `json:"stopped,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// Label is the ready label in force.
func (s State) Label() string {
	if s.ReadyLabel == "" {
		return DefaultReadyLabel
	}
	return s.ReadyLabel
}

// Load reads the state file; a missing one is off.
func Load(path string) (State, error) {
	var s State
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Save writes the state file atomically.
func (s State) Save(path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ParseUntil reads --until: a wall-clock "HH:MM" (the next one after now,
// in now's zone) or an RFC 3339 time.
func ParseUntil(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	hm, err := time.Parse("15:04", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--until %q: want HH:MM or an RFC 3339 time", s)
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), hm.Hour(), hm.Minute(), 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t, nil
}

// ParseUsage reads --until-usage: "90%", "90" or "0.9", as a fraction in (0, 1].
func ParseUsage(s string) (float64, error) {
	s = strings.TrimSpace(s)
	pct := strings.HasSuffix(s, "%")
	v, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
	if err != nil {
		return 0, fmt.Errorf("--until-usage %q: want a percentage like 90%%", s)
	}
	if pct || v > 1 {
		v /= 100
	}
	if v <= 0 || v > 1 {
		return 0, fmt.Errorf("--until-usage %q: want more than 0%% and at most 100%%", s)
	}
	return v, nil
}
