package tui

import (
	"fmt"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// answerDecision takes the user's text as the answer to the oldest open
// decision card when it is a bare option number (#404): it confirms in one
// line and asks the orchestrator to run the chosen option. It reports
// whether the text was an answer.
func (m *model) answerDecision(text string) bool {
	c, opt, ok, err := m.app.AnswerDecision(text, time.Now())
	if err != nil {
		m.addChat(store.ChatEvent, "Could not answer the decision card: "+err.Error())
		return false
	}
	if !ok {
		return false
	}
	m.addChat(store.ChatEvent, c.Line())
	m.sendUser(fmt.Sprintf("I answered the decision card %q with option %d (%s). Do it now: %s",
		c.Headline, c.Answer, opt.Label, opt.Action))
	return true
}

// openRiskyCard opens a decision card for a worker's risky prompt.
func (m *model) openRiskyCard(att attention) {
	if err := m.app.RiskyPromptDecision(att.task, promptLine(att.screen)); err != nil {
		m.addChat(store.ChatEvent, "Could not open a decision card for "+att.task+": "+err.Error())
	}
}
