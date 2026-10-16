package app

import (
	"context"
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
	ctx, f := context.Background(), a.Forge()
	comments, err := f.Comments(ctx, url)
	if err != nil {
		return err
	}
	for _, comment := range comments {
		if !strings.HasPrefix(comment.Body, stackCommentMarker) {
			continue
		}
		if comment.Body == body {
			return nil
		}
		return f.EditComment(ctx, comment.ID, body)
	}
	return f.AddComment(ctx, url, body)
}
