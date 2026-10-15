package planner

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/brandonapol/saddle/internal/forge"
)

// Issues is the part of the forge Push uses; forge.Forge is one.
type Issues interface {
	CreateIssue(ctx context.Context, i forge.NewIssue) (forge.Issue, error)
	SubIssueNumbers(ctx context.Context, repo string, n int) ([]int, error)
	AddSubIssue(ctx context.Context, repo string, parent, child int) error
}

// ErrNotApproved is returned when pushing a plan that hasn't been approved.
var ErrNotApproved = errors.New("plan is not approved; approve it before pushing it as issues")

// Push publishes the approved plan at path as issues on the forge: an epic issue
// (the one the epic came from, if any) and one sub-issue per task, linked to
// the epic. Each task records its issue in Issues so the merge train can
// close it when the task lands.
//
// Push saves the plan after every issue it creates, and on each run links
// any task issue the epic doesn't list yet, so after a failure running it
// again finishes the job without duplicating issues.
func Push(ctx context.Context, f Issues, path string, serial []string, limit int) (Doc, error) {
	d, err := LoadDoc(path)
	if err != nil {
		return Doc{}, err
	}
	if !d.Approved {
		return d, ErrNotApproved
	}
	repo, refPrefix := d.Repo, d.Repo
	save := func() error { return WriteDoc(path, d, serial, limit) }
	create := func(title, body string) (int, error) {
		is, err := f.CreateIssue(ctx, forge.NewIssue{Repo: repo, Title: title, Body: body})
		if err != nil {
			return 0, err
		}
		if is.Number <= 0 {
			return 0, fmt.Errorf("creating an issue returned %q, want an issue URL", is.URL)
		}
		return is.Number, nil
	}

	if d.Issue == 0 {
		n, err := create(d.Epic, epicBody(d))
		if err != nil {
			return d, fmt.Errorf("create epic issue: %w", err)
		}
		d.Issue = n
		if err := save(); err != nil {
			return d, err
		}
	}
	ns, err := f.SubIssueNumbers(ctx, repo, d.Issue)
	if err != nil {
		return d, fmt.Errorf("list sub-issues of #%d: %w", d.Issue, err)
	}
	linked := map[int]bool{}
	for _, n := range ns {
		linked[n] = true
	}

	for i := range d.Tasks {
		t := &d.Tasks[i]
		n, ok := taskIssue(*t, refPrefix)
		if !ok {
			if n, err = create(t.Title, taskBody(d, *t, refPrefix)); err != nil {
				return d, fmt.Errorf("create issue for %s: %w", t.ID, err)
			}
			t.Issues = append(t.Issues, refPrefix+"#"+strconv.Itoa(n))
			if err := save(); err != nil {
				return d, err
			}
		}
		if linked[n] {
			continue
		}
		if err := f.AddSubIssue(ctx, repo, d.Issue, n); err != nil {
			return d, fmt.Errorf("link #%d under epic #%d: %w", n, d.Issue, err)
		}
		linked[n] = true
	}
	return d, nil
}

// taskIssue returns the number of the task's issue in the plan's repo.
func taskIssue(t Task, prefix string) (int, bool) {
	for _, r := range t.Issues {
		if num, ok := strings.CutPrefix(r, prefix+"#"); ok {
			if n, err := strconv.Atoi(num); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

func epicBody(d Doc) string {
	return strings.TrimSpace(d.Text) + "\n\n_Planned by saddle into " + strconv.Itoa(len(d.Tasks)) + " tasks; each is a sub-issue._\n"
}

func taskBody(d Doc, t Task, prefix string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Part of %s#%d.\n\n%s\n", prefix, d.Issue, strings.TrimSpace(t.Plan))
	fmt.Fprintf(&b, "\n**Claims:** `%s`\n", strings.Join(t.Claims, "`, `"))
	if len(t.After) > 0 {
		var deps []string
		for _, id := range t.After {
			dep := id
			for _, o := range d.Tasks {
				if n, ok := taskIssue(o, prefix); ok && o.ID == id {
					dep = fmt.Sprintf("%s (%s#%d)", id, prefix, n)
				}
			}
			deps = append(deps, dep)
		}
		fmt.Fprintf(&b, "\n**Runs after %s.**\n", strings.Join(deps, ", "))
	}
	if t.Barrier {
		b.WriteString("\n**Barrier:** runs alone.\n")
	}
	b.WriteString("\n**Done when:**\n")
	for _, c := range t.DoneWhen {
		fmt.Fprintf(&b, "- [ ] %s\n", c)
	}
	fmt.Fprintf(&b, "\n_saddle task `%s`_\n", t.ID)
	return b.String()
}
