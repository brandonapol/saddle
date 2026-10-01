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

// ensureIntegration creates the integration branch from base on first use.
func (a *App) ensureIntegration() error {
	if gitx.BranchExists(a.Root, a.Cfg.Integration) {
		return nil
	}
	_, err := gitx.Run(a.Root, "branch", a.Cfg.Integration, a.Cfg.Base)
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
	t = store.Task{
		ID: id, Title: r.Title, Prompt: r.Prompt, Parent: r.Parent, Role: store.RoleWorker, Model: model,
		Branch: "saddle/" + name, Worktree: a.stateDir("worktrees", name), Status: store.Running,
	}
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
			a.Store.SetStatus(id, store.Killed)
			return t, err
		}
	}
	if err := gitx.WorktreeAdd(a.Root, t.Worktree, t.Branch, base); err != nil {
		a.Store.SetStatus(id, store.Killed)
		return t, err
	}
	win, err := a.launch(t, r.Claims)
	if err != nil {
		a.Store.SetStatus(id, store.Killed)
		return t, err
	}
	t.Window = win
	a.Store.Event(id, "spawn", fmt.Sprintf("parent=%s model=%s claims=%s", r.Parent, model, strings.Join(r.Claims, ",")))
	if r.Parent != "" && r.Parent != OrchestratorID {
		a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf("%s spawned sub-task %s %q.", r.Parent, id, r.Title))
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

// Up starts the orchestrator (window 0) if it isn't running. epic, if set, is its first prompt.
func (a *App) Up(epic string) (store.Task, bool, error) {
	if err := a.Init(); err != nil {
		return store.Task{}, false, err
	}
	if err := a.ensureIntegration(); err != nil {
		return store.Task{}, false, err
	}
	t, err := a.Store.Task(OrchestratorID)
	if err == nil && t.Window != "" && a.Tmux.Alive(t.Window) {
		if epic != "" {
			return t, false, a.Tmux.SendText(t.Window, epic)
		}
		return t, false, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		t = store.Task{ID: OrchestratorID, Title: "orchestrator", Role: store.RoleOrchestrator,
			Model: a.Cfg.Claude.OrchestratorModel, Worktree: a.Root, Status: store.Running}
		if err := a.Store.CreateTask(t); err != nil {
			return t, false, err
		}
	} else if err != nil {
		return t, false, err
	}
	a.Store.SetStatus(t.ID, store.Running)
	l := agent.Launch{
		Root: a.Root, Bin: a.Bin, Task: t.ID, Title: "orchestrator", Dir: a.Root, Model: t.Model,
		Mode: a.Cfg.Claude.PermissionMode, Cmd: a.Cfg.Claude.Cmd, RunDir: a.stateDir("run", t.ID),
		Brief: a.orchestratorBrief(), Prompt: epic,
	}
	cmd, err := l.Write()
	if err != nil {
		return t, false, err
	}
	var win string
	if a.Tmux.HasSession() {
		win, err = a.Tmux.NewWindow("control", a.Root, cmd)
	} else {
		win, err = a.Tmux.NewSession("control", a.Root, cmd)
	}
	if err != nil {
		return t, false, err
	}
	t.Window = win
	return t, true, a.Store.SetField(t.ID, "window", win)
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

// Notify queues a notice for a task. Action notices wake an idle agent by
// typing into its window; info notices arrive with its next tool call.
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
		if a.Tmux.Alive(t.Window) {
			return a.Tmux.SendText(t.Window, "[saddle] You have new notices. Read them and act on them.")
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
		a.Store.SetField(task, "summary", summary)
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

// Kill stops a task's window and releases its claims. With rm, its worktree is removed too.
func (a *App) Kill(task string, rm bool) error {
	t, err := a.Store.Task(task)
	if err != nil {
		return err
	}
	if t.Window != "" && a.Tmux.Alive(t.Window) {
		a.Tmux.KillWindow(t.Window)
	}
	if rm && t.Role == store.RoleWorker {
		gitx.WorktreeRemove(a.Root, t.Worktree)
	}
	a.Store.Release(task)
	a.Store.Event(task, "kill", "")
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
