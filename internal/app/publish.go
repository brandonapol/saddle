package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// PublishReq is one saddle publish (#220).
type PublishReq struct {
	Target     string // a task id or a branch
	Base       string // the PR's base; defaults to the configured base
	BranchName string // the branch to push; defaults to saddle/<slug>
	Draft      bool
}

// PublishResult is what one publish did.
type PublishResult struct {
	Task     string // "" when the target was a plain branch
	Branch   string
	URL      string
	Commits  int
	Existing bool // the PR already existed; nothing was pushed or opened
}

// publishedPR is an independently published PR, kept in .saddle/published.json.
type publishedPR struct {
	Branch string `json:"branch"`
	URL    string `json:"url"`
	Head   string `json:"head"`
}

// Publish is the escape hatch for when the PR stack can't publish a task
// (#220): it replays the target's own commits, and nothing else, onto base,
// pushes them to a fresh branch as the saddle process (the ref guard lets only
// the train push saddle's branches, so the model never needs `git push`) and
// opens an independent PR against base. It refuses when the commits need work
// base doesn't have, when they would carry another task's commits (checked by
// patch-id), or when the branch is one saddle already uses; when the PR
// already exists it returns its URL. No task's branch moves, and prs leaves a
// published task's PR alone.
func (a *App) Publish(req PublishReq) (PublishResult, error) {
	var res PublishResult
	unlock, err := a.lockTrain()
	if err != nil {
		return res, err
	}
	defer unlock()
	if req.Base == "" {
		req.Base = a.Cfg.Base
	}
	all, err := a.landedAll()
	if err != nil {
		return res, err
	}
	t, isTask, err := a.publishTarget(req.Target)
	if err != nil {
		return res, err
	}
	res.Task = t.ID
	if isTask && t.PR != "" {
		res.URL, res.Existing = t.PR, true
		return res, nil
	}

	branch := req.BranchName
	if branch == "" {
		title := t.Title
		if !isTask {
			title = req.Target
		}
		branch = "saddle/" + slug(title)
	}
	if err := a.publishBranchOK(branch, req.Base); err != nil {
		return res, err
	}
	res.Branch = branch

	_, _ = gitx.Run(a.Root, "fetch", "-q", "origin", req.Base)
	base, err := gitx.RevParse(a.Root, "refs/remotes/origin/"+req.Base)
	if err != nil {
		if base, err = gitx.RevParse(a.Root, req.Base); err != nil {
			return res, fmt.Errorf("can't find base %s: %w", req.Base, err)
		}
	}

	from, to, below, err := a.ownRange(t, isTask, req.Target, base, all)
	if err != nil {
		return res, err
	}
	own, err := patchIDs(a.Root, from+".."+to)
	if err != nil {
		return res, err
	}
	if len(own) == 0 {
		return res, fmt.Errorf("%s has no commits of its own to publish", req.Target)
	}
	if others := othersCommits(a.Root, own, t.ID, all); len(others) > 0 {
		return res, fmt.Errorf("%s carries commits that belong to %s, so it wasn't published; publish only a task's own work (saddle publish <task>)",
			req.Target, strings.Join(others, ", "))
	}

	dir := a.stateDir("publish")
	_ = os.RemoveAll(dir)
	_, _ = gitx.Run(a.Root, "worktree", "prune")
	if _, err := gitx.Run(a.Root, "worktree", "add", "--detach", dir, base); err != nil {
		return res, err
	}
	defer func() { _ = gitx.WorktreeRemove(a.Root, dir) }()
	head, ok, err := replayOnto(dir, base, from, to)
	if err != nil {
		return res, err
	}
	if !ok {
		why := "work " + req.Base + " doesn't have yet"
		if len(below) > 0 {
			why = "work from " + strings.Join(below, ", ") + " below it, which " + req.Base + " doesn't have yet"
		}
		return res, fmt.Errorf("%s's own commits don't apply to %s without %s, so nothing was pushed. Publish or merge that first, then publish %s again",
			req.Target, req.Base, why, req.Target)
	}
	if head == base {
		return res, fmt.Errorf("%s's work is already on %s; nothing to publish", req.Target, req.Base)
	}
	got, err := patchIDs(dir, base+".."+head)
	if err != nil {
		return res, err
	}
	mine := map[string]bool{}
	for _, id := range own {
		mine[id] = true
	}
	for _, id := range got {
		if !mine[id] {
			return res, fmt.Errorf("replaying %s onto %s produced a commit that isn't its own (patch-id %s), so nothing was pushed", req.Target, req.Base, short(id))
		}
	}
	res.Commits = len(got)

	title, body, err := a.publishText(t, isTask, base, head)
	if err != nil {
		return res, err
	}
	if _, err := trainGit(a.Root, "push", "origin", head+":refs/heads/"+branch); err != nil {
		return res, err
	}
	args := []string{"pr", "create", "--base", req.Base, "--head", branch, "--title", title, "--body", body}
	if req.Draft {
		args = append(args, "--draft")
	}
	out, err := gh(a.Root, args...)
	if err != nil {
		url := prURLRe.FindString(err.Error())
		if url == "" || !strings.Contains(err.Error(), "already exists") {
			return res, err
		}
		out, res.Existing = url, true
	}
	res.URL = lastLine(out)
	if isTask {
		if err := a.Store.SetField(t.ID, "pr", res.URL); err != nil {
			return res, err
		}
		if err := a.recordPublished(t.ID, publishedPR{Branch: branch, URL: res.URL, Head: head}); err != nil {
			return res, err
		}
	}
	a.Store.Event(t.ID, "published", branch+" "+short(head)+" "+res.URL)
	return res, nil
}

