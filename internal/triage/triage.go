package triage

import (
	"context"
	"regexp"
	"strings"
)

// Routes for an agent event.
const (
	Drop         = "drop"         // nothing to do: the agent is still working
	AutoApprove  = "auto_approve" // a routine prompt the task clearly needs; answer it
	Orchestrator = "orchestrator" // let the orchestrator decide (the default)
	Human        = "human"        // risky or a real decision: tell the user now
)

// Verdicts Jev chooses between for a worker's screen.
var screenCriteria = map[string]string{
	"working":        "The agent is still working: output is streaming, a spinner or 'thinking' line is showing, or a tool is running. Nothing is being asked.",
	"routine_prompt": "A permission prompt for an ordinary step of the agent's own coding task inside its worktree: reading or editing project files, creating files, running the project's build, tests, linters or formatters, or git add/commit/status/diff.",
	"risky_prompt":   "A permission prompt for something destructive or outside the task: deleting many files, rm -rf, git push or force, changing git history, network or curl to unknown hosts, installing system packages, reading secrets or credentials, touching paths outside the worktree, or anything production.",
	"question":       "The agent is asking a question or offering choices that need a product, design or scope decision from a person.",
	"stuck":          "The agent hit an error it is not recovering from, is looping, crashed, is rate limited, or is waiting with no clear question.",
	"finished":       "The agent says the work is complete but has not reported done.",
}

// Decision is what to do with an agent event.
type Decision struct {
	Route      string
	Verdict    string
	Confidence float64
	Keys       []string // for AutoApprove
}

// Thresholds follow TypeSafe's confidence-gated routing guidance: act on
// low-stakes calls above 0.6, high-stakes ones (answering a prompt) above 0.85.
const (
	lowStakes  = 0.6
	highStakes = 0.85
)

var numberedYes = regexp.MustCompile(`(?m)^\s*(?:❯\s*)?1\.\s*Yes\b`)

// Screen triages a worker's terminal. task is a one-line description of what
// the agent is meant to be doing, so "routine" can be judged against it.
func Screen(ctx context.Context, c *Client, task, screen string) (Decision, error) {
	state := "Task the agent was given: " + task + "\n\nThe agent's terminal (Claude Code):\n" + screen
	ans, err := c.Ask(ctx, state, map[string]Question{
		"verdict": {
			Type:         "choice",
			Instructions: "What is this coding agent's terminal showing right now?",
			Criteria:     screenCriteria,
		},
	})
	if err != nil {
		return Decision{Route: Orchestrator}, err
	}
	return route(ans["verdict"], screen), nil
}

func route(a Answer, screen string) Decision {
	d := Decision{Route: Orchestrator, Verdict: a.Choice, Confidence: a.Confidence}
	switch a.Choice {
	case "working":
		if a.Confidence >= lowStakes {
			d.Route = Drop
		}
	case "routine_prompt":
		// Only answer menus whose first option is a plain "Yes"; anything else
		// goes to the orchestrator, which can read the options.
		if a.Confidence >= highStakes && numberedYes.MatchString(screen) {
			d.Route, d.Keys = AutoApprove, []string{"1"}
		}
	case "risky_prompt", "question":
		if a.Confidence >= lowStakes {
			d.Route = Human
		}
	}
	return d
}

// NeedsUser reports whether an orchestrator message asks the user to act or
// decide, as opposed to a status update. It returns false on error.
func NeedsUser(ctx context.Context, c *Client, message string) (bool, error) {
	ans, err := c.Ask(ctx, strings.TrimSpace(message), map[string]Question{
		"needs_user": {
			Type:         "noul",
			Instructions: "This message asks the reader to do something, answer a question, approve something, or make a decision. A pure status update (started, still running, landed, done) is false.",
		},
	})
	if err != nil {
		return false, err
	}
	return ans["needs_user"].Noul >= 0.5, nil
}
