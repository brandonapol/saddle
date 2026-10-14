package hook

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/sre"
)

// RuleSREGuard is the read-only production guard (#550).
const RuleSREGuard = "sre-guard"

// SREDecisionClass is the decision-queue class of a production action an
// unlocked guard proposed. A [[preauth]] rule for it takes the recommended
// option, which holds the action.
const SREDecisionClass = "sre-action"

// The options of an sre-action card.
const (
	sreRunOption  = 1
	sreHoldOption = 2
)

// EventSREProposed is logged when the guard files a production action.
const EventSREProposed = "sre_action_proposed"

// sreGuard holds an incident's investigators read-only: a tool call that may
// mutate is denied until a root cause is verified by a second agent, then
// filed in the decision queue as irreversible, and allowed only once the
// owner picks "run" there. nil lets the call go on to the other checks.
func sreGuard(a *app.App, task string, in Input) *Output {
	ro, _ := readOnlyTool(in.ToolName, in.ToolInput, nil)
	inc, err := sre.LoadIncident(a.Root)
	if err != nil {
		if ro {
			return nil
		}
		// Unreadable state can't say who is guarded: refuse what may mutate
		// production, and say how to get out.
		return deny(in, RuleSREGuard, fmt.Sprintf("%v. The guard can't tell whether production is read-only, so it refuses actions that may change it. "+
			"Read-only commands still work. The orchestrator or owner can fix or remove %s to clear this.", err, sre.IncidentFile))
	}
	if !inc.Guarded(task) {
		return nil
	}
	if ro, _ = readOnlyTool(in.ToolName, in.ToolInput, inc.ReadOnly); ro {
		return nil
	}
	action := actionText(in)
	if approved(a, task, action) {
		a.Store.Event(task, "sre_action_approved", action)
		return nil
	}
	var p sre.Proposal
	err = sre.UpdateIncident(a.Root, func(inc *sre.Incident) error {
		var err error
		p, err = inc.Propose(task, action, time.Now())
		return err
	})
	if errors.Is(err, sre.ErrLocked) {
		return deny(in, RuleSREGuard, fmt.Sprintf("incident %s: %v, so this may not run: %s. "+
			"Keep investigating with read-only commands (logs, metrics, status, kubectl get/describe/logs). "+
			"When you have a root cause, state it with `saddle sre root-cause \"<cause>\" --evidence \"<what shows it>\"`; "+
			"a different investigator checks it independently and runs `saddle sre verify <id> --evidence \"...\"`. "+
			"Then a production action goes to the owner's decision queue instead of running.", inc.ID, sre.ErrLocked, action))
	}
	if err != nil {
		return deny(in, RuleSREGuard, fmt.Sprintf("incident %s: can't record the proposal (%v); nothing ran", inc.ID, err))
	}
	c, err := a.FileDecision(sreDecision(inc.ID, task, p), time.Now())
	if err != nil {
		return deny(in, RuleSREGuard, fmt.Sprintf("incident %s: can't file the action in the decision queue (%v); nothing ran", inc.ID, err))
	}
	if !c.Open() && c.Answer == sreRunOption {
		return nil // a [[preauth]] rule picked "run"
	}
	a.Store.Event(task, EventSREProposed, c.ID)
	return deny(in, RuleSREGuard, fmt.Sprintf("incident %s: the root cause is verified (%d, stated by %s, verified by %s), but production actions are the owner's call. "+
		"Filed in the decision queue as irreversible (%s); nothing ran. You will get a notice if the owner picks it; then run the same command again. "+
		"Meanwhile keep working read-only.", inc.ID, p.Hypothesis, p.StatedBy, p.VerifiedBy, c.ID))
}

// actionText is what the call would do: its shell command, or the tool and
// its input.
func actionText(in Input) string {
	if cmd, _ := in.ToolInput["command"].(string); cmd != "" {
		return cmd
	}
	b, _ := json.Marshal(in.ToolInput)
	return in.ToolName + " " + string(b)
}

// approved reports whether the owner (or a [[preauth]] rule) picked "run"
// on task's card for action. A default taken on timeout never counts.
func approved(a *app.App, task, action string) bool {
	cs, err := a.Decisions()
	if err != nil {
		return false
	}
	return slices.ContainsFunc(cs, func(c app.DecisionCard) bool {
		return c.Class == SREDecisionClass && c.Task == task && c.Subject == action && !c.Open() &&
			c.Answer == sreRunOption && (c.By == app.DecidedByOwner || c.By == app.DecidedPreauth)
	})
}

func sreDecision(incident, task string, p sre.Proposal) app.DecisionReq {
	return app.DecisionReq{
		By: task, Task: task, Class: SREDecisionClass, Subject: p.Command,
		Question: fmt.Sprintf("Run a production action for incident %s: %s", incident, p.Command),
		What: fmt.Sprintf("Root cause %d, stated by %s and verified independently by %s: %s",
			p.Hypothesis, p.StatedBy, p.VerifiedBy, p.RootCause),
		Blocking: "Only " + task + "'s action; every investigator keeps working read-only.",
		Options: []app.DecisionOption{
			{Label: "Run it", Cost: "changes production; it may not be undoable", Action: p.Command},
			{Label: "Hold", Cost: "production stays as it is; investigation goes on read-only",
				Action: "saddle message " + task + " \"Not approved: don't run it; keep investigating read-only.\""},
		},
		Recommended:  sreHoldOption,
		Irreversible: true,
	}
}
