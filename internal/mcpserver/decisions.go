package mcpserver

import (
	"errors"
	"fmt"
	"time"

	"github.com/brandonapol/saddle/internal/app"
)

// DecisionsIn drives the decisions tool (#464).
type DecisionsIn struct {
	Action       string             `json:"action,omitempty" jsonschema:"list (default), file, answer or allow"`
	Task         string             `json:"task,omitempty" jsonschema:"file: the task it blocks; yours by default"`
	Class        string             `json:"class,omitempty" jsonschema:"file, allow: the kind of action, e.g. dependency, release-tag, gate-tolerance"`
	Subject      string             `json:"subject,omitempty" jsonschema:"file: what it acts on, e.g. a module path or a tag; [[preauth]] rules match it"`
	Question     string             `json:"question,omitempty" jsonschema:"file: the one-line question for the owner"`
	What         string             `json:"what,omitempty" jsonschema:"file: one line of context"`
	Options      []DecisionOptionIn `json:"options,omitempty" jsonschema:"file: the choices, at least one; a lone option gets a 'Not now' beside it"`
	Recommended  int                `json:"recommended,omitempty" jsonschema:"file: the recommended option, 1-based (default 1); it runs on a timeout if reversible"`
	Irreversible bool               `json:"irreversible,omitempty" jsonschema:"file: the recommended option can't be undone (a tag push, a publish), so it never runs without the owner or a [[preauth]] rule"`
	Timeout      string             `json:"timeout,omitempty" jsonschema:"file: how long a reversible card waits, e.g. 2h; default [stall.kinds] owner"`
	ID           string             `json:"id,omitempty" jsonschema:"answer: the card's id"`
	Option       int                `json:"option,omitempty" jsonschema:"answer: the option the owner chose; allow: the option the rule takes (default the recommended one)"`
	Match        string             `json:"match,omitempty" jsonschema:"allow: a glob over the subject, e.g. v* or github.com/aws/*; empty allows the whole class"`
	Note         string             `json:"note,omitempty" jsonschema:"allow: why the owner allows it"`
}

type DecisionOptionIn struct {
	Label  string `json:"label"`
	Action string `json:"action" jsonschema:"what runs for it"`
	Cost   string `json:"cost,omitempty"`
}

type DecisionsOut struct {
	Message   string             `json:"message,omitempty"`
	Decisions []app.DecisionCard `json:"decisions,omitempty"`
}

const decisionsDesc = "The owner decision queue. file queues an owner-gated action (a new dependency, a release tag, a gate tolerance) as a card with options, a recommended default and whether it can be undone, " +
	"so unrelated work keeps running instead of waiting on the owner: a reversible card runs its recommended option after its budget, an irreversible one waits for the owner, " +
	"and a [[preauth]] rule the owner set (class + subject glob) allows it at once. The owner gets one digest a day, not a ping per cycle. " +
	"list shows open and recently closed cards. Orchestrator only: answer runs the owner's choice by card id; allow adds a [[preauth]] rule, only when the owner asked."

func decisionsTool(a *app.App, task string, in DecisionsIn) (DecisionsOut, error) {
	now := time.Now()
	orch := func() error {
		if task != app.OrchestratorID {
			return fmt.Errorf("only the orchestrator can %s decisions", in.Action)
		}
		return nil
	}
	switch in.Action {
	case "", "list":
		cs, err := a.Decisions()
		return DecisionsOut{Decisions: cs}, err
	case "file":
		if task == "" {
			return DecisionsOut{}, errors.New("SADDLE_TASK is not set; this MCP server must be started by saddle")
		}
		req := app.DecisionReq{By: task, Task: in.Task, Class: in.Class, Subject: in.Subject, Question: in.Question,
			What: in.What, Recommended: in.Recommended, Irreversible: in.Irreversible}
		for _, o := range in.Options {
			req.Options = append(req.Options, app.DecisionOption{Label: o.Label, Action: o.Action, Cost: o.Cost})
		}
		if in.Timeout != "" {
			d, err := time.ParseDuration(in.Timeout)
			if err != nil {
				return DecisionsOut{}, fmt.Errorf("timeout %q: want a duration such as 2h", in.Timeout)
			}
			req.Timeout = d
		}
		c, err := a.FileDecision(req, now)
		if err != nil {
			return DecisionsOut{}, err
		}
		msg := c.Line()
		if c.Open() {
			msg = c.Render() + "\nQueued as " + c.ID + ". Keep every other lane running; the answer arrives as a [saddle] message."
		}
		return DecisionsOut{Message: msg, Decisions: []app.DecisionCard{c}}, nil
	case "answer":
		if err := orch(); err != nil {
			return DecisionsOut{}, err
		}
		c, err := a.AnswerDecisionID(in.ID, in.Option, now)
		if err != nil {
			return DecisionsOut{}, err
		}
		return DecisionsOut{Message: c.Line(), Decisions: []app.DecisionCard{c}}, nil
	case "allow":
		if err := orch(); err != nil {
			return DecisionsOut{}, err
		}
		r := app.PreauthRule{Class: in.Class, Match: in.Match, Option: in.Option, Note: in.Note}
		done, err := a.AllowClass(r, now)
		return DecisionsOut{Message: fmt.Sprintf("Added %s; it closed %d waiting decision(s).", r, len(done)), Decisions: done}, err
	}
	return DecisionsOut{}, fmt.Errorf("action %q: want list, file, answer or allow", in.Action)
}
