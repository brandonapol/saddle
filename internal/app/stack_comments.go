package app

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/store"
)

const stackCommentMarker = "<!-- saddle:stack -->"

func (a *App) updateStackComments(stack []store.Task, groups []int, bases []string) error {
	for i, task := range stack {
		var mine []store.Task
		for j, other := range stack {
			if groups[i] == groups[j] {
				mine = append(mine, other)
			}
		}
		var b strings.Builder
		b.WriteString(stackCommentMarker + "\n\n")
		if len(mine) > 1 {
			b.WriteString("**Stack** (opened by saddle; merge bottom-up)\n\n")
			for j := len(mine) - 1; j >= 0; j-- {
				mark := ""
				if mine[j].ID == task.ID {
					mark = " 👈"
				}
				fmt.Fprintf(&b, "%d. %s %s%s\n", j+1, mine[j].PR, mine[j].Title, mark)
			}
		} else {
			b.WriteString("Opened by saddle; no other published layer in this stack.\n")
		}
		fmt.Fprintf(&b, "\nBase: `%s`\n", bases[i])
		if err := a.upsertStackComment(task.PR, b.String()); err != nil {
			return err
		}
	}
	return nil
}

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