var prURLRe = regexp.MustCompile(`https://\S+/pull/\d+`)

// publishTarget resolves a task id or branch. A branch that is a task's
// branch resolves to that task.
func (a *App) publishTarget(target string) (store.Task, bool, error) {
	if target == "" {
		return store.Task{}, false, errors.New("publish needs a task or branch")
	}
	if t, err := a.Store.Task(target); err == nil {
		return t, true, nil
	}
	ts, err := a.Store.Tasks()
	if err != nil {
		return store.Task{}, false, err
	}
	for _, t := range ts {
		if t.Branch == target || t.Branch == "saddle/"+target {
			return t, true, nil
		}
	}
	if !gitx.BranchExists(a.Root, target) {
		return store.Task{}, false, fmt.Errorf("%s is neither a task nor a branch", target)
	}
	return store.Task{}, false, nil
}

// publishBranchOK refuses a branch publish must not push: base, the
// integration branch, or any task's branch.
func (a *App) publishBranchOK(branch, base string) error {
	if branch == base || branch == a.Cfg.Base || branch == a.Cfg.Integration {
		return fmt.Errorf("publish won't push to %s; pick another --branch-name", branch)
	}
	ts, err := a.Store.Tasks()
	if err != nil {
		return err
	}
	for _, t := range ts {
		if t.Branch == branch {
			return fmt.Errorf("%s is %s's branch, which publish never moves; pick another --branch-name", branch, t.ID)
		}
	}
	return nil
}

// ownRange is the commits from..to that are the target's own work, and the
// stacked tasks below it that base may lack. A landed task's are the range
// the train landed; an unlanded task's are its branch past integration; a
// plain branch's are its commits past base.
func (a *App) ownRange(t store.Task, isTask bool, target, base string, all []landedTask) (from, to string, below []string, err error) {
	if !isTask {
		return base, target, nil, nil
	}
	prev := ""
	for _, l := range all {
		if l.ID != t.ID {
			if l.stacked() {
				below = append(below, l.ID)
			}
			prev = l.To
			continue
		}
		if l.Lost != "" {
			return "", "", nil, fmt.Errorf("%s's landed work can't be told apart (%s), so publish can't pick out its own commits", t.ID, l.Lost)
		}
		from = l.From
		if from == "" {
			if from = prev; from == "" {
				from, _ = gitx.Run(a.Root, "merge-base", base, l.To)
			}
		}
		return from, l.To, below, nil
	}
	var stackedBelow []string
	for _, l := range all {
		if l.stacked() {
			stackedBelow = append(stackedBelow, l.ID)
		}
	}
	return a.Cfg.Integration, t.Branch, stackedBelow, nil
}

// othersCommits names the stacked tasks, other than self, whose landed
// commits share a patch-id with ids.
func othersCommits(dir string, ids []string, self string, all []landedTask) []string {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []string
	for _, l := range all {
		if l.ID == self || !l.stacked() || l.From == "" || l.Lost != "" {
			continue
		}
		theirs, err := patchIDs(dir, l.From+".."+l.To)
		if err != nil {
			continue
		}
		for _, id := range theirs {
			if want[id] {
				out = append(out, l.ID)
				break
			}
		}
	}
	return out
}

