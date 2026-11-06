package app

import (
	"fmt"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// Answering ask_owner (#794). The ask-owner card tells its owner to reply
// with saddle message <asker> …, so that message is the answer: it closes
// the asker's open ask-owner cards addressed to the sender as answered,
// with the message as the reply, and their "go ahead on your best guess"
// default never runs over what the owner actually said.

// UserID is who a message from the CLI, or from an MCP session with no
// task, is from: the owner at the keyboard.
const UserID = "user"

// Message sends text from from to task as an action notice, waking it if
// idle. A message from the owner of an ask-owner question task asked (or
// from the orchestrator or the user, who answer for every owner) answers
// that question.
func (a *App) Message(from, task, text string) error {
	if from == "" {
		from = UserID
	}
	if err := a.answerAskOwner(from, task, text, time.Now()); err != nil {
		return err
	}
	who := from
	if from == UserID {
		who = "the user"
	}
	return a.Notify(task, store.NoticeAction, fmt.Sprintf("Message from %s: %s", who, text))
}

// answerAskOwner closes task's open ask-owner cards that from may answer,
// taking text as the answer.
func (a *App) answerAskOwner(from, task, text string, now time.Time) error {
	var answered []DecisionCard
	err := a.withDecisions(func(cs *[]DecisionCard) error {
		for i, c := range *cs {
			if c.Kind != DecisionAskOwner || c.Task != task || !c.Open() {
				continue
			}
			if c.To != from && from != OrchestratorID && from != UserID {
				continue
			}
			c.Answer, c.By, c.Closed, c.Reply = 1, DecidedByOwner, now.UTC(), oneLine(text)
			(*cs)[i] = c
			answered = append(answered, c)
		}
		return nil
	})
	for _, c := range answered {
		a.Store.Event(c.Task, EventDecisionAnswered, fmt.Sprintf("%s: %s answered: %s", c.ID, from, c.Reply))
	}
	return err
}
