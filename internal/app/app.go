// Package app is saddle's core: spawning agents, claims, the merge train and
// notices. The CLI, hooks and MCP server are thin layers over it.
package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/claims"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/tmux"
)

const OrchestratorID = "t0"

type App struct {
	Root  string
	Cfg   config.Config
	Store *store.Store
	Tmux  tmux.Driver
	Bin   string
}

// Open finds the repo from dir (SADDLE_ROOT wins) and opens its state.
func Open(dir string) (*App, error) {
	root := os.Getenv("SADDLE_ROOT")
	if root == "" {
		var err error
		if root, err = gitx.Root(dir); err != nil {
			return nil, fmt.Errorf("not in a git repo: %w", err)
		}
	}
	cfg, err := config.Load(root)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(root, ".saddle", "state.db"))
	if err != nil {
		return nil, err
	}
	bin, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if b, err := filepath.EvalSymlinks(bin); err == nil {
		bin = b
	}
	return &App{Root: root, Cfg: cfg, Store: st, Tmux: tmux.Tmux{Session: cfg.Session}, Bin: bin}, nil
}

func (a *App) Close() error { return a.Store.Close() }

func (a *App) stateDir(parts ...string) string {
	return filepath.Join(append([]string{a.Root, ".saddle"}, parts...)...)
}

// Init creates .saddle/, a config template and a git exclude entry.
func (a *App) Init() error {
	if err := os.MkdirAll(a.stateDir("worktrees"), 0o755); err != nil {
		return err
	}
	cfgPath := a.stateDir("config.toml")
	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(cfgPath, []byte(config.Template), 0o644); err != nil {
			return err
		}
	}
	if err := a.detectTestCmd(); err != nil {
		return err
	}
	exclude := filepath.Join(a.Root, ".git", "info", "exclude")
	b, _ := os.ReadFile(exclude)
	if !strings.Contains(string(b), "/.saddle/") {
		if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(exclude, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.WriteString("/.saddle/\n"); err != nil {
			return err
		}
	}
	return nil
}

// ensureIntegration fetches base and creates the integration branch from
// <remote>/<base> on first use. Later it reports when integration falls behind.
func (a *App) ensureIntegration() error {
	base := a.freshBase()
	if gitx.BranchExists(a.Root, a.Cfg.Integration) {
		a.checkBehind(base)
		return nil
	}
	_, err := gitx.Run(a.Root, "branch", "--no-track", a.Cfg.Integration, base)
	return err
}

type SpawnReq struct {
	ID     string
	Title  string
	Prompt string
	Claims []string
	Model  string
	Parent string
	Base   string // defaults to the integration branch
	Issue  int    // GitHub issue the task implements; its PR will close it
	Force  bool   // ignore claim conflicts and the concurrency cap
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 32 {
		s = strings.TrimRight(s[:32], "-")
	}
	if s == "" {
		s = "task"
	}
	return s
}

func (a *App) activeWorkers() (int, error) {
	ts, err := a.Store.Tasks()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range ts {
		if t.Role == store.RoleWorker && (t.Status == store.Running || t.Status == store.Idle || t.Status == store.NeedsYou) {
			n++
		}
	}
	return n, nil
}

