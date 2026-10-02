package planner

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// GH runs the gh CLI and returns its stdout.
type GH func(ctx context.Context, args ...string) (string, error)

// ErrNotApproved is returned when pushing a plan that hasn't been approved.
var ErrNotApproved = errors.New("plan is not approved; approve it before pushing it as issues")

// Push publishes the approved plan at path as GitHub issues: an epic issue
// (the one the epic came from, if any) and one sub-issue per task, linked to
// the epic. Each task records its issue in Issues so the merge train can
// close it when the task lands.
//
// Push saves the plan after every issue it creates, and on each run links
// any task issue the epic doesn't list yet, so after a failure running it
// again finishes the job without duplicating issues.
func Push(ctx context.Context, gh GH, path string, serial []string, limit int) (Doc, error) {
	d, err := LoadDoc(path)
	if err != nil {
		return Doc{}, err
	}
	if !d.Approved {
		return d, ErrNotApproved
	}
	repoArgs, api, refPrefix := []string(nil), "repos/{owner}/{repo}", ""
	if d.Repo != "" {
		repoArgs, api, refPrefix = []string{"-R", d.Repo}, "repos/"+d.Repo, d.Repo
	}
	save := func() error { return WriteDoc(path, d, serial, limit) }
	create := func(title, body string) (int, error) {
		out, err := gh(ctx, append([]string{"issue", "create", "--title", title, "--body", body}, repoArgs...)...)
		if err != nil {
			return 0, err
		}
		return issueNumber(out)
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
	subs := fmt.Sprintf("%s/issues/%d/sub_issues", api, d.Issue)
	out, err := gh(ctx, "api", subs, "--paginate", "--jq", ".[].number")
	if err != nil {
		return d, fmt.Errorf("list sub-issues of #%d: %w", d.Issue, err)
	}
	linked := map[int]bool{}
	for f := range strings.FieldsSeq(out) {
		if n, err := strconv.Atoi(f); err == nil {
			linked[n] = true
		}
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
		id, err := gh(ctx, "api", fmt.Sprintf("%s/issues/%d", api, n), "--jq", ".id")
		if err != nil {
			return d, fmt.Errorf("look up issue #%d: %w", n, err)
		}
		if _, err := gh(ctx, "api", subs, "-X", "POST", "-F", "sub_issue_id="+strings.TrimSpace(id)); err != nil {
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

// issueNumber reads the issue number off the URL gh issue create prints.
func issueNumber(url string) (int, error) {
	url = strings.TrimSpace(url)
	n, err := strconv.Atoi(url[strings.LastIndex(url, "/")+1:])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("gh issue create printed %q, want an issue URL", url)
	}
	return n, nil
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
