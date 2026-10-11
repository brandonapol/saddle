package narrator

import (
	"context"
	"errors"
	"strings"
)

// Draft has the model write prose for saddle from a system prompt and the
// facts it is given, such as a blocker issue's summary (#407). It spends
// from the same daily cap as narration and returns ErrCapReached once the
// cap is hit, so the caller can fall back to a template.
func (n *Narrator) Draft(ctx context.Context, system, facts string, maxTokens int) (string, error) {
	if n.CapReached() {
		return "", ErrCapReached
	}
	if maxTokens <= 0 {
		maxTokens = n.cfg.MaxTokens
	}
	text, err := n.complete(ctx, Request{MaxTokens: maxTokens, System: system, Blocks: []Block{{Text: facts}}})
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("narrator: empty draft")
	}
	return text, nil
}