// Spawn creates a task with its own branch and worktree and starts Claude in a new tmux window.
func (a *App) Spawn(r SpawnReq) (store.Task, error) {
	var t store.Task
	if strings.TrimSpace(r.Title) == "" {
		return t, errors.New("spawn: title is required")
	}
	if !r.Force {
		n, err := a.activeWorkers()
		if err != nil {
			return t, err
		}
		if n >= a.Cfg.Concurrency {
			return t, fmt.Errorf("at concurrency cap (%d running); wait for a task to finish or raise concurrency in .saddle/config.toml", n)
		}
	}
	for i, c := range r.Claims {
		r.Claims[i] = claims.Clean(c)
	}
	if err := a.Init(); err != nil {
		return t, err
	}
	if err := a.ensureIntegration(); err != nil {
		return t, err
	}
	// Claims are checked before the row exists, so a conflict leaves nothing behind.
	if len(r.Claims) > 0 && !r.Force {
		all, err := a.Store.Claims()
		if err != nil {
			return t, err
		}
		if c := claims.Conflicts(all, "", r.Claims); len(c) > 0 {
			return t, conflictErr(c)
		}
	}
	id := r.ID
	if id == "" {
		var err error
		if id, err = a.Store.NextID(); err != nil {
			return t, err
		}
	}
	if _, err := a.Store.Task(id); err == nil {
		return t, fmt.Errorf("task %s already exists", id)
	}
	name := id + "-" + slug(r.Title)
	base := r.Base
	if base == "" {
		base = a.Cfg.Integration
	}
	model := r.Model
	if model == "" {
		model = a.Cfg.Claude.Model
	}
	hint := a.retryHint(r.Title)
	t = store.Task{
		ID: id, Title: r.Title, Prompt: r.Prompt, Parent: r.Parent, Role: store.RoleWorker, Model: model,
		Branch: "saddle/" + name, Worktree: a.stateDir("worktrees", name), Status: store.Running, Issue: r.Issue,
	}
	hadBranch := gitx.BranchExists(a.Root, t.Branch)
	if err := a.Store.CreateTask(t); err != nil {
		return t, err
	}
	// Claims go in before the worktree exists so a racing spawn sees them.
	if len(r.Claims) > 0 {
		_, err := a.Store.Claim(id, func(all map[string][]string) ([]string, error) {
			if c := claims.Conflicts(all, id, r.Claims); len(c) > 0 && !r.Force {
				return nil, conflictErr(c)
			}
			return r.Claims, nil
		})
		if err != nil {
			return t, a.spawnFailed(t, false, hadBranch, err)
		}
	}
	if err := gitx.WorktreeAdd(a.Root, t.Worktree, t.Branch, base); err != nil {
		return t, a.spawnFailed(t, false, hadBranch, fmt.Errorf("worktree add: %w", err))
	}
	win, err := a.launch(t, r.Claims)
	if err != nil {
		return t, a.spawnFailed(t, true, hadBranch, fmt.Errorf("launch: %w", err))
	}
	t.Window = win
	a.Store.Event(id, "spawn", fmt.Sprintf("parent=%s model=%s claims=%s%s", r.Parent, model, strings.Join(r.Claims, ","), hint))
	if r.Parent != "" && r.Parent != OrchestratorID {
		if err := a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf("%s spawned sub-task %s %q.", r.Parent, id, r.Title)); err != nil {
			return t, err
		}
	}
	return t, nil
}

func conflictErr(c map[string]string) error {
	var lines []string
	for want, owner := range c {
		lines = append(lines, fmt.Sprintf("%s overlaps %s", want, owner))
	}
	sort.Strings(lines)
	return fmt.Errorf("claim conflict: %s", strings.Join(lines, "; "))
}

func (a *App) launch(t store.Task, cl []string) (string, error) {
	l := agent.Launch{
		Root: a.Root, Bin: a.Bin, Task: t.ID, Title: t.Title, Dir: t.Worktree, Model: t.Model,
		Mode: a.Cfg.Claude.PermissionMode, Cmd: a.Cfg.Claude.Cmd, RunDir: a.stateDir("run", t.ID),
		Brief: a.workerBrief(t, cl), Prompt: t.Prompt,
	}
	cmd, err := l.Write()
	if err != nil {
		return "", err
	}
	var win string
	if a.Tmux.HasSession() {
		win, err = a.Tmux.NewWindow(t.ID+"-"+slug(t.Title), t.Worktree, cmd)
	} else {
		win, err = a.Tmux.NewSession(t.ID+"-"+slug(t.Title), t.Worktree, cmd)
	}
	if err != nil {
		return "", err
	}
	return win, a.Store.SetField(t.ID, "window", win)
}

// Orchestrator ensures the orchestrator task exists and returns the launch
// for its headless Claude Code process, plus the session to resume (if any).
func (a *App) Orchestrator() (agent.Launch, string, error) {
	if err := a.Init(); err != nil {
		return agent.Launch{}, "", err
	}
	if err := a.ensureIntegration(); err != nil {
		return agent.Launch{}, "", err
	}
	t, err := a.Store.Task(OrchestratorID)
	if errors.Is(err, store.ErrNotFound) {
		t = store.Task{ID: OrchestratorID, Title: "orchestrator", Role: store.RoleOrchestrator,
			Model: a.Cfg.Claude.OrchestratorModel, Worktree: a.Root, Status: store.Running}
		if err := a.Store.CreateTask(t); err != nil {
			return agent.Launch{}, "", err
		}
	} else if err != nil {
		return agent.Launch{}, "", err
	}
	// It runs headless; a window left from an old tmux-based run is not its own.
	if err := errors.Join(a.Store.SetStatus(t.ID, store.Running), a.Store.SetField(t.ID, "window", "")); err != nil {
		return agent.Launch{}, "", err
	}
	l := agent.Launch{
		Root: a.Root, Bin: a.Bin, Task: t.ID, Title: "orchestrator", Dir: a.Root,
		Model: a.Cfg.Claude.OrchestratorModel, Mode: a.Cfg.Claude.PermissionMode, Cmd: a.Cfg.Claude.Cmd,
		RunDir: a.stateDir("run", t.ID), Brief: a.orchestratorBrief(), Allow: agent.OrchestratorAllow(),
		Deny: agent.OrchestratorDeny(),
	}
	return l, t.SessionID, nil
}

