package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

const stackCommentMarker = "<!-- saddle:stack -->"

// stackEntry is one published layer, for its stack comment and its
// saddle/base status.
type stackEntry struct {
	Task  store.Task
	Group int    // the stack it is in
	Base  string // the branch its PR targets
	Why   string // why it is stacked; "" when it isn't
	Head  string // the commit its PR branch is at
}

// baseStatus is the commit status saddle sets on every PR it publishes:
// red while the PR targets anything but base, since merging it then lands
// its work on another task's branch, not on base (#751).
const baseStatus = "saddle/base"

func (a *App) updateStackComments(es []stackEntry) error {
	for i, e := range es {
		var mine []stackEntry
		for _, o := range es {
			if o.Group == e.Group {
				mine = append(mine, o)
			}
		}
		var b strings.Builder
		b.WriteString(stackCommentMarker + "\n\n")
		if len(mine) > 1 {
			b.WriteString("**Stack** (opened by saddle; merge bottom-up)\n\n")
			for j := len(mine) - 1; j >= 0; j-- {
				mark := ""
				if mine[j].Task.ID == e.Task.ID {
					mark = " 👈"
				}
				fmt.Fprintf(&b, "%d. %s %s%s\n", j+1, mine[j].Task.PR, mine[j].Task.Title, mark)
			}
			b.WriteString("\n")
			if mine[0].Task.ID == e.Task.ID {
				fmt.Fprintf(&b, "Merge this one first: it targets `%s`.\n", a.Cfg.Base)
			} else {
				fmt.Fprintf(&b, "Merge first: %s. Until it merges, this PR targets `%s`, so merging it puts its work on that branch, not on `%s`.\n",
					mine[0].Task.PR, e.Base, a.Cfg.Base)
			}
		} else {
			b.WriteString("Opened by saddle; no other published layer in this stack.\n")
		}
		if e.Why != "" {
			fmt.Fprintf(&b, "Stacked because %s.\n", strings.TrimSuffix(e.Why, "."))
		}
		if line := a.diffAgainstBase(es, i); line != "" {
			b.WriteString(line + "\n")
		}
		fmt.Fprintf(&b, "\nBase: `%s`\n", e.Base)
		if err := a.upsertStackComment(e.Task.PR, b.String()); err != nil {
			return err
		}
		a.setBaseStatus(e)
	}
	return nil
}

// diffAgainstBase says how big layer i's PR is against base and how much of
// that is the layers below it, which merge first: "" when git can't say.
func (a *App) diffAgainstBase(es []stackEntry, i int) string {
	e := es[i]
	if e.Head == "" {
		return ""
	}
	base := a.baseRef()
	n, err := gitx.Run(a.Root, "rev-list", "--count", base+".."+e.Head)
	if err != nil {
		return ""
	}
	files, err := gitx.Run(a.Root, "diff", "--name-only", base+"..."+e.Head)
	if err != nil {
		return ""
	}
	commits, _ := strconv.Atoi(n)
	line := fmt.Sprintf("Diff against `%s`: %s, %s", a.Cfg.Base, plural(commits, "commit"), plural(len(strings.Fields(files)), "file"))
	if e.Base == a.Cfg.Base {
		return line + "."
	}
	for _, o := range es {
		if o.Task.Branch != e.Base || o.Head == "" {
			continue
		}
		if own, err := gitx.Run(a.Root, "rev-list", "--count", o.Head+".."+e.Head); err == nil {
			if k, _ := strconv.Atoi(own); commits > k {
				line += fmt.Sprintf("; %d from the layers below, which merge first", commits-k)
			}
		}
	}
	return line + "."
}

// setBaseStatus sets layer e's saddle/base status at its head: green on
// base, red stacked on another branch. A forge without commit statuses, or
// a failed call, is only an event: it never stops publishing.
func (a *App) setBaseStatus(e stackEntry) {
	if e.Head == "" {
		return
	}
	state, desc := forge.StatusSuccess, "targets "+a.Cfg.Base
	if e.Base != a.Cfg.Base {
		state = forge.StatusFailure
		desc = "targets " + e.Base + ", not " + a.Cfg.Base + ": merge the PRs below first"
	}
	err := a.Forge().SetStatus(context.Background(), e.Head, baseStatus, state, desc)
	if err != nil && !errors.Is(err, forge.ErrUnsupported) {
		a.Store.Event(e.Task.ID, "base_status_failed", err.Error())
	}
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
