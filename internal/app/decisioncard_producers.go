package app

import (
	"fmt"
	"strings"
)

// The decision cards saddle produces for the needs-you situations it knows
// (#404). Each fills every field, including a non-destructive default.

// blockingLine says in plain words what a red layer holds up.
func blockingLine(above []string) string {
	switch len(above) {
	case 0:
		return "Nothing else waits on it. Everything else keeps running."
	case 1:
		return fmt.Sprintf("1 layer above it (%s). Everything else keeps running.", above[0])
	}
	return fmt.Sprintf("%d layers above it (%s). Everything else keeps running.", len(above), strings.Join(above, ", "))
}

// stuckCard is the card for a stuck-stack alarm. label names the stuck
// layer by what it does; above lists the layers it holds.
func stuckCard(al StuckAlarm, label string, above []string) DecisionCard {
	what := map[string]string{StuckCIRed: "red", StuckPrepublish: "red at its own tip", StuckAtRisk: "at risk"}[al.Reason]
	first := DecisionOption{Label: "Spawn a fix on " + al.Layer + "'s layer", Cost: "an agent does it, ~15 min",
		Action: "spawn a task to fix " + al.Layer + " on its layer: " + oneLine(al.Detail), Recommended: true}
	if al.Reason == StuckAtRisk {
		first = DecisionOption{Label: "Restack the stack onto base", Cost: "saddle does it, ~1 min; a real conflict still needs a fix",
			Action: "saddle restack", Recommended: true}
	}
	return DecisionCard{
		ID:       DecisionStuckStack + ":" + al.Stack + "|" + al.Reason,
		Kind:     DecisionStuckStack,
		To:       OrchestratorID,
		Task:     al.Layer,
		Headline: fmt.Sprintf("Stuck stack: the stack from %s is %s at layer %s and needs you.", al.Stack, what, label),
		What:     fmt.Sprintf("For %s with no task fixing it: %s.", fmtDuration(al.For), oneLine(al.Detail)),
		Blocking: blockingLine(above),
		Options: []DecisionOption{
			first,
			{Label: "Drop " + al.Layer + " from the stack", Cost: "the layers above keep merging; it waits as its own PR",
				Action: "saddle unstack " + al.Layer},
			{Label: "Stop it holding the layers above", Cost: "they merge on top of it as it is",
				Action: "saddle sentinel ack"},
		},
		Default: 2,
		Timeout: DefaultDecisionTimeout,
	}
}

// ciRepairCapCard is the card for a CI-red layer whose repairs ran out.
func ciRepairCapCard(label string, l CIRedLayer, n int) DecisionCard {
	return DecisionCard{
		ID:       DecisionCIRepairCap + ":" + l.Task + "@" + l.Head,
		Kind:     DecisionCIRepairCap,
		To:       OrchestratorID,
		Task:     l.Task,
		Headline: fmt.Sprintf("CI on %s's PR %s is still red after %d repair attempts and needs you.", label, l.PR, n),
		What:     fmt.Sprintf("%s fails at %s and saddle won't spawn another repair.", strings.Join(l.Checks, ", "), short(l.Head)),
		Blocking: blockingLine(l.Held),
		Options: []DecisionOption{
			{Label: "Look at the failing checks and fix them on " + l.Task + "'s layer", Cost: "you or an agent you spawn, ~10 min",
				Action: "spawn a task to fix " + l.Task + "'s CI (" + strings.Join(l.Checks, ", ") + ") on its layer", Recommended: true},
			{Label: "Drop " + l.Task + " from the stack", Cost: "the layers above keep merging; it waits as its own PR",
				Action: "saddle unstack " + l.Task},
			{Label: "Stop it holding the layers above", Cost: "they merge on top of red CI",
				Action: "saddle sentinel ack"},
		},
		Default: 2,
		Timeout: DefaultDecisionTimeout,
	}
}

// askOwnerCard is the card for an ask_owner question from task to owner
// about path, which owner holds through glob.
func askOwnerCard(task, label, owner, path, glob, question string) DecisionCard {
	q := oneLine(question)
	return DecisionCard{
		ID:       DecisionAskOwner + ":" + task + "|" + path + "|" + q,
		Kind:     DecisionAskOwner,
		To:       owner,
		Task:     task,
		Headline: fmt.Sprintf("Question from %s about %s (%s).", label, path, glob),
		What:     q,
		Blocking: "Only " + task + ", which may wait on the answer. Everything else keeps running.",
		Options: []DecisionOption{
			{Label: "Answer it", Cost: "a one-line reply", Action: "saddle message " + task + " <your answer>", Recommended: true},
			{Label: "Tell " + task + " to go ahead on its best guess", Cost: "it notes the guess in its done summary",
				Action: "saddle message " + task + " \"No answer: go ahead on your best guess about " + path + " and say so in your done summary.\""},
			{Label: "Tell " + task + " to leave " + path + " alone", Cost: "the change waits for a later task",
				Action: "saddle message " + task + " \"Leave " + path + " alone; note what you needed in your done summary.\""},
		},
		Default: 2,
		Timeout: DefaultDecisionTimeout,
	}
}

// RiskyPromptCard is the card for a worker sitting on a permission prompt
// that triage flagged as risky. prompt is the prompt's question.
func RiskyPromptCard(task, label, prompt string) DecisionCard {
	p := oneLine(prompt)
	if p == "" {
		p = "a permission prompt"
	}
	return DecisionCard{
		ID:       DecisionRiskyPrompt + ":" + task + "|" + p,
		Kind:     DecisionRiskyPrompt,
		To:       OrchestratorID,
		Task:     task,
		Headline: fmt.Sprintf("%s wants to do something risky and needs you.", label),
		What:     "It asks: " + p,
		Blocking: "Only " + task + ", which waits on the prompt. Everything else keeps running.",
		Options: []DecisionOption{
			{Label: "Allow it once", Cost: "the agent goes ahead", Action: "send_keys " + task + " 1"},
			{Label: "Deny it and tell the agent to find another way", Cost: "it keeps working without it",
				Action: "send_keys " + task + " Escape, then saddle message " + task + " \"Denied: find another way, or explain why you need it.\"", Recommended: true},
			{Label: "Stop the task", Cost: "its work so far stays on its branch", Action: "saddle kill " + task},
		},
		Default: 2,
		Timeout: DefaultDecisionTimeout,
	}
}

// RiskyPromptDecision opens the card for task's risky prompt.
func (a *App) RiskyPromptDecision(task, prompt string) error {
	return a.OpenDecision(RiskyPromptCard(task, a.taskLabel(task), prompt))
}