// Down stops every agent (the tmux session) and releases their claims.
// Worktrees and branches stay, so no committed work is lost.
func (a *App) Down() (int, error) {
	ts, err := a.Store.Tasks()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range ts {
		if t.Role == store.RoleWorker && t.Active() && t.Status != store.Done && t.Status != StatusFailed {
			if err := errors.Join(a.Store.Release(t.ID), a.Store.SetStatus(t.ID, store.Killed)); err != nil {
				return n, err
			}
			n++
		}
	}
	if a.Tmux.HasSession() {
		if err := a.Tmux.KillSession(); err != nil {
			return n, err
		}
	}
	return n, nil
}

// Peek returns the last lines of a task's terminal.
func (a *App) Peek(task string, lines int) (string, error) {
	t, err := a.Store.Task(task)
	if err != nil {
		return "", err
	}
	if t.Window == "" || !a.Tmux.Alive(t.Window) {
		return "", fmt.Errorf("%s has no live window (status %s)", task, t.Status)
	}
	if lines <= 0 {
		lines = 40
	}
	out, err := a.Tmux.Capture(t.Window, lines)
	return strings.TrimRight(out, "\n "), err
}

// SendKeys presses keys in a task's terminal, e.g. "1" to pick a prompt option
// or "Escape". With text set, it types that text and presses Enter instead.
func (a *App) SendKeys(task, text string, keys []string) error {
	t, err := a.Store.Task(task)
	if err != nil {
		return err
	}
	if !a.ownWindow(t) {
		return fmt.Errorf("%s has no live window saddle opened for it", task)
	}
	a.Store.Event(task, "keys", text+strings.Join(keys, " "))
	if text != "" {
		return a.Tmux.SendText(t.Window, text)
	}
	return a.Tmux.SendKeys(t.Window, keys...)
}

// ClaimResult reports what a claim request got.
type ClaimResult struct {
	Granted []string          `json:"granted"`
	Denied  map[string]string `json:"denied,omitempty"`
}

// Claim grants every requested glob that no other active task overlaps.
func (a *App) Claim(task string, want []string) (ClaimResult, error) {
	res := ClaimResult{Denied: map[string]string{}}
	for i, w := range want {
		want[i] = claims.Clean(w)
	}
	granted, err := a.Store.Claim(task, func(all map[string][]string) ([]string, error) {
		c := claims.Conflicts(all, task, want)
		var g []string
		for _, w := range want {
			if owner, bad := c[w]; bad {
				res.Denied[w] = owner
			} else {
				g = append(g, w)
			}
		}
		return g, nil
	})
	res.Granted = granted
	if len(granted) > 0 {
		a.Store.Event(task, "claim", strings.Join(granted, ","))
	}
	return res, err
}

// Decision is the verdict on a write by an agent.
type Decision struct {
	Allow  bool
	Reason string
}

// CheckWrite decides whether task may write the file at abs. Paths nobody owns
// are claimed for the task on first write, so claims track real work.
func (a *App) CheckWrite(task, abs string) Decision {
	t, err := a.Store.Task(task)
	if err != nil || t.Role != store.RoleWorker || !t.Active() {
		return Decision{Allow: true}
	}
	abs = filepath.Clean(abs)
	wt := filepath.Clean(t.Worktree)
	rel, err := filepath.Rel(wt, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		if r, err := filepath.Rel(a.Root, abs); err == nil && !strings.HasPrefix(r, "..") {
			return Decision{Reason: fmt.Sprintf("%s is outside your worktree. You work in %s on branch %s; edit your copy of the file there instead.", abs, wt, t.Branch)}
		}
		return Decision{Allow: true} // outside the repo entirely (scratch files etc.)
	}
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, ".git/") {
		return Decision{Allow: true}
	}
	for _, g := range a.Cfg.Serial {
		if claims.Match(g, rel) {
			mine, _ := a.Store.Claims()
			for _, own := range mine[task] {
				if claims.Match(own, rel) {
					return Decision{Allow: true}
				}
			}
			return Decision{Reason: fmt.Sprintf("%s is a serial file (%s) owned by the merge train. Don't edit it; it is regenerated when your branch lands. If you truly need a change, say so in your done summary.", rel, g)}
		}
	}
	var reason string
	_, err = a.Store.Claim(task, func(all map[string][]string) ([]string, error) {
		for _, own := range all[task] {
			if claims.Match(own, rel) {
				return nil, nil
			}
		}
		if owner, glob := claims.Owner(all, task, rel); owner != "" {
			ot, _ := a.Store.Task(owner)
			reason = fmt.Sprintf("%s is claimed by %s (%q, claim %s). Don't edit it: work around it (depend on an interface, stub it in tests), or finish your part and mention the needed change in your done summary. The orchestrator can sequence the work.", rel, owner, ot.Title, glob)
			return nil, nil
		}
		return []string{rel}, nil
	})
	if err != nil {
		return Decision{Allow: true} // fail open
	}
	if reason != "" {
		a.Store.Event(task, "deny", rel)
		return Decision{Reason: reason}
	}
	return Decision{Allow: true}
}

