package app

import (
	"encoding/json"
	"fmt"
	"strings"
)

const stackCommentMarker = "<!-- saddle:stack -->"

// upsertStackComment owns a marked comment, never the author's PR body.
// Looking it up on GitHub makes repeated publication stable across restarts.
func (a *App) upsertStackComment(url, body string) error {
	out, err := gh(a.Root, "pr", "view", url, "--json", "comments")
	if err != nil {
		return err
	}
	var response struct {
		Comments []struct {
			ID   string `json:"id"`
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return fmt.Errorf("reading stack comments for %s: %w", url, err)
	}
	for _, comment := range response.Comments {
		if !strings.HasPrefix(comment.Body, stackCommentMarker) {
			continue
		}
		if comment.Body == body {
			return nil
		}
		_, err := gh(a.Root, "api", "graphql", "-f", "query=mutation($id: ID!, $body: String!) { updateIssueComment(input: {id: $id, body: $body}) { issueComment { id } } }", "-f", "id="+comment.ID, "-f", "body="+body)
		return err
	}
	_, err = gh(a.Root, "pr", "comment", url, "--body", body)
	return err
}
