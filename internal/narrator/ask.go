package narrator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrCapReached means today's spend has hit the daily cap, so the narrator
// won't call the model again until tomorrow.
var ErrCapReached = errors.New("narrator: daily cost cap reached")

// Question is something the user asks the narrator.
type Question struct {
	Text string
	// Task is the agent the question is about, if any.
	Task string
	// Screen is the agent's recent screen. The user opts in per question;
	// only its last maxScreenLines lines are sent.
	Screen string
}

const (
	maxScreenLines = 60
	answerTokens   = 300
)

const askPrompt = `You narrate a team of parallel coding agents for the human supervising them, and now the human asks you a question.

You get the task roster, each task's current status, the lines you narrated recently and, if the human chose to share it, the last lines of one agent's screen. Answer in at most three short sentences of plain English, no markdown. Say what you know from what you were given; if it doesn't answer the question, say so and suggest where to look (the agent's window, or asking the orchestrator).`

// Ask answers a question about the agents. It spends from the same daily
// cap as narration and returns ErrCapReached once the cap is hit. The answer
// is returned, not emitted: the caller threads it under the question.
func (n *Narrator) Ask(ctx context.Context, q Question) (Line, error) {
	q.Text = strings.TrimSpace(q.Text)
	if q.Text == "" {
		return Line{}, errors.New("narrator: empty question")
	}
	if n.CapReached() {
		return Line{}, ErrCapReached
	}
	tasks, err := n.deps.Roster.Tasks()
	if err != nil {
		return Line{}, fmt.Errorf("narrator: roster: %w", err)
	}
	var status strings.Builder
	status.WriteString("Status now (id | status | title):\n")
	sorted := append(tasks[:0:0], tasks...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, t := range sorted {
		if t.Active() {
			fmt.Fprintf(&status, "%s | %s | %s\n", t.ID, t.Status, t.Title)
		}
	}
	n.mu.Lock()
	recent := append([]Line(nil), n.recent...)
	n.mu.Unlock()
	var lines strings.Builder
	lines.WriteString("Your recent lines:\n")
	if len(recent) == 0 {
		lines.WriteString("(none yet)\n")
	}
	for _, l := range recent {
		lines.WriteString(l.String() + "\n")
	}
	var ask strings.Builder
	if q.Task != "" {
		fmt.Fprintf(&ask, "The question is about task %s.\n", q.Task)
	}
	if s := tail(strings.TrimRight(q.Screen, "\n "), maxScreenLines); s != "" {
		fmt.Fprintf(&ask, "Its terminal, last lines:\n```\n%s\n```\n", s)
	}
	ask.WriteString("Question: " + q.Text)

	text, err := n.complete(ctx, Request{
		MaxTokens: answerTokens,
		System:    askPrompt,
		Blocks: []Block{
			{Text: rosterText(tasks), Cache: true},
			{Text: status.String()},
			{Text: lines.String()},
			{Text: ask.String()},
		},
	})
	if err != nil {
		return Line{}, err
	}
	return Line{Time: n.deps.Clock.Now(), Task: q.Task, Text: strings.TrimSpace(text)}, nil
}

// tail returns the last n lines of s.
func tail(s string, n int) string {
	if s == "" {
		return ""
	}
	ls := strings.Split(s, "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}