// Notify queues a notice for a task. Action notices wake an idle worker by
// typing into its window; info notices arrive with its next tool call. The
// orchestrator is never typed at: the TUI delivers its notices.
func (a *App) Notify(task, kind, text string) error {
	if err := a.Store.Notify(task, kind, text); err != nil {
		return err
	}
	if kind != store.NoticeAction {
		return nil
	}
	t, err := a.Store.Task(task)
	if err != nil || t.Window == "" || !t.Active() {
		return nil
	}
	if t.Status == store.Idle || t.Status == store.Done || t.Status == store.Conflict {
		if a.ownWindow(t) {
			tmux.SendWhenIdle(a.Tmux, t.Window, "[saddle] You have new notices. Read them and act on them.", func() bool {
				n, err := a.Store.PendingNotices(task)
				return err != nil || n > 0
			})
		}
	}
	return nil
}

// Done marks a task finished and queues its branch in the merge train.
func (a *App) Done(task, summary string) error {
	t, err := a.Store.Task(task)
	if err != nil {
		return err
	}
	if t.Role != store.RoleWorker {
		return errors.New("only worker tasks can be done")
	}
	dirty, err := gitx.Dirty(t.Worktree)
	if err != nil {
		return err
	}
	if len(dirty) > 0 {
		return fmt.Errorf("worktree has uncommitted changes; commit them first:\n%s", strings.Join(dirty, "\n"))
	}
	n, err := gitx.CommitsBetween(t.Worktree, a.Cfg.Integration, "HEAD")
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("branch has no commits beyond the integration branch; nothing to land")
	}
	if summary != "" {
		if err := a.Store.SetField(task, "summary", summary); err != nil {
			return err
		}
	}
	if err := a.Store.SetStatus(task, store.Done); err != nil {
		return err
	}
	if err := a.Store.Enqueue(task); err != nil {
		return err
	}
	a.Store.Event(task, "done", summary)
	return a.Notify(OrchestratorID, store.NoticeAction,
		fmt.Sprintf("%s %q is done and queued in the merge train: %s\nRun the saddle land tool when you're ready.", task, t.Title, summary))
}

// Kill stops a task's window and releases its claims. Unless keep is set it
// removes the worktree, and the branch too when it has no commits beyond
// integration. A branch with commits, or a worktree with uncommitted
// changes, is always kept.
func (a *App) Kill(task string, keep bool) error {
	t, err := a.Store.Task(task)
	if err != nil {
		return err
	}
	if t.Window != "" && a.Tmux.Alive(t.Window) {
		if err := a.Tmux.KillWindow(t.Window); err != nil {
			return err
		}
	}
	note := ""
	if !keep && t.Role == store.RoleWorker {
		if note, err = a.cleanup(t); err != nil {
			return err
		}
	}
	if err := a.Store.Release(task); err != nil {
		return err
	}
	a.Store.Event(task, "kill", note)
	return a.Store.SetStatus(task, store.Killed)
}

// TaskForDir finds the worker task whose worktree contains dir.
func (a *App) TaskForDir(dir string) (store.Task, error) {
	top, err := gitx.Toplevel(dir)
	if err != nil {
		return store.Task{}, err
	}
	ts, err := a.Store.Tasks()
	if err != nil {
		return store.Task{}, err
	}
	for _, t := range ts {
		if t.Role == store.RoleWorker && filepath.Clean(t.Worktree) == filepath.Clean(top) {
			return t, nil
		}
	}
	return store.Task{}, fmt.Errorf("%s is not a saddle task worktree", top)
}