var closesRe = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?) #(\d+)`)

// publishText is the PR's title and body: the commit's message when there is
// one commit, else the task's title and summary with the commits listed;
// laid into the repo's PR template when it has one; closing the task's issue
// and any the commits close.
func (a *App) publishText(t store.Task, isTask bool, base, head string) (string, string, error) {
	log, err := gitx.Run(a.Root, "log", "--reverse", "--format=%s%x00%b%x01", base+".."+head)
	if err != nil {
		return "", "", err
	}
	type commit struct{ subject, body string }
	var cs []commit
	for _, rec := range strings.Split(log, "\x01") {
		subj, body, _ := strings.Cut(strings.TrimSpace(rec), "\x00")
		if subj != "" {
			cs = append(cs, commit{subj, strings.TrimSpace(body)})
		}
	}
	if len(cs) == 0 {
		return "", "", errors.New("nothing to publish")
	}
	var title, desc string
	if len(cs) == 1 {
		title, desc = cs[0].subject, cs[0].body
		if isTask && t.Summary != "" && desc == "" {
			desc = t.Summary
		}
	} else {
		title, desc = cs[0].subject, ""
		if isTask {
			title, desc = t.Title, t.Summary
		}
		var b strings.Builder
		b.WriteString(desc)
		if desc != "" {
			b.WriteString("\n\n")
		}
		b.WriteString("Commits:\n")
		for _, c := range cs {
			b.WriteString("- " + c.subject + "\n")
		}
		desc = strings.TrimSpace(b.String())
	}

	issues := map[int]bool{}
	if isTask && t.Issue > 0 {
		issues[t.Issue] = true
	}
	for _, c := range cs {
		for _, m := range closesRe.FindAllStringSubmatch(c.subject+"\n"+c.body, -1) {
			n, _ := strconv.Atoi(m[1])
			issues[n] = true
		}
	}
	for _, m := range closesRe.FindAllStringSubmatch(desc, -1) {
		n, _ := strconv.Atoi(m[1])
		delete(issues, n) // already closed in the text
	}
	var ns []int
	for n := range issues {
		ns = append(ns, n)
	}
	sort.Ints(ns)
	for _, n := range ns {
		desc += fmt.Sprintf("\n\nCloses #%d", n)
	}
	desc = strings.TrimSpace(desc)

	body := fillTemplate(a.prTemplate(base), desc)
	body += "\n\n---\nPublished by `saddle publish`, on its own and outside saddle's PR stack.\n"
	return title, body, nil
}

// prTemplate is the repo's pull request template at rev, or "".
func (a *App) prTemplate(rev string) string {
	for _, p := range []string{
		".github/pull_request_template.md", ".github/PULL_REQUEST_TEMPLATE.md",
		"docs/pull_request_template.md", "pull_request_template.md", "PULL_REQUEST_TEMPLATE.md",
	} {
		if s, err := gitx.Run(a.Root, "show", rev+":"+p); err == nil && s != "" {
			return s
		}
	}
	return ""
}

var commentRe = regexp.MustCompile(`(?s)^\s*<!--.*?-->`)

// fillTemplate puts desc under the template's first heading, in place of the
// comment that prompts for it; without a heading it goes above the template.
func fillTemplate(tmpl, desc string) string {
	if strings.TrimSpace(tmpl) == "" {
		return desc
	}
	lines := strings.SplitAfter(tmpl, "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, "#") {
			continue
		}
		head := strings.Join(lines[:i+1], "")
		rest := strings.Join(lines[i+1:], "")
		if loc := commentRe.FindStringIndex(rest); loc != nil {
			rest = rest[loc[1]:]
		}
		if !strings.HasSuffix(head, "\n") {
			head += "\n"
		}
		return strings.TrimSpace(head + desc + "\n" + rest)
	}
	return strings.TrimSpace(desc + "\n\n" + tmpl)
}

func (a *App) publishedPath() string { return a.stateDir("published.json") }

// publishedPRs returns the tasks published on their own, keyed by task id.
func (a *App) publishedPRs() map[string]publishedPR {
	out := map[string]publishedPR{}
	b, err := os.ReadFile(a.publishedPath())
	if err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func (a *App) recordPublished(task string, p publishedPR) error {
	m := a.publishedPRs()
	m[task] = p
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.publishedPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.publishedPath())
}
